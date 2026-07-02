#!/usr/bin/env sh
# install.sh — one-line installer for aiclibridge (Unix: macOS / Linux).
#
# Usage (curl|bash):
#   curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh
#   curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh -s -- --bin /usr/local/bin
#   curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh -s -- --version v0.4.1
#
# The script detects OS (darwin/linux) and ARCH (amd64/arm64), fetches the
# matching release tarball from GitHub, verifies the sha256 checksum, and
# installs the binary to --bin (default: /usr/local/bin, falling back to
# $HOME/.local/bin when /usr/local/bin is not writable). It never runs the
# binary post-install — the user invokes `aiclibridge version` themselves.
#
# Design notes:
#   - POSIX sh (not bash) so it works on minimal containers (alpine, distroless).
#   - No deps beyond curl/tar/sha256sum/shasum. macOS ships shasum; Linux
#     usually ships sha256sum. We try both.
#   - Fail-fast: every command is checked, errors propagate to exit.
#   - No telemetry, no sudo-by-default: if /usr/local/bin needs root we
#     retry with sudo only when interactive, otherwise fall back to ~/.local/bin.

set -eu

OWNER="tgcz2011"
REPO="aiclibridge"
GITHUB_RELEASES="https://github.com/${OWNER}/${REPO}/releases/latest"
GITHUB_API="https://api.github.com/repos/${OWNER}/${REPO}"
GITHUB_DOWNLOAD="https://github.com/${OWNER}/${REPO}/releases/download"
# User-Agent: GitHub API requires a UA; some proxies/firewalls also
# block requests without one. curl's default UA works but we set an
# explicit one for clarity in access logs.
INSTALLER_UA="aiclibridge-installer/1.0"

# CURL_FIX: --http1.1 avoids "Error in the HTTP2 framing layer" (curl
# bug with HTTP/2 + TLS over flaky/proxied connections, common in
# mainland China). --retry adds resilience to transient failures.
# --connect-timeout prevents hanging for 75s on unreachable hosts.
CURL_FIX="--http1.1 --retry 3 --retry-delay 2 --connect-timeout 30"

# GITHUB_MIRROR: optional prefix for release-download URLs. Set it if
# github.com is blocked in your region, e.g.:
#   GITHUB_MIRROR=https://ghproxy.com sh scripts/install.sh
# The mirror is only used for asset downloads (not for tag resolution,
# which goes through api.github.com as a fallback).
MIRROR="${GITHUB_MIRROR:-}"

# ── defaults ──
INSTALL_BIN_DIR="/usr/local/bin"
INSTALL_VERSION=""   # empty = latest from GitHub API
FORCE=0
VERBOSE=0

# ── arg parsing ──
while [ $# -gt 0 ]; do
    case "$1" in
        --bin)
            INSTALL_BIN_DIR="$2"; shift 2 ;;
        --version)
            INSTALL_VERSION="$2"; shift 2 ;;
        --mirror)
            MIRROR="$2"; shift 2 ;;
        --force)
            FORCE=1; shift ;;
        -v|--verbose)
            VERBOSE=1; shift ;;
        -h|--help)
            cat <<EOF
aiclibridge installer

Usage: install.sh [options]

Options:
  --bin <dir>       Install directory (default: /usr/local/bin, fallback: \$HOME/.local/bin)
  --version <ver>   Version to install (default: latest, e.g. v0.5.0)
  --mirror <url>    Mirror prefix for download URLs (or set \$GITHUB_MIRROR)
  --force           Overwrite an existing aiclibridge binary without prompting.
  -v, --verbose     Verbose output.
  -h, --help        Show this help and exit.

Examples:
  curl -fsSL https://github.com/${OWNER}/${REPO}/raw/main/scripts/install.sh | sh
  curl -fsSL https://github.com/${OWNER}/${REPO}/raw/main/scripts/install.sh | sh -s -- --bin \$HOME/.local/bin --version v0.5.0
  GITHUB_MIRROR=https://ghproxy.com sh scripts/install.sh   # use a mirror

Tips for users behind GFW:
  - If downloads timeout, set https_proxy or use --mirror
  - If api.github.com is rate-limited, pass --version explicitly
  - Alternatively: go install github.com/${OWNER}/${REPO}/cmd/aiclibridge@latest
EOF
            exit 0 ;;
        *)
            echo "install.sh: unknown option: $1" >&2
            echo "run 'install.sh --help' for usage." >&2
            exit 2 ;;
    esac
done

log() { [ "$VERBOSE" -ge 1 ] && printf '%s\n' "$*" || true; }
err() { printf 'install.sh: %s\n' "$*" >&2; }

# ── detect OS ──
OS="$(uname -s)"
case "$OS" in
    Darwin) GOOS="darwin" ;;
    Linux)  GOOS="linux" ;;
    *)
        err "unsupported OS: $OS (only darwin/linux are supported by this script)"
        err "for Windows, use install.ps1 instead."
        exit 1 ;;
esac

# ── detect ARCH ──
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)   GOARCH="amd64" ;;
    arm64|aarch64)  GOARCH="arm64" ;;
    *)
        err "unsupported architecture: $ARCH (only amd64/arm64 are supported)"
        exit 1 ;;
esac

log "detected: goos=$GOOS goarch=$GOARCH"

# ── resolve version ──
if [ -z "$INSTALL_VERSION" ]; then
    log "fetching latest release tag ..."

    # Primary: follow the releases/latest 302 redirect. The URL
    # https://github.com/<owner>/<repo>/releases/latest redirects to
    # .../releases/tag/<tag>. We extract <tag> from the Location
    # header WITHOUT following the redirect (so we don't download the
    # release page HTML). This avoids api.github.com entirely — the API
    # endpoint is frequently 403'd by rate limits or region-blocking in
    # mainland China, while github.com itself stays reachable.
    LATEST_TAG="$(curl $CURL_FIX -fsSI -H "User-Agent: ${INSTALLER_UA}" \
        "$GITHUB_RELEASES" 2>/dev/null \
        | grep -i '^location:' \
        | sed -E 's#.*/tag/##' \
        | tr -d '\r\n')"

    # Fallback: GitHub REST API (consumes the 60/h unauthenticated quota).
    if [ -z "$LATEST_TAG" ]; then
        log "redirect method failed; trying GitHub API ..."
        LATEST_TAG="$(curl $CURL_FIX -fsSL -H "User-Agent: ${INSTALLER_UA}" \
            "${GITHUB_API}/releases/latest" 2>/dev/null \
            | grep -m1 '"tag_name"' \
            | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
    fi

    if [ -z "$LATEST_TAG" ]; then
        err "could not determine latest release tag (tried redirect + API)"
        err "check network, or pass --version v0.5.0 explicitly."
        err "example: curl -fsSL ... | sh -s -- --version v0.5.0"
        exit 1
    fi
    INSTALL_VERSION="$LATEST_TAG"
fi

# Strip a leading 'v' for the version comparison/log; keep the original
# form for the download URL (GitHub releases URL includes the 'v').
VERSION_NO_V="${INSTALL_VERSION#v}"
log "installing version: $INSTALL_VERSION"

# ── build asset names ──
ASSET="aiclibridge-${GOOS}-${GOARCH}.tar.gz"
ASSET_SHA256="${ASSET}.sha256"
DOWNLOAD_URL="${GITHUB_DOWNLOAD}/${INSTALL_VERSION}/${ASSET}"
SHA256_URL="${GITHUB_DOWNLOAD}/${INSTALL_VERSION}/${ASSET_SHA256}"

# Apply mirror prefix to download URLs if GITHUB_MIRROR is set.
# Format: GITHUB_MIRROR=https://ghproxy.com → https://ghproxy.com/https://github.com/...
if [ -n "$MIRROR" ]; then
    DOWNLOAD_URL="${MIRROR%/}/${DOWNLOAD_URL}"
    SHA256_URL="${MIRROR%/}/${SHA256_URL}"
    log "using mirror: ${MIRROR}"
fi

# ── temp work dir ──
TMPDIR="$(mktemp -d 2>/dev/null || mktemp -d -t aiclibridge)"
trap 'rm -rf "$TMPDIR"' EXIT
log "temp dir: $TMPDIR"

# ── download ──
log "downloading $DOWNLOAD_URL"
if ! curl $CURL_FIX -fsSL "$DOWNLOAD_URL" -o "$TMPDIR/$ASSET"; then
    err "download failed: $DOWNLOAD_URL"
    err "verify the version ($INSTALL_VERSION) has a $ASSET asset on the release page."
    err "if github.com is blocked, try a mirror: GITHUB_MIRROR=https://ghproxy.com sh scripts/install.sh"
    err "or set https_proxy, or use: go install github.com/${OWNER}/${REPO}/cmd/aiclibridge@${INSTALL_VERSION}"
    exit 1
fi

log "downloading $SHA256_URL"
if ! curl $CURL_FIX -fsSL "$SHA256_URL" -o "$TMPDIR/$ASSET_SHA256"; then
    err "sha256 file download failed: $SHA256_URL"
    err "refusing to install without integrity verification."
    err "if github.com is blocked, try a mirror: GITHUB_MIRROR=https://ghproxy.com sh scripts/install.sh"
    exit 1
fi

# ── verify sha256 ──
# macOS ships shasum; Linux usually ships sha256sum. Try both.
EXPECTED="$(awk '{print $1}' "$TMPDIR/$ASSET_SHA256")"
if [ -z "$EXPECTED" ]; then
    err "could not parse expected sha256 from $ASSET_SHA256"
    exit 1
fi

ACTUAL=""
if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$TMPDIR/$ASSET" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "$TMPDIR/$ASSET" | awk '{print $1}')"
else
    err "neither sha256sum nor shasum found; cannot verify checksum."
    err "install one of them, or use the Go install path: go install github.com/${OWNER}/${REPO}/cmd/aiclibridge@latest"
    exit 1
fi

if [ "$ACTUAL" != "$EXPECTED" ]; then
    err "checksum mismatch!"
    err "  expected: $EXPECTED"
    err "  actual:   $ACTUAL"
    err "the download may be corrupted or tampered with. Aborting."
    exit 1
fi
log "checksum OK: $ACTUAL"

# ── extract ──
log "extracting $ASSET"
tar -xzf "$TMPDIR/$ASSET" -C "$TMPDIR"
# Binary name inside the tarball. v0.5.2+ ships a plain 'aiclibridge';
# older releases (v0.5.0/v0.5.1) shipped 'aiclibridge-{goos}-{goarch}'.
# Try canonical first, fall back to the platform-suffixed name.
BIN_NAME="aiclibridge"
if [ ! -f "$TMPDIR/$BIN_NAME" ]; then
    BIN_NAME="aiclibridge-${GOOS}-${GOARCH}"
    if [ ! -f "$TMPDIR/$BIN_NAME" ]; then
        err "extracted archive did not contain an aiclibridge binary"
        err "expected 'aiclibridge' or 'aiclibridge-${GOOS}-${GOARCH}' in $ASSET"
        exit 1
    fi
fi

# ── pick install dir (handle non-writable /usr/local/bin) ──
# The installed command is always 'aiclibridge' regardless of the
# source binary name (old releases shipped aiclibridge-{goos}-{goarch}).

can_write_to() {
    # True if dir exists and is writable by the current user.
    [ -d "$1" ] && [ -w "$1" ]
}

if ! can_write_to "$INSTALL_BIN_DIR"; then
    # Try sudo when interactive; otherwise fall back to ~/.local/bin.
    # NOTE: under `curl | sh` stdin is the curl pipe (not a tty), so
    # [ -t 0 ] is false and we take the fallback path — no sudo prompt.
    if [ -t 0 ] && [ -t 2 ] && command -v sudo >/dev/null 2>&1; then
        log "$INSTALL_BIN_DIR not writable; retrying with sudo"
        SUDO="sudo"
    else
        FALLBACK="$HOME/.local/bin"
        err "warning: $INSTALL_BIN_DIR not writable and no sudo available; falling back to $FALLBACK"
        err "make sure $FALLBACK is on your PATH."
        INSTALL_BIN_DIR="$FALLBACK"
        mkdir -p "$INSTALL_BIN_DIR" || {
            err "could not create $INSTALL_BIN_DIR"
            exit 1
        }
        SUDO=""
    fi
else
    SUDO=""
fi

# Compute TARGET AFTER the fallback may have changed INSTALL_BIN_DIR.
TARGET="${INSTALL_BIN_DIR%/}/aiclibridge"

# ── refuse overwrite unless --force ──
if [ -e "$TARGET" ] && [ "$FORCE" -ne 1 ]; then
    if [ -t 0 ] && [ -t 2 ]; then
        printf 'aiclibridge: %s already exists. Overwrite? [y/N] ' "$TARGET" >&2
        # Read from /dev/tty, NOT stdin: under `curl | sh` stdin is the
        # curl pipe, so `read` would consume script bytes and corrupt
        # execution. /dev/tty forces the real terminal.
        read ANSWER </dev/tty 2>/dev/null || ANSWER=""
        case "$ANSWER" in
            y|Y|yes|YES) ;;
            *)
                err "aborted; rerun with --force to skip this prompt."
                exit 1 ;;
        esac
    else
        err "refusing to overwrite existing $TARGET (non-interactive); rerun with --force."
        exit 1
    fi
fi

# ── install ──
log "installing to $TARGET"
if [ -n "$SUDO" ]; then
    $SUDO install -m 0755 "$TMPDIR/$BIN_NAME" "$TARGET"
else
    install -m 0755 "$TMPDIR/$BIN_NAME" "$TARGET"
fi

# ── verify ──
log "verifying install"
INSTALLED_VERSION="$("$TARGET" version 2>/dev/null | head -1 | awk '{print $2}')"
if [ -z "$INSTALLED_VERSION" ]; then
    err "installed binary at $TARGET did not respond to 'version'; check it is executable."
    exit 1
fi

# ── done ──
cat <<EOF

aiclibridge $INSTALLED_VERSION installed to $TARGET

Next steps:
  aiclibridge version
  aiclibridge --help
  aiclibridge start                  # background daemon on 127.0.0.1:8787
  aiclibridge run --model claude/anthropic/claude-sonnet-4.5 "hello"

Make sure $INSTALL_BIN_DIR is on your PATH:
  export PATH="$INSTALL_BIN_DIR:\$PATH"   # add to ~/.zshrc or ~/.bashrc

Docs: https://github.com/${OWNER}/${REPO}#readme
EOF
