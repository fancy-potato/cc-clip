#!/bin/sh
# POSIX sh only — no bashisms (runs under dash on Debian)
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
	# Clean up on any abort: `set -eu` here means an awk failure would
	# otherwise leave the temp file behind. Use a trap so we reap the temp
	# file on every exit path.
	trap 'rm -f "$TMP_FILE"' EXIT HUP INT TERM

	# Write the rewritten output to the temp file first — NOT directly to
	# $TARGET — so a mid-awk failure never leaves $TARGET half-written. On
	# success we `cat "$TMP_FILE" > "$TARGET"` to truncate-and-write the
	# original file in place. That preserves $TARGET's inode, mode, owner,
	# and extended attributes exactly, which matters when the operator runs
	# this script under sudo against a user-owned rc file / config.toml —
	# a classic `mv temp target` would replace the user's file with
	# temp-file metadata (root-owned, mktemp-mode), silently breaking
	# subsequent shell startup.
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
			# Buffer the RAW (pre-trim) start-marker line so that on an
			# unterminated-block recovery via the END flush_buffer path
			# we reproduce the original leading whitespace verbatim. A
			# prior revision stored the trimmed line, which silently
			# ate indentation from only the start marker if the block
			# was missing its end sentinel.
			buffered[++buffered_count] = raw
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

	# Truncate-and-write the original file in place, preserving inode and
	# metadata. If this fails (disk full, permission), the original may be
	# partially written but its inode/owner/mode are still intact.
	if ! cat "$TMP_FILE" >"$TARGET"; then
		printf 'warning: failed to write %s in place; file may be partially updated\n' "$TARGET" >&2
		rm -f "$TMP_FILE"
		trap - EXIT HUP INT TERM
		return 0
	fi
	rm -f "$TMP_FILE"
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
	# Resolve cc-clip on the remote via PATH, falling back to ~/.local/bin
	# when the shell was invoked non-interactively (ssh's default command
	# shell often skips .bashrc, so `command -v cc-clip` can miss a
	# user-level install). This lets operators with a non-default
	# installation path still pass the peer-count guard.
	#
	# Capture stderr to a temp file so the operator sees actionable
	# diagnostics ("cc-clip not found on remote PATH or ~/.local/bin",
	# host key issues, BatchMode auth refusal, etc.) instead of the prior
	# "2>/dev/null" silent black hole. Preserve stderr on success too so
	# benign warnings (MOTD noise, host-key update chatter) are still
	# visible to operators.
	SSH_ERR=$(mktemp "${TMPDIR:-/tmp}/cc-clip-uninstall-all-ssh-err.XXXXXX") || die "mktemp failed"
	trap 'rm -f "$SSH_ERR"' EXIT HUP INT TERM
	if ! PEER_JSON=$(ssh -- "$HOST" 'if command -v cc-clip >/dev/null 2>&1; then cc-clip peer list; elif [ -x ~/.local/bin/cc-clip ]; then ~/.local/bin/cc-clip peer list; else echo "cc-clip not found on remote PATH or ~/.local/bin" >&2; exit 127; fi' 2>"$SSH_ERR"); then
		SSH_ERR_MSG=$(cat "$SSH_ERR" 2>/dev/null || true)
		if [ -n "$SSH_ERR_MSG" ]; then
			die "could not query remote peer registry on '$HOST' via ssh: $SSH_ERR_MSG. Re-run with --force-shared if you have already verified no other laptops use this remote account."
		fi
		die "could not query remote peer registry on '$HOST' via ssh. Re-run with --force-shared if you have already verified no other laptops use this remote account."
	fi
	# Even on success, surface any stderr diagnostics so operators see
	# "cc-clip not found" / deprecation warnings without having to rerun.
	if [ -s "$SSH_ERR" ]; then
		while IFS= read -r line; do
			printf 'note: remote stderr: %s\n' "$line" >&2
		done <"$SSH_ERR"
	fi
	# Count entries in the JSON array. jq is strongly preferred; when it
	# is missing we fall back to a python or awk parser rather than
	# fragile grep -o '"peer_id"', which is coupled to json.Marshal's
	# single-line layout and silently misreports if the Go encoder ever
	# emits indented JSON.
	PEER_COUNT=""
	SELF_PRESENT=0
	LOCAL_PEER_ID=""
	LOCAL_PEER_ID_FILE=$HOME/.cache/cc-clip/local-peer-id
	if [ -f "$LOCAL_PEER_ID_FILE" ]; then
		LOCAL_PEER_ID=$(head -n 1 "$LOCAL_PEER_ID_FILE" 2>/dev/null | tr -d '\r\n')
	fi
	if command -v jq >/dev/null 2>&1; then
		PEER_COUNT=$(printf '%s' "$PEER_JSON" | jq -er 'if type == "array" then length else error("not a JSON array") end' 2>/dev/null) || PEER_COUNT=""
		if [ -z "$PEER_COUNT" ]; then
			die "could not parse remote peer registry JSON via jq fallback. Re-run with --force-shared if you have already verified no other laptops use this remote account."
		fi
		if [ -n "$LOCAL_PEER_ID" ]; then
			if printf '%s' "$PEER_JSON" | jq -e --arg id "$LOCAL_PEER_ID" 'if type == "array" then any(.[]?; .peer_id == $id) else error("not a JSON array") end' >/dev/null 2>&1; then
				SELF_PRESENT=1
			fi
		fi
	elif command -v python3 >/dev/null 2>&1; then
		# Capture the python3 fallback's stderr to a temp file the same
		# way the ssh-err path above does, so `parse_error:…` diagnostics
		# (malformed JSON, non-array top level) are surfaced in the die
		# message instead of being discarded with `2>/dev/null`. Without
		# this, an operator seeing "could not count peers" had no way to
		# tell whether the remote registry returned broken JSON or
		# python3 itself was missing / crashed. Preserve exit code 2 as
		# the "parse error" signal so callers can still distinguish "bad
		# JSON" from "interpreter blew up".
		PY_ERR=$(mktemp "${TMPDIR:-/tmp}/cc-clip-uninstall-all-py-err.XXXXXX") || die "mktemp failed"
		# The python3 heredoc below is parsed verbatim by the interpreter
		# (single-quoted, shell does not touch it), so indentation must be
		# pure-spaces and consistent. Tabs would mix with the file's
		# tab-indented bash and trigger IndentationError BEFORE the script
		# can even start. Pinned by TestUninstallAllPythonFallbackUsesRealPython3.
		if PY_OUT=$(printf '%s' "$PEER_JSON" | LOCAL_PEER_ID="$LOCAL_PEER_ID" python3 -c '
import json, os, sys
try:
    data = json.load(sys.stdin)
except Exception as e:
    print("parse_error:%s" % e, file=sys.stderr)
    sys.exit(2)
if not isinstance(data, list):
    print("parse_error:not a JSON array", file=sys.stderr)
    sys.exit(2)
want = os.environ.get("LOCAL_PEER_ID", "")
self_present = 0
if want:
    for item in data:
        if isinstance(item, dict) and item.get("peer_id") == want:
            self_present = 1
            break
print("%d %d" % (len(data), self_present))
	' 2>"$PY_ERR"); then
			PY_EXIT=0
		else
			PY_EXIT=$?
		fi
		PY_ERR_MSG=$(cat "$PY_ERR" 2>/dev/null || true)
		rm -f "$PY_ERR"
		if [ "$PY_EXIT" -ne 0 ]; then
			if [ "$PY_EXIT" -eq 2 ]; then
				if [ -n "$PY_ERR_MSG" ]; then
					die "could not parse remote peer registry JSON via python3 fallback: $PY_ERR_MSG. Re-run with --force-shared if you have already verified no other laptops use this remote account."
				fi
				die "could not parse remote peer registry JSON via python3 fallback (no stderr captured). Re-run with --force-shared if you have already verified no other laptops use this remote account."
			fi
			if [ -n "$PY_ERR_MSG" ]; then
				die "python3 peer-count fallback failed (exit $PY_EXIT): $PY_ERR_MSG. Re-run with --force-shared if you have already verified no other laptops use this remote account."
			fi
			die "python3 peer-count fallback failed (exit $PY_EXIT, no stderr captured). Re-run with --force-shared if you have already verified no other laptops use this remote account."
		fi
		if [ -n "$PY_OUT" ]; then
			PEER_COUNT=${PY_OUT%% *}
			SELF_PRESENT=${PY_OUT##* }
		fi
	else
		die "neither jq nor python3 is available to parse the remote peer registry. Install jq (\`brew install jq\` on macOS, \`apt install jq\` on Debian/Ubuntu) and rerun, or pass --force-shared if you have already verified no other laptops use this remote account."
	fi
	if [ -z "$PEER_COUNT" ] || ! [ "$PEER_COUNT" -ge 0 ] 2>/dev/null; then
		die "could not count peers in remote registry output. Re-run with --force-shared if you accept the risk."
	fi
	# Safe to proceed only when the registry is empty OR the sole remaining
	# peer is provably this workstation. Anything ambiguous fails closed.
	if [ "$PEER_COUNT" -gt 0 ] && [ -z "$LOCAL_PEER_ID" ]; then
		die "remote peer registry on '$HOST' lists $PEER_COUNT peer(s), but this machine has no local peer identity to prove whether one is ours. Re-run with --force-shared only if you have already verified no other laptops use this remote account."
	fi
	if [ "$PEER_COUNT" -gt 0 ] && [ "$SELF_PRESENT" -ne 1 ]; then
		die "remote peer registry on '$HOST' lists $PEER_COUNT peer(s), but local peer id \"$LOCAL_PEER_ID\" is not present in the remote registry. Refusing to assume the sole peer is this workstation; re-run with --force-shared only if you have already verified no other laptops use this remote account."
	fi
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
