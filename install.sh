#!/bin/sh
# install.sh — download the latest prebuilt juice binary into the current directory.
#
#   ./install.sh              install (or update) ./juice from the latest release
#   ./install.sh --uninstall  remove ./juice; preserve juice.db and juice.json
#   ./install.sh --help       show this help
#
# Everything is local: the binary, its database (juice.db), and its config
# (juice.json) all live in the directory you run it from. No sudo, no PATH
# changes, no system files. Ollama and TinyGo are NOT installed — if you want
# the LLM and compile actions, install them yourself first (see preflight).

set -eu

REPO="daios-ai/juice"
BINARY="juice"
# Files that hold state — never removed by --uninstall.
DATA_FILES="juice.db juice.db-wal juice.db-shm juice.json"

say()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
}

# --- uninstall -------------------------------------------------------------

uninstall() {
    if [ -f "./$BINARY" ]; then
        rm -f "./$BINARY"
        say "Removed ./$BINARY"
    else
        say "No ./$BINARY found in $(pwd) — nothing to remove."
    fi
    kept=""
    for f in $DATA_FILES; do
        [ -e "./$f" ] && kept="$kept ./$f"
    done
    if [ -n "$kept" ]; then
        say ""
        say "Your data was left untouched:$kept"
        say "Delete it by hand if you really want a clean slate."
    fi
    exit 0
}

# --- platform detection ----------------------------------------------------

detect_platform() {
    os=$(uname -s)
    arch=$(uname -m)
    case "$os" in
        Linux)  os=linux ;;
        Darwin) os=darwin ;;
        *) die "unsupported OS: $os (only Linux and macOS are supported)" ;;
    esac
    case "$arch" in
        x86_64|amd64)  arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die "unsupported architecture: $arch" ;;
    esac
    PLATFORM="${os}_${arch}"
}

# --- preflight: report optional deps, never install them -------------------

preflight() {
    command -v curl >/dev/null 2>&1 || die "curl is required but not found."
    command -v tar  >/dev/null 2>&1 || die "tar is required but not found."

    command -v ollama >/dev/null 2>&1 || warn \
        "ollama not found — @sys/lookup and @sys/llm/* will be unavailable. Install: https://ollama.com/download"
    command -v tinygo >/dev/null 2>&1 || warn \
        "tinygo not found — @sys/tinygo/compile and @sys/make will be unavailable. Install: https://tinygo.org/getting-started/install/"
}

# pick an available SHA-256 tool; sets SHACMD or leaves it empty
detect_sha() {
    if command -v sha256sum >/dev/null 2>&1; then
        SHACMD="sha256sum"
    elif command -v shasum >/dev/null 2>&1; then
        SHACMD="shasum -a 256"
    else
        SHACMD=""
    fi
}

# --- install ---------------------------------------------------------------

install() {
    detect_platform
    preflight
    detect_sha

    asset="${BINARY}_${PLATFORM}.tar.gz"
    base="https://github.com/${REPO}/releases/latest/download"

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT

    say "Downloading latest ${asset}..."
    curl -fSL --proto '=https' -o "$tmp/$asset" "$base/$asset" \
        || die "download failed — has a release been published for $PLATFORM yet?"

    if [ -n "$SHACMD" ]; then
        if curl -fSL --proto '=https' -o "$tmp/checksums.txt" "$base/checksums.txt" 2>/dev/null; then
            expected=$(grep " $asset\$" "$tmp/checksums.txt" | awk '{print $1}')
            if [ -n "$expected" ]; then
                actual=$( (cd "$tmp" && $SHACMD "$asset") | awk '{print $1}')
                [ "$expected" = "$actual" ] || die "checksum mismatch for $asset"
                say "Checksum verified."
            else
                warn "no checksum entry for $asset; skipping verification."
            fi
        else
            warn "checksums.txt unavailable; skipping verification."
        fi
    else
        warn "no sha256 tool found; skipping checksum verification."
    fi

    tar -xzf "$tmp/$asset" -C "$tmp" || die "failed to extract $asset"
    [ -f "$tmp/$BINARY" ] || die "archive did not contain a '$BINARY' binary"

    mv "$tmp/$BINARY" "./$BINARY"
    chmod +x "./$BINARY"

    say ""
    say "Installed ./$BINARY ($("./$BINARY" --version 2>/dev/null || echo "version unknown"))"
    say "Run it with:  ./$BINARY serve"
}

# --- main ------------------------------------------------------------------

case "${1:-}" in
    --uninstall|uninstall) uninstall ;;
    --help|-h|help)        usage ;;
    "")                    install ;;
    *)                     die "unknown argument: $1 (try --help)" ;;
esac
