// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  BearerTunnel – one persistent TCP connection from the local machine
//           through a single DNS-tunnel SOCKS5 proxy to the remote Aggregator.
//
// Architecture position:
//
//   LogicalStream → [pick bearer] → BearerTunnel.SendFrame()
//                                         │  (single TCP conn through DNS tunnel)
//                                         ▼
//                                   Aggregator service
//                                         │  (response frames flow back)
//                                         ▼
//                             BearerTunnel.readLoop() → inboundCh → Manager
//
// Each BearerTunnel runs a background goroutine (runLoop) that:
//   1. Dials the DNS-tunnel SOCKS5 proxy and issues CONNECT to AggregatorAddr.
//   2. Reads incoming MacroFrames and enqueues them to a shared inbound channel.
//   3. On read error, marks itself dead and re-dials after ReconnectDelay.
//
// Writes are serialised by writeMu so that concurrent goroutines (one per
// LogicalStream) never interleave frame bytes on the shared TCP pipe.
// ==============================================================================

package multipath

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Bearer state
// ──────────────────────────────────────────────────────────────────────────────

type bearerState uint32

const (
	stateConnecting bearerState = iota
	stateConnected
	stateDead
)

// ──────────────────────────────────────────────────────────────────────────────
// InboundFrame
// ──────────────────────────────────────────────────────────────────────────────

// InboundFrame carries a fully decoded macro frame received from the Aggregator
// along with a back-reference to the BearerTunnel it arrived on.
type InboundFrame struct {
	Header  MacroFrameHeader
	Payload []byte
	From    *BearerTunnel
}

// ──────────────────────────────────────────────────────────────────────────────
// BearerTunnel
// ──────────────────────────────────────────────────────────────────────────────

// BearerTunnel maintains a single TCP connection to the Aggregator service
// routed through one DNS-tunnel SOCKS5 proxy.  It is the physical carrier for
// all macro frames assigned to it by the TunnelPool dispatcher.
type BearerTunnel struct {
	endpoint    TunnelEndpoint
	aggAddr     string
	dialTimeout time.Duration
	reconnDelay time.Duration
	log         Logger

	// inboundCh is the shared channel used to deliver received frames to the
	// MultipathManager's demux loop.
	inboundCh chan<- InboundFrame

	// connMu guards conn; held only during swaps, not during I/O.
	connMu sync.RWMutex
	conn   net.Conn

	// writeMu serialises multi-byte frame writes so concurrent LogicalStream
	// goroutines never interleave bytes on the TCP stream.
	writeMu sync.Mutex

	// state is one of stateConnecting / stateConnected / stateDead.
	state atomic.Uint32

	// quit is closed by Stop() to signal the runLoop to exit permanently.
	quit   chan struct{}
	closed atomic.Bool

	// Telemetry counters (read-only from outside via accessors).
	framesSent atomic.Uint64
	bytesSent  atomic.Uint64
	bytesRecvd atomic.Uint64
}

// NewBearerTunnel constructs a BearerTunnel.
// Call Start() to begin the background connect-and-read loop.
//
// Parameters:
//   - ep          : DNS-tunnel SOCKS5 endpoint and label
//   - aggAddr     : remote Aggregator host:port
//   - dialTimeout : SOCKS5 connect + handshake deadline
//   - reconnDelay : pause between reconnection attempts
//   - inboundCh   : shared channel; caller owns it
//   - log         : logger (must not be nil)
func NewBearerTunnel(
	ep TunnelEndpoint,
	aggAddr string,
	dialTimeout, reconnDelay time.Duration,
	inboundCh chan<- InboundFrame,
	log Logger,
) *BearerTunnel {
	return &BearerTunnel{
		endpoint:    ep,
		aggAddr:     aggAddr,
		dialTimeout: dialTimeout,
		reconnDelay: reconnDelay,
		inboundCh:   inboundCh,
		quit:        make(chan struct{}),
		log:         log,
	}
}

// Start launches the background reconnect + read loop.
// It is non-blocking and idempotent.
func (b *BearerTunnel) Start() {
	go b.runLoop()
}

// Stop permanently shuts down this bearer tunnel.
// Subsequent calls are no-ops.
func (b *BearerTunnel) Stop() {
	if b.closed.CompareAndSwap(false, true) {
		close(b.quit)
		b.dropConn()
	}
}

// Label returns the human-readable name of this tunnel endpoint.
func (b *BearerTunnel) Label() string { return b.endpoint.Label }

// IsHealthy reports whether the bearer currently has an active connection.
func (b *BearerTunnel) IsHealthy() bool {
	return bearerState(b.state.Load()) == stateConnected
}

// FramesSent returns the total number of macro frames written to this bearer.
func (b *BearerTunnel) FramesSent() uint64 { return b.framesSent.Load() }

// BytesSent returns the total bytes written (header + payload) to this bearer.
func (b *BearerTunnel) BytesSent() uint64 { return b.bytesSent.Load() }

// BytesRecvd returns the total bytes read from this bearer.
func (b *BearerTunnel) BytesRecvd() uint64 { return b.bytesRecvd.Load() }

// ──────────────────────────────────────────────────────────────────────────────
// Frame transmission
// ──────────────────────────────────────────────────────────────────────────────

// SendFrame writes a pre-built macro frame atomically to the TCP connection.
// If the tunnel is not connected it returns an error immediately so the
// TunnelPool can retry on a different bearer.
//
// The write deadline is set per-call to 15 s; callers should not rely on
// any specific value.
func (b *BearerTunnel) SendFrame(frame []byte) error {
	if !b.IsHealthy() {
		return errors.New("bearer " + b.endpoint.Label + ": not connected")
	}

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	b.connMu.RLock()
	conn := b.conn
	b.connMu.RUnlock()

	if conn == nil {
		return errors.New("bearer " + b.endpoint.Label + ": conn is nil")
	}

	if err := conn.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		b.markDead(conn)
		return err
	}
	n, err := conn.Write(frame)
	_ = conn.SetWriteDeadline(time.Time{})

	if err != nil {
		b.markDead(conn)
		return err
	}

	b.framesSent.Add(1)
	b.bytesSent.Add(uint64(n))
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Background loops
// ──────────────────────────────────────────────────────────────────────────────

func (b *BearerTunnel) runLoop() {
	for {
		select {
		case <-b.quit:
			return
		default:
		}

		b.state.Store(uint32(stateConnecting))
		b.log.Infof("[Bearer:%s] Dialing SOCKS5 %s → Aggregator %s",
			b.endpoint.Label, b.endpoint.SOCKS5Addr, b.aggAddr)

		conn, err := dialSOCKS5(b.endpoint.SOCKS5Addr, b.aggAddr, b.dialTimeout)
		if err != nil {
			b.log.Warnf("[Bearer:%s] Connection failed: %v — retry in %s",
				b.endpoint.Label, err, b.reconnDelay)
			b.sleepOrQuit(b.reconnDelay)
			continue
		}

		b.connMu.Lock()
		b.conn = conn
		b.connMu.Unlock()
		b.state.Store(uint32(stateConnected))
		b.log.Infof("[Bearer:%s] Connected", b.endpoint.Label)

		b.readLoop(conn)

		if b.closed.Load() {
			return
		}

		b.log.Warnf("[Bearer:%s] Connection lost — retry in %s",
			b.endpoint.Label, b.reconnDelay)
		b.markDead(conn)
		b.sleepOrQuit(b.reconnDelay)
	}
}

func (b *BearerTunnel) readLoop(conn net.Conn) {
	for {
		hdr, err := DecodeFrameHeader(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !b.closed.Load() {
				b.log.Warnf("[Bearer:%s] Frame header read error: %v", b.endpoint.Label, err)
			}
			return
		}

		var payload []byte
		if hdr.PayloadLen > 0 {
			payload = make([]byte, hdr.PayloadLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				b.log.Warnf("[Bearer:%s] Payload read error: %v", b.endpoint.Label, err)
				return
			}
		}

		b.bytesRecvd.Add(uint64(MacroFrameHeaderSize) + uint64(hdr.PayloadLen))

		select {
		case b.inboundCh <- InboundFrame{Header: hdr, Payload: payload, From: b}:
		case <-b.quit:
			return
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

func (b *BearerTunnel) markDead(conn net.Conn) {
	b.state.Store(uint32(stateDead))
	b.connMu.Lock()
	if b.conn == conn {
		_ = conn.Close()
		b.conn = nil
	}
	b.connMu.Unlock()
}

func (b *BearerTunnel) dropConn() {
	b.connMu.Lock()
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	b.connMu.Unlock()
	b.state.Store(uint32(stateDead))
}

func (b *BearerTunnel) sleepOrQuit(d time.Duration) {
	select {
	case <-time.After(d):
	case <-b.quit:
	}
}
