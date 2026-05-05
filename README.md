# MasterDNS Multipath Aggregator

> **Five DNS tunnels. One blazing-fast pipe.**  
> A high-performance multipath overlay that bonds up to five independent MasterDnsVPN tunnels into a single, resilient connection.

[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8?style=flat&logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Repo](https://img.shields.io/badge/GitHub-DarkPoesidon%2FMultiMasterDnsAggregator-blue?logo=github)](https://github.com/DarkPoesidon/MultiMasterDnsAggregator)

---

## One-Click Server Install

Paste this single command into your fresh Linux VPS as root:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/DarkPoesidon/MultiMasterDnsAggregator/main/install.sh)
```

Or with `wget`:

```bash
bash <(wget -qO- https://raw.githubusercontent.com/DarkPoesidon/MultiMasterDnsAggregator/main/install.sh)
```

The script will:
- ✅ Verify root access
- ✅ Detect your OS & CPU architecture  
- ✅ Install all missing system dependencies (curl, git, ufw…)
- ✅ Download and install Go ≥ 1.22 if needed
- ✅ Clone this repository to `/opt/masterdns-aggregator`
- ✅ Compile the server binary with size-optimized flags
- ✅ Open the required TCP port in UFW / iptables
- ✅ Install, enable, and start a `systemd` service
- ✅ Print your server IP, port, and log commands in a success banner

> **Idempotent** – safe to run multiple times. Re-running updates the binary to the latest commit.

---

## Architecture

```
Your device (Client)
├── masterdns-agg  (this repo, client binary)
│   ├── tunnel-1 ──► masterdnsvpn-client :18001 ──► DNS VPN Server 1 ──┐
│   ├── tunnel-2 ──► masterdnsvpn-client :18002 ──► DNS VPN Server 2 ──┤
│   ├── tunnel-3 ──► masterdnsvpn-client :18003 ──► DNS VPN Server 3 ──┼──► Aggregator VPS ──► Internet
│   ├── tunnel-4 ──► masterdnsvpn-client :18004 ──► DNS VPN Server 4 ──┤
│   └── tunnel-5 ──► masterdnsvpn-client :18005 ──► DNS VPN Server 5 ──┘
│
└── Exposes SOCKS5 at 127.0.0.1:19000 for your browser/apps
```

---

## Repository Structure

```
MultiMasterDnsAggregator/
├── cmd/
│   ├── masterdns-agg-server/   # ← Server binary entry point (deploy to VPS)
│   │   └── main.go
│   └── masterdns-agg/          # ← Client binary entry point (run on your machine)
│       └── main.go
├── internal/
│   ├── aggregator/             # Server-side: bearer management, stream routing,
│   │   ├── aggregator_stream.go  #             frame reassembly, upstream dialling
│   │   ├── bearer_registry.go
│   │   ├── config.go
│   │   ├── inbound_bearer.go
│   │   ├── reassembler.go
│   │   ├── server.go
│   │   ├── stream_router.go
│   │   └── util.go
│   └── multipath/              # Client-side: bearer pool, dispatcher, sequencer
│       ├── bearer.go
│       ├── config.go
│       ├── dispatcher.go
│       ├── frame.go
│       ├── logger.go
│       ├── manager.go
│       ├── pool.go
│       ├── sequencer.go
│       ├── socks5dialer.go
│       └── stream.go
├── install.sh                  # ← One-click VPS installer  ★
├── go.mod
├── go.sum
├── LICENSE
└── README.md
```

---

## Manual Build

### Prerequisites
- Go 1.22 or later ([download](https://go.dev/dl/))
- Git

### Build the server binary

```bash
git clone https://github.com/DarkPoesidon/MultiMasterDnsAggregator
cd MultiMasterDnsAggregator
go build -o masterdns-agg-server ./cmd/masterdns-agg-server/
```

### Build the client binary

```bash
go build -o masterdns-agg ./cmd/masterdns-agg/
```

---

## Server Usage

```
masterdns-agg-server [flags]

Flags:
  -listen string          TCP address to bind on             (default ":9000")
  -chunk int              Max payload bytes per MacroFrame   (default 4096)
  -dial-timeout duration  Upstream dial timeout              (default 30s)
  -streams int            Max concurrent streams, 0=∞        (default 0)
  -inbound-depth int      Frame channel buffer depth         (default 4096)
  -reassembly-buf int     Max OOO frames per stream          (default 512)
```

Example:
```bash
masterdns-agg-server -listen :9000
```

---

## Client Usage

```
masterdns-agg [flags]

Flags:
  -listen string          Local SOCKS5 listener address      (default "127.0.0.1:19000")
  -agg string             Remote Aggregator address          (default "127.0.0.1:9000")
  -chunk int              Max payload bytes per MacroFrame   (default 1024)
  -dial-timeout duration  SOCKS5 bearer connect timeout      (default 10s)
  -reconnect duration     Bearer reconnect delay             (default 3s)
  -t1..t5 string          DNS tunnel SOCKS5 addresses        (defaults :18001–:18005)
```

Example (replace `1.2.3.4` with your VPS IP):
```bash
masterdns-agg \
  -listen 127.0.0.1:19000 \
  -agg    1.2.3.4:9000    \
  -t1 127.0.0.1:18001 \
  -t2 127.0.0.1:18002 \
  -t3 127.0.0.1:18003 \
  -t4 127.0.0.1:18004 \
  -t5 127.0.0.1:18005
```

Then configure your browser SOCKS5 proxy to `127.0.0.1:19000`.

---

## Service Management (after install)

| Task | Command |
|------|---------|
| Watch live logs | `journalctl -u masterdns-aggregator -f` |
| Check status | `systemctl status masterdns-aggregator` |
| Restart | `systemctl restart masterdns-aggregator` |
| Stop | `systemctl stop masterdns-aggregator` |
| Update to latest | Re-run `install.sh` |

---

## Security Notes

- The server binary runs under `systemd` with `NoNewPrivileges`, `ProtectSystem`, and `PrivateTmp` hardening enabled.
- Only port `9000/tcp` (or your chosen port) needs to be reachable from the DNS VPN servers.
- No credentials or sensitive data are stored to disk by the server.

---

## License

MIT – see [LICENSE](LICENSE).
