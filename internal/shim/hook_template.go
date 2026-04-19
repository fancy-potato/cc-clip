package shim

import "fmt"

const hookTemplate = `#!/usr/bin/env bash
# cc-clip-hook — Claude Code hook bridge
# Reads hook JSON from stdin, forwards to cc-clip daemon via tunnel

set -euo pipefail

_CC_CLIP_PORT="${CC_CLIP_PORT:-%d}"
_CC_CLIP_STATE_DIR="${CC_CLIP_STATE_DIR:-${HOME}/.cache/cc-clip}"
_CC_CLIP_NONCE_FILE="${_CC_CLIP_STATE_DIR}/notify.nonce"
# Fallback chain is deliberately stable across invocations: every branch
# except the final "unknown" returns the same value on a given host, so
# notifications from the same remote carry the same _cc_clip_host and
# look like one sender in the recipient UI. An earlier "unknown-$$" literal
# embedded the hook script's PID, making each invocation's alias unique
# and thrashing the display name on hosts where all three probes failed.
_CC_CLIP_HOST_ALIAS="${CC_CLIP_HOST_ALIAS:-$(hostname -s 2>/dev/null || hostname 2>/dev/null || uname -n 2>/dev/null || echo unknown)}"
_CC_CLIP_HEALTH_FILE="${_CC_CLIP_STATE_DIR}/notify-health.log"
_CC_CLIP_STRICT="${CC_CLIP_STRICT:-0}"
_curl_err_file=""
_curl_err=""

_nonce=""
if [ -f "$_CC_CLIP_NONCE_FILE" ]; then
	# Command substitution already strips trailing LF; trim a trailing CR so a
	# hand-edited CRLF nonce file doesn't corrupt the Authorization header.
	_nonce=$(head -1 "$_CC_CLIP_NONCE_FILE")
	_nonce=${_nonce%%$'\r'}
fi

_payload=$(cat)

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
_http_code=$(curl -sS --connect-timeout 2 --max-time 5 -o /dev/null -w '%%{http_code}' -X POST \
	-H "Authorization: Bearer $_nonce" \
	-H "Content-Type: application/x-claude-hook" \
	-H "User-Agent: cc-clip-hook/0.1" \
	--data-raw "$_payload" \
	"http://127.0.0.1:${_CC_CLIP_PORT}/notify" \
	2>"${_curl_err_file:-/dev/null}") || _http_code="000"
if [ -n "$_curl_err_file" ] && [ -f "$_curl_err_file" ]; then
	_curl_err=$(head -1 "$_curl_err_file" 2>/dev/null || true)
	rm -f "$_curl_err_file" 2>/dev/null || true
fi

if [ "$_http_code" != "204" ] && [ "$_http_code" != "200" ]; then
	# Ensure the parent dir exists before appending. Without this guard the
	# >> append silently fails when $_CC_CLIP_STATE_DIR is missing, so every
	# failure the operator is trying to diagnose disappears. mkdir -p is
	# idempotent and best-effort; if it fails we still try the append.
	mkdir -p "$(dirname -- "$_CC_CLIP_HEALTH_FILE")" 2>/dev/null || true
	echo "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) FAIL http=$_http_code" >> "$_CC_CLIP_HEALTH_FILE" 2>/dev/null || true
	if [ "$_CC_CLIP_STRICT" = "1" ]; then
		# Exact wording pinned by TestHookScriptStrictModePropagatesHTTP500:
		# runRemoteNotificationHealthProbe in cmd/cc-clip/main.go parses this
		# stdout line to surface the failure. Do not reword without updating
		# both the test and the probe.
		if [ "$_http_code" = "000" ] && [ -n "$_curl_err" ]; then
			echo "cc-clip-hook health probe failed: http=$_http_code ($_curl_err)"
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
func HookScript(port int) string {
	return fmt.Sprintf(hookTemplate, port)
}
