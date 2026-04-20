//go:build sshconfig_demo

package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSmokeRoundTrip is a deliberately verbose end-to-end check that
// exercises the apply / glob-reject / missing-block / idempotent /
// remove flow on a realistic ~/.ssh/config sample. It prints the
// before/after to test logs (visible with -v) so a contributor reading
// the test can see what the public API does without re-reading the
// implementation. Equivalent to a manual smoke run, executed in CI.
func TestSmokeRoundTrip(t *testing.T) {
	const original = `# my ssh config

Host srv-a
  HostName a.example.com
  User alice

Host srv-shared
  HostName shared.example.com
  User shareduser
  IdentityFile ~/.ssh/id_ed25519

Host *.example.com
  ForwardAgent no
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	env := map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/shareduser/.cache/cc-clip/peers/abc123def456",
	}

	if err := ApplyToFile(path, "srv-shared", env); err != nil {
		t.Fatalf("Apply srv-shared: %v", err)
	}
	afterApply, _ := os.ReadFile(path)
	t.Logf("=== after Apply ===\n%s", afterApply)

	if err := ApplyToFile(path, "no-such-alias.example.com", map[string]string{"CC_CLIP_PORT": "1"}); !errors.Is(err, ErrOnlyGlobMatch) {
		t.Fatalf("expected ErrOnlyGlobMatch for glob-only alias, got %v", err)
	}
	if err := ApplyToFile(path, "no-such-alias", map[string]string{"CC_CLIP_PORT": "1"}); !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing for unknown alias, got %v", err)
	}

	if err := ApplyToFile(path, "srv-shared", env); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	afterReApply, _ := os.ReadFile(path)
	if string(afterApply) != string(afterReApply) {
		t.Fatalf("re-apply was not idempotent")
	}

	if err := RemoveFromFile(path, "srv-shared"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	afterRemove, _ := os.ReadFile(path)
	if string(afterRemove) != original {
		t.Fatalf("round-trip not byte-equivalent:\n--- got\n%s\n--- want\n%s", afterRemove, original)
	}
}
