#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

HOST=""
PORT=18339
CC_CLIP_BIN=${CC_CLIP_BIN:-}
PEER_ID=""
ENABLE_CODEX=0
CLEANUP_ONLY=0
INSTALL_ONLY=0

usage() {
	cat <<EOF
Usage: $(basename "$0") --host HOST [options]

Fully clean and/or reinstall cc-clip on a remote host.

Options:
  --host HOST              SSH host alias to manage
  --port PORT              Local daemon port to use for setup (default: 18339)
  --cc-clip-bin PATH       Local cc-clip binary to invoke
  --peer-id ID             Explicit local peer id (default: ~/.cache/cc-clip/local-peer-id)
  --codex                  Reinstall with Codex support
  --cleanup-only           Only remove remote/SSH-managed state
  --install-only           Only run setup/install
  -h, --help               Show this help

Notes:
  - Cleanup removes PATH/Codex markers, peer lease, managed SSH config, tunnel
    state, remote hook/wrapper files, and the remote cc-clip binary.
  - Install runs "cc-clip setup HOST" with the selected port and optional --codex.
EOF
}

log() {
	printf '%s\n' "$*"
}

warn() {
	printf 'warning: %s\n' "$*" >&2
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

require_arg() {
	if [ $# -lt 2 ] || [ -z "$2" ]; then
		die "$1 requires a value"
	fi
}

resolve_cc_clip_bin() {
	if [ -n "$CC_CLIP_BIN" ] && [ -x "$CC_CLIP_BIN" ]; then
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

load_peer_id() {
	if [ -n "$PEER_ID" ]; then
		printf '%s\n' "$PEER_ID"
		return 0
	fi
	PEER_FILE="$HOME/.cache/cc-clip/local-peer-id"
	if [ -f "$PEER_FILE" ]; then
		head -n 1 "$PEER_FILE" | tr -d '\r'
		return 0
	fi
	return 1
}

run_cleanup_command() {
	DESC=$1
	shift
	if "$LOCAL_CC_CLIP" "$@"; then
		log "  $DESC: ok"
		return 0
	fi
	warn "$DESC failed and was ignored"
	CLEANUP_WARNINGS=1
	return 0
}

remote_managed_file_cleanup() {
	if ssh "$HOST" /bin/sh <<'EOF'
set -eu

remove_if_contains() {
	PATHNAME=$1
	MARKER=$2
	if [ -f "$PATHNAME" ] && grep -q "$MARKER" "$PATHNAME" 2>/dev/null; then
		rm -f "$PATHNAME"
	fi
}

remove_if_contains "$HOME/.local/bin/xclip" "cc-clip"
remove_if_contains "$HOME/.local/bin/wl-paste" "cc-clip"
remove_if_contains "$HOME/.local/bin/cc-clip-hook" "cc-clip-hook"
remove_if_contains "$HOME/.local/bin/clipcc" "cc-clip clipcc wrapper"

if [ -f "$HOME/.local/bin/claude" ] && grep -q "cc-clip claude wrapper" "$HOME/.local/bin/claude" 2>/dev/null; then
	if [ -f "$HOME/.local/bin/claude.cc-clip-bak" ]; then
		mv -f "$HOME/.local/bin/claude.cc-clip-bak" "$HOME/.local/bin/claude"
	else
		rm -f "$HOME/.local/bin/claude"
	fi
fi

rm -f "$HOME/.local/bin/cc-clip"
rm -f \
	"$HOME/.cache/cc-clip/deploy.json" \
	"$HOME/.cache/cc-clip/session.token" \
	"$HOME/.cache/cc-clip/session.id" \
	"$HOME/.cache/cc-clip/notify.nonce" \
	"$HOME/.cache/cc-clip/notify-health.log"

if [ -f "$HOME/.codex/config.toml" ]; then
	sed -i.cc-clip-bak '/# >>> cc-clip notify (do not edit) >>>/,/# <<< cc-clip notify (do not edit) <<</d' "$HOME/.codex/config.toml" 2>/dev/null || true
	rm -f "$HOME/.codex/config.toml.cc-clip-bak"
fi
EOF
	then
		log "  remote managed file cleanup: ok"
		return 0
	fi
	warn "remote managed file cleanup failed and was ignored"
	CLEANUP_WARNINGS=1
	return 0
}

run_cleanup() {
	REMOTE_PEER_ID=$(load_peer_id) || die "cannot find local peer id; pass --peer-id explicitly or restore ~/.cache/cc-clip/local-peer-id first"

	log "[1/2] Cleaning remote state on $HOST..."
	run_cleanup_command "remote Codex cleanup" uninstall --codex --host "$HOST"
	run_cleanup_command "remote PATH marker cleanup" uninstall --host "$HOST"
	run_cleanup_command "remote peer + SSH config cleanup" uninstall --peer "$REMOTE_PEER_ID" --host "$HOST"
	remote_managed_file_cleanup
}

run_install() {
	log "[2/2] Reinstalling remote state on $HOST..."
	if [ "$ENABLE_CODEX" -eq 1 ]; then
		"$LOCAL_CC_CLIP" setup "$HOST" --port "$PORT" --codex
	else
		"$LOCAL_CC_CLIP" setup "$HOST" --port "$PORT"
	fi
}

while [ $# -gt 0 ]; do
	case "$1" in
		--host)
			require_arg "$@"
			HOST=$2
			shift 2
			;;
		--port)
			require_arg "$@"
			PORT=$2
			shift 2
			;;
		--cc-clip-bin)
			require_arg "$@"
			CC_CLIP_BIN=$2
			shift 2
			;;
		--peer-id)
			require_arg "$@"
			PEER_ID=$2
			shift 2
			;;
		--codex)
			ENABLE_CODEX=1
			shift
			;;
		--cleanup-only)
			CLEANUP_ONLY=1
			shift
			;;
		--install-only)
			INSTALL_ONLY=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			die "unknown option: $1"
			;;
	esac
done

[ -n "$HOST" ] || die "--host is required"
[ "$CLEANUP_ONLY" -eq 0 ] || [ "$INSTALL_ONLY" -eq 0 ] || die "--cleanup-only and --install-only are mutually exclusive"

LOCAL_CC_CLIP=$(resolve_cc_clip_bin) || die "cannot find a usable cc-clip binary; build this repo or install cc-clip first"
CLEANUP_WARNINGS=0

if [ "$INSTALL_ONLY" -eq 0 ]; then
	run_cleanup
fi

if [ "$CLEANUP_ONLY" -eq 0 ]; then
	run_install
fi

log
if [ "$CLEANUP_WARNINGS" -eq 1 ]; then
	log "Remote script completed with cleanup warnings."
else
	log "Remote script completed."
fi
log "  host: $HOST"
log "  port: $PORT"
