// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  LogicalStream – represents one local TCP connection being chunked
//           and distributed across the TunnelPool.
//
// Lifecycle:
//   1. MultipathDispatcher.handleConn() creates a LogicalStream for each
//      accepted local connection.
//   2. LogicalStream.Run() starts two goroutines:
//        • runOutbound: reads from localConn → chunks → dispatches macro frames
//        • runInbound:  drains inbound chan   → writes to localConn
//   3. Either goroutine exiting triggers closeOnce, which closes localConn and
//      signals done, causing the other goroutine to exit.
//   4. MultipathManager.CloseStream(id) removes the stream from the registry.
//
// Macro-level sequence numbering:
//   Every chunk carries GlobalSeq = the byte-offset of the chunk's first byte
//   within this logical stream.  The remote Aggregator uses this to reconstruct
//   the original byte order even though chunks may arrive via different bearer
//   tunnels in a different order.
//
//   This is the KEY extension on top of the existing per-tunnel ARQ:
//     per-tunnel ARQ seqnum (uint16) → handles reliability within ONE tunnel
//     LogicalStream.seq (uint64)     → handles ordering ACROSS all 5 tunnels
// ==============================================================================

package multipath

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// LogicalStream
// ──────────────────────────────────────────────────────────────────────────────

// LogicalStream wraps one local net.Conn (e.g. from a browser or CLI tool) and
// provides the outbound chunk-dispatch and inbound reassembly plumbing.
type LogicalStream struct {
	// id is the globally unique stream identifier embedded in every macro frame.
	id uint32

	// localConn is the TCP connection accepted from the local application.
	localConn net.Conn

	// pool is the bearer pool; LogicalStream borrows it (does not own it).
	pool *TunnelPool

	// cfg is a pointer to the shared configuration; never mutated after init.
	cfg *MultipathConfig

	// log is the logger.
	log Logger

	// seq tracks the next byte-offset to assign to the next outbound chunk.
	seq ByteSequencer

	// inbound carries raw payload bytes received from the Aggregator and
	// destined for localConn.  Buffered to absorb small bursts.
	inbound chan []byte

	// done is closed by closeOnce to signal all goroutines to exit.
	done chan struct{}

	// closeOnce ensures localConn is closed and done is closed exactly once.
	closeOnce sync.Once

	// targetAddr is the upstream host:port to reach through the Aggregator.
	// Encoded in the SYN frame payload so the Aggregator knows where to dial.
	// Format: "host:port" (e.g. "93.184.216.34:443" or "example.com:443").
	targetAddr string

	// createdAt is used for telemetry / timeout checks.
	createdAt time.Time
}

// NewLogicalStream constructs a LogicalStream.  It does not start any
// goroutines; call Run() for that.
//
// targetAddr is the upstream "host:port" the Aggregator should dial when it
// receives the SYN frame.  It is encoded verbatim in the SYN payload.
func NewLogicalStream(
	id uint32,
	localConn net.Conn,
	pool *TunnelPool,
	cfg *MultipathConfig,
	log Logger,
	targetAddr string,
) *LogicalStream {
	return &LogicalStream{
		id:         id,
		localConn:  localConn,
		pool:       pool,
		cfg:        cfg,
		log:        log,
		targetAddr: targetAddr,
		inbound:    make(chan []byte, 256),
		done:       make(chan struct{}),
		createdAt:  time.Now(),
	}
}

// ID returns the stream's unique 32-bit identifier.
func (s *LogicalStream) ID() uint32 { return s.id }

// ──────────────────────────────────────────────────────────────────────────────
// Run – main entry point
// ──────────────────────────────────────────────────────────────────────────────

// Run starts outbound and inbound goroutines and blocks until both exit.
// It is the caller's responsibility to call Close() and remove the stream from
// the registry after Run returns.
func (s *LogicalStream) Run() {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		s.runOutbound()
	}()

	go func() {
		defer wg.Done()
		s.runInbound()
	}()

	wg.Wait()
}

// Close signals the stream to shut down immediately.  Safe to call multiple
// times and from any goroutine.
func (s *LogicalStream) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.localConn.Close()
	})
}

// DeliverInbound queues payload bytes received from the Aggregator.
// Called by the MultipathManager's inbound demux goroutine.
func (s *LogicalStream) DeliverInbound(payload []byte) {
	if len(payload) == 0 {
		return
	}
	// Copy to avoid aliasing the InboundFrame buffer.
	data := make([]byte, len(payload))
	copy(data, payload)

	select {
	case s.inbound <- data:
	case <-s.done:
	default:
		// Inbound channel full: drop and log.  The Aggregator's own flow
		// control should prevent this in normal operation.
		s.log.Warnf("[Stream %d] inbound channel full – dropping %d bytes", s.id, len(data))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Outbound: localConn → macro frames → bearer pool
// ──────────────────────────────────────────────────────────────────────────────

func (s *LogicalStream) runOutbound() {
	defer s.Close()

	// Announce the new stream to the Aggregator, encoding the target address
	// in the SYN payload so the Aggregator knows where to dial.
	synPayload := EncodeTarget(s.targetAddr)
	synFrame := BuildFrame(s.id, 0, FlagSYN, synPayload)
	if err := s.dispatchWithRetry(synFrame); err != nil {
		s.log.Warnf("[Stream %d] SYN dispatch failed (target %s): %v", s.id, s.targetAddr, err)
		return
	}
	s.log.Debugf("[Stream %d] SYN sent → %s", s.id, s.targetAddr)

	chunkSize := s.cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 1024
	}

	buf := make([]byte, chunkSize)

	for {
		n, err := s.localConn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			// Reserve byte-offset BEFORE building the frame so that
			// concurrent streams never share the same offset.
			offset := s.seq.Reserve(uint64(n))
			frame := BuildFrame(s.id, offset, 0, chunk)

			if dispatchErr := s.dispatchWithRetry(frame); dispatchErr != nil {
				s.log.Warnf("[Stream %d] chunk dispatch failed at offset %d: %v",
					s.id, offset, dispatchErr)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Debugf("[Stream %d] local read: %v", s.id, err)
			}
			break
		}
	}

	// Notify the Aggregator that no more data will be sent from this side.
	finFrame := BuildFINFrame(s.id, s.seq.Current())
	_ = s.dispatchWithRetry(finFrame)
	s.log.Debugf("[Stream %d] FIN sent (total bytes dispatched: %d)", s.id, s.seq.Current())
}

// ──────────────────────────────────────────────────────────────────────────────
// Inbound: inbound chan → localConn
// ──────────────────────────────────────────────────────────────────────────────

func (s *LogicalStream) runInbound() {
	defer s.Close()

	for {
		select {
		case data, ok := <-s.inbound:
			if !ok {
				return
			}
			if _, err := s.localConn.Write(data); err != nil {
				s.log.Debugf("[Stream %d] local write: %v", s.id, err)
				return
			}
		case <-s.done:
			return
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Dispatch with retry
// ──────────────────────────────────────────────────────────────────────────────

// dispatchWithRetry tries to send frame on a healthy bearer.
// It attempts up to cfg.DispatchRetries different bearers before giving up.
// 0 retries means "try every bearer at least once".
func (s *LogicalStream) dispatchWithRetry(frame []byte) error {
	maxAttempts := s.cfg.DispatchRetries
	if maxAttempts <= 0 {
		maxAttempts = s.pool.TotalCount()
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		bearer, err := s.pool.Pick()
		if err != nil {
			// No healthy bearers at all.
			return err
		}
		if err := bearer.SendFrame(frame); err == nil {
			return nil
		} else {
			lastErr = err
			s.log.Debugf("[Stream %d] bearer %s send failed: %v – retrying",
				s.id, bearer.Label(), err)
		}
	}
	return lastErr
}
