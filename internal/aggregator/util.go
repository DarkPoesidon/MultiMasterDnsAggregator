// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/aggregator
// Purpose:  Shared utilities: Logger interface, I/O helpers, time helpers.
// ==============================================================================

package aggregator

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

// MacroFrameHeaderSize mirrors the constant from the multipath package so
// inbound_bearer.go can reference it without an import cycle.
const MacroFrameHeaderSize = multipath.MacroFrameHeaderSize

// ──────────────────────────────────────────────────────────────────────────────
// Logger interface
// ──────────────────────────────────────────────────────────────────────────────

// Logger is the same interface used by the multipath package, reproduced here
// so callers don't need to import the multipath package just to supply a logger.
type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// StdLogger is a simple Logger backed by the standard library's log package.
// Debug output is gated on the AGGREGATOR_DEBUG environment variable.
type StdLogger struct {
	l     *log.Logger
	debug bool
}

// NewStdLogger creates a StdLogger with the given name prefix.
func NewStdLogger(name string) *StdLogger {
	return &StdLogger{
		l:     log.New(os.Stderr, fmt.Sprintf("[%s] ", name), log.LstdFlags|log.Lmsgprefix),
		debug: os.Getenv("AGGREGATOR_DEBUG") != "",
	}
}

func (s *StdLogger) Debugf(format string, args ...any) {
	if s.debug {
		s.l.Printf("DEBUG "+format, args...)
	}
}
func (s *StdLogger) Infof(format string, args ...any)  { s.l.Printf("INFO  "+format, args...) }
func (s *StdLogger) Warnf(format string, args ...any)  { s.l.Printf("WARN  "+format, args...) }
func (s *StdLogger) Errorf(format string, args ...any) { s.l.Printf("ERROR "+format, args...) }

// NopLogger silently discards all log output.  Useful in tests.
type NopLogger struct{}

func (NopLogger) Debugf(_ string, _ ...any) {}
func (NopLogger) Infof(_ string, _ ...any)  {}
func (NopLogger) Warnf(_ string, _ ...any)  {}
func (NopLogger) Errorf(_ string, _ ...any) {}

// ──────────────────────────────────────────────────────────────────────────────
// I/O helpers
// ──────────────────────────────────────────────────────────────────────────────

// readFull reads exactly len(buf) bytes from r, handling short reads.
func readFull(r io.Reader, buf []byte) (int, error) {
	return io.ReadFull(r, buf)
}

// ──────────────────────────────────────────────────────────────────────────────
// Time helpers
// ──────────────────────────────────────────────────────────────────────────────

// deadlineFromNow returns a time.Time that is d seconds from now.
func deadlineFromNow(seconds int) time.Time {
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

// ──────────────────────────────────────────────────────────────────────────────
// SYN payload: target address encoding / decoding
// ──────────────────────────────────────────────────────────────────────────────
//
// Wire format:
//   [0..1]  uint16 big-endian: byte length of the address string
//   [2..N]  UTF-8 bytes of "host:port"
//
// This is the server-side counterpart to multipath.EncodeTarget().

// DecodeTarget extracts the "host:port" string from a SYN frame payload.
// Returns an error if the payload is malformed.
func DecodeTarget(payload []byte) (string, error) {
	if len(payload) < 2 {
		return "", fmt.Errorf("aggregator: SYN payload too short (%d bytes)", len(payload))
	}
	addrLen := int(uint16(payload[0])<<8 | uint16(payload[1]))
	if addrLen == 0 {
		return "", fmt.Errorf("aggregator: SYN payload: zero-length address")
	}
	if len(payload) < 2+addrLen {
		return "", fmt.Errorf("aggregator: SYN payload truncated (need %d, have %d)", 2+addrLen, len(payload))
	}
	addr := string(payload[2 : 2+addrLen])
	// Basic sanity: must contain a colon (host:port).
	if !containsColon(addr) {
		return "", fmt.Errorf("aggregator: SYN target %q is not a host:port", addr)
	}
	return addr, nil
}

func containsColon(s string) bool {
	for _, c := range s {
		if c == ':' {
			return true
		}
	}
	return false
}

// isNetClosedError reports whether err represents a closed-connection error.
func isNetClosedError(err error) bool {
	if err == nil {
		return false
	}
	return err == net.ErrClosed ||
		containsSubstring(err.Error(), "use of closed network connection")
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
