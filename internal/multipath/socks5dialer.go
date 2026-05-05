// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  Minimal SOCKS5 client dialer.
//
// This file intentionally avoids golang.org/x/net/proxy so the multipath
// package stays within the existing go.mod without adding new dependencies.
//
// Supported SOCKS5 negotiation path:
//   Client hello  → NO_AUTH (0x00)
//   Server choice → confirms NO_AUTH
//   CONNECT       → IPv4 / IPv6 / domain
//   Reply         → success (rep=0x00)
//
// If the target DNS-tunnel SOCKS5 server requires username/password auth,
// supply non-empty user/pass to dialSOCKS5WithAuth.
// ==============================================================================

package multipath

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// dialSOCKS5 connects to proxyAddr (a local SOCKS5 listener exposed by one
// running masterdnsvpn-client instance) and issues a CONNECT to targetAddr.
// On success it returns a net.Conn whose reads/writes flow through the DNS
// tunnel to the Aggregator.
func dialSOCKS5(proxyAddr, targetAddr string, timeout time.Duration) (net.Conn, error) {
	return dialSOCKS5WithAuth(proxyAddr, targetAddr, "", "", timeout)
}

// dialSOCKS5WithAuth is like dialSOCKS5 but supports username/password auth.
// Pass empty strings for user and pass to request NO_AUTH.
func dialSOCKS5WithAuth(proxyAddr, targetAddr, user, pass string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial proxy %s: %w", proxyAddr, err)
	}

	// All I/O during the handshake is bounded by timeout.
	deadline := time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 set deadline: %w", err)
	}

	// ── Step 1: Client greeting ───────────────────────────────────────────
	useAuth := user != "" || pass != ""
	if err := socks5Greeting(conn, useAuth); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// ── Step 2: Auth (if required) ────────────────────────────────────────
	if useAuth {
		if err := socks5UserPassAuth(conn, user, pass); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	// ── Step 3: CONNECT request ───────────────────────────────────────────
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 parse target %q: %w", targetAddr, err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 resolve port %q: %w", portStr, err)
	}

	req := buildConnectRequest(host, uint16(port))
	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 CONNECT write: %w", err)
	}

	// ── Step 4: CONNECT reply ─────────────────────────────────────────────
	if err := readConnectReply(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Clear the handshake deadline; the caller sets its own.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 clear deadline: %w", err)
	}

	return conn, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

func socks5Greeting(conn net.Conn, useAuth bool) error {
	var methods []byte
	if useAuth {
		// Offer NO_AUTH (0x00) and USERNAME/PASSWORD (0x02).
		methods = []byte{0x05, 0x02, 0x00, 0x02}
	} else {
		// Offer NO_AUTH only.
		methods = []byte{0x05, 0x01, 0x00}
	}
	if _, err := conn.Write(methods); err != nil {
		return fmt.Errorf("socks5 greeting write: %w", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 greeting read: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("socks5 greeting: unexpected version byte 0x%02x", reply[0])
	}
	if reply[1] == 0xFF {
		return fmt.Errorf("socks5 greeting: server rejected all auth methods")
	}
	if useAuth && reply[1] != 0x00 && reply[1] != 0x02 {
		return fmt.Errorf("socks5 greeting: server chose unsupported method 0x%02x", reply[1])
	}
	if !useAuth && reply[1] != 0x00 {
		return fmt.Errorf("socks5 greeting: server requires auth (method 0x%02x)", reply[1])
	}
	return nil
}

func socks5UserPassAuth(conn net.Conn, user, pass string) error {
	// RFC 1929 sub-negotiation: VER=1, ULEN, UNAME..., PLEN, PASSWD...
	req := make([]byte, 3+len(user)+len(pass))
	req[0] = 0x01
	req[1] = byte(len(user))
	copy(req[2:], user)
	req[2+len(user)] = byte(len(pass))
	copy(req[3+len(user):], pass)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5 auth write: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 auth read: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5 auth: server rejected credentials (status 0x%02x)", resp[1])
	}
	return nil
}

// buildConnectRequest builds the SOCKS5 CONNECT request for the given host
// and port.  host may be an IPv4 address, IPv6 address, or domain name.
func buildConnectRequest(host string, port uint16) []byte {
	var req []byte
	ip := net.ParseIP(host)

	switch {
	case ip != nil && ip.To4() != nil:
		// ATYP = 0x01 (IPv4)
		req = make([]byte, 10)
		req[0] = 0x05 // VER
		req[1] = 0x01 // CMD = CONNECT
		req[2] = 0x00 // RSV
		req[3] = 0x01 // ATYP = IPv4
		copy(req[4:8], ip.To4())
		binary.BigEndian.PutUint16(req[8:10], port)

	case ip != nil && ip.To16() != nil:
		// ATYP = 0x04 (IPv6)
		req = make([]byte, 22)
		req[0] = 0x05
		req[1] = 0x01
		req[2] = 0x00
		req[3] = 0x04 // ATYP = IPv6
		copy(req[4:20], ip.To16())
		binary.BigEndian.PutUint16(req[20:22], port)

	default:
		// ATYP = 0x03 (domain)
		hostBytes := []byte(host)
		req = make([]byte, 7+len(hostBytes))
		req[0] = 0x05
		req[1] = 0x01
		req[2] = 0x00
		req[3] = 0x03 // ATYP = domain
		req[4] = byte(len(hostBytes))
		copy(req[5:5+len(hostBytes)], hostBytes)
		binary.BigEndian.PutUint16(req[5+len(hostBytes):], port)
	}
	return req
}

// readConnectReply consumes the SOCKS5 CONNECT reply and verifies success.
func readConnectReply(conn net.Conn) error {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return fmt.Errorf("socks5 reply header: %w", err)
	}
	if hdr[1] != 0x00 {
		return fmt.Errorf("socks5 CONNECT denied: rep=0x%02x", hdr[1])
	}

	// Drain the bound-address field from the reply so the connection is left
	// in a clean state for the caller's application data.
	switch hdr[3] {
	case 0x01: // IPv4 (4 bytes) + port (2 bytes)
		drain := make([]byte, 6)
		_, _ = io.ReadFull(conn, drain)
	case 0x03: // domain: 1-byte length prefix + domain + port (2 bytes)
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("socks5 reply domain length: %w", err)
		}
		drain := make([]byte, int(lenBuf[0])+2)
		_, _ = io.ReadFull(conn, drain)
	case 0x04: // IPv6 (16 bytes) + port (2 bytes)
		drain := make([]byte, 18)
		_, _ = io.ReadFull(conn, drain)
	default:
		return fmt.Errorf("socks5 reply: unknown ATYP 0x%02x", hdr[3])
	}
	return nil
}
