// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  BearerRegistry – thread-safe registry of live InboundBearers.
//
// The Aggregator must send response MacroFrames (upstream→client) back through
// any available bearer.  The BearerRegistry maintains the set of currently
// connected bearers and provides a Pick() method that selects a healthy bearer
// via round-robin for response delivery.
//
// Bearers self-register on Start() and deregister on Stop()/failure.
// All methods are safe for concurrent use.
// ==============================================================================

package aggregator

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrNoBearer is returned by Pick() when no live bearer is registered.
var ErrNoBearer = errors.New("aggregator: no live bearer available")

// ──────────────────────────────────────────────────────────────────────────────
// BearerRegistry
// ──────────────────────────────────────────────────────────────────────────────

// BearerRegistry is a concurrency-safe set of *InboundBearer values.
type BearerRegistry struct {
	mu      sync.RWMutex
	bearers []*InboundBearer

	// rr is an atomic round-robin counter for Pick().
	rr atomic.Uint64
}

// NewBearerRegistry returns an empty BearerRegistry.
func NewBearerRegistry() *BearerRegistry {
	return &BearerRegistry{}
}

// Register adds a bearer to the registry.
func (r *BearerRegistry) Register(b *InboundBearer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bearers = append(r.bearers, b)
}

// Deregister removes a bearer from the registry.
func (r *BearerRegistry) Deregister(b *InboundBearer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.bearers[:0]
	for _, existing := range r.bearers {
		if existing != b {
			out = append(out, existing)
		}
	}
	r.bearers = out
}

// Pick returns a live bearer for sending response frames, using round-robin
// selection across all registered (non-closed) bearers.
// Returns ErrNoBearer if the registry is empty or all bearers are dead.
func (r *BearerRegistry) Pick() (*InboundBearer, error) {
	r.mu.RLock()
	snapshot := make([]*InboundBearer, len(r.bearers))
	copy(snapshot, r.bearers)
	r.mu.RUnlock()

	n := uint64(len(snapshot))
	if n == 0 {
		return nil, ErrNoBearer
	}

	// Walk up to n bearers starting at the current RR position.
	start := r.rr.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		b := snapshot[(start+i)%n]
		if !b.IsClosed() {
			return b, nil
		}
	}
	return nil, ErrNoBearer
}

// PickFor returns the bearer that originally carried the SYN for streamID,
// enabling "sticky" response routing.  Falls back to Pick() if not found.
func (r *BearerRegistry) PickFor(streamID uint32) (*InboundBearer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, b := range r.bearers {
		if !b.IsClosed() && b.HasStream(streamID) {
			return b, nil
		}
	}
	// Not found on any specific bearer – fall back to round-robin.
	return r.Pick()
}

// Count returns the number of currently registered bearers.
func (r *BearerRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.bearers)
}

// LiveCount returns the number of non-closed bearers.
func (r *BearerRegistry) LiveCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, b := range r.bearers {
		if !b.IsClosed() {
			count++
		}
	}
	return count
}

// ForEach calls fn for each registered bearer while holding a read lock.
// fn must not call any BearerRegistry method that acquires a write lock.
func (r *BearerRegistry) ForEach(fn func(*InboundBearer)) {
	r.mu.RLock()
	snapshot := make([]*InboundBearer, len(r.bearers))
	copy(snapshot, r.bearers)
	r.mu.RUnlock()
	for _, b := range snapshot {
		fn(b)
	}
}
