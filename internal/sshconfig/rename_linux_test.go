//go:build linux

package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteAtomicRefusesSymlinkTarget pins P2-C on Linux: attempting
// to rewrite ~/.ssh/config when the path is a pre-existing symlink
// must be refused by writeAtomic's renameat2(RENAME_NOREPLACE) +
// Lstat-after-EEXIST sequence. The test is non-racy: we set up the
// symlink BEFORE calling ApplyToFile, so the Lstat inside readConfig
// already fires first and returns ErrSymlinkConfig. But an attacker
// who raced between readConfig and writeAtomic could still swap a
// symlink in, and the renameat2 guard is what closes that window.
// We simulate by seeding a real file through readConfig, then
// checking the renameNoReplace helper directly against a symlink dst.
func TestRenameNoReplaceRejectsSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("existing-content\n"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	// Plant a symlink at dst pointing somewhere benign. An attacker's
	// symlink swap in the real writeAtomic path would look like this.
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("victim-content\n"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatalf("symlink dst: %v", err)
	}

	// Create a tmp file with the new content we'd normally rename into
	// place. Real writeAtomic does this via os.CreateTemp.
	tmp := filepath.Join(dir, "tmp")
	if err := os.WriteFile(tmp, []byte("new-content\n"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}

	// renameNoReplace must refuse: dst exists as a symlink, so the
	// syscall returns EEXIST → we see os.ErrExist. If it instead
	// silently followed the link and wrote new-content into victim,
	// that would be the attack we're blocking. Verify victim is
	// untouched.
	ok, err := renameNoReplace(tmp, dst)
	if ok {
		t.Fatal("renameNoReplace must not silently overwrite symlink dst")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected os.ErrExist from renameat2 against symlink dst, got %v", err)
	}
	victimAfter, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatalf("read victim: %v", rerr)
	}
	if string(victimAfter) != "victim-content\n" {
		t.Fatalf("victim was overwritten through symlink: got %q", victimAfter)
	}
	// dst still a symlink, untouched.
	info, lerr := os.Lstat(dst)
	if lerr != nil {
		t.Fatalf("lstat dst: %v", lerr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dst expected to remain a symlink, mode=%v", info.Mode())
	}
}

// TestRenameAtomicRefusesSymlinkAtDst pins the writeAtomic-level
// integration: renameAtomic itself (the wrapper used in writeAtomic)
// returns ErrSymlinkConfig when dst is a symlink, regardless of which
// code path (Linux renameat2 → EEXIST → Lstat, or the fallback Lstat)
// actually fired.
func TestRenameAtomicRefusesSymlinkAtDst(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.WriteFile(tmp, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("victim\n"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatalf("symlink dst: %v", err)
	}
	err := renameAtomic(tmp, dst)
	if !errors.Is(err, ErrSymlinkConfig) {
		t.Fatalf("expected ErrSymlinkConfig from renameAtomic against symlink dst, got %v", err)
	}
	// victim must not have been touched through the symlink.
	got, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatalf("read victim: %v", rerr)
	}
	if string(got) != "victim\n" {
		t.Fatalf("victim was overwritten: %q", got)
	}
}

// TestRenameAtomicSucceedsWhenDstMissing covers the straight-through
// success path: when dst does not exist, renameat2(RENAME_NOREPLACE)
// does the atomic rename and the fallback is not consulted.
func TestRenameAtomicSucceedsWhenDstMissing(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.WriteFile(tmp, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	if err := renameAtomic(tmp, dst); err != nil {
		t.Fatalf("renameAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "content\n" {
		t.Fatalf("dst content mismatch: %q", got)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp should be gone after rename, stat err = %v", err)
	}
}

// TestRenameAtomicReplacesRegularFile covers the common case: dst is
// a pre-existing regular file (the normal ssh_config rewrite). After
// renameat2 returns EEXIST, the Lstat confirms it is NOT a symlink and
// the fallback os.Rename swaps the content atomically.
func TestRenameAtomicReplacesRegularFile(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.WriteFile(tmp, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	if err := renameAtomic(tmp, dst); err != nil {
		t.Fatalf("renameAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new\n" {
		t.Fatalf("dst content mismatch: %q", got)
	}
}
