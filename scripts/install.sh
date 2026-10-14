#!/bin/bash
# SLB Installation Script
# Downloads and installs the latest release of SLB

set -euo pipefail

# Configuration
REPO="Dicklesworthstone/slb"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BINARY="slb"
DOWNLOADER_UA="slb-installer (+https://github.com/Dicklesworthstone/slb)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info() { echo -e "${BLUE}[INFO]${NC} $1"; }
success() { echo -e "${GREEN}[OK]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1" >&2; exit 1; }

download() {
    curl --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 20 --max-time 180 --user-agent "$DOWNLOADER_UA" "$@"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        if ! sha256sum "$1" | awk '{print tolower($1)}'; then
            error "sha256sum could not read the file"
        fi
    elif command -v shasum >/dev/null 2>&1; then
        if ! shasum -a 256 "$1" | awk '{print tolower($1)}'; then
            error "shasum could not read the file"
        fi
    else
        error "sha256sum or shasum is required to verify releases"
    fi
}

# Detect OS and architecture
detect_platform() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    ARCH="$(uname -m)"

    case "$OS" in
        linux) OS="linux" ;;
        darwin) OS="darwin" ;;
        mingw*|msys*|cygwin*) OS="windows" ;;
        *) error "Unsupported OS: $OS" ;;
    esac

    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        arm64|aarch64) ARCH="arm64" ;;
        *) error "Unsupported architecture: $ARCH" ;;
    esac

    EXECUTABLE="$BINARY"
    if [ "$OS" = "windows" ]; then
        [ "$ARCH" = "amd64" ] || error "Windows arm64 releases are not available"
        EXECUTABLE="${BINARY}.exe"
    fi

    PLATFORM="${OS}_${ARCH}"
    info "Detected platform: $PLATFORM"
}

# Resolve one stable tag; every subsequent asset lookup uses this same tag.
get_latest_version() {
    local release_url
    local release_prefix="https://github.com/${REPO}/releases/tag/"
    info "Fetching latest version..."
    if ! release_url="$(download --head --output /dev/null --write-out '%{url_effective}' \
        "https://github.com/${REPO}/releases/latest")"; then
        error "Failed to resolve the latest release"
    fi
    case "$release_url" in
        "$release_prefix"*) VERSION="${release_url#"$release_prefix"}" ;;
        *) error "Latest release did not resolve to the expected repository tag" ;;
    esac
    [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || error "Latest release has an invalid stable version tag"
    info "Latest version: $VERSION"
}

# Workspaces are retained, including on failure. For controlled qualification,
# SLB_RETAIN_WORK_ROOT must be an absolute, fresh path with an existing parent.
prepare_workspace() {
    if [ -n "${SLB_RETAIN_WORK_ROOT:-}" ]; then
        case "$SLB_RETAIN_WORK_ROOT" in
            /*) ;;
            *) error "SLB_RETAIN_WORK_ROOT must be absolute" ;;
        esac
        if [ -e "$SLB_RETAIN_WORK_ROOT" ] || [ -L "$SLB_RETAIN_WORK_ROOT" ]; then
            error "SLB_RETAIN_WORK_ROOT must be a fresh path"
        fi
        if ! mkdir -m 700 "$SLB_RETAIN_WORK_ROOT"; then
            error "Could not create retained workspace"
        fi
        SLB_WORK_DIR="$SLB_RETAIN_WORK_ROOT"
    else
        if ! SLB_WORK_DIR="$(mktemp -d)"; then
            error "Could not create installer workspace"
        fi
    fi
    info "Installer workspace retained at $SLB_WORK_DIR"
}

verify_archive() {
    local archive="$1"
    local expected actual
    if ! expected="$(LC_ALL=C awk -v name="$archive" '
        $2 == name || $2 == "*" name {
            count++
            if (NF != 2 || length($1) != 64 || $1 ~ /[^[:xdigit:]]/) bad = 1
            digest = tolower($1)
        }
        END {
            if (count != 1 || bad) exit 1
            print digest
        }
    ' "${SLB_WORK_DIR}/checksums.txt")"; then
        error "Release checksum manifest must contain exactly one valid entry for $archive"
    fi
    if ! actual="$(sha256_file "${SLB_WORK_DIR}/${archive}")"; then
        error "Could not compute archive checksum"
    fi
    [[ "$actual" =~ ^[0-9a-f]{64}$ ]] || error "Archive checksum tool returned an invalid digest"
    [ "$actual" = "$expected" ] || error "Archive checksum mismatch for $archive"
    success "Verified checksum for $archive"
}

# Download and install
install() {
    local EXT="tar.gz"
    if [ "$OS" = "windows" ]; then
        EXT="zip"
    fi

    local ARCHIVE="${BINARY}_${VERSION#v}_${PLATFORM}.${EXT}"
    local URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"
    local CHECKSUM_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

    prepare_workspace
    info "Downloading $URL..."
    if ! download --output "${SLB_WORK_DIR}/${ARCHIVE}" "$URL"; then
        error "Failed to download $URL"
    fi
    if ! download --output "${SLB_WORK_DIR}/checksums.txt" "$CHECKSUM_URL"; then
        error "Failed to download release checksums"
    fi
    verify_archive "$ARCHIVE"

    info "Extracting..."
    if [ "$EXT" = "tar.gz" ]; then
        if ! tar -xzf "${SLB_WORK_DIR}/${ARCHIVE}" -C "$SLB_WORK_DIR"; then
            error "Could not extract release archive"
        fi
    else
        if ! command -v unzip >/dev/null 2>&1; then
            error "unzip is required to install Windows releases"
        fi
        if ! unzip -q "${SLB_WORK_DIR}/${ARCHIVE}" -d "$SLB_WORK_DIR"; then
            error "Could not extract release archive"
        fi
    fi
    if [ ! -f "${SLB_WORK_DIR}/${EXECUTABLE}" ] || [ -L "${SLB_WORK_DIR}/${EXECUTABLE}" ]; then
        error "Release archive does not contain the expected executable $EXECUTABLE"
    fi

    info "Installing to ${INSTALL_DIR}..."
    if [ -w "$INSTALL_DIR" ]; then
        if ! cp "${SLB_WORK_DIR}/${EXECUTABLE}" "${INSTALL_DIR}/${EXECUTABLE}"; then
            error "Could not copy release executable to destination"
        fi
        if ! chmod +x "${INSTALL_DIR}/${EXECUTABLE}"; then
            error "Could not make installed executable runnable"
        fi
    else
        warn "Installing to ${INSTALL_DIR} requires sudo"
        if ! sudo cp "${SLB_WORK_DIR}/${EXECUTABLE}" "${INSTALL_DIR}/${EXECUTABLE}"; then
            error "Could not copy release executable to destination"
        fi
        if ! sudo chmod +x "${INSTALL_DIR}/${EXECUTABLE}"; then
            error "Could not make installed executable runnable"
        fi
    fi

    success "Installed ${BINARY} to ${INSTALL_DIR}/${EXECUTABLE}"
}

# Verify the exact destination, even if PATH contains a different slb.
verify() {
    local installed="${INSTALL_DIR}/${EXECUTABLE}"
    [ -f "$installed" ] || error "Installed executable is missing: $installed"
    info "Verifying installation..."
    if ! "$installed" version; then
        error "Installed executable could not report its version"
    fi
    success "SLB installed successfully!"
    if ! command -v "$BINARY" >/dev/null 2>&1; then
        warn "Binary not in PATH. You may need to add ${INSTALL_DIR} to your PATH."
    fi
}

# Main
main() {
    echo ""
    echo "╔══════════════════════════════════════╗"
    echo "║   SIMULTANEOUS LAUNCH BUTTON (SLB)   ║"
    echo "║           Installation Script        ║"
    echo "╚══════════════════════════════════════╝"
    echo ""

    detect_platform
    get_latest_version
    install
    verify

    echo ""
    info "Get started:"
    echo "  slb init              # Initialize SLB in current directory"
    echo "  slb daemon start      # Start approval daemon"
    echo "  slb run \"<command>\"   # Submit command for approval"
    echo ""
    echo "Tip: You can also install via Homebrew:"
    echo "  brew install dicklesworthstone/tap/slb"
    echo ""
}

main
