#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)

HOST=""
INSTALL_DIR=${CC_CLIP_INSTALL_DIR:-"$HOME/.local/bin"}
FORCE_SHARED=0

usage() {
	cat <<EOF
Usage: $(basename "$0") --host HOST [options]

Dangerous full purge for cc-clip on both remote and local sides.

Options:
  --host HOST              SSH host alias to clean
  --install-dir DIR        Local install dir to clean (default: \$HOME/.local/bin)
  --force-shared           Proceed even if other laptops are still registered
                           with the remote peer registry (DESTRUCTIVE)
  -h, --help               Show this help

Notes:
  - This script is intentionally destructive. It removes all cc-clip files
    and state from the target remote Unix account, then removes all local
    cc-clip files and state from this machine.
  - Symlinked remote rc/config files and a symlinked local ~/.ssh/config are
    preserved with a warning instead of being rewritten.
  - Use this only when tearing down cc-clip completely.
  - SAFETY: if multiple laptops share the same remote Unix account, this
    script first queries the remote peer registry via \`cc-clip peer list\`
    and REFUSES to run when other peers are still registered. Pass
    \`--force-shared\` to override (you will break those laptops' clipboard
    until they reinstall). The recommended path on a shared account is
    \`cc-clip uninstall --host HOST --peer\` per laptop; the last peer's
    release cleans up the shared assets on its own.
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

remove_remote_cc_clip() {
	ssh -- "$HOST" /bin/sh <<'EOF'
set -eu

remove_if_contains() {
	PATHNAME=$1
	MARKER=$2
	if [ -f "$PATHNAME" ] && grep -q "$MARKER" "$PATHNAME" 2>/dev/null; then
		rm -f "$PATHNAME"
	fi
}

remove_marked_block() {
	TARGET=$1
	START_LINE=$2
	END_PREFIX=$3

	if [ -L "$TARGET" ]; then
		printf 'warning: skipping %s because it is a symlink\n' "$TARGET" >&2
		return 0
	fi
	if [ ! -f "$TARGET" ]; then
		return 0
	fi

	TMP_FILE=$(mktemp "${TMPDIR:-/tmp}/cc-clip-remote.XXXXXX") || return 0
	# Clean up on any abort: `set -eu` here means an awk/mv failure would
	# otherwise leave the temp file behind AND (worse) a half-written
	# redirect could already have truncated $TMP_FILE. Chain awk->mv with
	# `&&` below so we NEVER overwrite $TARGET from a failed awk, and use a
	# trap so we reap the temp file on every exit path.
	trap 'rm -f "$TMP_FILE"' EXIT HUP INT TERM

	if ! awk -v start_line="$START_LINE" -v end_prefix="$END_PREFIX" '
	function flush_buffer(    i) {
		for (i = 1; i <= buffered_count; i++) {
			print buffered[i]
		}
		buffered_count = 0
	}
	{
		raw = $0
		line = raw
		sub(/^[[:space:]]+/, "", line)
		if (skip) {
			buffered[++buffered_count] = raw
			if (line == end_prefix || index(line, end_prefix) == 1) {
				skip = 0
				buffered_count = 0
			}
			next
		}
		if (line == start_line || index(line, start_line) == 1) {
			skip = 1
			buffered_count = 0
			buffered[++buffered_count] = line
			next
		}
		print raw
	}
	END {
		if (skip) {
			flush_buffer()
		}
	}
	' "$TARGET" >"$TMP_FILE"; then
		printf 'warning: awk failed while rewriting %s; leaving file unchanged\n' "$TARGET" >&2
		rm -f "$TMP_FILE"
		trap - EXIT HUP INT TERM
		return 0
	fi

	if ! mv "$TMP_FILE" "$TARGET"; then
		printf 'warning: mv failed while replacing %s; leaving file unchanged\n' "$TARGET" >&2
		rm -f "$TMP_FILE"
		trap - EXIT HUP INT TERM
		return 0
	fi
	trap - EXIT HUP INT TERM
}

stop_pid_file() {
	PID_FILE=$1
	PATTERN=$2

	if [ ! -f "$PID_FILE" ]; then
		return 0
	fi

	PID=$(head -n 1 "$PID_FILE" 2>/dev/null | tr -d '\r')
	if [ -n "$PID" ] && ps -p "$PID" -o args= 2>/dev/null | grep -F "$PATTERN" >/dev/null 2>&1; then
		kill "$PID" >/dev/null 2>&1 || true
		sleep 1
		if ps -p "$PID" -o args= 2>/dev/null | grep -F "$PATTERN" >/dev/null 2>&1; then
			kill -9 "$PID" >/dev/null 2>&1 || true
		fi
	fi

	rm -f "$PID_FILE"
}

if [ -d "$HOME/.cache/cc-clip" ]; then
	find "$HOME/.cache/cc-clip" -type f \( -name bridge.pid -o -name xvfb.pid \) -print 2>/dev/null | while IFS= read -r pid_file; do
		case "$pid_file" in
			*/bridge.pid) stop_pid_file "$pid_file" "cc-clip x11-bridge" ;;
			*/xvfb.pid) stop_pid_file "$pid_file" "Xvfb" ;;
		esac
	done
fi

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
rm -f "$HOME/.local/bin/claude.cc-clip-bak"

remove_marked_block "$HOME/.bashrc" "# >>> cc-clip PATH (do not edit) >>>" "# <<< cc-clip PATH (do not edit) <<<"
remove_marked_block "$HOME/.bashrc" "# >>> cc-clip Codex DISPLAY (do not edit) >>>" "# <<< cc-clip Codex DISPLAY (do not edit) <<<"
remove_marked_block "$HOME/.zshrc" "# >>> cc-clip PATH (do not edit) >>>" "# <<< cc-clip PATH (do not edit) <<<"
remove_marked_block "$HOME/.zshrc" "# >>> cc-clip Codex DISPLAY (do not edit) >>>" "# <<< cc-clip Codex DISPLAY (do not edit) <<<"
remove_marked_block "$HOME/.codex/config.toml" "# >>> cc-clip notify (do not edit) >>>" "# <<< cc-clip notify (do not edit) <<<"

rm -rf "$HOME/.cache/cc-clip"
EOF
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
		--force-shared)
			FORCE_SHARED=1
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

# Guard against an operator passing a value that would look like an option
# to ssh itself (e.g. `--host -oProxyCommand=…`). `ssh -- host` is the
# canonical way to stop ssh from re-parsing; doing the check here ALSO means
# `ssh` never sees it in the first place, which is easier to audit.
case "$HOST" in
	-*) die "refusing --host value starting with '-': $HOST" ;;
esac

# Multi-peer safety: if other laptops are still registered with the remote
# peer registry, this script's blanket purge would break their clipboard
# until they reinstall. Query the REMOTE registry (`cc-clip peer list` runs
# against the local registry on whichever host invokes it, so we ssh to
# $HOST and run it there) and refuse unless the operator opts in via
# --force-shared. Mirrors `countRemoteOtherPeersOverSSH` in cmd/cc-clip.
#
# Fail-safe: an unreadable peer count (cc-clip absent on remote, ssh down,
# malformed JSON) is treated as "other peers may exist" — we error out
# rather than silently proceed.
if [ "$FORCE_SHARED" -eq 0 ]; then
	if ! PEER_JSON=$(ssh -o BatchMode=yes -- "$HOST" '~/.local/bin/cc-clip peer list' 2>/dev/null); then
		die "could not query remote peer registry on '$HOST' via ssh. Re-run with --force-shared if you have already verified no other laptops use this remote account."
	fi
	# Count entries by counting "peer_id" keys in the JSON array. jq is
	# preferred when available; otherwise grep -c is exact for the JSON the
	# Go side emits (json.Marshal produces a single line, no embedded
	# `"peer_id"` substrings appear in any other field).
	if command -v jq >/dev/null 2>&1; then
		PEER_COUNT=$(printf '%s' "$PEER_JSON" | jq 'length' 2>/dev/null) || PEER_COUNT=""
	else
		PEER_COUNT=$(printf '%s' "$PEER_JSON" | grep -o '"peer_id"' 2>/dev/null | wc -l | tr -d ' ' || true)
	fi
	if [ -z "$PEER_COUNT" ] || ! [ "$PEER_COUNT" -ge 0 ] 2>/dev/null; then
		die "could not count peers in remote registry output. Re-run with --force-shared if you accept the risk."
	fi
	# Count > 1 means at least one peer besides this workstation exists.
	# Count == 1 is treated as "only us" (best the shell can do without
	# parsing local identity files); count == 0 means already clean.
	if [ "$PEER_COUNT" -gt 1 ]; then
		die "remote peer registry on '$HOST' lists $PEER_COUNT peers — running this script would break up to $((PEER_COUNT - 1)) other laptop(s). Run 'cc-clip uninstall --host $HOST --peer' on each laptop instead, or pass --force-shared to override."
	fi
fi

printf '%s\n' "WARNING: this script permanently removes cc-clip installation and state"
printf '%s\n' "from remote host '$HOST' and from this machine (except preserved symlinked config files)."
printf '%s\n' "If other laptops share this remote Unix account, their clipboard + notifications"
printf '%s\n' "will break until they reinstall. Use 'cc-clip uninstall --host $HOST --peer' for"
printf '%s\n' "the multi-peer-safe path instead; it only deletes shared assets when no other"
printf '%s\n' "peers remain in the remote registry."
printf '\n'

printf '%s\n' "[1/2] Cleaning remote side..."
remove_remote_cc_clip
printf '%s\n' "Remote purge complete."

printf '%s\n' "[2/2] Cleaning local side..."
"$SCRIPT_DIR/uninstall-local.sh" --install-dir "$INSTALL_DIR"

printf '\n%s\n' "Full uninstall complete."
printf '  host: %s\n' "$HOST"
printf '  install dir: %s\n' "$INSTALL_DIR"
