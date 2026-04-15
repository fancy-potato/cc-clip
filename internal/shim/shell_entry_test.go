package shim

import (
	"strings"
	"testing"
)

func TestShellEntryScriptStartsInteractiveShell(t *testing.T) {
	got := ShellEntryScript()

	if !strings.Contains(got, `exec "${SHELL:-/bin/bash}" -i`) {
		t.Fatalf("expected shell entry script to start an interactive shell, got %q", got)
	}
	if strings.Contains(got, `exec "${SHELL:-/bin/bash}" -l`) {
		t.Fatalf("shell entry script should not start a login shell, got %q", got)
	}
}

func TestShellEntryScriptExportsPeerScopedState(t *testing.T) {
	got := ShellEntryScript()

	for _, needle := range []string{
		`export CC_CLIP_PEER="$_cc_peer"`,
		`export CC_CLIP_PEER_LABEL="$_cc_label"`,
		`export CC_CLIP_PORT="$_cc_port"`,
		`export CC_CLIP_STATE_DIR="${HOME}/.cache/cc-clip/peers/${_cc_peer}"`,
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("expected shell entry script to contain %q, got %q", needle, got)
		}
	}
}
