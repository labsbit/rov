#!/bin/sh
# install.sh — install rov (Run, Observe, Verify).
#
#   curl -fsSL https://raw.githubusercontent.com/labsbit/rov/main/install.sh | sh
#   ./install.sh                  # inside a checkout: build from source
#
# Environment:
#   ROV_INSTALL_DIR  where the binary goes (default ~/.local/bin)
#   ROV_VERSION      release tag to download, e.g. v0.1.0 (default: latest)
#   ROV_FROM_SOURCE  1 = always build with `go install` (needs Go)
set -eu

REPO=labsbit/rov
MODULE=github.com/labsbit/rov
BIN_DIR=${ROV_INSTALL_DIR:-"$HOME/.local/bin"}

say() { printf 'rov: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

check_tmux() {
    if ! have tmux; then
        say "warning: tmux is not installed; rov needs tmux >= 3.2 (apt install tmux / brew install tmux)"
        return
    fi
    v=$(tmux -V | sed -E 's/^tmux (next-)?([0-9]+)\.([0-9]+).*/\2 \3/')
    set -- $v
    if [ "${1:-0}" -lt 3 ] || { [ "${1:-0}" -eq 3 ] && [ "${2:-0}" -lt 2 ]; }; then
        say "warning: $(tmux -V) found; rov needs tmux >= 3.2"
    fi
}

path_hint() {
    case ":$PATH:" in
        *":$1:"*) ;;
        *) say "note: $1 is not on PATH; add it, e.g.: export PATH=\"$1:\$PATH\"" ;;
    esac
}

# Inside a checkout (or when asked): build from source with the local Go.
src_dir=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)
if [ "${ROV_FROM_SOURCE:-}" = 1 ] || { [ -n "$src_dir" ] && [ -f "$src_dir/go.mod" ] && grep -q "^module $MODULE\$" "$src_dir/go.mod"; }; then
    have go || die "building from source needs Go (https://go.dev/dl/)"
    if [ -n "$src_dir" ] && [ -f "$src_dir/go.mod" ]; then
        say "building from $src_dir"
        (cd "$src_dir" && GOBIN="$BIN_DIR" go install ./cmd/rov)
    else
        say "building $MODULE@${ROV_VERSION:-latest}"
        GOBIN="$BIN_DIR" go install "$MODULE/cmd/rov@${ROV_VERSION:-latest}"
    fi
    say "installed $("$BIN_DIR/rov" --version)"
    check_tmux
    path_hint "$BIN_DIR"
    exit 0
fi

# Otherwise: download a release binary.
case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported OS $(uname -s); build from source: go install $MODULE/cmd/rov@latest" ;;
esac
case $(uname -m) in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m); build from source: go install $MODULE/cmd/rov@latest" ;;
esac

if have curl; then
    fetch() { curl -fsSL "$1"; }
elif have wget; then
    fetch() { wget -qO- "$1"; }
else
    die "need curl or wget"
fi

version=${ROV_VERSION:-}
if [ -z "$version" ]; then
    version=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
        sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) || true
    [ -n "$version" ] || die "could not find the latest release of $REPO (private repo or no release yet?); use: go install $MODULE/cmd/rov@latest"
fi

asset="rov_${version#v}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $asset ($version)"
fetch "$base/$asset" >"$tmp/$asset" || die "download failed: $base/$asset"
if fetch "$base/checksums.txt" >"$tmp/checksums.txt" 2>/dev/null; then
    want=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
    if have sha256sum; then got=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
    else got=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1); fi
    [ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for $asset"
fi

tar -xzf "$tmp/$asset" -C "$tmp" rov
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/rov" "$BIN_DIR/rov" 2>/dev/null || { cp "$tmp/rov" "$BIN_DIR/rov" && chmod 0755 "$BIN_DIR/rov"; }
say "installed $("$BIN_DIR/rov" --version) → $BIN_DIR/rov"
check_tmux
path_hint "$BIN_DIR"
