// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  TunnelPool – manages a set of BearerTunnels and provides a
//           weighted round-robin dispatch algorithm for outbound macro frames.
//
// Dispatch algorithm:
//   The pool expands each TunnelEndpoint into Weight consecutive slots in a
//   flat ring slice.  A round-robin atomic counter walks the ring on every
//   Pick() call.  If the selected bearer is unhealthy, the pool continues
//   walking the ring (up to len(ring) attempts) to find a live bearer.
//
//   Example: tunnels with weights [2, 1, 1] produce ring [0,0,1,2].
//   Tunnel 0 receives 50 % of all dispatched frames; tunnels 1 and 2 share
//   the remaining 50 %.
// ==============================================================================

package multipath

import (
	"errors"
	"sync/atomic"
)

// ErrNoHealthyBearer is returned when all bearers in the pool are unhealthy.
var ErrNoHealthyBearer = errors.New("multipath: no healthy bearer available")

// ──────────────────────────────────────────────────────────────────────────────
// TunnelPool
// ──────────────────────────────────────────────────────────────────────────────

// TunnelPool manages a fixed set of BearerTunnels and exposes a Pick() method
// that returns the next bearer to use for an outbound macro frame.
//
// The pool does NOT take ownership of bearers; the MultipathManager owns them.
// The pool only holds read references.
type TunnelPool struct {
	// bearers is the canonical ordered list of all tunnels (one per DNS-tunnel
	// SOCKS5 endpoint).  Index matches the TunnelEndpoint slice in the config.
	bearers []*BearerTunnel

	// ring is the weighted round-robin dispatch ring.
	// ring[i] is an index into bearers.
	ring []int

	// rrCounter is the position in ring for the next Pick() call.
	rrCounter atomic.Uint64

	// inboundCh is the channel all bearers write received frames to.
	inboundCh <-chan InboundFrame
}

// NewTunnelPool creates a TunnelPool from the provided bearers.
// inboundCh must be the same channel passed to each BearerTunnel constructor.
func NewTunnelPool(bearers []*BearerTunnel, inboundCh <-chan InboundFrame) *TunnelPool {
	p := &TunnelPool{
		bearers:   bearers,
		inboundCh: inboundCh,
	}
	p.buildRing()
	return p
}

// buildRing constructs the weighted round-robin ring.
func (p *TunnelPool) buildRing() {
	total := 0
	for _, b := range p.bearers {
		total += b.endpoint.effectiveWeight()
	}
	if total == 0 {
		total = len(p.bearers)
	}

	ring := make([]int, 0, total)
	for idx, b := range p.bearers {
		w := b.endpoint.effectiveWeight()
		for i := 0; i < w; i++ {
			ring = append(ring, idx)
		}
	}
	p.ring = ring
}

// Pick returns the next healthy BearerTunnel using weighted round-robin.
// It tries every slot in the ring at most once before giving up.
func (p *TunnelPool) Pick() (*BearerTunnel, error) {
	n := len(p.ring)
	if n == 0 {
		return nil, ErrNoHealthyBearer
	}

	for attempt := 0; attempt < n; attempt++ {
		pos := int(p.rrCounter.Add(1)-1) % n
		bearerIdx := p.ring[pos]
		b := p.bearers[bearerIdx]
		if b.IsHealthy() {
			return b, nil
		}
	}
	return nil, ErrNoHealthyBearer
}

// PickAll returns all healthy bearers in ring order.
// Useful for duplication-mode dispatch (send the same frame to every bearer).
func (p *TunnelPool) PickAll() []*BearerTunnel {
	seen := make(map[int]bool, len(p.bearers))
	out := make([]*BearerTunnel, 0, len(p.bearers))
	for idx, b := range p.bearers {
		if seen[idx] {
			continue
		}
		seen[idx] = true
		if b.IsHealthy() {
			out = append(out, b)
		}
	}
	return out
}

// HealthyCount returns the number of currently connected bearers.
func (p *TunnelPool) HealthyCount() int {
	count := 0
	for _, b := range p.bearers {
		if b.IsHealthy() {
			count++
		}
	}
	return count
}

// TotalCount returns the total number of bearer tunnels (healthy or not).
func (p *TunnelPool) TotalCount() int {
	return len(p.bearers)
}

// InboundCh exposes the shared inbound-frame channel for the Manager's demux
// goroutine to read from.
func (p *TunnelPool) InboundCh() <-chan InboundFrame {
	return p.inboundCh
}

// Start launches all bearer tunnels.
func (p *TunnelPool) Start() {
	for _, b := range p.bearers {
		b.Start()
	}
}

// Stop shuts down all bearer tunnels.
func (p *TunnelPool) Stop() {
	for _, b := range p.bearers {
		b.Stop()
	}
}
