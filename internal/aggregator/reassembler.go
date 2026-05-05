// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  Reassembler – out-of-order frame buffer for a single logical stream.
//
// Problem:
//   The 5 bearer tunnels deliver MacroFrames for the same logical stream through
//   independent DNS VPN paths.  DNS/UDP latency is asymmetric and unpredictable,
//   so frames may arrive at the Aggregator in any order.
//   However the upstream TCP connection requires a contiguous byte stream.
//
// Solution:
//   Each MacroFrame carries a GlobalSeq field that is the byte-offset of the
//   frame's first payload byte within the logical stream.  The Reassembler
//   maintains an internal min-heap of buffered frames, sorted by GlobalSeq.
//   It emits frames to an output channel only when they arrive in contiguous
//   order (i.e. when GlobalSeq == nextExpected).
//
// Threading model:
//   Push() and Out() are accessed from a SINGLE goroutine (the Aggregator's
//   route loop).  The output channel is drained by the AggregatorStream's
//   pumpUpstream goroutine.  No mutex is required for Push() itself.
//
// Memory safety:
//   MaxBuffer caps the number of buffered out-of-order frames.  If exceeded,
//   the Reassembler sets an overflow flag and Push() returns ErrReassemblyOverflow
//   so the caller can reset the stream.
// ==============================================================================

package aggregator

import (
	"container/heap"
	"errors"
)

// ErrReassemblyOverflow is returned by Push when the out-of-order buffer
// has exceeded MaxBuffer.  The caller should reset the logical stream.
var ErrReassemblyOverflow = errors.New("aggregator: reassembly buffer overflow")

// ──────────────────────────────────────────────────────────────────────────────
// Internal min-heap (sorted by GlobalSeq)
// ──────────────────────────────────────────────────────────────────────────────

type pendingFrame struct {
	seq     uint64
	payload []byte
}

type frameHeap []pendingFrame

func (h frameHeap) Len() int           { return len(h) }
func (h frameHeap) Less(i, j int) bool { return h[i].seq < h[j].seq }
func (h frameHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *frameHeap) Push(x any)        { *h = append(*h, x.(pendingFrame)) }
func (h *frameHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = pendingFrame{} // allow GC
	*h = old[:n-1]
	return x
}

// ──────────────────────────────────────────────────────────────────────────────
// Reassembler
// ──────────────────────────────────────────────────────────────────────────────

// Reassembler reorders incoming MacroFrame payloads by GlobalSeq and emits
// them in contiguous order.
type Reassembler struct {
	// expected is the byte offset of the next chunk we need to emit.
	expected uint64

	// buf holds out-of-order frames in a min-heap keyed by GlobalSeq.
	buf frameHeap

	// maxBuffer caps the heap size.  0 = unlimited.
	maxBuffer int

	// outCh is the ordered output channel.
	// Buffered to decouple Push() from the upstream writer.
	outCh chan []byte

	// bytesDelivered tracks total bytes emitted (telemetry).
	bytesDelivered uint64
}

// NewReassembler creates a Reassembler.
//   - chanDepth: depth of the output channel.
//   - maxBuffer: maximum out-of-order frames buffered (0 = unlimited).
func NewReassembler(chanDepth, maxBuffer int) *Reassembler {
	if chanDepth <= 0 {
		chanDepth = 256
	}
	r := &Reassembler{
		maxBuffer: maxBuffer,
		outCh:     make(chan []byte, chanDepth),
	}
	heap.Init(&r.buf)
	return r
}

// ──────────────────────────────────────────────────────────────────────────────
// Push – deliver a frame payload to the reassembler
// ──────────────────────────────────────────────────────────────────────────────

// Push delivers a decoded payload with its byte-offset to the reassembler.
// Must be called from a single goroutine.
//
// Returns:
//   - nil if the frame was accepted (either emitted or buffered).
//   - ErrReassemblyOverflow if MaxBuffer is exceeded.
func (r *Reassembler) Push(globalSeq uint64, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}

	// Duplicate or already-delivered frame: drop silently.
	if globalSeq < r.expected {
		return nil
	}

	// Fast-path: frame arrives exactly in order.
	if globalSeq == r.expected {
		data := copyBytes(payload)
		r.send(data)
		r.expected += uint64(len(payload))
		r.flush()
		return nil
	}

	// Out-of-order: check buffer cap.
	if r.maxBuffer > 0 && r.buf.Len() >= r.maxBuffer {
		return ErrReassemblyOverflow
	}

	data := copyBytes(payload)
	heap.Push(&r.buf, pendingFrame{seq: globalSeq, payload: data})
	return nil
}

// flush emits buffered frames that are now contiguous with expected.
func (r *Reassembler) flush() {
	for r.buf.Len() > 0 {
		top := r.buf[0]
		if top.seq != r.expected {
			// Gap still present – keep waiting.
			break
		}
		heap.Pop(&r.buf)
		r.send(top.payload)
		r.expected += uint64(len(top.payload))
	}
}

// send writes a slice to the output channel.
// Uses a blocking send; callers must ensure the channel consumer (pumpUpstream)
// runs concurrently to prevent a deadlock.
func (r *Reassembler) send(data []byte) {
	r.outCh <- data
	r.bytesDelivered += uint64(len(data))
}

// ──────────────────────────────────────────────────────────────────────────────
// Accessors
// ──────────────────────────────────────────────────────────────────────────────

// Out returns the ordered output channel.
// The consumer (AggregatorStream.pumpUpstream) reads from this.
func (r *Reassembler) Out() <-chan []byte { return r.outCh }

// NextExpected returns the byte offset of the next frame the reassembler is
// waiting for.  Useful for health-check / stall detection.
func (r *Reassembler) NextExpected() uint64 { return r.expected }

// BufferedCount returns the number of out-of-order frames currently held.
func (r *Reassembler) BufferedCount() int { return r.buf.Len() }

// BytesDelivered returns the total bytes emitted so far.
func (r *Reassembler) BytesDelivered() uint64 { return r.bytesDelivered }

// Reset clears all state.  Must only be called when the stream is fully closed.
func (r *Reassembler) Reset() {
	r.buf = r.buf[:0]
	heap.Init(&r.buf)
	r.expected = 0
	r.bytesDelivered = 0
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func copyBytes(src []byte) []byte {
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}
