package shim

import "fmt"

// hookTemplate takes three %d substitutions: the default port (used in three
// places, both guarded below) — once as the initial value for
// _CC_CLIP_PORT, once as the rejection fallback when CC_CLIP_PORT
// contains non-digits, and once as the numeric-range fallback when the
// value is outside the TCP port range.
const hookTemplate = `#!/usr/bin/env bash
# cc-clip-hook — Claude Code hook bridge
# Reads hook JSON from stdin, forwards to cc-clip daemon via tunnel

set -euo pipefail

_CC_CLIP_PORT="${CC_CLIP_PORT:-%d}"
# Reject any non-numeric CC_CLIP_PORT. An attacker able to seed CC_CLIP_PORT
# via the managed SSH SetEnv block (or an operator typo) could otherwise
# splice arbitrary URL path components — e.g. CC_CLIP_PORT=18339/attacker.example/x
# would redirect the notification POST to http://127.0.0.1:18339/attacker.example/x/notify,
# and CC_CLIP_PORT=$(cmd) would execute a subshell through the later URL
# interpolation. Fall back to the template-embedded default port so the
# hook remains fire-and-forget rather than failing outright.
case "$_CC_CLIP_PORT" in
	''|*[!0-9]*) _CC_CLIP_PORT=%d ;;
esac
if [ "$_CC_CLIP_PORT" -lt 1 ] || [ "$_CC_CLIP_PORT" -gt 65535 ]; then
	_CC_CLIP_PORT=%d
fi
_CC_CLIP_STATE_DIR="${CC_CLIP_STATE_DIR:-${HOME}/.cache/cc-clip}"
# Reject relative or path-traversal CC_CLIP_STATE_DIR values. A relative
# path would resolve against PWD at hook-run time (unpredictable - hooks
# fire under whatever cwd Claude Code was started from) and a dot-dot
# path component could be weaponized by an attacker able to plant a
# pre-resolved prefix in the managed SSH SetEnv block to read notify.nonce
# from outside the per-peer state dir. Absolute paths (including /tmp for
# tests) remain valid; the sharper attack surface is narrower than the
# review feared because a shared-account peer already has HOME access
# and can edit the legitimate nonce file anyway.
#
# The glob pattern checks are explicit path-segment matches so a legitimate
# directory name containing ".." (e.g. /home/a..b/.cache/cc-clip) is NOT
# falsely rejected — only a true ".." path component is.
case "$_CC_CLIP_STATE_DIR" in
	/*)
		case "$_CC_CLIP_STATE_DIR" in
			*/../*|*/..|../*)
				_CC_CLIP_STATE_DIR="${HOME}/.cache/cc-clip"
				;;
		esac ;;
	*)
		_CC_CLIP_STATE_DIR="${HOME}/.cache/cc-clip"
		;;
esac
_CC_CLIP_NONCE_FILE="${_CC_CLIP_STATE_DIR}/notify.nonce"
# Fallback chain is deliberately stable across invocations: every branch
# except the final "unknown" returns the same value on a given host, so
# notifications from the same remote carry the same _cc_clip_host and
# look like one sender in the recipient UI. An earlier "unknown-$$" literal
# embedded the hook script's PID, making each invocation's alias unique
# and thrashing the display name on hosts where all three probes failed.
_CC_CLIP_HOST_ALIAS="${CC_CLIP_HOST_ALIAS:-$(hostname -s 2>/dev/null || hostname 2>/dev/null || uname -n 2>/dev/null || echo unknown)}"
_CC_CLIP_HEALTH_FILE="${_CC_CLIP_STATE_DIR}/notify-health.log"
# _CC_CLIP_STRICT accepts ONLY the literal string "1" as the truthy value.
# Deliberately strict: runRemoteNotificationHealthProbe in cmd/cc-clip/main.go
# invokes the hook with CC_CLIP_STRICT=1 to validate end-to-end reachability
# and parses the HookHealthFailurePrefix constant (defined in Go) from
# stdout to surface failures. Accepting other truthy values ("true", "yes",
# "on", …) would silently widen the contract surface and risk operators
# enabling strict mode in steady-state production (the default is fire-and-
# forget so a remote daemon hiccup never blocks Claude Code).
_CC_CLIP_STRICT="${CC_CLIP_STRICT:-0}"
_curl_err_file=""
_curl_err=""

# Consolidated health-log directory creation. The three append sites
# below (nonce-empty, cat-rc warn, http-failure) previously each
# duplicated this mkdir call; the helper makes the "always best-effort
# before append" invariant obvious and reduces diff surface if the path
# resolution ever needs to change.
_cc_clip_ensure_health_dir() {
	mkdir -p "$(dirname -- "$_CC_CLIP_HEALTH_FILE")" 2>/dev/null || true
}

_nonce=""
if [ -f "$_CC_CLIP_NONCE_FILE" ]; then
	# Command substitution already strips trailing LF; trim a trailing CR so a
	# hand-edited CRLF nonce file doesn't corrupt the Authorization header.
	_nonce=$(head -1 "$_CC_CLIP_NONCE_FILE")
	# Four percent signs below render as two percent signs (bash
	# longest-match suffix strip). fmt.Sprintf collapses each pair of
	# percent signs to one, so the doubling is required to emit bash
	# longest-match. Writing only two percent signs here would degrade
	# to shortest-match (equivalent for a single trailing CR today,
	# but a silent footgun for any future longest-match pattern added).
	_nonce=${_nonce%%%%$'\r'}
	# Defense in depth: a mid-line CR or NUL in the nonce file (race with
	# rotation, hand-edit, or corruption) would slip past the trailing-CR
	# trim above and land directly in the Authorization: header, where CR
	# can split the HTTP request and NUL can terminate it short on some
	# libcurls. Strip those smuggling characters using bash-native
	# parameter expansion (no reliance on 'tr', which is not always on the
	# hook's minimal PATH). The daemon's legitimate 64-char hex nonces
	# contain none of these bytes, so this is a silent no-op for them.
	_nonce=${_nonce//$'\r'/}
	_nonce=${_nonce//$'\n'/}
	_nonce=${_nonce//$'\0'/}
	_nonce=${_nonce//$'\t'/}
	# If the sanitized nonce is empty (all-control-char file or empty
	# file), drop the request and log. An empty Authorization: Bearer
	# header would be rejected by the daemon with 401 anyway, but logging
	# the specific "nonce missing" reason here is more useful.
	if [ -z "$_nonce" ]; then
		_cc_clip_ensure_health_dir
		echo "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) FAIL nonce-empty-or-corrupt" >> "$_CC_CLIP_HEALTH_FILE" 2>/dev/null || true
		if [ "$_CC_CLIP_STRICT" = "1" ]; then
			echo "cc-clip-hook health probe failed: http=000 (nonce file empty or corrupt)"
			exit 1
		fi
		exit 0
	fi
fi

# P3-F: "cat || true" rather than bare "cat". Under set -e, a cat that
# fails (stdin closed by the caller, EPIPE on the reader side, etc.)
# would otherwise abort the whole hook script before we had a chance to
# forward what the caller actually wrote. An empty payload is a legitimate
# no-op from Claude Code's side — the classifier on the daemon side
# tolerates it — so falling through with an empty $_payload is strictly
# safer than failing the hook and dropping the notification entirely.
#
# Capture cat's exit status so writer failures (EPIPE, closed stdin,
# partial write mid-stream) become observable. The previous "cat || true"
# silently absorbed non-zero statuses, so an operator watching the health
# log had no way to tell a truncated payload from a normally-empty hook
# event. Persist the exit status in a temp file rather than appending a
# marker string to stdout: hook JSON is arbitrary user/assistant text, so
# a literal "__cc_clip_cat_rc=" inside the payload would otherwise collide
# with the marker and truncate the forwarded body.
_cat_rc=0
_cat_rc_file=""
if command -v mktemp >/dev/null 2>&1; then
	_cat_rc_file=$(mktemp 2>/dev/null || true)
fi
_payload="$(
	set +e
	cat
	_cc_clip_cat_rc=$?
	if [ -n "${_cat_rc_file:-}" ]; then
		printf '%%s' "$_cc_clip_cat_rc" >"$_cat_rc_file"
	fi
	exit 0
)"
# The command substitution above strips a single trailing LF from cat's
# stdout. $_payload therefore matches what bash's <<< "$_payload" will emit
# (one trailing LF) exactly as before. Do not try to re-append the LF here.
if [ -n "$_cat_rc_file" ] && [ -f "$_cat_rc_file" ]; then
	if ! IFS= read -r _cat_rc <"$_cat_rc_file"; then
		_cat_rc=0
	fi
	rm -f "$_cat_rc_file" 2>/dev/null || true
fi
if [ "$_cat_rc" != "0" ]; then
	_cc_clip_ensure_health_dir
	echo "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) WARN cat-rc=$_cat_rc payload-bytes=${#_payload}" >> "$_CC_CLIP_HEALTH_FILE" 2>/dev/null || true
	if [ "$_CC_CLIP_STRICT" = "1" ]; then
		echo "cc-clip-hook health probe failed: http=000 (stdin reader exited rc=$_cat_rc; payload may be truncated)"
		exit 1
	fi
fi

# Host-alias injection requires python3. If python3 is missing or errors,
# we post the payload WITHOUT the _cc_clip_host field — the classifier
# treats it as optional. This means host attribution is lost on such
# remotes, but we prefer forwarding the notification over blocking it.
# TestHookScriptHostAliasMissingPython3FallbackPreservesPayload pins this
# degraded behavior so a future refactor can't silently drop notifications.
_payload=$(CC_CLIP_HOST_ALIAS="$_CC_CLIP_HOST_ALIAS" python3 -c '
import os, sys, json
d = json.load(sys.stdin)
d["_cc_clip_host"] = os.environ.get("CC_CLIP_HOST_ALIAS", "")
json.dump(d, sys.stdout)
' <<<"$_payload" 2>/dev/null || echo "$_payload")

# Note: do NOT pass -f here. With -f, curl exits non-zero on 4xx/5xx, the
# ||"000" branch wins, and %%{http_code} is discarded — so operators and the
# strict-mode probe would see "000" regardless of the real status. Use -s
# (quiet) + -w (print status) and rely on the printed code for classification.
if command -v mktemp >/dev/null 2>&1; then
	_curl_err_file=$(mktemp 2>/dev/null || true)
fi
# Payload is streamed on stdin via --data-binary @- rather than passed as
# an argv element. argv is visible to any local user via /proc/<pid>/cmdline
# on Linux and ps -ww on macOS, which would leak the notification body
# (session IDs, hook-event contents, etc.) to unrelated processes on a
# shared remote. stdin is not exposed that way.
#
# Here-string trailing LF note: bash's <<< operator appends exactly one
# trailing LF to the expanded word. An empty $_payload therefore produces
# a 1-byte stdin (single LF), not 0 bytes. The /notify classifier
# tolerates both (empty and bare-LF bodies are parsed as the no-op hook
# event), so the LF is harmless. We call this out explicitly so a future
# refactor doesn't try to "fix" the LF and accidentally break the
# set -euo pipefail invariant — for example, piping a quieter emitter into
# curl without handling pipefail on the emitter upstream.
_http_code=$(curl -sS --connect-timeout 2 --max-time 5 -o /dev/null -w '%%{http_code}' -X POST \
	-H "Authorization: Bearer $_nonce" \
	-H "Content-Type: application/x-claude-hook" \
	-H "User-Agent: cc-clip-hook/0.1" \
	--data-binary @- \
	"http://127.0.0.1:${_CC_CLIP_PORT}/notify" \
	2>"${_curl_err_file:-/dev/null}" <<<"$_payload") || _http_code="000"
if [ -n "$_curl_err_file" ] && [ -f "$_curl_err_file" ]; then
	_curl_err=$(head -1 "$_curl_err_file" 2>/dev/null || true)
	rm -f "$_curl_err_file" 2>/dev/null || true
fi

if [ "$_http_code" != "204" ] && [ "$_http_code" != "200" ]; then
	# Ensure the parent dir exists before appending. Without this guard the
	# >> append silently fails when $_CC_CLIP_STATE_DIR is missing, so every
	# failure the operator is trying to diagnose disappears.
	_cc_clip_ensure_health_dir
	echo "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) FAIL http=$_http_code" >> "$_CC_CLIP_HEALTH_FILE" 2>/dev/null || true
	if [ "$_CC_CLIP_STRICT" = "1" ]; then
		# Exact wording pinned by TestHookScriptStrictModePropagatesHTTP500:
		# runRemoteNotificationHealthProbe in cmd/cc-clip/main.go parses this
		# stdout line to surface the failure. Do not reword without updating
		# both the test and the probe.
		#
		# Scrub $_curl_err to printable ASCII (plus tab/space) before
		# echoing. $_curl_err is the first stderr line from curl, which
		# a compromised remote or MITM could populate with ANSI / CSI /
		# OSC escape sequences that would rewrite the operator's terminal
		# when the strict-mode stdout is surfaced by
		# runRemoteNotificationHealthProbe. Stripping every byte outside
		# 0x20-0x7E (plus \t) eliminates that vector without losing the
		# diagnostic value: curl's real error strings are already printable
		# ASCII.
		if [ "$_http_code" = "000" ] && [ -n "$_curl_err" ]; then
			_curl_err_safe=""
			for ((_i=0; _i<${#_curl_err}; _i++)); do
				_ch=${_curl_err:_i:1}
				case "$_ch" in
					[[:print:]]|$'\t') _curl_err_safe="${_curl_err_safe}${_ch}" ;;
				esac
			done
			echo "cc-clip-hook health probe failed: http=$_http_code ($_curl_err_safe)"
		else
			echo "cc-clip-hook health probe failed: http=$_http_code"
		fi
		exit 1
	fi
fi

exit 0
`

// HookHealthFailurePrefix is the exact stdout prefix the cc-clip-hook
// script emits in strict mode when the POST to /notify returns a non-2xx
// status. The string is a contract between three places:
//
//  1. internal/shim/hook_template.go (the bash template that emits it),
//  2. cmd/cc-clip/main.go runRemoteNotificationHealthProbe (the Go probe
//     that wraps the surfaced stdout into a user-facing error), and
//  3. internal/shim/hook_template_test.go +
//     cmd/cc-clip/main_test.go (which assert the wording end-to-end).
//
// Pinning the prefix here means a future template refactor that changes
// the wording will fail TestHookScriptStrictModePrefixIsExportedConstant
// (which asserts the constant matches the rendered script) AND the
// probe-side test in main_test.go simultaneously, instead of silently
// drifting one half of the contract out of sync with the other.
const HookHealthFailurePrefix = "cc-clip-hook health probe failed: http="

// HookScript returns the cc-clip-hook bash script with the given port baked in.
// This script is installed to ~/.local/bin/cc-clip-hook on the remote. Claude Code
// hooks pipe JSON to stdin, which the script forwards to the cc-clip daemon via
// the SSH tunnel. Authentication uses the notification nonce (not the clipboard
// session token).
//
// port is clamped to the valid TCP range [1, 65535]. A caller-side bug that
// passes 0 (unset) or a negative value would otherwise bake "0" into the
// template's three fallback branches, so CC_CLIP_PORT validation could
// silently route traffic to port 0 (kernel-assigned, effectively useless).
// Values above 65535 are clamped to 65535 so the generated script still
// passes the template's own `-gt 65535` range check at runtime.
func HookScript(port int) string {
	if port < 1 {
		port = 1
	}
	if port > 65535 {
		port = 65535
	}
	// port is substituted three times: once as the `${CC_CLIP_PORT:-<port>}`
	// default, and once as the non-numeric-rejection fallback inside the
	// `case "$_CC_CLIP_PORT"` guard, and once as the out-of-range fallback.
	// All three MUST be the same template-embedded value — a mismatch would
	// let an attacker-controlled invalid CC_CLIP_PORT fall back to a
	// different port than the documented default.
	return fmt.Sprintf(hookTemplate, port, port, port)
}
