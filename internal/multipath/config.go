// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  Configuration types for the MultipathManager and Dispatcher.
//
// This package is NEW and intentionally isolated from the production codebase.
// No files in internal/ are modified.
// ==============================================================================

package multipath

import "time"

// ──────────────────────────────────────────────────────────────────────────────
// Wire-level constants
// ──────────────────────────────────────────────────────────────────────────────

const (
	// MacroFrameHeaderSize is the fixed byte length of every MacroFrameHeader.
	// Layout: Magic(4) + Version(1) + Flags(1) + StreamID(4) + GlobalSeq(8) +
	//         PayloadLen(2) + Checksum(1) = 21 bytes.
	MacroFrameHeaderSize = 21

	// MacroMagic is the 4-byte sentinel that opens every macro frame.
	// ASCII "MPVN" (MultiPath VPN).
	MacroMagic = uint32(0x4D50564E)

	// MacroVersion identifies the current frame format revision.
	MacroVersion = uint8(1)
)

// Flags carried in the MacroFrameHeader.Flags byte.
const (
	FlagSYN = uint8(1 << 0) // First frame for this logical stream (stream open).
	FlagFIN = uint8(1 << 1) // Last frame for this logical stream (clean close).
	FlagRST = uint8(1 << 2) // Forceful reset of the logical stream.
	FlagACK = uint8(1 << 3) // Acknowledgement (reserved for future use).
)

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

// TunnelEndpoint describes one running DNS-tunnel client's SOCKS5 listener.
// Each of the 5 endpoints corresponds to an independent client.Client instance
// running in the background (e.g. on ports 18001–18005).
type TunnelEndpoint struct {
	// SOCKS5Addr is the host:port of the DNS-tunnel client's local SOCKS5 listener.
	// Example: "127.0.0.1:18001"
	SOCKS5Addr string

	// Label is a short human-readable name used in logs.
	Label string

	// Weight controls how many round-robin slots this bearer occupies.
	// 0 or negative is treated as 1.
	Weight int
}

// MultipathConfig is the complete configuration for the multipath overlay.
type MultipathConfig struct {
	// ListenAddr is the TCP address the MultipathDispatcher binds to in order
	// to accept incoming local connections from browsers / apps.
	// Example: "127.0.0.1:19000"
	ListenAddr string

	// AggregatorAddr is the remote TCP address of the Aggregator service.
	// All 5 bearer tunnels connect to this address through their respective
	// DNS-tunnel SOCKS5 proxies.
	// Example: "agg.yourdomain.com:9000"
	AggregatorAddr string

	// Tunnels is the ordered list of DNS-tunnel SOCKS5 endpoints.
	// The multipath layer will open exactly one bearer connection per entry.
	Tunnels []TunnelEndpoint

	// ChunkSize is the maximum payload size (bytes) per macro frame.
	// Must fit within the upload MTU of the narrowest tunnel.
	// Recommended: 900–1200 bytes.
	ChunkSize int

	// DialTimeout is the maximum time allowed for each SOCKS5 bearer connection
	// to establish (dial + handshake).
	DialTimeout time.Duration

	// ReconnectDelay is how long a bearer waits before retrying after a failure.
	ReconnectDelay time.Duration

	// ReadBufferSize is the size of the per-connection read buffer in bytes.
	ReadBufferSize int

	// InboundChannelDepth controls the buffered-channel depth for inbound frames
	// from all bearers combined. A larger value tolerates burst traffic better.
	InboundChannelDepth int

	// DispatchRetries is how many different bearers the dispatcher tries before
	// giving up on a single frame. A value of 0 means "try all".
	DispatchRetries int
}

// DefaultConfig returns a MultipathConfig with safe production defaults.
func DefaultConfig() MultipathConfig {
	return MultipathConfig{
		ListenAddr:          "127.0.0.1:19000",
		AggregatorAddr:      "127.0.0.1:9000",
		ChunkSize:           1024,
		DialTimeout:         10 * time.Second,
		ReconnectDelay:      3 * time.Second,
		ReadBufferSize:      32 * 1024,
		InboundChannelDepth: 2048,
		DispatchRetries:     0, // try all bearers
	}
}

// effectiveWeight returns at least 1.
func (ep TunnelEndpoint) effectiveWeight() int {
	if ep.Weight < 1 {
		return 1
	}
	return ep.Weight
}
