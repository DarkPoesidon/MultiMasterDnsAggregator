// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  StreamRouter – routes AggFrames to the correct AggregatorStream
//           based on the MacroFrame StreamID field.
//
// Routing rules (same as the client-side MultipathManager.routeFrame):
//
//   SYN  → parse target from SYN payload → NewAggregatorStream → stream.Start()
//   RST  → close and deregister stream immediately
//   FIN  → deliver any final payload → close and deregister
//   data → stream.Push(globalSeq, payload)
//
// All Route() calls come from a SINGLE goroutine (the aggregator server's route
// loop), so no mutex is needed on the hot path.  The sync.Map is used only for
// the CloseStream / Range operations which may be called from stream goroutines.
//
// MaxStreams enforcement:
//   If cfg.MaxStreams > 0 and the current stream count equals the limit,
//   new SYN frames are refused with an RST reply.
// ==============================================================================

package aggregator

import (
	"sync"
	"sync/atomic"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

// ──────────────────────────────────────────────────────────────────────────────
// StreamRouter
// ──────────────────────────────────────────────────────────────────────────────

// StreamRouter maintains the StreamID → *AggregatorStream mapping and
// implements the per-frame routing logic.
type StreamRouter struct {
	cfg      *AggregatorConfig
	log      Logger
	registry *BearerRegistry

	// streams maps uint32 streamID → *AggregatorStream.
	streams sync.Map

	// streamCount is an atomic count used for MaxStreams enforcement.
	streamCount atomic.Int64
}

// NewStreamRouter constructs a router.
func NewStreamRouter(cfg *AggregatorConfig, registry *BearerRegistry, log Logger) *StreamRouter {
	return &StreamRouter{
		cfg:      cfg,
		log:      log,
		registry: registry,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Route – main per-frame dispatch
// ──────────────────────────────────────────────────────────────────────────────

// Route processes a single AggFrame.  Must be called from a single goroutine.
func (r *StreamRouter) Route(f AggFrame) {
	hdr := f.Header

	// RST: tear down stream immediately.
	if hdr.HasFlag(multipath.FlagRST) {
		r.log.Debugf("[Router] RST for stream %d", hdr.StreamID)
		r.closeStream(hdr.StreamID)
		return
	}

	// SYN: create new stream.
	if hdr.HasFlag(multipath.FlagSYN) {
		r.handleSYN(f)
		return
	}

	// Data or FIN.
	v, ok := r.streams.Load(hdr.StreamID)
	if !ok {
		// Stream already closed or never existed.  Ignore.
		return
	}
	s := v.(*AggregatorStream)

	if len(f.Payload) > 0 {
		s.Push(hdr.GlobalSeq, f.Payload)
	}

	if hdr.HasFlag(multipath.FlagFIN) {
		r.log.Debugf("[Router] FIN for stream %d", hdr.StreamID)
		r.closeStream(hdr.StreamID)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// SYN handling
// ──────────────────────────────────────────────────────────────────────────────

func (r *StreamRouter) handleSYN(f AggFrame) {
	hdr := f.Header

	// Check stream limit.
	if r.cfg.MaxStreams > 0 && int(r.streamCount.Load()) >= r.cfg.MaxStreams {
		r.log.Warnf("[Router] MaxStreams (%d) reached – refusing stream %d",
			r.cfg.MaxStreams, hdr.StreamID)
		r.sendRSTViaBearers(hdr.StreamID, f.Bearer)
		return
	}

	// Decode target address from SYN payload.
	target, err := DecodeTarget(f.Payload)
	if err != nil {
		r.log.Warnf("[Router] SYN stream %d: bad target payload: %v", hdr.StreamID, err)
		r.sendRSTViaBearers(hdr.StreamID, f.Bearer)
		return
	}

	r.log.Infof("[Router] SYN stream %d → %s", hdr.StreamID, target)

	stream := NewAggregatorStream(
		hdr.StreamID,
		target,
		r.cfg,
		r.registry,
		r.log,
		r.onStreamClose,
	)

	r.streams.Store(hdr.StreamID, stream)
	r.streamCount.Add(1)
	stream.Start()
}

// ──────────────────────────────────────────────────────────────────────────────
// Stream close / cleanup
// ──────────────────────────────────────────────────────────────────────────────

// closeStream removes and closes a stream.  Safe to call from any goroutine.
func (r *StreamRouter) closeStream(id uint32) {
	if v, ok := r.streams.LoadAndDelete(id); ok {
		r.streamCount.Add(-1)
		v.(*AggregatorStream).Close()
	}
}

// onStreamClose is the callback installed in every AggregatorStream.
// Called when the stream closes itself (e.g. upstream error).
func (r *StreamRouter) onStreamClose(id uint32) {
	if _, ok := r.streams.LoadAndDelete(id); ok {
		r.streamCount.Add(-1)
	}
	// Forget the stream from all bearers' sticky-routing tables.
	r.forgetFromBearers(id)
}

// forgetFromBearers removes streamID from all registered bearers' routing maps.
func (r *StreamRouter) forgetFromBearers(id uint32) {
	r.registry.ForEach(func(b *InboundBearer) {
		b.ForgetStream(id)
	})
}

// CloseAll closes every registered stream.  Called on server shutdown.
func (r *StreamRouter) CloseAll() {
	r.streams.Range(func(key, value any) bool {
		r.streams.Delete(key)
		r.streamCount.Add(-1)
		value.(*AggregatorStream).Close()
		return true
	})
}

// ActiveCount returns the number of currently registered streams.
func (r *StreamRouter) ActiveCount() int {
	return int(r.streamCount.Load())
}

// ──────────────────────────────────────────────────────────────────────────────
// RST helpers
// ──────────────────────────────────────────────────────────────────────────────

// sendRSTViaBearers sends an RST frame for streamID, preferring the bearer
// that delivered the SYN if possible.
func (r *StreamRouter) sendRSTViaBearers(streamID uint32, synBearer *InboundBearer) {
	rstFrame := multipath.BuildRSTFrame(streamID)
	if synBearer != nil && !synBearer.IsClosed() {
		_ = synBearer.SendFrame(rstFrame)
		return
	}
	if b, err := r.registry.Pick(); err == nil {
		_ = b.SendFrame(rstFrame)
	}
}
