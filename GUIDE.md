# MasterDNS Aggregator – Complete User Guide

**Who this guide is for:** Someone who has never used MasterDnsVPN before and wants to run
the full Multipath Aggregator stack from scratch.  No Go, networking, or Linux knowledge is
assumed.  Every command is spelled out exactly.

---

## Table of Contents

1. [What is the MasterDNS Aggregator?](#1-what-is-the-masterdns-aggregator)
2. [How the whole system works (plain English)](#2-how-the-whole-system-works)
3. [What you need before you start](#3-what-you-need-before-you-start)
4. [Overview of the three machines involved](#4-overview-of-the-three-machines-involved)
5. [Step 1 – Set up the Server VPS (MasterDnsVPN servers × 5)](#5-step-1--set-up-the-server-vps)
6. [Step 2 – Set up the Aggregator VPS](#6-step-2--set-up-the-aggregator-vps)
7. [Step 3 – Set up the Client machine](#7-step-3--set-up-the-client-machine)
8. [Step 4 – Run everything](#8-step-4--run-everything)
9. [Step 5 – Configure your browser or app](#9-step-5--configure-your-browser-or-app)
10. [Verifying it works](#10-verifying-it-works)
11. [Troubleshooting](#11-troubleshooting)
12. [All command-line flags reference](#12-all-command-line-flags-reference)
13. [Security checklist](#13-security-checklist)
14. [Stopping everything gracefully](#14-stopping-everything-gracefully)

---

## 1. What is the MasterDNS Aggregator?

MasterDnsVPN is a tool that hides your internet traffic inside ordinary DNS queries – the same
kind of DNS lookup that every computer does thousands of times a day.  Censorship systems and
firewalls generally cannot block DNS traffic without breaking the internet for everyone, so this
is one of the hardest traffic types to block.

The **Aggregator** is an extension that runs **five independent DNS tunnels at the same time**
and combines ("aggregates") them into a single, faster, more reliable connection.  Think of it
like having five water pipes instead of one: if one gets blocked or slows down, the others keep
flowing.

**Normal MasterDnsVPN (without Aggregator):**

```
Your computer ──(DNS tunnel)──► One server ──► Internet
```

**MasterDnsVPN with Aggregator:**

```
Your computer ──(tunnel 1)──► Server 1 ──┐
              ──(tunnel 2)──► Server 2 ──┤
              ──(tunnel 3)──► Server 3 ──┼──► Aggregator VPS ──► Internet
              ──(tunnel 4)──► Server 4 ──┤
              ──(tunnel 5)──► Server 5 ──┘
```

---

## 2. How the whole system works

Here is the exact journey a single web request takes:

1. **Your browser** is set to use a SOCKS5 proxy at `127.0.0.1:19000`.
2. **The Aggregator Client** (running on your PC) receives the SOCKS5 connection, learns
   the target website (e.g. `example.com:443`), and splits the data into small chunks.
3. Each chunk is stamped with a sequence number and sent down one of **5 DNS tunnels**, chosen
   in round-robin.
4. Each **DNS tunnel** (a `masterdnsvpn-client` process) encodes the chunk as a DNS query and
   sends it to a **DNS VPN server**.
5. The **DNS VPN server** decodes the query and forwards the chunk as a TCP connection to the
   **Aggregator Server** on your VPS.
6. The **Aggregator Server** collects chunks from all five bearers, reorders them using the
   sequence numbers, and delivers a clean byte stream to the real website.
7. The website's response travels back in reverse: Aggregator → DNS servers → your PC → browser.

---

## 3. What you need before you start

### Hardware / accounts

| What | Where to get it | Notes |
|------|-----------------|-------|
| A VPS (Virtual Private Server) for the **5 DNS VPN servers** | Any VPS provider (Hetzner, DigitalOcean, Vultr, etc.) | Needs **port 53 UDP** open. Can be the same machine for all 5, using different domains, OR 5 separate VPS. This guide uses 5 separate VPS for maximum benefit. |
| A VPS for the **Aggregator** | Same or different provider | Needs **port 9000 TCP** open outbound AND inbound. Must have unrestricted outbound internet access. |
| Your **local machine** | The computer in front of you | Windows, Linux, or macOS. |

### Software

- **Go 1.22 or later** – the programming language the code is written in.
  Download from https://go.dev/dl/ – pick your OS.
- **Git** – to download the source code.
  Download from https://git-scm.com/downloads

### Domain names (required for DNS tunneling)

You need **5 domain names** (or 5 subdomains) that you control, one for each DNS VPN server.
Example:
```
v1.yourdomain.com  →  points to Server 1's IP
v2.yourdomain.com  →  points to Server 2's IP
v3.yourdomain.com  →  points to Server 3's IP
v4.yourdomain.com  →  points to Server 4's IP
v5.yourdomain.com  →  points to Server 5's IP
```

How to set DNS records: log in to your domain registrar (e.g. Cloudflare, Namecheap), go to
DNS settings, add an **A record** for each subdomain pointing to the corresponding server IP.

---

## 4. Overview of the three machines involved

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  YOUR LOCAL MACHINE                                                         │
│                                                                             │
│  masterdnsvpn-client (×5)       ← 5 separate client processes              │
│    client1: SOCKS5 on :18001, domain v1.yourdomain.com                     │
│    client2: SOCKS5 on :18002, domain v2.yourdomain.com                     │
│    client3: SOCKS5 on :18003, domain v3.yourdomain.com                     │
│    client4: SOCKS5 on :18004, domain v4.yourdomain.com                     │
│    client5: SOCKS5 on :18005, domain v5.yourdomain.com                     │
│                                                                             │
│  masterdns_agg (Aggregator Client)  ← SOCKS5 server on :19000             │
│    Connects through the 5 clients above to reach the Aggregator VPS        │
│                                                                             │
│  Your browser / app  ← configured to use SOCKS5 proxy 127.0.0.1:19000    │
└─────────────────────────────────────────────────────────────────────────────┘
           │ DNS queries (UDP port 53) to 5 different servers
           ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│  5 × SERVER VPS (each running masterdnsvpn-server)                          │
│                                                                             │
│  Server 1: v1.yourdomain.com, listens UDP :53                              │
│  Server 2: v2.yourdomain.com, listens UDP :53                              │
│  ...                                                                        │
│  Each server decodes DNS queries → forwards TCP to the Aggregator VPS       │
└──────────────────────────────────────────────────────────────────────────────┘
           │ TCP connections to Aggregator VPS port 9000
           ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│  AGGREGATOR VPS (running masterdns_agg_server)                              │
│                                                                             │
│  Receives frames from all 5 servers, reorders them, and makes              │
│  outbound TCP connections to real websites on your behalf.                 │
└──────────────────────────────────────────────────────────────────────────────┘
           │ TCP to real websites
           ▼
        Internet
```

---

## 5. Step 1 – Set up the Server VPS

Repeat all the sub-steps below **5 times** (once per server VPS).  Call them Server 1 through
Server 5, each with a different domain name.

### 5.1 Install Go on the server

SSH into your server VPS:

```bash
ssh root@<SERVER_IP>
```

Install Go:

```bash
# Download Go (check https://go.dev/dl/ for the latest version)
wget https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# Verify
go version
# Should print: go version go1.25.0 linux/amd64
```

### 5.2 Download the project

```bash
git clone https://github.com/masterking32/MasterDnsVPN.git
cd MasterDnsVPN
```

### 5.3 Create the server config file

Copy the sample config:

```bash
cp server_config.toml.simple server_config.toml
```

Edit it:

```bash
nano server_config.toml
```

Find and change these lines (replace the example values with your own):

```toml
# The domain for THIS server only
DOMAIN = ["v1.yourdomain.com"]

# How this server handles connections: SOCKS5 mode so the Aggregator can
# choose the destination for each connection.
PROTOCOL_TYPE = "SOCKS5"

# Encryption must match all clients. Choose one and use it everywhere.
# 2 = ChaCha20  (recommended)
DATA_ENCRYPTION_METHOD = 2

# A secret key shared between client and server.
# Must be the SAME on every client and every server.
# Use any random string, e.g.: openssl rand -hex 32
ENCRYPTION_KEY = "your-random-secret-key-here"
```

Save and exit (`Ctrl+X` then `Y` then `Enter` in nano).

### 5.4 Open UDP port 53

If your VPS provider has a firewall panel (e.g. Hetzner Firewall or AWS Security Groups),
allow inbound **UDP port 53** from all IPs (0.0.0.0/0).

On the server itself:

```bash
# Ubuntu/Debian:
ufw allow 53/udp
ufw enable
```

### 5.5 Build and run the server

```bash
go build -o masterdnsvpn-server ./cmd/server/
```

Run it (as root – port 53 requires root on Linux):

```bash
sudo ./masterdnsvpn-server
```

You should see output like:

```
[Server] Listening on UDP 0.0.0.0:53
[Server] Domain: v1.yourdomain.com
```

**To keep it running after you close the SSH session**, use a systemd service or `screen`:

```bash
# Simple option using screen:
screen -S masterdns-server
sudo ./masterdnsvpn-server
# Press Ctrl+A then D to detach
```

**Repeat steps 5.1–5.5 for Server 2 through Server 5**, changing the domain name each time:
`v2.yourdomain.com`, `v3.yourdomain.com`, etc.

---

## 6. Step 2 – Set up the Aggregator VPS

This is a **separate VPS** (or the same one if resources allow, though a separate one is better).

### 6.1 Install Go

Same steps as Section 5.1.

### 6.2 Download the project

```bash
git clone https://github.com/masterking32/MasterDnsVPN.git
cd MasterDnsVPN
```

### 6.3 Open port 9000

In your VPS firewall panel AND on the server:

```bash
ufw allow 9000/tcp
ufw enable
```

### 6.4 Build the Aggregator Server binary

```bash
go build -o masterdns-agg-server ./masterdns_aggregator/cmd/masterdns_agg_server/
```

### 6.5 Run the Aggregator Server

```bash
./masterdns-agg-server -listen :9000
```

You should see:

```
[MasterDnsVPN-Aggregator] INFO  ============================================================
[MasterDnsVPN-Aggregator] INFO  MasterDnsVPN – Aggregator Server
[MasterDnsVPN-Aggregator] INFO  Listen        : :9000
[MasterDnsVPN-Aggregator] INFO  ChunkSize     : 4096 bytes
...
[MasterDnsVPN-Aggregator] INFO  ============================================================
[Aggregator] Listening on :9000
```

Keep it running with `screen` as before:

```bash
screen -S masterdns-agg
./masterdns-agg-server -listen :9000
# Ctrl+A then D to detach
```

Note your Aggregator VPS's public IP address.  You will need it in Step 3.
You can see it with:

```bash
curl ifconfig.me
```

---

## 7. Step 3 – Set up the Client machine

All of this runs on **your local computer** (Windows, Linux, or macOS).

### 7.1 Install Go

**Windows:**
1. Go to https://go.dev/dl/
2. Download `go1.25.0.windows-amd64.msi`
3. Run the installer – click Next through everything
4. Open a new Command Prompt and type `go version` to verify

**Linux/macOS:**
```bash
wget https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

### 7.2 Download the project

**Windows (Command Prompt or PowerShell):**
```powershell
git clone https://github.com/masterking32/MasterDnsVPN.git
cd MasterDnsVPN
```

**Linux/macOS:**
```bash
git clone https://github.com/masterking32/MasterDnsVPN.git
cd MasterDnsVPN
```

### 7.3 Create 5 client config files

You need one config file per DNS tunnel.  The key difference between each file is:
- The **domain** (`DOMAINS`) — matches the server it connects to
- The **listen port** (`LISTEN_PORT`) — must be unique: 18001, 18002, 18003, 18004, 18005

**Windows — PowerShell:**
```powershell
for ($i = 1; $i -le 5; $i++) {
    Copy-Item client_config.toml.simple "client${i}.toml"
}
```

**Linux/macOS:**
```bash
for i in 1 2 3 4 5; do
    cp client_config.toml.simple client${i}.toml
done
```

Now edit each file.  Open `client1.toml` in any text editor (Notepad on Windows, `nano` on Linux):

**client1.toml** — change these lines:
```toml
DOMAINS = ["v1.yourdomain.com"]   # ← your Server 1 domain
LISTEN_PORT = 18001                # ← unique port for this tunnel
DATA_ENCRYPTION_METHOD = 2         # ← must match the server
ENCRYPTION_KEY = "your-random-secret-key-here"   # ← same key as server
```

**client2.toml:**
```toml
DOMAINS = ["v2.yourdomain.com"]
LISTEN_PORT = 18002
DATA_ENCRYPTION_METHOD = 2
ENCRYPTION_KEY = "your-random-secret-key-here"
```

Repeat for `client3.toml` (port 18003), `client4.toml` (18004), `client5.toml` (18005),
changing the domain and port each time.

### 7.4 Build the binaries

**Build the standard DNS tunnel client:**

```powershell
# Windows
go build -o masterdnsvpn-client.exe ./cmd/client/
```

```bash
# Linux/macOS
go build -o masterdnsvpn-client ./cmd/client/
```

**Build the Aggregator Client:**

```powershell
# Windows
go build -o masterdns-agg.exe ./masterdns_aggregator/cmd/masterdns_agg/
```

```bash
# Linux/macOS
go build -o masterdns-agg ./masterdns_aggregator/cmd/masterdns_agg/
```

---

## 8. Step 4 – Run everything

You will open **6 terminal windows** (or tabs) on your local machine.

### Terminals 1–5: The 5 DNS tunnel clients

Open a terminal window for each one.

**Windows (PowerShell):**

Terminal 1:
```powershell
.\masterdnsvpn-client.exe -config client1.toml
```

Terminal 2:
```powershell
.\masterdnsvpn-client.exe -config client2.toml
```

Terminal 3:
```powershell
.\masterdnsvpn-client.exe -config client3.toml
```

Terminal 4:
```powershell
.\masterdnsvpn-client.exe -config client4.toml
```

Terminal 5:
```powershell
.\masterdnsvpn-client.exe -config client5.toml
```

**Linux/macOS:**
Replace `.\masterdnsvpn-client.exe` with `./masterdnsvpn-client`.

Each terminal should print something like:
```
[Client] SOCKS5 proxy listening on 127.0.0.1:18001
[Client] Using domain: v1.yourdomain.com
```

**Wait until all 5 clients are running before continuing.**

### Terminal 6: The Aggregator Client

Replace `<AGGREGATOR_VPS_IP>` with the public IP of your Aggregator VPS:

**Windows:**
```powershell
.\masterdns-agg.exe `
  -listen  127.0.0.1:19000 `
  -agg     <AGGREGATOR_VPS_IP>:9000 `
  -t1      127.0.0.1:18001 `
  -t2      127.0.0.1:18002 `
  -t3      127.0.0.1:18003 `
  -t4      127.0.0.1:18004 `
  -t5      127.0.0.1:18005
```

**Linux/macOS:**
```bash
./masterdns-agg \
  -listen  127.0.0.1:19000 \
  -agg     <AGGREGATOR_VPS_IP>:9000 \
  -t1      127.0.0.1:18001 \
  -t2      127.0.0.1:18002 \
  -t3      127.0.0.1:18003 \
  -t4      127.0.0.1:18004 \
  -t5      127.0.0.1:18005
```

You should see:

```
[MasterDnsVPN-Multipath] INFO  ============================================================
[MasterDnsVPN-Multipath] INFO  MasterDnsVPN – Multipath Overlay Layer
[MasterDnsVPN-Multipath] INFO  Listen  : 127.0.0.1:19000
[MasterDnsVPN-Multipath] INFO  Aggregator: <AGGREGATOR_VPS_IP>:9000
[MasterDnsVPN-Multipath] INFO  Tunnels :
[MasterDnsVPN-Multipath] INFO    tunnel-1     → 127.0.0.1:18001 (weight 1)
...
[MultipathDispatcher] SOCKS5 listening on 127.0.0.1:19000 → Aggregator <IP>:9000 (5 tunnels)
```

---

## 9. Step 5 – Configure your browser or app

The Aggregator Client is now a **SOCKS5 proxy** running on `127.0.0.1:19000`.
Point your browser or app to it.

### Firefox

1. Open Firefox → Hamburger menu (☰) → Settings
2. Search for "proxy" → click **Settings…**
3. Select **Manual proxy configuration**
4. SOCKS Host: `127.0.0.1` — Port: `19000`
5. Select **SOCKS v5**
6. Check **Proxy DNS when using SOCKS v5** (important for privacy)
7. Click **OK**

### Chrome / Chromium / Edge

Chrome does not have its own proxy settings on all OSes.  The easiest option is to use
the **Proxy SwitchyOmega** extension:

1. Install "Proxy SwitchyOmega" from the Chrome Web Store
2. Open extension options → New Profile → name it "MasterDNS"
3. Protocol: SOCKS5  Server: 127.0.0.1  Port: 19000
4. Click the extension icon → select "MasterDNS"

Alternatively, launch Chrome with a flag:

**Windows:**
```powershell
Start-Process "chrome.exe" -ArgumentList "--proxy-server=socks5://127.0.0.1:19000"
```

**Linux:**
```bash
google-chrome --proxy-server="socks5://127.0.0.1:19000"
```

### curl (command line)

```bash
curl --socks5-hostname 127.0.0.1:19000 https://example.com
```

### System-wide proxy (Windows)

1. Settings → Network & Internet → Proxy
2. Manual proxy setup → On
3. Address: `127.0.0.1`  Port: `19000`
4. Save

---

## 10. Verifying it works

### Check your public IP

Open a browser (with proxy set) and visit:

```
https://ifconfig.me
```

or

```
https://whatismyipaddress.com
```

The IP address shown should be the **Aggregator VPS's IP**, not your home IP.
If it shows your home IP, the proxy is not configured correctly (see Troubleshooting).

### Test with curl

```bash
curl --socks5-hostname 127.0.0.1:19000 https://ifconfig.me
```

Should print your Aggregator VPS IP.

### Check the Aggregator Server logs

In the Aggregator VPS terminal, you should see lines like:

```
[Router] SYN stream 1 → example.com:443
[AggStream 1] upstream connected to example.com:443
[AggStream 1] FIN sent (response bytes: 5432)
```

This confirms data is flowing end-to-end.

---

## 11. Troubleshooting

### "Connection refused" on port 19000

The Aggregator Client (`masterdns-agg`) is not running.  Check Terminal 6.

### Browser shows "The proxy server is refusing connections"

Same as above — confirm the Aggregator Client is running and listening on port 19000.

### Pages load but very slowly

- Check that all 5 DNS tunnel clients are running (Terminals 1–5).
- Check the Aggregator Client logs — if you see `bearer X send failed`, it means some tunnels
  cannot reach the Aggregator Server.
- Verify port 9000 is open on the Aggregator VPS.

### "SYN dispatch failed"

The Aggregator Client cannot reach the Aggregator Server at all.  Check:
1. Aggregator Server is running: `screen -r masterdns-agg` on the Aggregator VPS.
2. Port 9000 is open: `ufw status` on the Aggregator VPS should show `9000/tcp ALLOW`.
3. The IP in `-agg` flag is the correct public IP of the Aggregator VPS.

### "SOCKS5 handshake failed"

A client connected to port 19000 without speaking SOCKS5 first.
Make sure your browser/app is configured as **SOCKS5**, not HTTP proxy.

### DNS tunnel clients exit immediately

Check the client config file:
- `DOMAINS` must match the domain the server is using.
- `ENCRYPTION_KEY` must be identical on client and server.
- `DATA_ENCRYPTION_METHOD` must be identical on client and server.

Enable debug logging on the server:

```bash
MASTERDNS_DEBUG=1 ./masterdnsvpn-server
```

### Page loads but shows "SOCKS connection failed" for HTTPS

Make sure **"Proxy DNS when using SOCKS v5"** is checked in your browser settings.
Without it, the browser leaks DNS queries outside the tunnel and HTTPS may fail.

### Port 53 already in use on the server

On modern Linux distributions, `systemd-resolved` occupies port 53.  Disable it:

```bash
systemctl stop systemd-resolved
systemctl disable systemd-resolved
# Then remove the resolv.conf symlink and point to a real DNS:
rm /etc/resolv.conf
echo "nameserver 1.1.1.1" > /etc/resolv.conf
```

---

## 12. All command-line flags reference

### Aggregator Client (`masterdns-agg`)

| Flag | Default | Description |
|------|---------|-------------|
| `-listen` | `127.0.0.1:19000` | Local SOCKS5 address for browsers/apps to connect to |
| `-agg` | `127.0.0.1:9000` | Remote Aggregator Server address (host:port) |
| `-chunk` | `1024` | Max bytes per macro frame.  Lower values work better on slow links. |
| `-dial-timeout` | `10s` | How long to wait for a bearer tunnel to connect |
| `-reconnect` | `3s` | How long to pause before retrying a failed bearer |
| `-t1` | `127.0.0.1:18001` | SOCKS5 address of DNS tunnel client 1 |
| `-t2` | `127.0.0.1:18002` | SOCKS5 address of DNS tunnel client 2 |
| `-t3` | `127.0.0.1:18003` | SOCKS5 address of DNS tunnel client 3 |
| `-t4` | `127.0.0.1:18004` | SOCKS5 address of DNS tunnel client 4 |
| `-t5` | `127.0.0.1:18005` | SOCKS5 address of DNS tunnel client 5 |

**Example – custom ports:**
```bash
./masterdns-agg -listen 127.0.0.1:10800 -agg 203.0.113.55:9000
```

### Aggregator Server (`masterdns-agg-server`)

| Flag | Default | Description |
|------|---------|-------------|
| `-listen` | `:9000` | TCP address to accept bearer connections on |
| `-chunk` | `4096` | Max bytes per response macro frame |
| `-dial-timeout` | `30s` | How long to wait when dialling an upstream website |
| `-streams` | `0` | Max concurrent streams (0 = unlimited) |
| `-inbound-depth` | `4096` | Internal frame buffer depth |
| `-reassembly-buf` | `512` | Max buffered out-of-order frames per stream before reset |

**Example – production server on a dedicated port with stream limit:**
```bash
./masterdns-agg-server -listen :9000 -streams 1000 -chunk 8192
```

### DNS Tunnel Client (`masterdnsvpn-client`)

| Flag / Config Key | Description |
|-------------------|-------------|
| `DOMAINS` | Domain(s) for this tunnel (must match the server) |
| `LISTEN_PORT` | Local SOCKS5 port (18001–18005) |
| `DATA_ENCRYPTION_METHOD` | 0=None 1=XOR 2=ChaCha20 3=AES-128-GCM 4=AES-192-GCM 5=AES-256-GCM |
| `ENCRYPTION_KEY` | Shared secret (must match server) |

---

## 13. Security checklist

Before using in production, go through this list:

- [ ] **Encryption is on** — `DATA_ENCRYPTION_METHOD` is 2 (ChaCha20) or higher in all configs.
- [ ] **The same encryption key is on all clients and all servers.**
- [ ] **The encryption key is long and random** — use at least 32 hex characters:
      ```bash
      openssl rand -hex 32
      ```
- [ ] **Port 9000 is only open to the 5 DNS VPN server IPs** (not the whole internet).
      On the Aggregator VPS:
      ```bash
      ufw allow from <SERVER1_IP> to any port 9000/tcp
      ufw allow from <SERVER2_IP> to any port 9000/tcp
      # ...etc for all 5 server IPs
      ufw deny 9000/tcp
      ```
- [ ] **The Aggregator Client's port 19000 is bound to 127.0.0.1** (local only, not 0.0.0.0).
      This prevents other machines on your network from using your proxy.
- [ ] **DNS over SOCKS5 is enabled in your browser** (see Section 9) to prevent DNS leaks.
- [ ] **Regularly rotate the `ENCRYPTION_KEY`** by updating it on all servers and clients.

---

## 14. Stopping everything gracefully

On each terminal window, press **Ctrl+C** once.  Each process will log its shutdown and exit.

**Order to shut down:**

1. Press Ctrl+C in Terminal 6 (Aggregator Client) — stops accepting new connections
2. Press Ctrl+C in Terminals 1–5 (DNS clients) — closes the DNS tunnels
3. On the Aggregator VPS: press Ctrl+C in the Aggregator Server terminal (or `screen -r masterdns-agg` first)
4. On each DNS VPN server VPS: press Ctrl+C in the server terminal

All processes save no state to disk and can be restarted at any time by re-running the same commands.

---

## Quick-start summary (for experienced users)

```
# EACH DNS VPN SERVER VPS (×5):
go build -o masterdnsvpn-server ./cmd/server/
sudo ./masterdnsvpn-server   # uses server_config.toml

# AGGREGATOR VPS:
go build -o masterdns-agg-server ./masterdns_aggregator/cmd/masterdns_agg_server/
./masterdns-agg-server -listen :9000

# LOCAL MACHINE (6 terminals):
go build -o masterdnsvpn-client ./cmd/client/
go build -o masterdns-agg ./masterdns_aggregator/cmd/masterdns_agg/

./masterdnsvpn-client -config client1.toml   # terminal 1, port 18001
./masterdnsvpn-client -config client2.toml   # terminal 2, port 18002
./masterdnsvpn-client -config client3.toml   # terminal 3, port 18003
./masterdnsvpn-client -config client4.toml   # terminal 4, port 18004
./masterdnsvpn-client -config client5.toml   # terminal 5, port 18005

./masterdns-agg -listen 127.0.0.1:19000 -agg <AGG_VPS_IP>:9000  # terminal 6

# BROWSER: SOCKS5 proxy → 127.0.0.1:19000
```
