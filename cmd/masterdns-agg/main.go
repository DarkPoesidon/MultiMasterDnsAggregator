// ==============================================================================
// MasterDNS Multipath Aggregator – Client Entry Point
// Repository: https://github.com/DarkPoesidon/MultiMasterDnsAggregator
//
// Usage:
//   masterdns-agg -listen 127.0.0.1:19000 -agg <server-ip>:9000 [-chunk 1024]
//
// This binary:
//   1. Reads configuration from CLI flags.
//   2. Constructs a MultipathManager backed by 5 DNS-tunnel SOCKS5 endpoints
//      (127.0.0.1:18001 → 127.0.0.1:18005 by default).
//   3. Starts the manager (connects bearer tunnels).
//   4. Starts the MultipathDispatcher to accept plain-TCP local connections.
//   5. Runs until SIGINT / SIGTERM.
//
// Prerequisites:
//   Five independent instances of masterdnsvpn-client must be running, each
//   with a distinct ListenPort (18001–18005) and each pointing at a different
//   remote DNS VPN server.  The dispatcher will connect through each of those
//   clients' SOCKS5 listeners to the Aggregator address.
// ==============================================================================

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

func main() {
	// ── CLI flags ────────────────────────────────────────────────────────────
	listenAddr := flag.String("listen", "127.0.0.1:19000",
		"Local TCP address the MultipathDispatcher will accept connections on")
	aggAddr := flag.String("agg", "127.0.0.1:9000",
		"Remote Aggregator TCP address (host:port)")
	chunkSize := flag.Int("chunk", 1024,
		"Max payload bytes per macro frame (should be ≤ tunnel upload MTU - 21)")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second,
		"SOCKS5 bearer connect timeout")
	reconnDelay := flag.Duration("reconnect", 3*time.Second,
		"Pause between bearer reconnection attempts")
	t1 := flag.String("t1", "127.0.0.1:18001", "DNS tunnel 1 SOCKS5 addr")
	t2 := flag.String("t2", "127.0.0.1:18002", "DNS tunnel 2 SOCKS5 addr")
	t3 := flag.String("t3", "127.0.0.1:18003", "DNS tunnel 3 SOCKS5 addr")
	t4 := flag.String("t4", "127.0.0.1:18004", "DNS tunnel 4 SOCKS5 addr")
	t5 := flag.String("t5", "127.0.0.1:18005", "DNS tunnel 5 SOCKS5 addr")
	flag.Parse()

	// ── Build configuration ──────────────────────────────────────────────────
	cfg := multipath.MultipathConfig{
		ListenAddr:          *listenAddr,
		AggregatorAddr:      *aggAddr,
		ChunkSize:           *chunkSize,
		DialTimeout:         *dialTimeout,
		ReconnectDelay:      *reconnDelay,
		ReadBufferSize:      32 * 1024,
		InboundChannelDepth: 4096,
		DispatchRetries:     0,
		Tunnels: []multipath.TunnelEndpoint{
			{SOCKS5Addr: *t1, Label: "tunnel-1", Weight: 1},
			{SOCKS5Addr: *t2, Label: "tunnel-2", Weight: 1},
			{SOCKS5Addr: *t3, Label: "tunnel-3", Weight: 1},
			{SOCKS5Addr: *t4, Label: "tunnel-4", Weight: 1},
			{SOCKS5Addr: *t5, Label: "tunnel-5", Weight: 1},
		},
	}

	// ── Logger ───────────────────────────────────────────────────────────────
	log := multipath.NewStdLogger("masterdns-agg")

	log.Infof("============================================================")
	log.Infof("MasterDNS Multipath Aggregator – Client")
	log.Infof("Listen    : %s", cfg.ListenAddr)
	log.Infof("Aggregator: %s", cfg.AggregatorAddr)
	log.Infof("Tunnels   :")
	for _, t := range cfg.Tunnels {
		w := t.Weight
		if w < 1 {
			w = 1
		}
		log.Infof("  %-12s → %s (weight %d)", t.Label, t.SOCKS5Addr, w)
	}
	log.Infof("ChunkSize : %d bytes", cfg.ChunkSize)
	log.Infof("============================================================")

	// ── Context wired to OS signals ──────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Manager ──────────────────────────────────────────────────────────────
	mgr := multipath.NewMultipathManager(cfg, log)
	mgr.Start(ctx)
	defer func() {
		mgr.Stop()
		log.Infof("MultipathManager stopped")
	}()

	// ── Dispatcher ───────────────────────────────────────────────────────────
	disp := multipath.NewMultipathDispatcher(cfg, mgr, log)

	if err := disp.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "dispatcher error: %v\n", err)
		os.Exit(1)
	}

	log.Infof("shutdown complete")
}
