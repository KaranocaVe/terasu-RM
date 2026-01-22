#!/usr/bin/env bash
set -euo pipefail

REPO="KaranocaVe/terasu-RM"
BIN_DIR="/usr/local/bin"
VERSION=""
COMPONENT="both"
SKIP_SUDO="false"
BIN_DIR_SET="false"
ENABLE_DOCKER="true"
DOCKER_CONFIG=""

usage() {
  cat <<'EOF'
Usage: install.sh [options]

Options:
  -b, --bin-dir <dir>     Install directory (default: /usr/local/bin)
  -v, --version <version> Release version (default: latest)
  --component <name>      rmirror | rmirrord | both (default: both)
  --no-docker             Skip auto-starting Docker mirror
  --docker-config <path>  Docker config path (default: ~/.config/rmirror/docker.json)
  --no-sudo               Do not use sudo
  -h, --help              Show help
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    -b|--bin-dir)
      BIN_DIR="${2:-}"
      BIN_DIR_SET="true"
      shift 2
      ;;
    -v|--version)
      VERSION="${2:-}"
      shift 2
      ;;
    --component)
      COMPONENT="${2:-}"
      shift 2
      ;;
    --no-docker)
      ENABLE_DOCKER="false"
      shift
      ;;
    --docker-config)
      DOCKER_CONFIG="${2:-}"
      shift 2
      ;;
    --no-sudo)
      SKIP_SUDO="true"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi

uname_s="$(uname -s)"
case "$uname_s" in
  Linux)
    OS="linux"
    ;;
  Darwin)
    OS="darwin"
    ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    OS="windows"
    ;;
  *)
    echo "unsupported OS: $uname_s" >&2
    exit 1
    ;;
esac

uname_m="$(uname -m)"
case "$uname_m" in
  x86_64|amd64)
    ARCH="amd64"
    ;;
  arm64|aarch64)
    ARCH="arm64"
    ;;
  *)
    echo "unsupported architecture: $uname_m" >&2
    exit 1
    ;;
esac

if [ "$OS" = "windows" ] && [ "$BIN_DIR_SET" = "false" ]; then
  BIN_DIR="${HOME:-.}/bin"
fi

case "$COMPONENT" in
  rmirror|rmirrord|both)
    ;;
  *)
    echo "unsupported component: $COMPONENT" >&2
    exit 1
    ;;
esac

if [ -z "$VERSION" ]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | awk -F'"' '/"tag_name":/ {print $4; exit}')"
fi
if [ -z "$VERSION" ]; then
  echo "failed to determine latest version" >&2
  exit 1
fi
if [[ "$VERSION" != v* ]]; then
  VERSION="v${VERSION}"
fi

EXT=""
if [ "$OS" = "windows" ]; then
  EXT=".exe"
fi

BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

ASSETS=()
if [ "$COMPONENT" = "rmirror" ] || [ "$COMPONENT" = "both" ]; then
  ASSETS+=("rmirror-${VERSION}-${OS}-${ARCH}${EXT}")
fi
if [ "$COMPONENT" = "rmirrord" ] || [ "$COMPONENT" = "both" ]; then
  ASSETS+=("rmirrord-${VERSION}-${OS}-${ARCH}${EXT}")
fi

for asset in "${ASSETS[@]}"; do
  url="${BASE_URL}/${asset}"
  dest="${TMP_DIR}/${asset}"
  echo "downloading ${url}"
  curl -fL --retry 3 --retry-delay 1 -o "$dest" "$url"
done

ensure_bin_dir() {
  if mkdir -p "$BIN_DIR" 2>/dev/null; then
    return 0
  fi
  if [ "$SKIP_SUDO" = "true" ] || [ "$OS" = "windows" ]; then
    echo "cannot create ${BIN_DIR}" >&2
    exit 1
  fi
  sudo mkdir -p "$BIN_DIR"
}

ensure_bin_dir

NEED_SUDO="false"
if [ ! -w "$BIN_DIR" ]; then
  if [ "$SKIP_SUDO" = "true" ] || [ "$OS" = "windows" ]; then
    echo "install dir not writable: ${BIN_DIR}" >&2
    exit 1
  fi
  NEED_SUDO="true"
fi

install_one() {
  src="$1"
  name="$2"
  dest="${BIN_DIR}/${name}${EXT}"
  if command -v install >/dev/null 2>&1; then
    if [ "$NEED_SUDO" = "true" ]; then
      sudo install -m 755 "$src" "$dest"
    else
      install -m 755 "$src" "$dest"
    fi
  else
    if [ "$NEED_SUDO" = "true" ]; then
      sudo cp "$src" "$dest"
      sudo chmod 755 "$dest"
    else
      cp "$src" "$dest"
      chmod 755 "$dest"
    fi
  fi
  echo "installed ${dest}"
}

for asset in "${ASSETS[@]}"; do
  src="${TMP_DIR}/${asset}"
  case "$asset" in
    rmirror-*)
      install_one "$src" "rmirror"
      ;;
    rmirrord-*)
      install_one "$src" "rmirrord"
      ;;
  esac
done

write_docker_config() {
  cfg="$1"
  cfg_dir="$(dirname "$cfg")"
  mkdir -p "$cfg_dir"
  if [ -s "$cfg" ]; then
    return 0
  fi
  raw_url="https://raw.githubusercontent.com/${REPO}/${VERSION}/examples/docker.json"
  if curl -fsSL -o "$cfg" "$raw_url"; then
    return 0
  fi
  cat > "$cfg" <<'EOF'
{
  "listen": "127.0.0.1:5000",
  "access_log": true,
  "transport": {
    "first_fragment_len": 3
  },
  "routes": [
    {
      "name": "docker-registry",
      "public_prefix": "/",
      "upstream": "https://registry-1.docker.io"
    },
    {
      "name": "docker-auth",
      "public_prefix": "/_auth",
      "upstream": "https://auth.docker.io"
    },
    {
      "name": "docker-blob",
      "public_prefix": "/_blob",
      "upstream": "https://production.cloudflare.docker.com"
    }
  ]
}
EOF
}

start_docker_mirror() {
  if [ "$COMPONENT" = "rmirrord" ]; then
    echo "rmirror not installed; skip docker mirror start"
    return 0
  fi

  cfg="${DOCKER_CONFIG}"
  if [ -z "$cfg" ]; then
    cfg_base="${XDG_CONFIG_HOME:-$HOME/.config}/rmirror"
    cfg="${cfg_base}/docker.json"
  fi
  cfg_dir="$(dirname "$cfg")"
  log_path="${cfg_dir}/rmirror-docker.log"
  pid_path="${cfg_dir}/rmirror-docker.pid"

  write_docker_config "$cfg"

  if [ -f "$pid_path" ]; then
    pid="$(cat "$pid_path" 2>/dev/null || true)"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      echo "rmirror already running (pid ${pid})"
      return 0
    fi
  fi

  rmirror_bin="${BIN_DIR}/rmirror${EXT}"
  if [ ! -x "$rmirror_bin" ]; then
    rmirror_bin="$(command -v rmirror || true)"
  fi
  if [ -z "$rmirror_bin" ]; then
    echo "rmirror binary not found; skip docker mirror start"
    return 0
  fi

  nohup "$rmirror_bin" -config "$cfg" >> "$log_path" 2>&1 &
  echo $! > "$pid_path"
  echo "rmirror started (pid $!)"
  echo "config: ${cfg}"
  echo "log: ${log_path}"
}

print_docker_instructions() {
  echo ""
  echo "Docker daemon config:"
  if [ "$OS" = "linux" ]; then
    echo "  file: /etc/docker/daemon.json"
    echo "  add:"
    echo '    {'
    echo '      "registry-mirrors": ["http://127.0.0.1:5000"],'
    echo '      "insecure-registries": ["127.0.0.1:5000"]'
    echo '    }'
    echo "  then restart Docker:"
    echo "    sudo systemctl restart docker"
  else
    echo "  Docker Desktop -> Settings -> Docker Engine"
    echo "  add:"
    echo '    {'
    echo '      "registry-mirrors": ["http://127.0.0.1:5000"],'
    echo '      "insecure-registries": ["127.0.0.1:5000"]'
    echo '    }'
  fi
  echo ""
}

if [ "$ENABLE_DOCKER" = "true" ]; then
  start_docker_mirror
  print_docker_instructions
fi
