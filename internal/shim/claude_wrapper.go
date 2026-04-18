package shim

import "fmt"

const claudeWrapperTemplate = `#!/usr/bin/env bash
# cc-clip clipcc wrapper — auto-inject notification hooks
# Installed by: cc-clip connect
# Remove with:  rm ~/.local/bin/clipcc

# Prefer the official claude launcher in the same directory so background
# upgrades that update ~/.local/bin/claude keep working.
_REAL_CLAUDE=""
_SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
_LOCAL_CLAUDE="$_SELF_DIR/claude"

if [ -x "$_LOCAL_CLAUDE" ]; then
    _REAL_CLAUDE="$_LOCAL_CLAUDE"
else
    # No sibling claude in our own bin dir — fall back to a PATH lookup.
    _REAL_CLAUDE="$(command -v claude || true)"
fi

if [ -z "$_REAL_CLAUDE" ]; then
    echo "cc-clip: real claude binary not found in PATH" >&2
    exit 1
fi

# Only inject hooks if cc-clip tunnel is alive
if curl -sf --connect-timeout 1 --max-time 2 "http://127.0.0.1:${CC_CLIP_PORT:-%d}/health" >/dev/null 2>&1; then
    exec "$_REAL_CLAUDE" --settings '{
  "hooks": {
    "Stop": [{"hooks":[{"type":"command","command":"cc-clip-hook"}]}],
    "Notification": [{"hooks":[{"type":"command","command":"cc-clip-hook"}]}]
  }
}' "$@"
else
    # Tunnel not available — run claude without hook injection
    exec "$_REAL_CLAUDE" "$@"
fi
`

// ClipCCWrapperScript returns the clipcc wrapper bash script with the
// given port baked in. This script is installed to ~/.local/bin/clipcc
// on the remote. When the cc-clip tunnel is alive, it injects Stop and
// Notification hooks via --settings. When the tunnel is down, it
// transparently passes through to the real claude binary.
func ClipCCWrapperScript(port int) string {
	return fmt.Sprintf(claudeWrapperTemplate, port)
}
