package shim

const shellEntryTemplate = `#!/usr/bin/env bash
# cc-clip shell entry — exports peer-scoped environment for interactive SSH sessions

set -euo pipefail

_cc_peer="${1:-}"
_cc_port="${2:-${CC_CLIP_PORT:-18339}}"
_cc_label="${3:-${_cc_peer}}"

if [ -z "$_cc_peer" ]; then
    echo "cc-clip-shell-enter: peer id is required" >&2
    exit 1
fi

export CC_CLIP_PEER="$_cc_peer"
export CC_CLIP_PEER_LABEL="$_cc_label"
export CC_CLIP_PORT="$_cc_port"
export CC_CLIP_STATE_DIR="${HOME}/.cache/cc-clip/peers/${_cc_peer}"

# Start an interactive shell so ~/.bashrc or ~/.zshrc loads the managed
# PATH and DISPLAY blocks. A login shell can skip those files on bash hosts.
# Use -i to ensure PS1 is set before sourcing rc files — some .bashrc files
# guard with [ -z "$PS1" ] && return which would skip our managed blocks.
exec "${SHELL:-/bin/bash}" -i
`

func ShellEntryScript() string {
	return shellEntryTemplate
}
