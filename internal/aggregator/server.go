// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  AggregatorServer – TCP listener that accepts bearer connections
//           and wires them into the StreamRouter pipeline.
//
// Each accepted connection becomes one InboundBearer.  A single background
// goroutine (routeLoop) drains the shared AggFrame channel and calls
// router.Route() to dispatch each frame.
//
// Threading model:
//   - One goroutine per InboundBearer (readLoop) – reads frames, writes to frameCh
//   - One routeLoop goroutine – drains frameCh, calls router.Route()
//   - Per-AggregatorStream goroutines (pumpUpstream + pumpResponse)
//
// This keeps the routing logic single-threaded (no lock contention on the
// stream table hot path) while all I/O is fully parallel.
// ==============================================================================

package aggregator

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
)

// ──────────────────────────────────────────────────────────────────────────────
// AggregatorServer
// ──────────────────────────────────────────────────────────────────────────────

// AggregatorServer is the top-level server object.
// Construct with NewAggregatorServer, then call Run(ctx).
type AggregatorServer struct {
	cfg      AggregatorConfig
	log      Logger
	registry *BearerRegistry
	router   *StreamRouter

	// frameCh is the shared inbound frame channel.
	// All InboundBearers write decoded frames here.
	// The single routeLoop goroutine reads from it.
	frameCh chan AggFrame

	// bearerIDCtr issues monotonically increasing bearer IDs.
	bearerIDCtr atomic.Uint32

	// listener is the active TCP listener; set during Run().
	listener net.Listener

	// running tracks whether the listener is active.
	running atomic.Bool

	// wg waits for the routeLoop goroutine.
	wg sync.WaitGroup
}

// NewAggregatorServer constructs a server.  Call Run(ctx) to start it.
func NewAggregatorServer(cfg AggregatorConfig, log Logger) *AggregatorServer {
	depth := cfg.InboundChannelDepth
	if depth <= 0 {
		depth = DefaultInboundChanDepth
	}

	registry := NewBearerRegistry()
	frameCh := make(chan AggFrame, depth)

	// The StreamRouter needs access to the registry internals (for bearer sticky
	// routing cleanup).  Pass the registry so the router can read bearer lists.
	router := NewStreamRouter(&cfg, registry, log)

	return &AggregatorServer{
		cfg:      cfg,
		log:      log,
		registry: registry,
		router:   router,
		frameCh:  frameCh,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Run – main blocking entry point
// ──────────────────────────────────────────────────────────────────────────────

// Run binds to cfg.ListenAddr, starts the route goroutine, and accepts bearer
// connections until ctx is cancelled.
// Returns nil on clean shutdown, or an error if the listener fails.
func (s *AggregatorServer) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.listener = ln
	s.running.Store(true)

	s.log.Infof("[Aggregator] Listening on %s", s.cfg.ListenAddr)

	// Close the listener when ctx is done.
	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	// Start the single route goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.routeLoop(ctx)
	}()

	// Accept loop.
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !s.running.Load() {
				// Clean shutdown.
				break
			}
			s.log.Errorf("[Aggregator] Accept error: %v", err)
			break
		}
		s.spawnBearer(conn)
	}

	// Wait for the route goroutine to exit.
	s.wg.Wait()

	// Close all remaining streams.
	s.router.CloseAll()

	s.log.Infof("[Aggregator] Stopped")
	return nil
}

// Stop closes the listener, causing Run() to return.
func (s *AggregatorServer) Stop() {
	if s.running.CompareAndSwap(true, false) {
		if s.listener != nil {
			_ = s.listener.Close()
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Bearer spawning
// ──────────────────────────────────────────────────────────────────────────────

// spawnBearer wraps the accepted conn in an InboundBearer and starts it.
// The bearer's readLoop writes frames to s.frameCh via a local shim.
func (s *AggregatorServer) spawnBearer(conn net.Conn) {
	id := s.bearerIDCtr.Add(1)

	// Build a RouterShim that writes AggFrames to our frameCh instead of
	// calling the router directly (keeping routing single-threaded).
	shim := &routerShim{frameCh: s.frameCh}

	b := NewInboundBearer(id, conn, shim, s.registry, s.log)
	b.Start()
	s.log.Infof("[Aggregator] Bearer %d accepted from %s", id, conn.RemoteAddr())
}

// ──────────────────────────────────────────────────────────────────────────────
// Route loop
// ──────────────────────────────────────────────────────────────────────────────

// routeLoop is the single goroutine that reads AggFrames from frameCh and
// dispatches them via the StreamRouter.
func (s *AggregatorServer) routeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Drain any remaining frames before exiting.
			for {
				select {
				case f := <-s.frameCh:
					s.router.Route(f)
				default:
					return
				}
			}
		case f, ok := <-s.frameCh:
			if !ok {
				return
			}
			s.router.Route(f)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Telemetry
// ──────────────────────────────────────────────────────────────────────────────

// ActiveStreams returns the number of currently active logical streams.
func (s *AggregatorServer) ActiveStreams() int { return s.router.ActiveCount() }

// LiveBearers returns the number of currently connected bearer tunnels.
func (s *AggregatorServer) LiveBearers() int { return s.registry.LiveCount() }

// ──────────────────────────────────────────────────────────────────────────────
// routerShim – adapts InboundBearer's router interface to a channel write
// ──────────────────────────────────────────────────────────────────────────────

// InboundBearer calls router.Route(f) from its readLoop goroutine.
// routerShim redirects that call into the shared frameCh, keeping all routing
// on the single routeLoop goroutine.
type routerShim struct {
	frameCh chan<- AggFrame
}

func (rs *routerShim) Route(f AggFrame) {
	// Non-blocking send: if the channel is full, drop and let the bearer
	// recover.  In practice the channel is deep (4096 frames) and the route
	// loop is fast, so this should never fire in normal operation.
	select {
	case rs.frameCh <- f:
	default:
		// Channel full; frame dropped.  Log would be noisy here; the stream
		// will either time out or recover on the next frame.
	}
}
