#!/bin/sh
set -eu

INSTALL_DIR=${CC_CLIP_INSTALL_DIR:-"$HOME/.local/bin"}

usage() {
	cat <<EOF
Usage: $(basename "$0") [options]

Dangerous full local purge for cc-clip.

Options:
  --install-dir DIR        Remove binary and local shims from DIR (default: \$HOME/.local/bin)
  -h, --help               Show this help

Notes:
  - This script is intentionally destructive. It removes local binaries,
    shims, launchd state, ~/.cache/cc-clip, and the cc-clip-managed blocks
    from ~/.ssh/config when that file is a regular file. Two block types
    are removed: the current per-peer SetEnv block
    \`# >>> cc-clip SetEnv (do not edit) >>>\` … \`# <<< cc-clip SetEnv (do not edit) <<<\`
    AND the legacy pre-daemon-tunnel block
    \`# >>> cc-clip managed host: … >>>\` … \`# <<< cc-clip managed host: … <<<\`.
    Stripping the legacy block on a full local purge is intentional: this
    script's contract is "tear down cc-clip on this machine completely",
    so the manual-cleanup guidance in AGENTS.md applies only to in-place
    upgrades, not to operators who chose to wipe everything.
  - Symlinked ~/.ssh/config files are preserved with a warning.
  - Use this only when tearing down cc-clip completely on this machine.
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

remove_managed_ssh_config_blocks() {
	SSH_CONFIG=$HOME/.ssh/config
	if [ -L "$SSH_CONFIG" ]; then
		warn "skipping ~/.ssh/config cleanup because it is a symlink"
		return 0
	fi
	if [ ! -f "$SSH_CONFIG" ]; then
		return 0
	fi

	TMP_FILE=$(mktemp "${TMPDIR:-/tmp}/cc-clip-ssh-config.XXXXXX")
	trap 'rm -f "$TMP_FILE"' EXIT HUP INT TERM

	awk '
	function flush_buffer(    i) {
		for (i = 1; i <= buffered_count; i++) {
			print buffered[i]
		}
		buffered_count = 0
	}
	function begin_block(kind) {
		skip = 1
		block_kind = kind
		buffered_count = 0
		buffered[++buffered_count] = $0
	}
	function end_matches(line) {
		if (block_kind == "setenv" && line == "# <<< cc-clip SetEnv (do not edit) <<<") return 1
		if (block_kind == "legacy" && index(line, "# <<< cc-clip managed host: ") == 1) return 1
		return 0
	}
	{
		raw = $0
		line = raw
		sub(/^[[:space:]]+/, "", line)
		if (skip) {
			buffered[++buffered_count] = raw
			if (end_matches(line)) {
				skip = 0
				block_kind = ""
				buffered_count = 0
			}
			next
		}
		if (line == "# >>> cc-clip SetEnv (do not edit) >>>") {
			begin_block("setenv")
			next
		}
		if (index(line, "# >>> cc-clip managed host: ") == 1) {
			begin_block("legacy")
			next
		}
		print raw
	}
	END {
		if (skip) {
			flush_buffer()
		}
	}
	' "$SSH_CONFIG" >"$TMP_FILE"

	mv "$TMP_FILE" "$SSH_CONFIG"
	trap - EXIT HUP INT TERM
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
	return 1
}

while [ $# -gt 0 ]; do
	case "$1" in
		--install-dir)
			require_arg "$@"
			INSTALL_DIR=$2
			shift 2
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

log "WARNING: this script permanently removes local cc-clip installation and state."
log "Symlinked ~/.ssh/config files are preserved with a warning."
log

log "[1/4] Uninstalling local cc-clip pieces..."
run_cc_clip_if_present "service uninstall" service uninstall
run_cc_clip_if_present "local Codex cleanup" uninstall --codex
run_cc_clip_if_present "local xclip shim cleanup" uninstall --target xclip --path "$INSTALL_DIR"
run_cc_clip_if_present "local wl-paste shim cleanup" uninstall --target wl-paste --path "$INSTALL_DIR"
cleanup_launchd
remove_managed_file "$INSTALL_DIR/xclip" "cc-clip" "$INSTALL_DIR/xclip"
remove_managed_file "$INSTALL_DIR/wl-paste" "cc-clip" "$INSTALL_DIR/wl-paste"

log "[2/4] Removing local binary..."
rm -f "$INSTALL_DIR/cc-clip"

log "[3/4] Cleaning local SSH config..."
remove_managed_ssh_config_blocks

log "[4/4] Removing local state..."
rm -rf "$HOME/.cache/cc-clip"

log
log "Local purge complete."
log "  install dir: $INSTALL_DIR"
log "  cache: removed"
