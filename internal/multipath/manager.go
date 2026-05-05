// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  MultipathManager – top-level orchestrator.
//
// Responsibilities:
//   1. Build and own the BearerTunnel slice and TunnelPool.
//   2. Maintain the stream registry (streamID → *LogicalStream).
//   3. Run the inbound demultiplexer: reads InboundFrames from all bearers'
//      shared channel and routes them to the correct LogicalStream.
//   4. Expose NewStream() so the Dispatcher can create streams on demand.
//
// The Manager deliberately does NOT manage the DNS-tunnel client.Client
// instances – those are assumed to already be running as independent
// processes / goroutines (each with their own SOCKS5 listener).
// ==============================================================================

package multipath

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
)

// ──────────────────────────────────────────────────────────────────────────────
// MultipathManager
// ──────────────────────────────────────────────────────────────────────────────

// MultipathManager is the root object of the multipath overlay.
// Construct one with NewMultipathManager, call Start, and then use
// NewStream to create streams for each accepted local connection.
type MultipathManager struct {
	cfg MultipathConfig
	log Logger

	// pool owns the bearer collection and the weighted round-robin logic.
	pool *TunnelPool

	// bearers is the flat list of all BearerTunnel instances; used for
	// lifecycle management (Start/Stop) and telemetry.
	bearers []*BearerTunnel

	// streamRegistry maps streamID (uint32) → *LogicalStream.
	// Use sync.Map for low-contention concurrent access across many streams.
	streamRegistry sync.Map

	// streamIDCtr issues monotonically increasing stream identifiers.
	streamIDCtr StreamIDCounter

	// inboundCh receives decoded InboundFrames from all bearer read loops.
	inboundCh chan InboundFrame

	// started prevents double-Start.
	started atomic.Bool

	// cancel shuts down the inbound demux goroutine.
	cancel context.CancelFunc

	// wg waits for the demux goroutine to exit before Stop() returns.
	wg sync.WaitGroup
}

// NewMultipathManager builds a MultipathManager from the provided config.
// It does not start any goroutines; call Start(ctx) for that.
func NewMultipathManager(cfg MultipathConfig, log Logger) *MultipathManager {
	if cfg.InboundChannelDepth <= 0 {
		cfg.InboundChannelDepth = 2048
	}

	inboundCh := make(chan InboundFrame, cfg.InboundChannelDepth)

	// Build bearer tunnels (one per configured endpoint).
	bearers := make([]*BearerTunnel, 0, len(cfg.Tunnels))
	for _, ep := range cfg.Tunnels {
		b := NewBearerTunnel(
			ep,
			cfg.AggregatorAddr,
			cfg.DialTimeout,
			cfg.ReconnectDelay,
			inboundCh,
			log,
		)
		bearers = append(bearers, b)
	}

	pool := NewTunnelPool(bearers, inboundCh)

	return &MultipathManager{
		cfg:       cfg,
		log:       log,
		pool:      pool,
		bearers:   bearers,
		inboundCh: inboundCh,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Lifecycle
// ──────────────────────────────────────────────────────────────────────────────

// Start connects all bearer tunnels and launches the inbound demux goroutine.
// Subsequent calls after the first are no-ops.
func (m *MultipathManager) Start(ctx context.Context) {
	if !m.started.CompareAndSwap(false, true) {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	// Launch all bearer background loops.
	m.pool.Start()

	// Launch the inbound demux goroutine.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.demuxLoop(ctx)
	}()

	m.log.Infof("[MultipathManager] Started: %d tunnels → Aggregator %s",
		len(m.bearers), m.cfg.AggregatorAddr)
}

// Stop shuts down all bearer tunnels and waits for the demux goroutine to exit.
func (m *MultipathManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.pool.Stop()
	m.wg.Wait()

	// Close all remaining logical streams.
	m.streamRegistry.Range(func(key, value any) bool {
		if s, ok := value.(*LogicalStream); ok {
			s.Close()
		}
		m.streamRegistry.Delete(key)
		return true
	})

	m.log.Infof("[MultipathManager] Stopped")
}

// ──────────────────────────────────────────────────────────────────────────────
// Stream management
// ──────────────────────────────────────────────────────────────────────────────

// NewStream creates a LogicalStream for the given local TCP connection,
// registers it in the stream table, and returns it ready for Run().
//
// targetAddr is the upstream "host:port" the Aggregator should dial when it
// receives the SYN frame for this stream.  Example: "example.com:443".
func (m *MultipathManager) NewStream(conn net.Conn, targetAddr string) *LogicalStream {
	id := m.streamIDCtr.NextID()
	cfg := m.cfg // copy; safe because MultipathConfig holds no pointers
	s := NewLogicalStream(id, conn, m.pool, &cfg, m.log, targetAddr)
	m.streamRegistry.Store(id, s)
	return s
}

// CloseStream removes a stream from the registry and closes it.
// Safe to call after LogicalStream.Run() has returned.
func (m *MultipathManager) CloseStream(id uint32) {
	if v, ok := m.streamRegistry.LoadAndDelete(id); ok {
		if s, ok := v.(*LogicalStream); ok {
			s.Close()
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Telemetry
// ──────────────────────────────────────────────────────────────────────────────

// HealthStatus returns (healthy, total) bearer counts.
func (m *MultipathManager) HealthStatus() (healthy, total int) {
	return m.pool.HealthyCount(), m.pool.TotalCount()
}

// ActiveStreamCount returns the number of currently registered streams.
func (m *MultipathManager) ActiveStreamCount() int {
	count := 0
	m.streamRegistry.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// Pool returns the underlying TunnelPool (read-only use by tests/tooling).
func (m *MultipathManager) Pool() *TunnelPool { return m.pool }

// ──────────────────────────────────────────────────────────────────────────────
// Inbound demultiplexer
// ──────────────────────────────────────────────────────────────────────────────

// demuxLoop reads InboundFrames from the shared channel and routes each one to
// the LogicalStream identified by the frame's StreamID field.
//
// Frame routing rules:
//
//	RST  → close and deregister the stream immediately
//	FIN  → deliver any final payload, then close and deregister
//	data → deliver payload to stream's inbound chan
func (m *MultipathManager) demuxLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case frame, ok := <-m.inboundCh:
			if !ok {
				return
			}
			m.routeFrame(frame)
		}
	}
}

func (m *MultipathManager) routeFrame(f InboundFrame) {
	hdr := f.Header

	// RST: tear down the stream immediately.
	if hdr.HasFlag(FlagRST) {
		m.log.Debugf("[Manager] RST received for stream %d", hdr.StreamID)
		m.CloseStream(hdr.StreamID)
		return
	}

	v, ok := m.streamRegistry.Load(hdr.StreamID)
	if !ok {
		// Unknown stream – Aggregator may have sent data after we already
		// cleaned up locally.  Ignore silently.
		return
	}
	s := v.(*LogicalStream)

	// Deliver any payload before acting on FIN.
	if len(f.Payload) > 0 {
		s.DeliverInbound(f.Payload)
	}

	// FIN: no more data from Aggregator side – close and deregister.
	if hdr.HasFlag(FlagFIN) {
		m.log.Debugf("[Manager] FIN received for stream %d", hdr.StreamID)
		m.CloseStream(hdr.StreamID)
	}
}
