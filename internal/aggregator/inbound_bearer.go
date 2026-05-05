// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  InboundBearer – one persistent TCP connection from a DNS-VPN-server.
//
// Architecture position:
//
//   DNS VPN Server 1..5
//       │  TCP → Aggregator :9000
//       ▼
//   InboundBearer (one per DNS-VPN-server connection)
//       │  DecodeFrameHeader + read payload
//       ▼
//   StreamRouter.Route(AggFrame)
//
// Each InboundBearer runs a single background goroutine (readLoop) that reads
// MacroFrames from the TCP connection and forwards them to the StreamRouter.
// The bearer is also used in the reverse direction: AggregatorStream calls
// SendFrame() to write response MacroFrames back to the client.
//
// Response routing strategy (sticky):
//   Each bearer tracks which StreamIDs it has seen a SYN for.  The response
//   pump prefers to send back on the bearer that received the SYN, so that
//   frame ordering in the response direction is guaranteed without a
//   client-side response reassembler.
//
// Thread safety:
//   readLoop runs in its own goroutine.
//   SendFrame() may be called from the pumpResponse goroutine of any stream.
//   writeMu serialises concurrent writes.
// ==============================================================================

package aggregator

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

// ──────────────────────────────────────────────────────────────────────────────
// AggFrame – inbound frame delivered to the StreamRouter
// ──────────────────────────────────────────────────────────────────────────────

// AggFrame is the decoded representation of a MacroFrame received from a bearer.
type AggFrame struct {
	Header  multipath.MacroFrameHeader
	Payload []byte
	Bearer  *InboundBearer // the bearer this frame arrived on
}

// FrameRouter is the interface InboundBearer uses to forward decoded frames.
// Both *StreamRouter (direct) and *routerShim (channel-based) satisfy it.
type FrameRouter interface {
	Route(f AggFrame)
}

// ──────────────────────────────────────────────────────────────────────────────
// InboundBearer
// ──────────────────────────────────────────────────────────────────────────────

// InboundBearer manages one accepted TCP connection from a DNS-VPN-server.
type InboundBearer struct {
	// id is a monotonically assigned identifier for logging.
	id uint32

	// conn is the accepted TCP connection.  It is read by readLoop and written
	// by SendFrame.
	conn net.Conn

	// router receives decoded AggFrames for routing by StreamID.
	router FrameRouter

	// registry is the BearerRegistry this bearer is a member of.
	registry *BearerRegistry

	// log is the aggregator logger.
	log Logger

	// writeMu serialises response frame writes on conn.
	writeMu sync.Mutex

	// closed is set to true once Stop() is called or readLoop exits.
	closed atomic.Bool

	// streamIDs tracks which logical stream SYNs arrived on this bearer.
	// Used by PickFor() for sticky response routing.
	streamMu  sync.RWMutex
	streamIDs map[uint32]struct{}

	// Telemetry.
	framesRecvd atomic.Uint64
	framesSent  atomic.Uint64
	bytesRecvd  atomic.Uint64
	bytesSent   atomic.Uint64
}

// NewInboundBearer creates an InboundBearer.  Call Start() to begin reading.
func NewInboundBearer(
	id uint32,
	conn net.Conn,
	router FrameRouter,
	registry *BearerRegistry,
	log Logger,
) *InboundBearer {
	return &InboundBearer{
		id:        id,
		conn:      conn,
		router:    router,
		registry:  registry,
		log:       log,
		streamIDs: make(map[uint32]struct{}),
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Lifecycle
// ──────────────────────────────────────────────────────────────────────────────

// Start registers the bearer and spawns the read goroutine.
func (b *InboundBearer) Start() {
	b.registry.Register(b)
	go b.readLoop()
}

// Stop closes the connection and deregisters the bearer.
// Safe to call multiple times.
func (b *InboundBearer) Stop() {
	if b.closed.CompareAndSwap(false, true) {
		_ = b.conn.Close()
		b.registry.Deregister(b)
	}
}

// IsClosed reports whether Stop() has been called or the connection has broken.
func (b *InboundBearer) IsClosed() bool { return b.closed.Load() }

// ID returns the bearer's numeric identifier.
func (b *InboundBearer) ID() uint32 { return b.id }

// ──────────────────────────────────────────────────────────────────────────────
// Stream tracking (for sticky response routing)
// ──────────────────────────────────────────────────────────────────────────────

// recordStream notes that a SYN for streamID arrived on this bearer.
func (b *InboundBearer) recordStream(streamID uint32) {
	b.streamMu.Lock()
	b.streamIDs[streamID] = struct{}{}
	b.streamMu.Unlock()
}

// ForgetStream removes a stream from the sticky-routing table.
func (b *InboundBearer) ForgetStream(streamID uint32) {
	b.streamMu.Lock()
	delete(b.streamIDs, streamID)
	b.streamMu.Unlock()
}

// HasStream reports whether a SYN for streamID was received on this bearer.
func (b *InboundBearer) HasStream(streamID uint32) bool {
	b.streamMu.RLock()
	_, ok := b.streamIDs[streamID]
	b.streamMu.RUnlock()
	return ok
}

// ──────────────────────────────────────────────────────────────────────────────
// Sending (response direction: Aggregator → DNS-VPN-server → client)
// ──────────────────────────────────────────────────────────────────────────────

// SendFrame writes a fully-encoded MacroFrame to the underlying TCP connection.
// Safe to call from multiple goroutines.
func (b *InboundBearer) SendFrame(frame []byte) error {
	if b.closed.Load() {
		return net.ErrClosed
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	// 15-second write deadline to prevent a slow client from blocking the pump.
	if err := b.conn.SetWriteDeadline(deadlineFromNow(15)); err != nil {
		b.markClosed()
		return err
	}

	n, err := b.conn.Write(frame)
	if err != nil {
		b.markClosed()
		return err
	}

	b.framesSent.Add(1)
	b.bytesSent.Add(uint64(n))
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Read loop
// ──────────────────────────────────────────────────────────────────────────────

// readLoop reads MacroFrames from conn until the connection is closed.
// It forwards each decoded frame to the StreamRouter.
func (b *InboundBearer) readLoop() {
	defer b.Stop()

	b.log.Infof("[Bearer %d] Connected from %s", b.id, b.conn.RemoteAddr())

	for {
		hdr, err := multipath.DecodeFrameHeader(b.conn)
		if err != nil {
			if !b.closed.Load() {
				b.log.Debugf("[Bearer %d] Read header error: %v", b.id, err)
			}
			return
		}

		var payload []byte
		if hdr.PayloadLen > 0 {
			payload = make([]byte, hdr.PayloadLen)
			if _, err := readFull(b.conn, payload); err != nil {
				b.log.Debugf("[Bearer %d] Read payload error: %v", b.id, err)
				return
			}
		}

		b.framesRecvd.Add(1)
		b.bytesRecvd.Add(uint64(MacroFrameHeaderSize + int(hdr.PayloadLen)))

		// Track which stream's SYN arrived on this bearer for sticky responses.
		if hdr.HasFlag(multipath.FlagSYN) {
			b.recordStream(hdr.StreamID)
		}

		b.router.Route(AggFrame{
			Header:  hdr,
			Payload: payload,
			Bearer:  b,
		})
	}
}

// markClosed marks the bearer as closed without closing the conn again
// (conn.Close is already called or imminent).
func (b *InboundBearer) markClosed() {
	if b.closed.CompareAndSwap(false, true) {
		b.registry.Deregister(b)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Telemetry
// ──────────────────────────────────────────────────────────────────────────────

func (b *InboundBearer) FramesRecvd() uint64 { return b.framesRecvd.Load() }
func (b *InboundBearer) FramesSent() uint64  { return b.framesSent.Load() }
func (b *InboundBearer) BytesRecvd() uint64  { return b.bytesRecvd.Load() }
func (b *InboundBearer) BytesSent() uint64   { return b.bytesSent.Load() }
