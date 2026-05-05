// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  AggregatorStream – per-logical-stream proxy between the reassembled
//           client byte stream and the upstream TCP target connection.
//
// Lifecycle (per stream):
//
//   1. StreamRouter receives a SYN frame.
//   2. StreamRouter calls NewAggregatorStream() with the parsed target addr.
//   3. StreamRouter calls stream.Start() which:
//        a. Dials the upstream TCP target (non-blocking, in a goroutine).
//        b. Starts pumpUpstream: Reassembler.Out() → upstream.Write()
//        c. Starts pumpResponse: upstream.Read() → chunked MacroFrames → bearer
//   4. Subsequent data frames are delivered via stream.Push(globalSeq, payload).
//   5. On FIN or RST, stream.Close() is called, terminating both goroutines.
//
// Response direction framing:
//   The response pump assigns its own GlobalSeq (byte-offset) to response chunks
//   and always writes back on the "sticky" bearer (the one that delivered the SYN)
//   via registry.PickFor(streamID).  This guarantees response frame ordering on
//   the client side without a response-direction reassembler.
//
// Error handling:
//   Any I/O error on either the upstream conn or the bearer causes Close() to
//   be called.  The StreamRouter's close callback deregisters the stream from
//   the routing table.  An RST frame is sent back to the client if possible.
// ==============================================================================

package aggregator

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

// ──────────────────────────────────────────────────────────────────────────────
// ByteSequencer for response direction
// ──────────────────────────────────────────────────────────────────────────────

// responseSeq is a simple atomic uint64 that assigns byte-offsets in the
// response direction.  It is private to AggregatorStream.
type responseSeq struct{ v atomic.Uint64 }

func (s *responseSeq) Reserve(n uint64) uint64 { return s.v.Add(n) - n }
func (s *responseSeq) Current() uint64         { return s.v.Load() }

// ──────────────────────────────────────────────────────────────────────────────
// AggregatorStream
// ──────────────────────────────────────────────────────────────────────────────

// AggregatorStream represents one logical stream on the Aggregator side.
// It is the counterpart to the client's LogicalStream.
type AggregatorStream struct {
	// id is the stream identifier from the SYN frame.
	id uint32

	// targetAddr is the upstream host:port to dial (from the SYN payload).
	targetAddr string

	// cfg is the Aggregator configuration.
	cfg *AggregatorConfig

	// log is the logger.
	log Logger

	// registry is used to pick a bearer for sending response frames.
	registry *BearerRegistry

	// reassembler reorders incoming client→upstream data.
	reassembler *Reassembler

	// upstream is the dialled TCP connection to the target host.
	// Set by the dial goroutine; nil until the dial succeeds.
	upstream      net.Conn
	upstreamOnce  sync.Once
	upstreamReady chan struct{} // closed once upstream is dialled

	// seq assigns byte-offsets to response (upstream→client) chunks.
	seq responseSeq

	// done is closed when the stream shuts down.
	done      chan struct{}
	closeOnce sync.Once

	// onClose is called by Close() to notify the StreamRouter.
	onClose func(id uint32)

	// wg tracks the pumpUpstream + pumpResponse goroutines.
	wg sync.WaitGroup

	// Telemetry.
	bytesFromClient atomic.Uint64
	bytesToClient   atomic.Uint64
	createdAt       time.Time
}

// NewAggregatorStream creates a stream but does NOT start its goroutines.
// Call Start() for that.
func NewAggregatorStream(
	id uint32,
	targetAddr string,
	cfg *AggregatorConfig,
	registry *BearerRegistry,
	log Logger,
	onClose func(id uint32),
) *AggregatorStream {
	return &AggregatorStream{
		id:            id,
		targetAddr:    targetAddr,
		cfg:           cfg,
		log:           log,
		registry:      registry,
		reassembler:   NewReassembler(512, cfg.MaxReassemblyBuffer),
		upstreamReady: make(chan struct{}),
		done:          make(chan struct{}),
		onClose:       onClose,
		createdAt:     time.Now(),
	}
}

// ID returns the stream identifier.
func (s *AggregatorStream) ID() uint32 { return s.id }

// ──────────────────────────────────────────────────────────────────────────────
// Lifecycle
// ──────────────────────────────────────────────────────────────────────────────

// Start dials the upstream target and launches the pumpUpstream / pumpResponse
// goroutines.  Returns immediately; the dial happens in a goroutine.
func (s *AggregatorStream) Start() {
	s.wg.Add(2)
	go s.dialAndPumpUpstream()
	go s.pumpResponse()
}

// Close tears down the stream.  Safe to call multiple times from any goroutine.
func (s *AggregatorStream) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.upstream != nil {
			_ = s.upstream.Close()
		}
		if s.onClose != nil {
			s.onClose(s.id)
		}
	})
}

// Wait blocks until both goroutines have exited.
func (s *AggregatorStream) Wait() {
	s.wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Push – deliver a client data frame to the reassembler
// ──────────────────────────────────────────────────────────────────────────────

// Push feeds a decoded payload into the reassembler.
// Must be called from a single goroutine (the route loop).
func (s *AggregatorStream) Push(globalSeq uint64, payload []byte) {
	if err := s.reassembler.Push(globalSeq, payload); err != nil {
		if errors.Is(err, ErrReassemblyOverflow) {
			s.log.Warnf("[AggStream %d] reassembly overflow – resetting stream", s.id)
			s.sendRST()
			s.Close()
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Dial + pumpUpstream goroutine
// ──────────────────────────────────────────────────────────────────────────────

// dialAndPumpUpstream dials the upstream target then drains the Reassembler
// output channel, writing each chunk to the upstream connection.
func (s *AggregatorStream) dialAndPumpUpstream() {
	defer s.wg.Done()
	defer s.Close()

	// Dial upstream.
	timeout := s.cfg.UpstreamDialTimeout
	if timeout <= 0 {
		timeout = DefaultUpstreamDialTimeout
	}

	conn, err := net.DialTimeout("tcp", s.targetAddr, timeout)
	if err != nil {
		s.log.Warnf("[AggStream %d] upstream dial %s failed: %v", s.id, s.targetAddr, err)
		s.sendRST()
		return
	}

	s.upstreamOnce.Do(func() {
		s.upstream = conn
		close(s.upstreamReady)
	})
	defer conn.Close()

	s.log.Debugf("[AggStream %d] upstream connected to %s", s.id, s.targetAddr)

	// Drain reassembler → write to upstream.
	for {
		select {
		case chunk, ok := <-s.reassembler.Out():
			if !ok {
				return
			}
			if _, err := conn.Write(chunk); err != nil {
				if !isNetClosedError(err) {
					s.log.Debugf("[AggStream %d] upstream write: %v", s.id, err)
				}
				return
			}
			s.bytesFromClient.Add(uint64(len(chunk)))

		case <-s.done:
			return
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// pumpResponse goroutine
// ──────────────────────────────────────────────────────────────────────────────

// pumpResponse waits until the upstream connection is ready, then reads response
// bytes, chunks them, and sends MacroFrames back to the client via a bearer.
func (s *AggregatorStream) pumpResponse() {
	defer s.wg.Done()
	defer s.Close()

	// Wait for the upstream dial to complete (or for the stream to be closed).
	select {
	case <-s.upstreamReady:
		// Good – upstream is available.
	case <-s.done:
		return
	}

	chunkSize := s.cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	buf := make([]byte, chunkSize)

	for {
		n, err := s.upstream.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			// Assign a byte-offset for this response chunk.
			offset := s.seq.Reserve(uint64(n))

			frame := multipath.BuildFrame(s.id, offset, 0, chunk)
			if sendErr := s.sendFrame(frame); sendErr != nil {
				s.log.Warnf("[AggStream %d] response send failed: %v", s.id, sendErr)
				return
			}
			s.bytesToClient.Add(uint64(n))
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && !isNetClosedError(err) {
				s.log.Debugf("[AggStream %d] upstream read: %v", s.id, err)
			}
			break
		}
	}

	// Upstream closed cleanly – send FIN back to client.
	finFrame := multipath.BuildFINFrame(s.id, s.seq.Current())
	_ = s.sendFrame(finFrame)
	s.log.Debugf("[AggStream %d] FIN sent (response bytes: %d)", s.id, s.seq.Current())
}

// ──────────────────────────────────────────────────────────────────────────────
// Frame send helpers
// ──────────────────────────────────────────────────────────────────────────────

// sendFrame picks a bearer (sticky by stream ID) and sends frame.
// Tries up to 3 different bearers before returning an error.
func (s *AggregatorStream) sendFrame(frame []byte) error {
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		b, err := s.registry.PickFor(s.id)
		if err != nil {
			return err
		}
		if err := b.SendFrame(frame); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// sendRST sends an RST frame back to the client.  Best-effort: errors ignored.
func (s *AggregatorStream) sendRST() {
	rstFrame := multipath.BuildRSTFrame(s.id)
	b, err := s.registry.PickFor(s.id)
	if err != nil {
		return
	}
	_ = b.SendFrame(rstFrame)
}

// ──────────────────────────────────────────────────────────────────────────────
// Telemetry
// ──────────────────────────────────────────────────────────────────────────────

func (s *AggregatorStream) BytesFromClient() uint64 { return s.bytesFromClient.Load() }
func (s *AggregatorStream) BytesToClient() uint64   { return s.bytesToClient.Load() }
func (s *AggregatorStream) TargetAddr() string      { return s.targetAddr }
func (s *AggregatorStream) CreatedAt() time.Time    { return s.createdAt }
