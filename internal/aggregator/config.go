// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  AggregatorConfig – configuration for the server-side Aggregator.
//
// The Aggregator is the remote endpoint that:
//   1. Accepts persistent TCP bearer connections from the 5 DNS-tunnel servers.
//   2. Demultiplexes MacroFrames by StreamID.
//   3. Reassembles out-of-order frames using the GlobalSeq byte-offset.
//   4. Dials the target host:port specified in each stream's SYN payload.
//   5. Pumps data between the reassembled stream and the upstream TCP connection.
//   6. Sends response MacroFrames back to the client over any live bearer.
// ==============================================================================

package aggregator

import "time"

// ──────────────────────────────────────────────────────────────────────────────
// Defaults
// ──────────────────────────────────────────────────────────────────────────────

const (
	// DefaultListenAddr is the TCP address the Aggregator binds to.
	DefaultListenAddr = ":9000"

	// DefaultChunkSize is the maximum payload bytes per response MacroFrame.
	// Should be ≤ min(tunnel download MTU) − MacroFrameHeaderSize (21 bytes).
	DefaultChunkSize = 4096

	// DefaultUpstreamDialTimeout is how long we wait for the upstream TCP dial
	// to complete when a new SYN frame arrives.
	DefaultUpstreamDialTimeout = 30 * time.Second

	// DefaultInboundChanDepth is the buffered-channel depth for the shared
	// inbound frame channel (all bearers write to this).
	DefaultInboundChanDepth = 4096

	// DefaultMaxReassemblyBuffer is the maximum number of out-of-order frames
	// held per stream before the stream is forcibly reset.  This caps memory
	// usage when a bearer is severely delayed.
	DefaultMaxReassemblyBuffer = 512

	// DefaultReassemblyTimeout is how long to wait for the next expected
	// sequence number before declaring a stream stalled and closing it.
	DefaultReassemblyTimeout = 60 * time.Second

	// DefaultMaxStreams is the maximum number of concurrent logical streams
	// the Aggregator will allow simultaneously.  0 = unlimited.
	DefaultMaxStreams = 0
)

// ──────────────────────────────────────────────────────────────────────────────
// AggregatorConfig
// ──────────────────────────────────────────────────────────────────────────────

// AggregatorConfig holds the full runtime configuration for the Aggregator.
type AggregatorConfig struct {
	// ListenAddr is the TCP address the Aggregator accepts bearer connections on.
	// Example: ":9000" or "0.0.0.0:9000"
	ListenAddr string

	// ChunkSize is the maximum payload bytes per response MacroFrame sent back
	// to the client.  Smaller values increase framing overhead; larger values
	// increase reassembly buffer requirements on the client.
	ChunkSize int

	// UpstreamDialTimeout caps the time spent dialling the upstream host:port
	// extracted from each stream's SYN payload.
	UpstreamDialTimeout time.Duration

	// InboundChannelDepth controls the buffered-channel depth for frames
	// arriving from all bearer connections combined.
	InboundChannelDepth int

	// MaxReassemblyBuffer is the per-stream cap on buffered out-of-order frames.
	// If exceeded the stream is reset (RST) to avoid unbounded memory growth.
	MaxReassemblyBuffer int

	// ReassemblyTimeout is the maximum idle time (no new sequential data)
	// before a stream is closed as stalled.
	ReassemblyTimeout time.Duration

	// MaxStreams caps the total concurrent logical streams.  0 = unlimited.
	MaxStreams int
}

// DefaultAggregatorConfig returns production-ready defaults.
func DefaultAggregatorConfig() AggregatorConfig {
	return AggregatorConfig{
		ListenAddr:          DefaultListenAddr,
		ChunkSize:           DefaultChunkSize,
		UpstreamDialTimeout: DefaultUpstreamDialTimeout,
		InboundChannelDepth: DefaultInboundChanDepth,
		MaxReassemblyBuffer: DefaultMaxReassemblyBuffer,
		ReassemblyTimeout:   DefaultReassemblyTimeout,
		MaxStreams:          DefaultMaxStreams,
	}
}
