#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)

HOST=""
PORT=18339
INSTALL_DIR=${CC_CLIP_INSTALL_DIR:-"$HOME/.local/bin"}
PRE_CC_CLIP_BIN=${CC_CLIP_BIN:-}
ENABLE_CODEX=0
USE_RELEASE=0

usage() {
	cat <<EOF
Usage: $(basename "$0") --host HOST [options]

End-to-end reinstall for both sides:
  1. clean remote state
  2. reinstall local cc-clip
  3. run remote setup again

Options:
  --host HOST              SSH host alias to manage
  --port PORT              Daemon/service port (default: 18339)
  --install-dir DIR        Local install dir (default: \$HOME/.local/bin)
  --cc-clip-bin PATH       Existing local cc-clip binary to use for remote cleanup
  --codex                  Reinstall with Codex support
  --use-release            Reinstall local binary from scripts/install.sh
  -h, --help               Show this help
EOF
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
		--install-dir)
			require_arg "$@"
			INSTALL_DIR=$2
			shift 2
			;;
		--cc-clip-bin)
			require_arg "$@"
			PRE_CC_CLIP_BIN=$2
			shift 2
			;;
		--codex)
			ENABLE_CODEX=1
			shift
			;;
		--use-release)
			USE_RELEASE=1
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

printf '%s\n' "[1/3] Cleaning remote side..."
if [ -n "$PRE_CC_CLIP_BIN" ]; then
	if [ "$ENABLE_CODEX" -eq 1 ]; then
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --cc-clip-bin "$PRE_CC_CLIP_BIN" --host "$HOST" --port "$PORT" --codex
	else
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --cc-clip-bin "$PRE_CC_CLIP_BIN" --host "$HOST" --port "$PORT"
	fi
else
	if [ "$ENABLE_CODEX" -eq 1 ]; then
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --host "$HOST" --port "$PORT" --codex
	else
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --host "$HOST" --port "$PORT"
	fi
fi

printf '%s\n' "[2/3] Reinstalling local side..."
if [ "$USE_RELEASE" -eq 1 ]; then
	"$SCRIPT_DIR/reinstall-local.sh" --install-dir "$INSTALL_DIR" --port "$PORT" --use-release
else
	"$SCRIPT_DIR/reinstall-local.sh" --install-dir "$INSTALL_DIR" --port "$PORT"
fi

POST_CC_CLIP_BIN="$INSTALL_DIR/cc-clip"
[ -x "$POST_CC_CLIP_BIN" ] || die "expected rebuilt binary at $POST_CC_CLIP_BIN, but it is missing"

printf '%s\n' "[3/3] Reinstalling remote side..."
if [ "$ENABLE_CODEX" -eq 1 ]; then
	"$SCRIPT_DIR/reinstall-remote.sh" --install-only --cc-clip-bin "$POST_CC_CLIP_BIN" --host "$HOST" --port "$PORT" --codex
else
	"$SCRIPT_DIR/reinstall-remote.sh" --install-only --cc-clip-bin "$POST_CC_CLIP_BIN" --host "$HOST" --port "$PORT"
fi

printf '\n%s\n' "All-in-one reinstall complete."
printf '  host: %s\n' "$HOST"
printf '  binary: %s\n' "$POST_CC_CLIP_BIN"
