package shim

import "fmt"

const hookTemplate = `#!/usr/bin/env bash
# cc-clip-hook — Claude Code hook bridge
# Reads hook JSON from stdin, forwards to cc-clip daemon via tunnel

set -euo pipefail

_CC_CLIP_PORT="${CC_CLIP_PORT:-%d}"
_CC_CLIP_STATE_DIR="${CC_CLIP_STATE_DIR:-${HOME}/.cache/cc-clip}"
_CC_CLIP_NONCE_FILE="${_CC_CLIP_STATE_DIR}/notify.nonce"
_CC_CLIP_HOST_ALIAS="${CC_CLIP_HOST_ALIAS:-$(hostname -s)}"
_CC_CLIP_HEALTH_FILE="${_CC_CLIP_STATE_DIR}/notify-health.log"
_CC_CLIP_STRICT="${CC_CLIP_STRICT:-0}"

_nonce=""
if [ -f "$_CC_CLIP_NONCE_FILE" ]; then
	_nonce=$(head -1 "$_CC_CLIP_NONCE_FILE")
fi

_payload=$(cat)

# Host-alias injection requires python3. If python3 is missing or errors,
# we post the payload WITHOUT the _cc_clip_host field — the classifier
# treats it as optional. This means host attribution is lost on such
# remotes, but we prefer forwarding the notification over blocking it.
# TestHookScriptFallbackPreservesPayloadWithoutPython pins the degraded
# behavior so a future refactor can't silently drop notifications instead.
_payload=$(CC_CLIP_HOST_ALIAS="$_CC_CLIP_HOST_ALIAS" python3 -c '
import os, sys, json
d = json.load(sys.stdin)
d["_cc_clip_host"] = os.environ.get("CC_CLIP_HOST_ALIAS", "")
json.dump(d, sys.stdout)
' <<<"$_payload" 2>/dev/null || echo "$_payload")

_http_code=$(curl -sf --connect-timeout 2 --max-time 5 -o /dev/null -w '%%{http_code}' -X POST \
	-H "Authorization: Bearer $_nonce" \
	-H "Content-Type: application/x-claude-hook" \
	-H "User-Agent: cc-clip-hook/0.1" \
	-d "$_payload" \
	"http://127.0.0.1:${_CC_CLIP_PORT}/notify" \
	2>/dev/null) || _http_code="000"

if [ "$_http_code" != "204" ] && [ "$_http_code" != "200" ]; then
	echo "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) FAIL http=$_http_code" >> "$_CC_CLIP_HEALTH_FILE" 2>/dev/null || true
	if [ "$_CC_CLIP_STRICT" = "1" ]; then
		echo "cc-clip-hook health probe failed: http=$_http_code"
		exit 1
	fi
fi

exit 0
`

// HookScript returns the cc-clip-hook bash script with the given port baked in.
// This script is installed to ~/.local/bin/cc-clip-hook on the remote. Claude Code
// hooks pipe JSON to stdin, which the script forwards to the cc-clip daemon via
// the SSH tunnel. Authentication uses the notification nonce (not the clipboard
// session token).
func HookScript(port int) string {
	return fmt.Sprintf(hookTemplate, port)
}
