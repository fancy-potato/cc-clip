#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

INSTALL_DIR=${CC_CLIP_INSTALL_DIR:-"$HOME/.local/bin"}
PORT=18339
INSTALL_MODE=source
PURGE_PEER_IDENTITY=0

usage() {
	cat <<EOF
Usage: $(basename "$0") [options]

Completely reinstall local cc-clip state on this machine.

Options:
  --install-dir DIR         Install cc-clip to DIR (default: \$HOME/.local/bin)
  --port PORT               Daemon/service port (default: 18339)
  --use-release             Reinstall from scripts/install.sh instead of building this repo
  --purge-peer-identity     Also remove ~/.cache/cc-clip/local-peer-id and local-peer-label
  -h, --help                Show this help

Notes:
  - By default this preserves the local peer identity so a later remote cleanup
    can still release the existing peer lease safely.
  - On macOS this also unloads/reloads the launchd service.
EOF
}

log() {
	printf '%s\n' "$*"
}

warn() {
	printf 'warning: %s\n' "$*" >&2
}

require_arg() {
	if [ $# -lt 2 ] || [ -z "$2" ]; then
		printf 'error: %s requires a value\n' "$1" >&2
		exit 1
	fi
}

resolve_existing_cc_clip_bin() {
	if [ -n "${CC_CLIP_BIN:-}" ] && [ -x "${CC_CLIP_BIN}" ]; then
		printf '%s\n' "$CC_CLIP_BIN"
		return 0
	fi
	if command -v cc-clip >/dev/null 2>&1; then
		command -v cc-clip
		return 0
	fi
	if [ -x "$REPO_ROOT/cc-clip" ]; then
		printf '%s\n' "$REPO_ROOT/cc-clip"
		return 0
	fi
	return 1
}

run_cc_clip_if_present() {
	DESC=$1
	shift
	if [ -z "${CURRENT_CC_CLIP:-}" ]; then
		log "  $DESC: skipped (cc-clip not found)"
		return 0
	fi
	if "$CURRENT_CC_CLIP" "$@" >/dev/null 2>&1; then
		log "  $DESC: ok"
		return 0
	fi
	warn "$DESC failed and was ignored"
	return 0
}

cleanup_launchd() {
	if [ "$(uname -s)" != "Darwin" ]; then
		return 0
	fi
	PLIST="$HOME/Library/LaunchAgents/com.cc-clip.daemon.plist"
	LOG_FILE="$HOME/Library/Logs/cc-clip.log"
	if [ -f "$PLIST" ]; then
		launchctl unload -w "$PLIST" >/dev/null 2>&1 || true
	fi
	launchctl remove com.cc-clip.daemon >/dev/null 2>&1 || true
	rm -f "$PLIST" "$LOG_FILE"
}

cleanup_cache_dir() {
	CACHE_DIR="$HOME/.cache/cc-clip"
	if [ ! -d "$CACHE_DIR" ]; then
		return 0
	fi

	if [ "$PURGE_PEER_IDENTITY" -eq 1 ]; then
		rm -rf "$CACHE_DIR"
		return 0
	fi

	TMP_DIR=$(mktemp -d)
	trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM

	for NAME in local-peer-id local-peer-label; do
		if [ -f "$CACHE_DIR/$NAME" ]; then
			cp "$CACHE_DIR/$NAME" "$TMP_DIR/$NAME"
		fi
	done

	rm -rf "$CACHE_DIR"
	mkdir -p "$CACHE_DIR"
	chmod 700 "$CACHE_DIR" 2>/dev/null || true

	for NAME in local-peer-id local-peer-label; do
		if [ -f "$TMP_DIR/$NAME" ]; then
			mv "$TMP_DIR/$NAME" "$CACHE_DIR/$NAME"
			chmod 600 "$CACHE_DIR/$NAME" 2>/dev/null || true
		fi
	done

	rm -rf "$TMP_DIR"
	trap - EXIT HUP INT TERM
}

install_from_source() {
	log "[3/4] Building and installing cc-clip from this repo..."
	(
		cd "$REPO_ROOT"
		make build
	)
	mkdir -p "$INSTALL_DIR"
	cp "$REPO_ROOT/cc-clip" "$INSTALL_DIR/cc-clip"
	chmod +x "$INSTALL_DIR/cc-clip"
	if [ "$(uname -s)" = "Darwin" ]; then
		xattr -cr "$INSTALL_DIR/cc-clip" 2>/dev/null || true
		codesign --force --sign - --identifier com.cc-clip.cli "$INSTALL_DIR/cc-clip" 2>/dev/null || true
	fi
}

install_from_release() {
	log "[3/4] Installing cc-clip from the latest release..."
	CC_CLIP_INSTALL_DIR="$INSTALL_DIR" sh "$REPO_ROOT/scripts/install.sh"
}

ensure_pngpaste() {
	if [ "$(uname -s)" != "Darwin" ]; then
		return 0
	fi
	if command -v pngpaste >/dev/null 2>&1 || [ -x /opt/homebrew/bin/pngpaste ] || [ -x /usr/local/bin/pngpaste ]; then
		log "  pngpaste: ok"
		return 0
	fi
	if ! command -v brew >/dev/null 2>&1; then
		warn "pngpaste is missing and Homebrew is not installed; install it manually with: brew install pngpaste"
		return 0
	fi
	log "  pngpaste: installing via Homebrew..."
	brew install pngpaste
}

install_service() {
	if [ "$(uname -s)" != "Darwin" ]; then
		log "[4/4] Local service install skipped on $(uname -s)."
		return 0
	fi
	log "[4/4] Installing launchd service..."
	"$INSTALL_DIR/cc-clip" service install --port "$PORT"
}

while [ $# -gt 0 ]; do
	case "$1" in
		--install-dir)
			require_arg "$@"
			INSTALL_DIR=$2
			shift 2
			;;
		--port)
			require_arg "$@"
			PORT=$2
			shift 2
			;;
		--use-release)
			INSTALL_MODE=release
			shift
			;;
		--purge-peer-identity)
			PURGE_PEER_IDENTITY=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			printf 'error: unknown option: %s\n' "$1" >&2
			usage >&2
			exit 1
			;;
	esac
done

if CURRENT_CC_CLIP=$(resolve_existing_cc_clip_bin 2>/dev/null); then
	:
else
	CURRENT_CC_CLIP=""
fi

log "[1/4] Uninstalling local cc-clip pieces..."
run_cc_clip_if_present "service uninstall" service uninstall
run_cc_clip_if_present "local Codex cleanup" uninstall --codex
run_cc_clip_if_present "local xclip shim cleanup" uninstall --target xclip
run_cc_clip_if_present "local wl-paste shim cleanup" uninstall --target wl-paste
cleanup_launchd

log "[2/4] Removing local state and installed binary..."
rm -f "$INSTALL_DIR/cc-clip"
cleanup_cache_dir

if [ "$INSTALL_MODE" = "release" ]; then
	install_from_release
else
	install_from_source
fi

ensure_pngpaste
install_service

log
log "Local reinstall complete."
log "  binary: $INSTALL_DIR/cc-clip"
log "  port:   $PORT"
