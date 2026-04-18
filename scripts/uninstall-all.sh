#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)

HOST=""
INSTALL_DIR=${CC_CLIP_INSTALL_DIR:-"$HOME/.local/bin"}
PRE_CC_CLIP_BIN=${CC_CLIP_BIN:-}
PEER_ID=""
KEEP_CACHE=0
KEEP_PEER_IDENTITY=0

usage() {
	cat <<EOF
Usage: $(basename "$0") --host HOST [options]

Uninstall cc-clip from both remote and local sides.

Options:
  --host HOST              SSH host alias to clean
  --install-dir DIR        Local install dir to clean (default: \$HOME/.local/bin)
  --cc-clip-bin PATH       Existing local cc-clip binary to use for remote cleanup
  --peer-id ID             Explicit local peer id for remote lease cleanup
  --keep-cache             Keep ~/.cache/cc-clip after local uninstall
  --keep-peer-identity     Preserve local-peer-id and local-peer-label after local uninstall
  -h, --help               Show this help

Notes:
  - This runs remote cleanup first, then local cleanup.
  - If you omit --peer-id, the remote cleanup reuses ~/.cache/cc-clip/local-peer-id.
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
		--peer-id)
			require_arg "$@"
			PEER_ID=$2
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
			die "unknown option: $1"
			;;
	esac
done

[ -n "$HOST" ] || die "--host is required"

printf '%s\n' "[1/2] Cleaning remote side..."
if [ -n "$PRE_CC_CLIP_BIN" ]; then
	if [ -n "$PEER_ID" ]; then
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --cc-clip-bin "$PRE_CC_CLIP_BIN" --peer-id "$PEER_ID" --host "$HOST"
	else
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --cc-clip-bin "$PRE_CC_CLIP_BIN" --host "$HOST"
	fi
else
	if [ -n "$PEER_ID" ]; then
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --peer-id "$PEER_ID" --host "$HOST"
	else
		"$SCRIPT_DIR/reinstall-remote.sh" --cleanup-only --host "$HOST"
	fi
fi

printf '%s\n' "[2/2] Cleaning local side..."
if [ "$KEEP_CACHE" -eq 1 ] && [ "$KEEP_PEER_IDENTITY" -eq 1 ]; then
	"$SCRIPT_DIR/uninstall-local.sh" --install-dir "$INSTALL_DIR" --keep-cache --keep-peer-identity
elif [ "$KEEP_CACHE" -eq 1 ]; then
	"$SCRIPT_DIR/uninstall-local.sh" --install-dir "$INSTALL_DIR" --keep-cache
elif [ "$KEEP_PEER_IDENTITY" -eq 1 ]; then
	"$SCRIPT_DIR/uninstall-local.sh" --install-dir "$INSTALL_DIR" --keep-peer-identity
else
	"$SCRIPT_DIR/uninstall-local.sh" --install-dir "$INSTALL_DIR"
fi

printf '\n%s\n' "Full uninstall complete."
printf '  host: %s\n' "$HOST"
printf '  install dir: %s\n' "$INSTALL_DIR"
