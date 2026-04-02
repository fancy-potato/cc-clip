package shim

import "fmt"

const claudeWrapperTemplate = `#!/usr/bin/env bash
# cc-clip claude wrapper — auto-inject notification hooks
# Installed by: cc-clip connect
# Remove with:  rm ~/.local/bin/claude

# Find the real claude binary (skip our own directory)
_REAL_CLAUDE=""
_SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
IFS=: read -ra _PATH_DIRS <<< "$PATH"
for _dir in "${_PATH_DIRS[@]}"; do
    [ "$_dir" = "$_SELF_DIR" ] && continue
    [ -x "$_dir/claude" ] && _REAL_CLAUDE="$_dir/claude" && break
done

if [ -z "$_REAL_CLAUDE" ]; then
    echo "cc-clip: real claude binary not found in PATH" >&2
    exit 1
fi

# Only inject hooks if cc-clip tunnel is alive
if curl -sf --connect-timeout 1 --max-time 2 "http://127.0.0.1:${CC_CLIP_PORT:-%d}/health" >/dev/null 2>&1; then
    exec "$_REAL_CLAUDE" --settings '{
  "hooks": {
    "Stop": [{"type":"command","command":"cc-clip-hook"}],
    "Notification": [{"type":"command","command":"cc-clip-hook"}]
  }
}' "$@"
else
    # Tunnel not available — run claude without hook injection
    exec "$_REAL_CLAUDE" "$@"
fi
`

// ClaudeWrapperScript returns the claude wrapper bash script with the
// given port baked in. This script is installed to ~/.local/bin/claude
// on the remote. When the cc-clip tunnel is alive, it injects Stop and
// Notification hooks via --settings so users don't need to manually
// configure hooks in ~/.claude/settings.json. When the tunnel is down,
// it transparently passes through to the real claude binary.
func ClaudeWrapperScript(port int) string {
	return fmt.Sprintf(claudeWrapperTemplate, port)
}
