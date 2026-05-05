// ==============================================================================
// MasterDNS Multipath Aggregator – Server Entry Point
// Repository: https://github.com/DarkPoesidon/MultiMasterDnsAggregator
//
// Usage:
//   masterdns-agg-server [-listen :9000] [-chunk 4096] [-streams 0]
//
// This binary is the server-side Aggregator.  It:
//   1. Binds on :9000 (or -listen) and accepts persistent TCP bearer connections
//      from 5 DNS-VPN-server → Aggregator upstream links.
//   2. Decodes MacroFrames from each bearer.
//   3. Routes frames to logical streams by StreamID.
//   4. On SYN: parses target host:port from the SYN payload, dials it.
//   5. Reassembles out-of-order frames, writes to upstream TCP.
//   6. Reads upstream responses, chunks them, and sends response MacroFrames
//      back to the client via the sticky bearer.
//   7. Runs until SIGINT / SIGTERM.
//
// Deployment:
//   Deploy on a VPS that has unrestricted outbound TCP.
//   The 5 DNS-VPN-server instances must be able to reach this host on
//   the configured port (default 9000).
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

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/aggregator"
)

func main() {
	// ── CLI flags ────────────────────────────────────────────────────────────
	listenAddr := flag.String("listen", ":9000",
		"TCP address to accept bearer connections on (host:port or :port)")
	chunkSize := flag.Int("chunk", 4096,
		"Max response payload bytes per MacroFrame")
	dialTimeout := flag.Duration("dial-timeout", 30*time.Second,
		"Upstream TCP dial timeout per stream")
	maxStreams := flag.Int("streams", 0,
		"Maximum concurrent logical streams (0 = unlimited)")
	inboundDepth := flag.Int("inbound-depth", 4096,
		"Inbound frame channel depth")
	maxReassembly := flag.Int("reassembly-buf", 512,
		"Max out-of-order frames buffered per stream before RST")
	flag.Parse()

	// ── Build configuration ──────────────────────────────────────────────────
	cfg := aggregator.AggregatorConfig{
		ListenAddr:          *listenAddr,
		ChunkSize:           *chunkSize,
		UpstreamDialTimeout: *dialTimeout,
		MaxStreams:          *maxStreams,
		InboundChannelDepth: *inboundDepth,
		MaxReassemblyBuffer: *maxReassembly,
		ReassemblyTimeout:   60 * time.Second,
	}

	// ── Logger ───────────────────────────────────────────────────────────────
	log := aggregator.NewStdLogger("masterdns-agg-server")

	log.Infof("============================================================")
	log.Infof("MasterDNS Multipath Aggregator – Server")
	log.Infof("Listen        : %s", cfg.ListenAddr)
	log.Infof("ChunkSize     : %d bytes", cfg.ChunkSize)
	log.Infof("DialTimeout   : %v", cfg.UpstreamDialTimeout)
	log.Infof("MaxStreams     : %d (0=unlimited)", cfg.MaxStreams)
	log.Infof("InboundDepth  : %d", cfg.InboundChannelDepth)
	log.Infof("ReassemblyBuf : %d frames", cfg.MaxReassemblyBuffer)
	log.Infof("============================================================")

	// ── Context wired to OS signals ──────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Server ───────────────────────────────────────────────────────────────
	srv := aggregator.NewAggregatorServer(cfg, log)

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "aggregator error: %v\n", err)
		os.Exit(1)
	}

	log.Infof("shutdown complete")
}
