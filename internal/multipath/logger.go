// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  Logger – minimal logging interface and a stdlib implementation.
//
// Using an interface keeps the multipath package free of any hard dependency on
// masterdnsvpn-go/internal/logger so it can be imported and tested in isolation.
// ==============================================================================

package multipath

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// ──────────────────────────────────────────────────────────────────────────────
// Interface
// ──────────────────────────────────────────────────────────────────────────────

// Logger is a minimal structured-log interface consumed by every component in
// this package.  In production, callers may wrap masterdnsvpn-go's own
// internal/logger.Logger to implement this interface.
type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// ──────────────────────────────────────────────────────────────────────────────
// StdLogger – stdlib/log-backed implementation
// ──────────────────────────────────────────────────────────────────────────────

// StdLogger wraps the standard library's log package.
// Use NewStdLogger to construct one.
type StdLogger struct {
	prefix string
	inner  *log.Logger
	debug  bool
}

// NewStdLogger returns a StdLogger that prefixes every message with name.
// Debug messages are suppressed unless the environment variable
// MULTIPATH_DEBUG is set to "1" or "true".
func NewStdLogger(name string) *StdLogger {
	debugEnv := os.Getenv("MULTIPATH_DEBUG")
	enableDebug := debugEnv == "1" || strings.EqualFold(debugEnv, "true")
	return &StdLogger{
		prefix: "[" + name + "] ",
		inner:  log.New(os.Stderr, "", log.LstdFlags),
		debug:  enableDebug,
	}
}

func (l *StdLogger) Debugf(format string, args ...any) {
	if !l.debug {
		return
	}
	l.inner.Output(2, l.prefix+"DEBUG "+fmt.Sprintf(format, args...)) //nolint:errcheck
}

func (l *StdLogger) Infof(format string, args ...any) {
	l.inner.Output(2, l.prefix+"INFO  "+fmt.Sprintf(format, args...)) //nolint:errcheck
}

func (l *StdLogger) Warnf(format string, args ...any) {
	l.inner.Output(2, l.prefix+"WARN  "+fmt.Sprintf(format, args...)) //nolint:errcheck
}

func (l *StdLogger) Errorf(format string, args ...any) {
	l.inner.Output(2, l.prefix+"ERROR "+fmt.Sprintf(format, args...)) //nolint:errcheck
}

// ──────────────────────────────────────────────────────────────────────────────
// NopLogger – discards all output (useful in tests)
// ──────────────────────────────────────────────────────────────────────────────

// NopLogger discards every log call.
type NopLogger struct{}

func (NopLogger) Debugf(string, ...any) {}
func (NopLogger) Infof(string, ...any)  {}
func (NopLogger) Warnf(string, ...any)  {}
func (NopLogger) Errorf(string, ...any) {}
