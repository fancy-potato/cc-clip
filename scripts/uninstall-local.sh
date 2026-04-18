#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

INSTALL_DIR=${CC_CLIP_INSTALL_DIR:-"$HOME/.local/bin"}
KEEP_CACHE=0
KEEP_PEER_IDENTITY=0

usage() {
	cat <<EOF
Usage: $(basename "$0") [options]

Uninstall local cc-clip components from this machine.

Options:
  --install-dir DIR        Remove binary and local shims from DIR (default: \$HOME/.local/bin)
  --keep-cache             Keep ~/.cache/cc-clip
  --keep-peer-identity     Preserve local-peer-id and local-peer-label while clearing the rest of the cache
  -h, --help               Show this help

Notes:
  - Default behavior removes the launchd service, local Codex runtime state,
    local managed shims, the installed cc-clip binary, and ~/.cache/cc-clip.
  - Use --keep-peer-identity if you still need to clean the remote peer lease later.
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

remove_managed_file() {
	PATHNAME=$1
	MARKER=$2
	DESC=$3
	if [ -f "$PATHNAME" ] && grep -q "$MARKER" "$PATHNAME" 2>/dev/null; then
		rm -f "$PATHNAME"
		log "  $DESC: removed"
	fi
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

	if [ "$KEEP_CACHE" -eq 1 ] || [ ! -d "$CACHE_DIR" ]; then
		return 0
	fi

	if [ "$KEEP_PEER_IDENTITY" -eq 0 ]; then
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

while [ $# -gt 0 ]; do
	case "$1" in
		--install-dir)
			require_arg "$@"
			INSTALL_DIR=$2
			shift 2
			;;
		--keep-cache)
			KEEP_CACHE=1
			shift
			;;
		--keep-peer-identity)
			KEEP_PEER_IDENTITY=1
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

log "[1/3] Uninstalling local cc-clip pieces..."
run_cc_clip_if_present "service uninstall" service uninstall
run_cc_clip_if_present "local Codex cleanup" uninstall --codex
run_cc_clip_if_present "local xclip shim cleanup" uninstall --target xclip --path "$INSTALL_DIR"
run_cc_clip_if_present "local wl-paste shim cleanup" uninstall --target wl-paste --path "$INSTALL_DIR"
cleanup_launchd
remove_managed_file "$INSTALL_DIR/xclip" "cc-clip" "$INSTALL_DIR/xclip"
remove_managed_file "$INSTALL_DIR/wl-paste" "cc-clip" "$INSTALL_DIR/wl-paste"

log "[2/3] Removing local binary..."
rm -f "$INSTALL_DIR/cc-clip"

log "[3/3] Removing local state..."
cleanup_cache_dir

log
log "Local uninstall complete."
log "  install dir: $INSTALL_DIR"
if [ "$KEEP_CACHE" -eq 1 ]; then
	log "  cache: kept"
elif [ "$KEEP_PEER_IDENTITY" -eq 1 ]; then
	log "  cache: cleared (peer identity preserved)"
else
	log "  cache: removed"
fi
