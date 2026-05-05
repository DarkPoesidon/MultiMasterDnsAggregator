// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  MultipathDispatcher – SOCKS5 proxy listener that accepts local
//           connections, performs the SOCKS5 handshake to extract the target
//           host:port, then forwards the connection through the multipath
//           bearer pool to the remote Aggregator.
//
// Architecture position:
//
//   Browser / curl / app (configured to use SOCKS5 proxy)
//       │  SOCKS5 to ListenAddr (e.g. 127.0.0.1:19000)
//       ▼
//   MultipathDispatcher.Run()   ← SOCKS5 server (RFC 1928 / RFC 1929)
//       │  extracts target "host:port" from CONNECT request
//       │  sends SOCKS5 success reply to local app
//       ▼
//   MultipathManager.NewStream(conn, targetAddr)
//       │
//       ▼
//   LogicalStream.Run()
//       ├── runOutbound: SYN(target)+chunks → bearer pool → Aggregator
//       └── runInbound:  Aggregator → bearer pool → demux → local app
//
// SOCKS5 support:
//   - Authentication: NO_AUTH (method 0x00) only
//   - Commands: CONNECT only (BIND and UDP ASSOCIATE are refused)
//   - Address types: IPv4, IPv6, and domain name (ATYP 0x01/0x04/0x03)
//   - The replied bound address is always 0.0.0.0:0 (we are a relay, not
//     a direct proxy, so no real local binding occurs)
//
// The SOCKS5 handshake happens BEFORE the stream is created, so the target
// address is available to encode in the SYN MacroFrame payload (Option 2).
// ==============================================================================

package multipath

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"
)

// ──────────────────────────────────────────────────────────────────────────────
// MultipathDispatcher
// ──────────────────────────────────────────────────────────────────────────────

// MultipathDispatcher accepts SOCKS5 connections from local applications and
// creates a LogicalStream for each one, routing it through the bearer pool.
type MultipathDispatcher struct {
	cfg     MultipathConfig
	manager *MultipathManager
	log     Logger

	// listener is the bound TCP listener; assigned in Run().
	listener net.Listener

	// running tracks whether the listener is active.
	running atomic.Bool
}

// NewMultipathDispatcher creates a dispatcher backed by manager.
// Call Run(ctx) to start listening.
func NewMultipathDispatcher(cfg MultipathConfig, manager *MultipathManager, log Logger) *MultipathDispatcher {
	return &MultipathDispatcher{
		cfg:     cfg,
		manager: manager,
		log:     log,
	}
}

// Run binds to cfg.ListenAddr (SOCKS5) and blocks until ctx is cancelled.
func (d *MultipathDispatcher) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return err
	}
	d.listener = ln
	d.running.Store(true)

	d.log.Infof("[MultipathDispatcher] SOCKS5 listening on %s → Aggregator %s (%d tunnels)",
		d.cfg.ListenAddr, d.cfg.AggregatorAddr, len(d.cfg.Tunnels))

	go func() {
		<-ctx.Done()
		d.Stop()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if !d.running.Load() {
				return nil
			}
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

// Stop closes the listener.
func (d *MultipathDispatcher) Stop() {
	if d.running.CompareAndSwap(true, false) {
		if d.listener != nil {
			_ = d.listener.Close()
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Per-connection handler
// ──────────────────────────────────────────────────────────────────────────────

// handleConn performs the SOCKS5 handshake, creates a LogicalStream with the
// parsed target address, runs it, and cleans up on exit.
func (d *MultipathDispatcher) handleConn(_ context.Context, conn net.Conn) {
	defer func() {
		// Final safety close in case something errors before stream takes ownership.
		_ = conn.Close()
	}()

	// ── SOCKS5 handshake ────────────────────────────────────────────────────
	target, err := socks5ServerHandshake(conn)
	if err != nil {
		d.log.Debugf("[MultipathDispatcher] SOCKS5 handshake from %s failed: %v",
			conn.RemoteAddr(), err)
		return
	}

	d.log.Debugf("[MultipathDispatcher] CONNECT %s from %s", target, conn.RemoteAddr())

	// ── Create stream and run ───────────────────────────────────────────────
	stream := d.manager.NewStream(conn, target)

	d.log.Infof("[MultipathDispatcher] Stream %d: %s → %s",
		stream.ID(), conn.RemoteAddr(), target)

	stream.Run() // blocks until both goroutines exit

	d.manager.CloseStream(stream.ID())

	d.log.Debugf("[MultipathDispatcher] Stream %d closed", stream.ID())
}

// ──────────────────────────────────────────────────────────────────────────────
// Minimal SOCKS5 server (RFC 1928, NO_AUTH only, CONNECT only)
// ──────────────────────────────────────────────────────────────────────────────

const (
	socks5Ver        = 0x05
	socks5AuthNoAuth = 0x00
	socks5AuthFail   = 0xFF // no acceptable methods
	socks5CmdConnect = 0x01
	socks5RSV        = 0x00
	socks5AtypIPv4   = 0x01
	socks5AtypDomain = 0x03
	socks5AtypIPv6   = 0x04
	socks5RepSuccess = 0x00
)

// socks5ServerHandshake completes the SOCKS5 server-side handshake on conn and
// returns the target "host:port" string from the CONNECT request.
func socks5ServerHandshake(conn net.Conn) (string, error) {
	// ── Phase 1: method negotiation ─────────────────────────────────────────
	// Read: VER(1) NMETHODS(1) METHODS(NMETHODS)
	header := make([]byte, 2)
	if _, err := readExact(conn, header); err != nil {
		return "", fmt.Errorf("reading method header: %w", err)
	}
	if header[0] != socks5Ver {
		return "", fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	nMethods := int(header[1])
	if nMethods == 0 {
		return "", fmt.Errorf("no authentication methods offered")
	}
	methods := make([]byte, nMethods)
	if _, err := readExact(conn, methods); err != nil {
		return "", fmt.Errorf("reading methods: %w", err)
	}

	// We support NO_AUTH (0x00) only.
	accepted := false
	for _, m := range methods {
		if m == socks5AuthNoAuth {
			accepted = true
			break
		}
	}
	if !accepted {
		_, _ = conn.Write([]byte{socks5Ver, socks5AuthFail})
		return "", fmt.Errorf("no acceptable auth method offered by client")
	}
	if _, err := conn.Write([]byte{socks5Ver, socks5AuthNoAuth}); err != nil {
		return "", fmt.Errorf("sending method select: %w", err)
	}

	// ── Phase 2: CONNECT request ─────────────────────────────────────────────
	// Read: VER(1) CMD(1) RSV(1) ATYP(1)
	reqHdr := make([]byte, 4)
	if _, err := readExact(conn, reqHdr); err != nil {
		return "", fmt.Errorf("reading request header: %w", err)
	}
	if reqHdr[0] != socks5Ver {
		return "", fmt.Errorf("unexpected version in request: %d", reqHdr[0])
	}
	if reqHdr[1] != socks5CmdConnect {
		// Refuse with "command not supported".
		sendSocks5Reply(conn, 0x07, net.IPv4zero, 0)
		return "", fmt.Errorf("unsupported SOCKS5 command 0x%02x", reqHdr[1])
	}

	atyp := reqHdr[3]
	var host string

	switch atyp {
	case socks5AtypIPv4:
		addr := make([]byte, 4)
		if _, err := readExact(conn, addr); err != nil {
			return "", fmt.Errorf("reading IPv4 addr: %w", err)
		}
		host = net.IP(addr).String()

	case socks5AtypIPv6:
		addr := make([]byte, 16)
		if _, err := readExact(conn, addr); err != nil {
			return "", fmt.Errorf("reading IPv6 addr: %w", err)
		}
		host = "[" + net.IP(addr).String() + "]"

	case socks5AtypDomain:
		lenBuf := make([]byte, 1)
		if _, err := readExact(conn, lenBuf); err != nil {
			return "", fmt.Errorf("reading domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := readExact(conn, domain); err != nil {
			return "", fmt.Errorf("reading domain: %w", err)
		}
		host = string(domain)

	default:
		sendSocks5Reply(conn, 0x08, net.IPv4zero, 0) // address type not supported
		return "", fmt.Errorf("unsupported ATYP 0x%02x", atyp)
	}

	// Read port (2 bytes big-endian).
	portBuf := make([]byte, 2)
	if _, err := readExact(conn, portBuf); err != nil {
		return "", fmt.Errorf("reading port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBuf)

	target := fmt.Sprintf("%s:%d", host, port)

	// Send success reply: bound address 0.0.0.0:0 (relay, not direct).
	sendSocks5Reply(conn, socks5RepSuccess, net.IPv4zero, 0)

	return target, nil
}

// sendSocks5Reply writes a SOCKS5 reply with an IPv4 bound address.
func sendSocks5Reply(conn net.Conn, rep byte, bndIP net.IP, bndPort uint16) {
	bnd := bndIP.To4()
	if bnd == nil {
		bnd = net.IPv4zero.To4()
	}
	reply := []byte{
		socks5Ver, rep, socks5RSV, socks5AtypIPv4,
		bnd[0], bnd[1], bnd[2], bnd[3],
		byte(bndPort >> 8), byte(bndPort),
	}
	_, _ = conn.Write(reply)
}

// readExact reads exactly len(buf) bytes from conn, handling short reads.
func readExact(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
