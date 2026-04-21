#!/bin/sh
set -eu

# cc-clip installer
# Usage: curl -fsSL https://raw.githubusercontent.com/fancy-potato/cc-clip/main/scripts/install.sh | sh

REPO="fancy-potato/cc-clip"
INSTALL_DIR="${CC_CLIP_INSTALL_DIR:-$HOME/.local/bin}"
LOCAL_SHARE_DIR="${CC_CLIP_SHARE_DIR:-$HOME/.local/share/cc-clip}"
SWIFTBAR_APP="/Applications/SwiftBar.app"
SWIFTBAR_PLUGIN_DIR_DEFAULT="$HOME/Documents/SwiftBar"
# SWIFTBAR_PLUGIN_DIR, if exported by the caller, is an explicit
# operator override and is honored as-is. Leave it empty here so
# install_swiftbar_plugin can distinguish "unset" from "set to
# default" — the former triggers the defaults-read lookup on a host
# that already has SwiftBar installed; the latter does not.
SWIFTBAR_PLUGIN_DIR="${SWIFTBAR_PLUGIN_DIR-}"
LOCAL_PLUGIN_DIR="${CC_CLIP_SWIFTBAR_PLUGIN_DIR:-$LOCAL_SHARE_DIR/swiftbar}"
LOCAL_SCRIPT_DIR="${CC_CLIP_SCRIPT_DIR:-$LOCAL_SHARE_DIR/scripts}"
PLUGIN_NAME="cc-clip-tunnels.30s.sh"
INSTALL_SWIFTBAR="${INSTALL_SWIFTBAR:-1}"
INSTALL_JQ="${INSTALL_JQ:-1}"
INSTALL_TERMINAL_NOTIFIER="${INSTALL_TERMINAL_NOTIFIER:-1}"

# Production must never silently swap the downloaded artifact via env vars.
# CC_CLIP_TEST_DOWNLOAD / CC_CLIP_TEST_VERSION are test-only overrides and
# now require CC_CLIP_ALLOW_TEST=1 as an explicit opt-in gate. Unset the
# overrides otherwise so a stray env leak cannot bypass the real download +
# checksum verification path.
if [ "${CC_CLIP_ALLOW_TEST:-0}" != "1" ]; then
    unset CC_CLIP_TEST_DOWNLOAD 2>/dev/null || true
    unset CC_CLIP_TEST_VERSION 2>/dev/null || true
    unset CC_CLIP_TEST_CHECKSUMS 2>/dev/null || true
    unset CC_CLIP_TEST_RAW_DIR 2>/dev/null || true
fi

die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "Unsupported architecture: $ARCH" ;;
    esac

    case "$OS" in
        darwin|linux) ;;
        windows|mingw*|cygwin*|msys*)
            die "Windows is not supported by this installer. See docs/windows-quickstart.md for the Windows workflow."
            ;;
        *) die "Unsupported OS: $OS (only macOS and Linux are supported)" ;;
    esac

    echo "${OS}_${ARCH}"
}

# Archive suffix derived from $OS so a future Windows pipeline that drops a
# .zip asset doesn't silently hit a 404 for .tar.gz. Windows is still
# explicitly rejected above — this is defense-in-depth.
archive_suffix() {
    case "$1" in
        windows) echo ".zip" ;;
        *) echo ".tar.gz" ;;
    esac
}

get_latest_version() {
    if [ -n "${CC_CLIP_TEST_VERSION:-}" ]; then
        echo "$CC_CLIP_TEST_VERSION"
        return 0
    fi
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | \
            grep '"tag_name"' | head -1 | cut -d'"' -f4
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | \
            grep '"tag_name"' | head -1 | cut -d'"' -f4
    else
        die "curl or wget required"
    fi
}

# download URL DEST — download URL to DEST. In test mode (CC_CLIP_ALLOW_TEST=1
# plus CC_CLIP_TEST_DOWNLOAD), copy from a local path instead. No `local`
# because this script is POSIX sh.
download() {
    if [ -n "${CC_CLIP_TEST_DOWNLOAD:-}" ]; then
        cp "$CC_CLIP_TEST_DOWNLOAD" "$2"
        return 0
    fi
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    else
        wget -qO "$2" "$1"
    fi
}

# download_checksums URL DEST — fetch the checksum manifest. Same test hook
# as download(), via CC_CLIP_TEST_CHECKSUMS, but still gated on
# CC_CLIP_ALLOW_TEST=1 at the top of the script.
download_checksums() {
    if [ -n "${CC_CLIP_TEST_CHECKSUMS:-}" ]; then
        cp "$CC_CLIP_TEST_CHECKSUMS" "$2"
        return 0
    fi
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    else
        wget -qO "$2" "$1"
    fi
}

download_repo_file() {
    repo_ref=$1
    repo_path=$2
    dest=$3
    if [ -n "${CC_CLIP_TEST_RAW_DIR:-}" ]; then
        src="${CC_CLIP_TEST_RAW_DIR%/}/${repo_path}"
        if [ ! -f "$src" ]; then
            return 1
        fi
        cp "$src" "$dest"
        return 0
    fi
    url="https://raw.githubusercontent.com/${REPO}/${repo_ref}/${repo_path}"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$dest"
    else
        wget -qO "$dest" "$url"
    fi
}

# pick_sha256_tool — echo the available SHA-256 CLI as argv suitable for
# piping through `read` (so "<tool> -a 256" etc. survive). Fails loudly if
# neither shasum nor sha256sum is present so we never silently skip
# verification.
pick_sha256_tool() {
    if command -v sha256sum >/dev/null 2>&1; then
        echo "sha256sum"
        return 0
    fi
    if command -v shasum >/dev/null 2>&1; then
        echo "shasum -a 256"
        return 0
    fi
    die "neither sha256sum nor shasum found; cannot verify download integrity. Install coreutils (Linux) or use the macOS built-in /usr/bin/shasum."
}

# verify_sha256 FILE EXPECTED_HEX — compute SHA-256 of FILE and compare
# against EXPECTED_HEX case-insensitively. Exits via die() on mismatch.
verify_sha256() {
    VF_FILE=$1
    VF_EXPECTED=$2
    VF_TOOL=$(pick_sha256_tool)
    # $VF_TOOL is intentionally unquoted so "shasum -a 256" splits correctly.
    # shellcheck disable=SC2086
    VF_ACTUAL=$($VF_TOOL "$VF_FILE" | awk '{print $1}')
    # Normalize to lowercase for comparison. Some tools emit uppercase.
    VF_EXPECTED_LC=$(printf '%s' "$VF_EXPECTED" | tr '[:upper:]' '[:lower:]')
    VF_ACTUAL_LC=$(printf '%s' "$VF_ACTUAL" | tr '[:upper:]' '[:lower:]')
    if [ "$VF_EXPECTED_LC" != "$VF_ACTUAL_LC" ]; then
        die "checksum mismatch for $VF_FILE: expected $VF_EXPECTED_LC, got $VF_ACTUAL_LC. Refusing to install a tampered artifact."
    fi
}

ensure_brew() {
    command -v brew >/dev/null 2>&1
}

install_swiftbar_if_missing() {
    if [ "$(uname -s)" != "Darwin" ]; then
        return 0
    fi
    if [ -d "$SWIFTBAR_APP" ]; then
        return 0
    fi
    if [ "$INSTALL_SWIFTBAR" != "1" ]; then
        return 0
    fi

    if ensure_brew; then
        echo "SwiftBar not found; installing via Homebrew..."
        brew install --cask swiftbar || true
    else
        echo "SwiftBar not found and Homebrew is missing." >&2
        echo "Install SwiftBar: brew install --cask swiftbar" >&2
    fi
}

install_jq_if_missing() {
    if [ "$(uname -s)" != "Darwin" ]; then
        return 0
    fi
    if command -v jq >/dev/null 2>&1; then
        return 0
    fi
    if [ "$INSTALL_JQ" != "1" ]; then
        return 0
    fi

    if ensure_brew; then
        echo "jq not found; installing via Homebrew..."
        brew install jq || true
    else
        echo "jq not found and Homebrew is missing." >&2
        echo "Install jq: brew install jq" >&2
    fi
}

install_terminal_notifier_if_missing() {
    if [ "$(uname -s)" != "Darwin" ]; then
        return 0
    fi
    if command -v terminal-notifier >/dev/null 2>&1; then
        return 0
    fi
    if [ "$INSTALL_TERMINAL_NOTIFIER" != "1" ]; then
        return 0
    fi

    if ensure_brew; then
        echo "terminal-notifier not found; installing via Homebrew..."
        brew install terminal-notifier || true
    else
        echo "terminal-notifier not found and Homebrew is missing." >&2
        echo "Install terminal-notifier: brew install terminal-notifier" >&2
    fi
}

resolve_swiftbar_plugin_dir() {
    # Resolve the plugin directory in priority order:
    #   1. SWIFTBAR_PLUGIN_DIR env var explicitly passed by the operator
    #   2. The existing SwiftBar `PluginDirectory` preference, when
    #      SwiftBar is already installed. This means "user had SwiftBar
    #      set up with their own plugin folder, we drop in alongside
    #      their other plugins and don't clobber their setting."
    #   3. Default `$HOME/Documents/SwiftBar` (the SwiftBar-recommended
    #      location, created on-demand below).
    # Side effect: sets SWIFTBAR_PLUGIN_DIR and SWIFTBAR_PLUGIN_DIR_SOURCE.
    if [ -n "$SWIFTBAR_PLUGIN_DIR" ]; then
        SWIFTBAR_PLUGIN_DIR_SOURCE="env"
        return 0
    fi

    if [ -d "$SWIFTBAR_APP" ]; then
        existing="$(defaults read com.ameba.SwiftBar PluginDirectory 2>/dev/null || true)"
        if [ -n "$existing" ]; then
            SWIFTBAR_PLUGIN_DIR="$existing"
            SWIFTBAR_PLUGIN_DIR_SOURCE="pref"
            return 0
        fi
    fi

    SWIFTBAR_PLUGIN_DIR="$SWIFTBAR_PLUGIN_DIR_DEFAULT"
    SWIFTBAR_PLUGIN_DIR_SOURCE="default"
}

install_swiftbar_plugin() {
    if [ "$(uname -s)" != "Darwin" ]; then
        return 0
    fi

    resolve_swiftbar_plugin_dir

    plugin_src="$1"
    local_plugin="$LOCAL_PLUGIN_DIR/$PLUGIN_NAME"
    plugin_link="$SWIFTBAR_PLUGIN_DIR/$PLUGIN_NAME"

    mkdir -p "$LOCAL_PLUGIN_DIR"
    mkdir -p "$SWIFTBAR_PLUGIN_DIR"

    cp "$plugin_src" "$local_plugin"
    chmod +x "$local_plugin"

    # Atomic replace: `ln -sfn` creates-or-replaces the symlink in one
    # rename(2), so SwiftBar never observes a moment where the link is
    # missing (which a `rm -f` + `ln -s` window would briefly expose).
    ln -sfn "$local_plugin" "$plugin_link"

    # Only write the PluginDirectory preference when WE chose the
    # directory (env override or first-time default). When the path
    # came from SwiftBar's own prefs (source=pref), the user already
    # configured it — rewriting would be a no-op at best and a clobber
    # of a path we racily misread at worst.
    if [ "$SWIFTBAR_PLUGIN_DIR_SOURCE" != "pref" ]; then
        defaults write com.ameba.SwiftBar PluginDirectory -string "$SWIFTBAR_PLUGIN_DIR" >/dev/null 2>&1 || true
    fi

    if [ -d "$SWIFTBAR_APP" ]; then
        osascript -e 'tell application "SwiftBar" to quit' >/dev/null 2>&1 || true
        open -a "SwiftBar" >/dev/null 2>&1 || true
    fi

    echo ""
    echo "SwiftBar plugin installed:"
    echo "  local script: $local_plugin"
    echo "  SwiftBar link: $plugin_link"
    case "$SWIFTBAR_PLUGIN_DIR_SOURCE" in
        pref)    echo "  plugin dir:    $SWIFTBAR_PLUGIN_DIR (from existing SwiftBar preference)" ;;
        env)     echo "  plugin dir:    $SWIFTBAR_PLUGIN_DIR (from SWIFTBAR_PLUGIN_DIR override)" ;;
        default) echo "  plugin dir:    $SWIFTBAR_PLUGIN_DIR (default)" ;;
    esac
}

install_maintenance_script() {
    script_name="$1"
    script_src="$2"
    mkdir -p "$LOCAL_SCRIPT_DIR"
    cp "$script_src" "$LOCAL_SCRIPT_DIR/$script_name"
    chmod +x "$LOCAL_SCRIPT_DIR/$script_name"
}

ensure_support_script() {
    script_name="$1"
    script_path="${TMP_DIR}/scripts/${script_name}"
    if [ -f "$script_path" ]; then
        return 0
    fi
    mkdir -p "${TMP_DIR}/scripts"
    echo "Support script ${script_name} missing from release archive; fetching from ${VERSION}..."
    if ! download_repo_file "$VERSION" "scripts/${script_name}" "$script_path"; then
        echo "Warning: could not fetch scripts/${script_name} from ${VERSION}" >&2
        return 1
    fi
    chmod +x "$script_path"
    return 0
}

main() {
    PLATFORM=$(detect_platform)
    VERSION=$(get_latest_version)

    if [ -z "$VERSION" ]; then
        die "could not determine latest version"
    fi

    # Split "$PLATFORM" ("darwin_arm64") back to $OS for archive_suffix.
    OS_ONLY=${PLATFORM%%_*}
    SUFFIX=$(archive_suffix "$OS_ONLY")

    echo "Installing cc-clip ${VERSION} for ${PLATFORM}..."

    ARCHIVE_NAME="cc-clip_${VERSION#v}_${PLATFORM}${SUFFIX}"
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"
    CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "$TMP_DIR"' EXIT

    echo "Downloading ${DOWNLOAD_URL}..."
    download "$DOWNLOAD_URL" "${TMP_DIR}/${ARCHIVE_NAME}"

    echo "Verifying checksum..."
    download_checksums "$CHECKSUMS_URL" "${TMP_DIR}/checksums.txt"
    # Expected format (goreleaser default): "<hex>  <filename>" per line.
    # Pull the line whose filename column exactly matches our archive.
    EXPECTED_SHA=$(awk -v name="$ARCHIVE_NAME" '$2 == name {print $1; exit}' "${TMP_DIR}/checksums.txt")
    if [ -z "$EXPECTED_SHA" ]; then
        die "no checksum entry for $ARCHIVE_NAME in checksums.txt (release asset may be corrupted or renamed)"
    fi
    verify_sha256 "${TMP_DIR}/${ARCHIVE_NAME}" "$EXPECTED_SHA"

    echo "Extracting..."
    case "$SUFFIX" in
        .tar.gz) tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "$TMP_DIR" ;;
        .zip)
            if command -v unzip >/dev/null 2>&1; then
                unzip -q -o "${TMP_DIR}/${ARCHIVE_NAME}" -d "$TMP_DIR"
            else
                die "unzip is required to extract $ARCHIVE_NAME but was not found"
            fi
            ;;
        *) die "unsupported archive suffix: $SUFFIX" ;;
    esac

    mkdir -p "$INSTALL_DIR"
    cp "${TMP_DIR}/cc-clip" "${INSTALL_DIR}/cc-clip"
    chmod +x "${INSTALL_DIR}/cc-clip"

    # macOS Gatekeeper fix: downloaded binaries are blocked in two ways:
    # 1. com.apple.quarantine / com.apple.provenance extended attributes
    # 2. Missing or invalid code signature (Identifier=a.out)
    # Clear all xattrs and re-sign with proper identifier to satisfy Gatekeeper.
    if [ "$(uname -s)" = "Darwin" ]; then
        xattr -cr "${INSTALL_DIR}/cc-clip" 2>/dev/null || true
        codesign --force --sign - --identifier com.cc-clip.cli "${INSTALL_DIR}/cc-clip" 2>/dev/null || true
    fi

    install_swiftbar_if_missing
    install_jq_if_missing
    install_terminal_notifier_if_missing
    ensure_support_script "${PLUGIN_NAME}" || true
    ensure_support_script "uninstall-local.sh" || true
    ensure_support_script "uninstall-all.sh" || true
    if [ -f "${TMP_DIR}/scripts/${PLUGIN_NAME}" ]; then
        install_swiftbar_plugin "${TMP_DIR}/scripts/${PLUGIN_NAME}"
    fi
    if [ -f "${TMP_DIR}/scripts/uninstall-local.sh" ]; then
        install_maintenance_script "uninstall-local.sh" "${TMP_DIR}/scripts/uninstall-local.sh"
    fi
    if [ -f "${TMP_DIR}/scripts/uninstall-all.sh" ]; then
        install_maintenance_script "uninstall-all.sh" "${TMP_DIR}/scripts/uninstall-all.sh"
    fi

    echo ""
    echo "cc-clip ${VERSION} installed to ${INSTALL_DIR}/cc-clip"
    if [ -d "$LOCAL_SCRIPT_DIR" ]; then
        echo "Maintenance scripts installed to ${LOCAL_SCRIPT_DIR}"
    fi

    if ! echo "$PATH" | tr ':' '\n' | grep -q "^${INSTALL_DIR}$"; then
        echo ""
        echo "Add to your PATH:"
        echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    fi

    echo ""
    echo "Quick start:"
    echo "  cc-clip setup HOST        # One command: deps, daemon, deploy"
    echo "  Ctrl+V in remote Claude Code"
}

main
