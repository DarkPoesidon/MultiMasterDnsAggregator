// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  StreamIDCounter – globally unique stream identifier generator.
//           ByteSequencer   – per-stream monotonic byte-offset counter.
//
// Design note:
//   The existing ARQ layer (internal/arq/arq.go) uses a *per-tunnel* uint16
//   sequence number that wraps at 65535. That is the micro-level sequence.
//
//   The ByteSequencer here operates at the MACRO level: it assigns a uint64
//   byte-offset to every chunk dispatched across ALL five bearers for a single
//   logical connection.  This offset is what the remote Aggregator uses to
//   reconstruct the byte stream in order, regardless of which bearer each
//   chunk arrived on.
//
//   Relationship summary:
//     MacroFrame.GlobalSeq  (uint64, byte offset, this file)
//       └── rides inside DNS-tunnel ARQ SequenceNum (uint16, per-tunnel)
//             └── rides inside DNS TXT record labels (per-query)
// ==============================================================================

package multipath

import "sync/atomic"

// ──────────────────────────────────────────────────────────────────────────────
// StreamIDCounter
// ──────────────────────────────────────────────────────────────────────────────

// StreamIDCounter issues process-wide unique 32-bit stream identifiers.
// Thread-safe; safe to share across all LogicalStream instances.
type StreamIDCounter struct {
	counter atomic.Uint32
}

// NextID returns a new stream ID.  The first returned value is 1;
// value 0 is never issued (reserved as "invalid").
func (c *StreamIDCounter) NextID() uint32 {
	for {
		id := c.counter.Add(1)
		if id != 0 {
			return id
		}
		// Overflow: uint32 wrapped to 0 → add 1 again.
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ByteSequencer
// ──────────────────────────────────────────────────────────────────────────────

// ByteSequencer tracks the next byte-offset to assign to an outgoing chunk.
// Each LogicalStream owns exactly one ByteSequencer.
//
// Usage:
//
//	offset := seq.Reserve(chunkLen)   // atomically advance and get old value
//	frame   := BuildFrame(id, offset, flags, chunk)
type ByteSequencer struct {
	next atomic.Uint64
}

// Reserve atomically advances the counter by n bytes and returns the
// byte-offset of the chunk's first byte (i.e. the old value).
// Callers MUST pass n == len(chunk); passing 0 is a no-op that returns the
// current position without advancing.
func (s *ByteSequencer) Reserve(n uint64) uint64 {
	if n == 0 {
		return s.next.Load()
	}
	return s.next.Add(n) - n
}

// Current returns the total number of bytes reserved so far.
// This equals the GlobalSeq value to use for the *next* chunk.
func (s *ByteSequencer) Current() uint64 {
	return s.next.Load()
}

// Reset resets the counter to zero.  Only call this when creating a fresh
// LogicalStream; never call during active operation.
func (s *ByteSequencer) Reset() {
	s.next.Store(0)
}
