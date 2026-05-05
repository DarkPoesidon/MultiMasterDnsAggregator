#!/usr/bin/env bash
# ==============================================================================
#  MasterDNS Multipath Aggregator – One-Click Server Installer
#  Repository : https://github.com/DarkPoesidon/MultiMasterDnsAggregator
#  Supports   : Ubuntu 20.04+, Debian 11+  (graceful warnings on others)
#  Idempotent : Yes – safe to re-run on an already-installed machine
# ==============================================================================
set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# 0. ANSI colour helpers
# ──────────────────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

info()    { echo -e "${CYAN}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; }
banner()  {
  echo -e ""
  echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}${CYAN}║  MasterDNS Multipath Aggregator – Server Installer       ║${RESET}"
  echo -e "${BOLD}${CYAN}║  https://github.com/DarkPoesidon/MultiMasterDnsAggregator ║${RESET}"
  echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════╝${RESET}"
  echo -e ""
}

# ──────────────────────────────────────────────────────────────────────────────
# 1. Root check
# ──────────────────────────────────────────────────────────────────────────────
check_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    error "This script must be run as root."
    echo -e ""
    echo -e "  Please re-run with sudo:"
    echo -e "  ${BOLD}sudo bash install.sh${RESET}"
    echo -e ""
    exit 1
  fi
}

# ──────────────────────────────────────────────────────────────────────────────
# 2. OS detection
# ──────────────────────────────────────────────────────────────────────────────
detect_os() {
  OS_ID=""
  PKG_MGR=""

  if [[ -f /etc/os-release ]]; then
    # shellcheck disable=SC1091
    source /etc/os-release
    OS_ID="${ID:-unknown}"
  fi

  case "${OS_ID}" in
    ubuntu|debian|linuxmint|pop)
      PKG_MGR="apt-get"
      ;;
    centos|rhel|fedora|rocky|almalinux)
      PKG_MGR="yum"
      warn "Detected ${OS_ID}. Primary support is Ubuntu/Debian."
      warn "The script will attempt yum-based installation – review if errors appear."
      ;;
    arch|manjaro)
      PKG_MGR="pacman"
      warn "Detected ${OS_ID}. Limited support – proceeding with pacman."
      ;;
    *)
      warn "Unknown OS '${OS_ID}'. Proceeding with apt-get; adjust manually if needed."
      PKG_MGR="apt-get"
      ;;
  esac

  # CPU architecture
  ARCH="$(uname -m)"
  case "${ARCH}" in
    x86_64)  GO_ARCH="amd64"  ;;
    aarch64) GO_ARCH="arm64"  ;;
    armv7*)  GO_ARCH="armv6l" ;;
    *)
      error "Unsupported CPU architecture: ${ARCH}"
      exit 1
      ;;
  esac

  info "OS: ${OS_ID:-unknown}  |  Architecture: ${ARCH} (Go arch: ${GO_ARCH})"
}

# ──────────────────────────────────────────────────────────────────────────────
# 3. Package dependency installation
# ──────────────────────────────────────────────────────────────────────────────
install_system_deps() {
  info "Checking system dependencies..."

  REQUIRED_PKGS=(curl wget git tar ca-certificates ufw)
  MISSING_PKGS=()

  for pkg in "${REQUIRED_PKGS[@]}"; do
    if ! command -v "${pkg}" &>/dev/null && ! dpkg -l "${pkg}" &>/dev/null 2>&1; then
      MISSING_PKGS+=("${pkg}")
    fi
  done

  if [[ ${#MISSING_PKGS[@]} -eq 0 ]]; then
    success "All system dependencies already present."
    return
  fi

  info "Installing missing packages: ${MISSING_PKGS[*]}"

  case "${PKG_MGR}" in
    apt-get)
      apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${MISSING_PKGS[@]}"
      ;;
    yum)
      yum install -y -q "${MISSING_PKGS[@]}" || true
      ;;
    pacman)
      pacman -Sy --noconfirm "${MISSING_PKGS[@]}" || true
      ;;
  esac

  success "System dependencies installed."
}

# ──────────────────────────────────────────────────────────────────────────────
# 4. Go installation / upgrade
# ──────────────────────────────────────────────────────────────────────────────
GO_INSTALL_DIR="/usr/local"
GO_MIN_MAJOR=1
GO_MIN_MINOR=22

version_gte() {
  # Returns 0 (true) if $1 >= $2 in "X.Y" form
  local IFS='.'
  read -ra A <<< "$1"
  read -ra B <<< "$2"
  (( ${A[0]:-0} > ${B[0]:-0} )) && return 0
  (( ${A[0]:-0} == ${B[0]:-0} && ${A[1]:-0} >= ${B[1]:-0} )) && return 0
  return 1
}

fetch_latest_go_version() {
  # Queries the Go downloads API for the latest stable version string (e.g. "1.22.4")
  local latest
  latest=$(curl -fsSL "https://go.dev/dl/?mode=json&include=all" \
    | grep -o '"version":"go[^"]*"' \
    | grep -v 'rc\|beta' \
    | head -1 \
    | sed 's/"version":"go\([^"]*\)"/\1/')
  echo "${latest}"
}

install_go() {
  local need_install=false
  local current_version=""

  if command -v go &>/dev/null; then
    current_version="$(go version | awk '{print $3}' | sed 's/go//')"
    local cur_minor
    cur_minor="$(echo "${current_version}" | cut -d. -f1-2)"
    if version_gte "${cur_minor}" "${GO_MIN_MAJOR}.${GO_MIN_MINOR}"; then
      success "Go ${current_version} is already installed and meets the minimum requirement (>= ${GO_MIN_MAJOR}.${GO_MIN_MINOR})."
      return
    else
      warn "Installed Go ${current_version} is below the required minimum ${GO_MIN_MAJOR}.${GO_MIN_MINOR}. Upgrading..."
      need_install=true
    fi
  else
    info "Go is not installed. Fetching latest stable version..."
    need_install=true
  fi

  if [[ "${need_install}" == true ]]; then
    local go_ver
    go_ver="$(fetch_latest_go_version)"

    if [[ -z "${go_ver}" ]]; then
      warn "Could not auto-detect latest Go version. Falling back to 1.22.4."
      go_ver="1.22.4"
    fi

    local tarball="go${go_ver}.linux-${GO_ARCH}.tar.gz"
    local download_url="https://dl.google.com/go/${tarball}"
    local tmp_file="/tmp/${tarball}"

    info "Downloading Go ${go_ver} for linux/${GO_ARCH}..."
    curl -fsSL -o "${tmp_file}" "${download_url}"

    info "Installing Go ${go_ver} to ${GO_INSTALL_DIR}..."
    rm -rf "${GO_INSTALL_DIR}/go"
    tar -C "${GO_INSTALL_DIR}" -xzf "${tmp_file}"
    rm -f "${tmp_file}"

    # ── Permanent PATH configuration ────────────────────────────────────────
    local profile_snippet='/etc/profile.d/golang.sh'
    cat > "${profile_snippet}" <<'EOF'
export GOROOT=/usr/local/go
export GOPATH=/root/go
export PATH=$PATH:$GOROOT/bin:$GOPATH/bin
EOF
    chmod +x "${profile_snippet}"

    # Apply for the current session immediately
    export GOROOT="/usr/local/go"
    export GOPATH="/root/go"
    export PATH="${PATH}:${GOROOT}/bin:${GOPATH}/bin"

    success "Go ${go_ver} installed successfully."
  fi
}

# ──────────────────────────────────────────────────────────────────────────────
# 5. Repository clone / update
# ──────────────────────────────────────────────────────────────────────────────
REPO_URL="https://github.com/DarkPoesidon/MultiMasterDnsAggregator"
INSTALL_DIR="/opt/masterdns-aggregator"

clone_or_pull_repo() {
  if [[ -d "${INSTALL_DIR}/.git" ]]; then
    info "Repository already exists at ${INSTALL_DIR}. Pulling latest changes..."
    git -C "${INSTALL_DIR}" fetch --all -q
    git -C "${INSTALL_DIR}" reset --hard origin/main -q 2>/dev/null \
      || git -C "${INSTALL_DIR}" reset --hard origin/master -q
    success "Repository updated."
  else
    info "Cloning repository into ${INSTALL_DIR}..."
    mkdir -p "$(dirname "${INSTALL_DIR}")"
    git clone --depth=1 "${REPO_URL}" "${INSTALL_DIR}"
    success "Repository cloned."
  fi
}

# ──────────────────────────────────────────────────────────────────────────────
# 6. User prompt – server port
# ──────────────────────────────────────────────────────────────────────────────
DEFAULT_PORT=9000
SERVER_PORT=${DEFAULT_PORT}

prompt_config() {
  echo ""
  echo -e "${BOLD}── Configuration ──────────────────────────────────────────${RESET}"
  read -r -p "$(echo -e "${YELLOW}Enter the port for the Aggregator server [Default: ${DEFAULT_PORT}]:${RESET} ")" user_port
  if [[ -n "${user_port}" ]]; then
    # Validate: must be a number between 1 and 65535
    if [[ "${user_port}" =~ ^[0-9]+$ ]] && (( user_port >= 1 && user_port <= 65535 )); then
      SERVER_PORT="${user_port}"
    else
      warn "Invalid port '${user_port}'. Using default: ${DEFAULT_PORT}"
      SERVER_PORT=${DEFAULT_PORT}
    fi
  fi
  success "Aggregator will listen on port ${SERVER_PORT}."
}

# ──────────────────────────────────────────────────────────────────────────────
# 7. Build the server binary
# ──────────────────────────────────────────────────────────────────────────────
BINARY_PATH="/usr/local/bin/masterdns-agg-server"

build_server() {
  info "Building masterdns-agg-server binary..."
  cd "${INSTALL_DIR}"

  # Tidy dependencies first (handles go.sum generation and cleanup)
  go mod download -x 2>&1 | tail -5 || true
  go mod tidy

  if ! go build \
      -ldflags="-s -w -X main.Version=$(git describe --tags --always 2>/dev/null || echo 'dev')" \
      -o "${BINARY_PATH}" \
      ./cmd/masterdns-agg-server/; then
    error "Build failed! Check the output above for details."
    error "The systemd service will NOT be created."
    exit 1
  fi

  chmod +x "${BINARY_PATH}"
  success "Binary built: ${BINARY_PATH}"
}

# ──────────────────────────────────────────────────────────────────────────────
# 8. Firewall configuration
# ──────────────────────────────────────────────────────────────────────────────
configure_firewall() {
  info "Configuring firewall for port ${SERVER_PORT}/tcp..."

  if command -v ufw &>/dev/null; then
    local ufw_status
    ufw_status="$(ufw status 2>/dev/null | head -1)"

    if echo "${ufw_status}" | grep -qi "active"; then
      ufw allow "${SERVER_PORT}/tcp" comment "masterdns-aggregator" > /dev/null
      ufw reload > /dev/null
      success "UFW: port ${SERVER_PORT}/tcp opened."
    else
      warn "UFW is installed but inactive. Enabling UFW and opening port ${SERVER_PORT}/tcp..."
      # Allow SSH (22) first to avoid locking ourselves out
      ufw allow 22/tcp > /dev/null 2>&1 || true
      ufw allow "${SERVER_PORT}/tcp" comment "masterdns-aggregator" > /dev/null
      ufw --force enable > /dev/null
      success "UFW enabled. Port ${SERVER_PORT}/tcp opened."
    fi

  elif command -v iptables &>/dev/null; then
    # Check if rule already exists to stay idempotent
    if ! iptables -C INPUT -p tcp --dport "${SERVER_PORT}" -j ACCEPT 2>/dev/null; then
      iptables -A INPUT -p tcp --dport "${SERVER_PORT}" -j ACCEPT
      success "iptables: port ${SERVER_PORT}/tcp rule added."
    else
      success "iptables: port ${SERVER_PORT}/tcp rule already exists."
    fi

    # Persist iptables rules if tools are available
    if command -v netfilter-persistent &>/dev/null; then
      netfilter-persistent save > /dev/null 2>&1 || true
    elif command -v iptables-save &>/dev/null; then
      iptables-save > /etc/iptables/rules.v4 2>/dev/null || true
    fi

  else
    warn "No firewall (ufw/iptables) found. Please open port ${SERVER_PORT}/tcp manually."
  fi
}

# ──────────────────────────────────────────────────────────────────────────────
# 9. Systemd service
# ──────────────────────────────────────────────────────────────────────────────
SERVICE_NAME="masterdns-aggregator"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

create_systemd_service() {
  info "Creating systemd service: ${SERVICE_NAME}..."

  # Resolve the public IP for display later (non-fatal)
  SERVER_IP="$(curl -fsSL --max-time 5 https://api.ipify.org 2>/dev/null \
    || curl -fsSL --max-time 5 https://ifconfig.me 2>/dev/null \
    || hostname -I 2>/dev/null | awk '{print $1}' \
    || echo '<your-server-ip>')"

  cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=MasterDNS Multipath Aggregator Server
Documentation=https://github.com/DarkPoesidon/MultiMasterDnsAggregator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BINARY_PATH} -listen :${SERVER_PORT}
Restart=always
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

# Security hardening
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=read-only
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable --now "${SERVICE_NAME}"

  success "Systemd service '${SERVICE_NAME}' created, enabled, and started."
}

# ──────────────────────────────────────────────────────────────────────────────
# 10. Final status check & success banner
# ──────────────────────────────────────────────────────────────────────────────
print_success_banner() {
  local svc_status
  svc_status="$(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo 'unknown')"

  echo ""
  echo -e "${BOLD}${GREEN}╔══════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}${GREEN}║           INSTALLATION COMPLETE – ALL SYSTEMS GO          ║${RESET}"
  echo -e "${BOLD}${GREEN}╚══════════════════════════════════════════════════════════╝${RESET}"
  echo ""
  echo -e "  ${BOLD}Service status :${RESET} ${GREEN}${svc_status}${RESET}"
  echo -e "  ${BOLD}Server IP      :${RESET} ${CYAN}${SERVER_IP}${RESET}"
  echo -e "  ${BOLD}Listening port :${RESET} ${CYAN}${SERVER_PORT}${RESET}"
  echo -e "  ${BOLD}Binary path    :${RESET} ${BINARY_PATH}"
  echo -e "  ${BOLD}Config dir     :${RESET} ${INSTALL_DIR}"
  echo ""
  echo -e "  ${BOLD}Useful commands:${RESET}"
  echo -e "  ${YELLOW}Check live logs  :${RESET}  journalctl -u ${SERVICE_NAME} -f"
  echo -e "  ${YELLOW}Service status   :${RESET}  systemctl status ${SERVICE_NAME}"
  echo -e "  ${YELLOW}Restart service  :${RESET}  systemctl restart ${SERVICE_NAME}"
  echo -e "  ${YELLOW}Stop service     :${RESET}  systemctl stop ${SERVICE_NAME}"
  echo ""
  echo -e "  ${BOLD}Point your clients at:${RESET}"
  echo -e "  ${CYAN}${SERVER_IP}:${SERVER_PORT}${RESET}"
  echo ""
  echo -e "${BOLD}${GREEN}══════════════════════════════════════════════════════════${RESET}"
  echo ""
}

# ──────────────────────────────────────────────────────────────────────────────
# MAIN – Execute all stages in order
# ──────────────────────────────────────────────────────────────────────────────
main() {
  banner
  check_root
  detect_os
  install_system_deps
  install_go
  prompt_config
  clone_or_pull_repo
  build_server
  configure_firewall
  create_systemd_service
  print_success_banner
}

main "$@"
