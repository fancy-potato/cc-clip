package setup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEnsureSSHConfig_NoTrailingNewlineAppends pins the behavior that when
// the existing config has no trailing newline after its last directive,
// appending a fresh managed Host block does not glue the new `Host <alias>`
// line onto the previous directive. The append path in ensureSSHConfigAt
// inserts a blank line separator; a regression that removes that guard
// would produce invalid ssh_config.
func TestEnsureSSHConfig_NoTrailingNewlineAppends(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	// No trailing newline after the final directive.
	initial := "Host foo\n    HostName 1.1.1.1"
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "bar", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(content)

	// The new block must start on its own line, never glued onto the
	// preceding directive. Detect the failure mode explicitly so a future
	// regression produces a readable diff.
	if strings.Contains(got, "1.1.1.1Host bar") || strings.Contains(got, "1.1.1.1    Host bar") {
		t.Fatalf("new Host block glued onto previous directive:\n%s", got)
	}
	if !strings.Contains(got, "\nHost bar\n") {
		t.Fatalf("Host bar block not on its own line:\n%s", got)
	}
}

func TestEnsureSSHConfig_AllowsIncludeDirectiveWhenManagingMainFileHostBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Include ~/.ssh/config.d/*\n\nHost myserver\n    HostName myserver\n"
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ensureSSHConfigAt(configPath, "myserver", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	if _, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{Host: "myserver", RemotePort: 19001, LocalPort: 18339}); err != nil {
		t.Fatalf("ensureManagedHostConfigAt: %v", err)
	}

	if err := removeManagedHostConfigAt(configPath, "myserver"); err != nil {
		t.Fatalf("removeManagedHostConfigAt: %v", err)
	}
}

// TestEnsureSSHConfig_LowercaseHostKeyword pins case-insensitive Host
// detection. ssh_config(5) keywords are case-insensitive and users do
// hand-edit their configs in mixed case. Without fold-matching, a user
// block `host prod` is invisible and cc-clip would append a duplicate.
func TestEnsureSSHConfig_LowercaseHostKeyword(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "host myserver\n    HostName 10.0.0.1\n"
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(content)
	// Must not have created a second Host block.
	if strings.Count(strings.ToLower(got), "host myserver") != 1 {
		t.Fatalf("expected exactly one host myserver block, got:\n%s", got)
	}
	// Directives should have been added inside the existing lowercase block.
	added := 0
	for _, c := range changes {
		if c.Action == "added" {
			added++
		}
	}
	if added != 3 {
		t.Fatalf("expected 3 added directives, got %d from %v", added, changes)
	}
}

func TestEnsureSSHConfig_NewHostBeforeHostStar(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host *\n    ServerAliveInterval 30\n"
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)

	if !strings.Contains(s, "Host myserver") {
		t.Fatal("Host myserver block not created")
	}

	myIdx := strings.Index(s, "Host myserver")
	starIdx := strings.Index(s, "Host *")
	if myIdx >= starIdx {
		t.Fatalf("Host myserver (%d) should come before Host * (%d)", myIdx, starIdx)
	}

	if !strings.Contains(s, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatal("RemoteForward not added")
	}
	if !strings.Contains(s, "ControlMaster no") {
		t.Fatal("ControlMaster no not added")
	}
	if !strings.Contains(s, "ControlPath none") {
		t.Fatal("ControlPath none not added")
	}

	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("expected 1 created change, got %v", changes)
	}
}

func TestEnsureSSHConfig_ExistingHostAddMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host myserver\n    HostName 10.0.0.1\n    User admin\n\nHost *\n    ServerAliveInterval 30\n"
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)

	if !strings.Contains(s, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatal("RemoteForward not added")
	}
	if !strings.Contains(s, "ControlMaster no") {
		t.Fatal("ControlMaster no not added")
	}

	addedCount := 0
	for _, c := range changes {
		if c.Action == "added" {
			addedCount++
		}
	}
	if addedCount != 3 {
		t.Fatalf("expected 3 added changes, got %d from %v", addedCount, changes)
	}
}

func TestEnsureSSHConfig_AlreadyConfigured(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host myserver\n    RemoteForward 18339 127.0.0.1:18339\n    ControlMaster no\n    ControlPath none\n"
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, c := range changes {
		if c.Action != "ok" {
			t.Fatalf("expected all ok, got %v", changes)
		}
	}

	backupPath := configPath + ".cc-clip-backup"
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatal("backup should not be created when no changes needed")
	}
}

func TestEnsureSSHConfig_NoFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	if !strings.Contains(string(content), "Host myserver") {
		t.Fatal("Host block not created")
	}
	if strings.HasPrefix(string(content), "\n") || strings.HasPrefix(string(content), "\r\n") {
		t.Fatalf("config should not begin with a blank line; got %q", string(content))
	}

	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("expected 1 created change, got %v", changes)
	}
}

// TestEnsureSSHConfig_EmptyFileNoLeadingBlankLine covers the edge case where
// an existing `~/.ssh/config` file is zero-length. splitSSHConfigLines on ""
// yields [""], which previously left a leading empty line before the managed
// block on write.
func TestEnsureSSHConfig_EmptyFileNoLeadingBlankLine(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, nil, 0600); err != nil {
		t.Fatalf("write empty config: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "myserver", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.HasPrefix(string(content), "\n") || strings.HasPrefix(string(content), "\r\n") {
		t.Fatalf("rewrite of empty config must not start with a blank line; got %q", string(content))
	}
	if !strings.HasPrefix(string(content), "Host myserver") {
		t.Fatalf("rewrite of empty config should begin with Host line; got %q", string(content))
	}
	backupContent, err := os.ReadFile(configPath + ".cc-clip-backup")
	if err != nil {
		t.Fatalf("expected empty pristine backup to be created: %v", err)
	}
	if len(backupContent) != 0 {
		t.Fatalf("expected empty pristine backup, got %q", string(backupContent))
	}
}

func TestEnsureSSHConfig_CreatesBackup(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host myserver\n    HostName 10.0.0.1\n"
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backupContent, err := os.ReadFile(configPath + ".cc-clip-backup")
	if err != nil {
		t.Fatal("backup not created")
	}
	if string(backupContent) != initial {
		t.Fatal("backup content doesn't match original")
	}
}

func TestEnsureSSHConfig_PreservesExistingDirectives(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host myserver\n    HostName 10.0.0.1\n    User admin\n    Port 2222\n    IdentityFile ~/.ssh/id_rsa\n\nHost *\n    ServerAliveInterval 30\n"
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)

	// Original directives preserved
	if !strings.Contains(s, "HostName 10.0.0.1") {
		t.Fatal("HostName lost")
	}
	if !strings.Contains(s, "User admin") {
		t.Fatal("User lost")
	}
	if !strings.Contains(s, "Port 2222") {
		t.Fatal("Port lost")
	}

	// Host * still at the end
	myIdx := strings.Index(s, "Host myserver")
	starIdx := strings.Index(s, "Host *")
	if myIdx >= starIdx {
		t.Fatal("Host myserver should come before Host *")
	}
}

func TestEnsureManagedHostConfigAtCreatesManagedFragment(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    User admin",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	changes, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("unexpected changes: %v", changes)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"Host myserver",
		"HostName 10.0.0.1",
		"User admin",
		"# >>> cc-clip managed host: myserver >>>",
		"RemoteForward 18340 127.0.0.1:18339",
		"ControlMaster no",
		"ControlPath none",
		"# <<< cc-clip managed host: myserver <<<",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
	if strings.Count(s, "Host myserver") != 1 {
		t.Fatalf("expected only the original Host block, got:\n%s", s)
	}
}

func TestEnsureManagedHostConfigAtPrefersEarlierExactAliasOverLaterSharedBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host staging",
		"    HostName staging.example.com",
		"",
		"Host prod staging",
		"    HostName prod.example.com",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "staging",
		RemotePort: 19001,
		LocalPort:  18339,
	}); err != nil {
		t.Fatalf("ensureManagedHostConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	firstHost := strings.Index(s, "Host staging")
	firstManaged := strings.Index(s, "# >>> cc-clip managed host: staging >>>")
	secondHost := strings.Index(s, "Host prod staging")
	if firstHost < 0 || firstManaged < 0 || secondHost < 0 {
		t.Fatalf("expected both host blocks and managed fragment, got:\n%s", s)
	}
	if firstManaged < firstHost || firstManaged > secondHost {
		t.Fatalf("expected managed fragment in the exact Host staging block, got:\n%s", s)
	}
}

func TestEnsureSSHConfigAtRejectsEarlierSharedBeforeLaterExactBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName prod.example.com",
		"",
		"Host staging",
		"    HostName staging.example.com",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureSSHConfigAt(configPath, "staging", 18339)
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}

	got, _ := os.ReadFile(configPath)
	if string(got) != initial {
		t.Fatalf("expected no mutation of shared-first config, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtRejectsEarlierSharedBeforeLaterExactBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName prod.example.com",
		"",
		"Host staging",
		"    HostName staging.example.com",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "staging",
		RemotePort: 19001,
		LocalPort:  18339,
	})
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}

	got, _ := os.ReadFile(configPath)
	if string(got) != initial {
		t.Fatalf("expected no mutation of shared-first config, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtUpdatesManagedFragmentInPlace(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18339 127.0.0.1:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"    # <<< cc-clip managed host: myserver <<<",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	changes, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "updated" {
		t.Fatalf("unexpected changes: %v", changes)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Count(s, "# >>> cc-clip managed host: myserver >>>") != 1 {
		t.Fatalf("expected exactly one managed host fragment, got:\n%s", s)
	}
	if strings.Contains(s, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatalf("expected old RemoteForward to be replaced, got:\n%s", s)
	}
	if !strings.Contains(s, "RemoteForward 18340 127.0.0.1:18339") {
		t.Fatalf("expected updated RemoteForward, got:\n%s", s)
	}
}

func TestRemoveManagedHostConfigAtPreservesHostBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    User admin",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18340 127.0.0.1:18339",
		"    # <<< cc-clip managed host: myserver <<<",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := removeManagedHostConfigAt(configPath, "myserver"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Contains(s, "cc-clip managed host: myserver") {
		t.Fatalf("expected managed fragment removal, got:\n%s", s)
	}
	for _, needle := range []string{"Host myserver", "HostName 10.0.0.1", "User admin"} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected host block to preserve %q, got:\n%s", needle, s)
		}
	}
}

func TestRemoveManagedHostConfigAtRejectsIncompleteManagedFragment(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18340 127.0.0.1:18339",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := removeManagedHostConfigAt(configPath, "myserver")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestRemoveManagedHostConfigAtPrefersEarlierExactAliasOverLaterSharedBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host staging",
		"    HostName staging.example.com",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"    # <<< cc-clip managed host: staging <<<",
		"",
		"Host prod staging",
		"    HostName prod.example.com",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := removeManagedHostConfigAt(configPath, "staging"); err != nil {
		t.Fatalf("removeManagedHostConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Contains(s, "# >>> cc-clip managed host: staging >>>") {
		t.Fatalf("expected managed fragment to be removed, got:\n%s", s)
	}
	if !strings.Contains(s, "Host prod staging") {
		t.Fatalf("expected later shared block to remain, got:\n%s", s)
	}
}

func TestRemoveManagedHostConfigAtRemovesLaterExactBlockWhenEarlierSharedAlsoMatches(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName prod.example.com",
		"",
		"Host staging",
		"    HostName staging.example.com",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"    # <<< cc-clip managed host: staging <<<",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := removeManagedHostConfigAt(configPath, "staging"); err != nil {
		t.Fatalf("removeManagedHostConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Contains(s, "cc-clip managed host: staging") {
		t.Fatalf("expected managed fragment removal, got:\n%s", s)
	}
	if !strings.Contains(s, "Host prod staging") || !strings.Contains(s, "Host staging") {
		t.Fatalf("expected both host stanzas to remain, got:\n%s", s)
	}
}

func TestRemoveManagedHostConfigAtRemovesManagedFragmentFromSharedHostBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName prod.example.com",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"    # <<< cc-clip managed host: staging <<<",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := removeManagedHostConfigAt(configPath, "staging"); err != nil {
		t.Fatalf("removeManagedHostConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Contains(s, "cc-clip managed host: staging") {
		t.Fatalf("expected managed fragment removal, got:\n%s", s)
	}
	if !strings.Contains(s, "Host prod staging") || !strings.Contains(s, "HostName prod.example.com") {
		t.Fatalf("expected shared host block to remain, got:\n%s", s)
	}
}

func TestRemoveManagedHostConfigAtRejectsIncompleteManagedFragmentPreservesTrailingContent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18340 127.0.0.1:18339",
		"    ControlMaster no",
		"    # user comment",
		"    User admin",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := removeManagedHostConfigAt(configPath, "myserver")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtRejectsMissingExactHostBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host other",
		"    HostName 10.0.0.2",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "missing exact Host myserver block") {
		t.Fatalf("expected exact host block error, got %v", err)
	}
}

func TestEnsureManagedHostConfigAtRejectsSharedHostBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "staging",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	content, _ := os.ReadFile(configPath)
	if err == nil || !strings.Contains(err.Error(), "shared stanza") {
		t.Fatalf("err = %v, want shared-stanza error", err)
	}
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtAllowsSingleHostWithNegatedExclusions(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host staging !staging-admin",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "staging",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err != nil {
		t.Fatalf("ensureManagedHostConfigAt: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("changes = %+v, want created managed block", changes)
	}
	content, _ := os.ReadFile(configPath)
	if !strings.Contains(string(content), "# >>> cc-clip managed host: staging >>>") {
		t.Fatalf("expected managed block, got:\n%s", string(content))
	}
}

func TestEnsureManagedHostConfigAtRejectsWildcardOnlyConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host *.corp",
		"    User admin",
		"",
		"Host *",
		"    ServerAliveInterval 30",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "db01.corp",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "missing exact Host db01.corp block") {
		t.Fatalf("expected wildcard-only config to be rejected, got %v", err)
	}
}

func TestEnsureManagedHostConfigAtRejectsConflictingControlMasterWithoutPartialWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    ControlMaster auto",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "ControlMaster") {
		t.Fatalf("expected conflicting directive error, got %v", err)
	}

	content, _ := os.ReadFile(configPath)
	if string(content) != initial {
		t.Fatalf("expected no partial write on conflict, got:\n%s", string(content))
	}
}

func TestEnsureManagedHostConfigAtAllowsCompatibleControlSettings(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    ControlMaster no",
		"    ControlPath none",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err != nil {
		t.Fatalf("ensureManagedHostConfigAt: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("changes = %+v, want one created change", changes)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Count(s, "ControlMaster no") != 2 {
		t.Fatalf("expected one preserved and one managed ControlMaster line, got:\n%s", s)
	}
	if strings.Count(s, "ControlPath none") != 2 {
		t.Fatalf("expected one preserved and one managed ControlPath line, got:\n%s", s)
	}
	if strings.Count(s, "RemoteForward 18340 127.0.0.1:18339") != 1 {
		t.Fatalf("expected one managed RemoteForward line, got:\n%s", s)
	}
	for _, needle := range []string{
		"ControlMaster no",
		"ControlPath none",
		"RemoteForward 18340 127.0.0.1:18339",
		"# >>> cc-clip managed host: myserver >>>",
		"# <<< cc-clip managed host: myserver <<<",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}

func TestEnsureManagedHostConfigAtAllowsCompatibleControlSettingsWithInlineComments(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    ControlMaster no # policy",
		"    ControlPath none # policy",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	}); err != nil {
		t.Fatalf("ensureManagedHostConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Count(s, "ControlMaster no # policy") != 1 {
		t.Fatalf("expected inline-comment ControlMaster to be preserved once, got:\n%s", s)
	}
	if strings.Count(s, "ControlPath none # policy") != 1 {
		t.Fatalf("expected inline-comment ControlPath to be preserved once, got:\n%s", s)
	}
	if strings.Count(s, "ControlMaster no") != 2 {
		t.Fatalf("expected one preserved and one managed ControlMaster line, got:\n%s", s)
	}
	if strings.Count(s, "ControlPath none") != 2 {
		t.Fatalf("expected one preserved and one managed ControlPath line, got:\n%s", s)
	}
	if strings.Count(s, "RemoteForward 18340 127.0.0.1:18339") != 1 {
		t.Fatalf("expected one managed RemoteForward line, got:\n%s", s)
	}
}

func TestEnsureManagedHostConfigAtRejectsConflictingRemoteForwardWithoutPartialWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    RemoteForward 18340 127.0.0.1:22",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "RemoteForward on port 18340") {
		t.Fatalf("expected conflicting RemoteForward error, got %v", err)
	}

	content, _ := os.ReadFile(configPath)
	if string(content) != initial {
		t.Fatalf("expected no partial write on conflict, got:\n%s", string(content))
	}
}

func TestEnsureManagedHostConfigAtRejectsIncompleteManagedFragment(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18339 127.0.0.1:18339",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtRejectsIncompleteManagedFragmentWithInterleavedUserDirective(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18339 127.0.0.1:18339",
		"    User admin",
		"    ControlPath none",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtRejectsMalformedManagedFragmentPreservesUserDirectives(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18339 127.0.0.1:18339",
		"    User admin",
		"    # >>> cc-clip managed host: myserver >>>",
		"    ControlPath none",
		"    # <<< cc-clip managed host: myserver <<<",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v, want malformed managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestRemoveManagedHostConfigAtRejectsIncompleteManagedFragmentAcrossComments(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18340 127.0.0.1:18339",
		"    # user comment",
		"    ControlPath none",
		"    User admin",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := removeManagedHostConfigAt(configPath, "myserver")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestRemoveManagedHostConfigAtRejectsIncompleteManagedFragmentWithInterleavedUserDirective(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18340 127.0.0.1:18339",
		"    User admin",
		"    ControlPath none",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := removeManagedHostConfigAt(configPath, "myserver")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete managed-block error", err)
	}

	content, _ := os.ReadFile(configPath)
	if got := string(content); got != initial {
		t.Fatalf("expected config to remain unchanged, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtInsertsBeforeMatchBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"Match user deploy",
		"    IdentityFile ~/.ssh/deploy",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "myserver",
		RemotePort: 18340,
		LocalPort:  18339,
	}); err != nil {
		t.Fatalf("ensureManagedHostConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	managedIdx := strings.Index(s, "# >>> cc-clip managed host: myserver >>>")
	matchIdx := strings.Index(s, "Match user deploy")
	if managedIdx < 0 || matchIdx < 0 {
		t.Fatalf("expected managed fragment and Match block, got:\n%s", s)
	}
	if managedIdx > matchIdx {
		t.Fatalf("expected managed fragment before Match block, got:\n%s", s)
	}
}

func TestReadManagedRemotePortRejectsMalformedManagedLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedRemotePort("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedRemotePortRejectsIncompleteManagedBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedRemotePort("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsRejectsOutOfRangePorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
	}{
		{name: "zero remote port", spec: "RemoteForward 0 127.0.0.1:18339"},
		{name: "negative remote port", spec: "RemoteForward -1 127.0.0.1:18339"},
		{name: "too large remote port", spec: "RemoteForward 65536 127.0.0.1:18339"},
		{name: "zero local port", spec: "RemoteForward 19001 127.0.0.1:0"},
		{name: "negative local port", spec: "RemoteForward 19001 127.0.0.1:-1"},
		{name: "too large local port", spec: "RemoteForward 19001 127.0.0.1:65536"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)

			sshDir := filepath.Join(home, ".ssh")
			if err := os.MkdirAll(sshDir, 0700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			config := strings.Join([]string{
				"Host example",
				"    # >>> cc-clip managed host: example >>>",
				"    " + tc.spec,
				"    # <<< cc-clip managed host: example <<<",
				"",
			}, "\n")
			if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			_, err := ReadManagedTunnelPorts("example")
			if !errors.Is(err, ErrManagedRemotePortInvalid) {
				t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
			}
		})
	}
}

func TestReadManagedTunnelPortsRejectsTrailingGarbage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18444 garbage",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsRejectsListenBindAddress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 0.0.0.0:19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsAcceptsExplicitLoopbackListenHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Semantically identical to the host-less form we emit ourselves; a
	// user who hand-edits this way should not be punished.
	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 127.0.0.1:19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("example")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports.RemotePort != 19001 || ports.LocalPort != 18444 {
		t.Fatalf("ports = %+v, want RemotePort=19001 LocalPort=18444", ports)
	}
}

func TestReadManagedTunnelPortsRejectsNonLoopbackTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 10.0.0.5:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsRejectsIPv6LoopbackTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 [::1]:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsRejectsMalformedDuplicateStartMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19002 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsRejectsDuplicateRemoteForward(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    RemoteForward 19002 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestEnsureSSHConfigAtDoesNotTreatSubstringDirectiveAsMatch(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    RemoteForward 118339 127.0.0.1:18339",
		"    ControlPath /tmp/none.sock",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"RemoteForward 18339 127.0.0.1:18339",
		"ControlMaster no",
		"ControlPath none",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}

	added := 0
	for _, change := range changes {
		if change.Action == "added" {
			added++
		}
	}
	if added != 3 {
		t.Fatalf("added = %d, want 3; changes=%v", added, changes)
	}
}

func TestEnsureSSHConfigAtRewritesLocalhostRemoteForwardTarget(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    RemoteForward 18339 localhost:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if got := strings.Count(s, "RemoteForward"); got != 1 {
		t.Fatalf("RemoteForward count = %d, want 1; config:\n%s", got, s)
	}
	if !strings.Contains(s, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatalf("expected RemoteForward to be rewritten, got:\n%s", s)
	}
	if changes[0].Action != "updated" {
		t.Fatalf("changes[0].Action = %q, want %q", changes[0].Action, "updated")
	}
}

func TestEnsureSSHConfigAtRewritesLocalhostRemoteForwardTargetWithInlineComment(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    RemoteForward 18339 localhost:18339 # keep",
		"    ControlMaster no # keep",
		"    ControlPath none # keep",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Count(s, "RemoteForward") != 1 {
		t.Fatalf("RemoteForward count = %d, want 1; config:\n%s", strings.Count(s, "RemoteForward"), s)
	}
	if strings.Count(s, "ControlMaster") != 1 {
		t.Fatalf("ControlMaster count = %d, want 1; config:\n%s", strings.Count(s, "ControlMaster"), s)
	}
	if strings.Count(s, "ControlPath") != 1 {
		t.Fatalf("ControlPath count = %d, want 1; config:\n%s", strings.Count(s, "ControlPath"), s)
	}
	if !strings.Contains(s, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatalf("expected RemoteForward to be rewritten, got:\n%s", s)
	}
	if changes[0].Action != "updated" {
		t.Fatalf("changes[0].Action = %q, want %q", changes[0].Action, "updated")
	}
}

func TestEnsureSSHConfigAtRewritesRemoteForwardWithWildcardListenAddress(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    RemoteForward 0.0.0.0:18339 127.0.0.1:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureSSHConfigAt(configPath, "myserver", 18339)
	if err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Contains(s, "0.0.0.0:18339") {
		t.Fatalf("expected wildcard listen address to be removed, got:\n%s", s)
	}
	if strings.Count(s, "RemoteForward") != 1 {
		t.Fatalf("RemoteForward count = %d, want 1; config:\n%s", strings.Count(s, "RemoteForward"), s)
	}
	if changes[0].Action != "updated" {
		t.Fatalf("changes[0].Action = %q, want %q", changes[0].Action, "updated")
	}
}

func TestReadManagedRemotePortReadsManagedBlockPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	port, err := ReadManagedRemotePort("example")
	if err != nil {
		t.Fatalf("ReadManagedRemotePort: %v", err)
	}
	if port != 19001 {
		t.Fatalf("port = %d, want 19001", port)
	}
}

// TestReadManagedTunnelPortsRejectsStartMarkerAtEOFWithoutNewline covers the
// edge case where the start marker is the final line of the file and the
// file ends with no terminating newline. findManagedRangeInBlock must
// classify this as malformed (start without end) rather than silently
// reporting zero ports.
func TestReadManagedTunnelPortsRejectsStartMarkerAtEOFWithoutNewline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Deliberately no trailing newline — the start marker is the very
	// last bytes of the file.
	config := "Host example\n    HostName 10.0.0.1\n    # >>> cc-clip managed host: example >>>"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if err == nil || !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid (partial managed block at EOF)", err)
	}
}

func TestReadManagedTunnelPortsRejectsStrayEndMarker(t *testing.T) {
	// A stray end marker before any start marker must be flagged as
	// malformed; ensureManagedHostConfigAt relies on this to detect and
	// refuse to double-write above the orphan marker.
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # <<< cc-clip managed host: example <<<",
		"    RemoteForward 19001 127.0.0.1:18339",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if err == nil || !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsRejectsDuplicateEndMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18339",
		"    # <<< cc-clip managed host: example <<<",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("example")
	if err == nil || !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

// TestEnsureThenReadManagedTunnelPortsRoundTrip writes the managed block via
// ensureManagedHostConfigAt and then reads it back with ReadManagedTunnelPorts
// to guarantee the two helpers stay in sync. If the managed-fragment format
// ever drifts (e.g. the RemoteForward emitter changes how it spells the
// target), this test fails at the format boundary instead of silently
// returning zero ports to auto-detect callers.
func TestEnsureThenReadManagedTunnelPortsRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		remotePort int
		localPort  int
	}{
		{name: "common", host: "myserver", remotePort: 18340, localPort: 18339},
		{name: "high ports", host: "example", remotePort: 62001, localPort: 65535},
		{name: "low remote port", host: "alpha", remotePort: 1024, localPort: 18339},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			sshDir := filepath.Join(home, ".ssh")
			if err := os.MkdirAll(sshDir, 0700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			configPath := filepath.Join(sshDir, "config")
			initial := "Host " + tc.host + "\n    HostName 10.0.0.1\n"
			if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
				Host:       tc.host,
				RemotePort: tc.remotePort,
				LocalPort:  tc.localPort,
			}); err != nil {
				t.Fatalf("ensureManagedHostConfigAt: %v", err)
			}
			got, err := ReadManagedTunnelPorts(tc.host)
			if err != nil {
				t.Fatalf("ReadManagedTunnelPorts: %v", err)
			}
			want := ManagedTunnelPorts{RemotePort: tc.remotePort, LocalPort: tc.localPort}
			if got != want {
				t.Fatalf("round-trip: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestReadManagedTunnelPortsReadsManagedBlockPorts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("example")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports.RemotePort != 19001 {
		t.Fatalf("RemotePort = %d, want 19001", ports.RemotePort)
	}
	if ports.LocalPort != 18444 {
		t.Fatalf("LocalPort = %d, want 18444", ports.LocalPort)
	}
}

func TestReadManagedTunnelPortsPrefersEarlierExactAliasOverLaterSharedBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host staging",
		"    HostName staging.example.com",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: staging <<<",
		"",
		"Host prod staging",
		"    HostName prod.example.com",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("staging")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports != (ManagedTunnelPorts{RemotePort: 19001, LocalPort: 18444}) {
		t.Fatalf("ports = %+v, want remote=19001 local=18444", ports)
	}
}

func TestReadManagedTunnelPortsRejectsEarlierSharedBeforeLaterExactBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host prod staging",
		"    HostName prod.example.com",
		"",
		"Host staging",
		"    HostName staging.example.com",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: staging <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("staging")
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want also wrap ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsIgnoresManagedFragmentInMatchBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    HostName 10.0.0.1",
		"Match user deploy",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("example")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports != (ManagedTunnelPorts{}) {
		t.Fatalf("ports = %+v, want zero-value ports", ports)
	}
}

func TestReadManagedTunnelPortsRejectsSharedHostBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host prod staging",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: staging <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("staging")
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedTunnelPortsAllowsSingleHostWithNegatedExclusions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host staging !staging-admin",
		"    # >>> cc-clip managed host: staging >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: staging <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("staging")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports != (ManagedTunnelPorts{RemotePort: 19001, LocalPort: 18444}) {
		t.Fatalf("ports = %+v, want remote=19001 local=18444", ports)
	}
}

func TestReadManagedTunnelPortsReadsManagedBlockWithInlineComment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18444 # comment",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("example")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports.RemotePort != 19001 {
		t.Fatalf("RemotePort = %d, want 19001", ports.RemotePort)
	}
	if ports.LocalPort != 18444 {
		t.Fatalf("LocalPort = %d, want 18444", ports.LocalPort)
	}
}

// F2: every public entry point must reject shared Host stanzas.

func TestEnsureSSHConfigAtRejectsSharedHostStanza(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureSSHConfigAt(configPath, "prod", 18339)
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}

	got, _ := os.ReadFile(configPath)
	if string(got) != initial {
		t.Fatalf("expected no mutation of shared stanza, got:\n%s", got)
	}
}

func TestEnsureManagedHostConfigAtSharedStanzaErrorIsErrSharedHostStanza(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host prod staging",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ensureManagedHostConfigAt(configPath, ManagedHostSpec{
		Host:       "prod",
		RemotePort: 19001,
		LocalPort:  18339,
	})
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}
}

func TestReadManagedTunnelPortsSharedStanzaErrorIsErrSharedHostStanza(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	config := strings.Join([]string{
		"Host prod staging",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedTunnelPorts("prod")
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}
	if !errors.Is(err, ErrManagedRemotePortInvalid) {
		t.Fatalf("err = %v, want also wrap ErrManagedRemotePortInvalid", err)
	}
}

func TestReadManagedRemotePortSharedStanzaErrorIsErrSharedHostStanza(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	config := strings.Join([]string{
		"Host prod staging",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManagedRemotePort("prod")
	if !errors.Is(err, ErrSharedHostStanza) {
		t.Fatalf("err = %v, want ErrSharedHostStanza", err)
	}
}

// F12: stripSSHInlineComment must handle multi-byte runes directly before '#'.

func TestStripSSHInlineCommentAfterMultiByteRune(t *testing.T) {
	// U+3000 IDEOGRAPHIC SPACE — 3 bytes in UTF-8. If the implementation
	// used byte indexing on a rune iteration, rune(line[i-1]) would read
	// the trailing continuation byte of U+3000 and compute a garbage
	// "prev" rune; the '#' could then be misclassified.
	const ideoSpace = "\u3000"
	line := "HostName example.com" + ideoSpace + "# trailing"
	got := stripSSHInlineComment(line)
	// U+3000 is not an ASCII-space per isSSHSpace, so '#' is preceded by a
	// non-space rune and must NOT start a comment. The trimmed line must
	// preserve both the ideographic space and the '#'.
	want := strings.TrimSpace(line)
	if got != want {
		t.Fatalf("stripSSHInlineComment = %q, want %q", got, want)
	}
}

func TestStripSSHInlineCommentTabBeforeHash(t *testing.T) {
	got := stripSSHInlineComment("HostName example.com\t# comment")
	want := "HostName example.com"
	if got != want {
		t.Fatalf("stripSSHInlineComment = %q, want %q", got, want)
	}
}

// F13: CRLF line endings must be preserved on rewrite.

func TestEnsureSSHConfigAtPreservesCRLFLineEndings(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := "Host myserver\r\n    HostName 10.0.0.1\r\n\r\nHost *\r\n    ServerAliveInterval 30\r\n"
	if err := os.WriteFile(configPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "myserver", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if !strings.Contains(s, "\r\n") {
		t.Fatalf("expected CRLF line endings preserved, got:\n%q", s)
	}
	// Post-write invariant: every LF must be at an index where the
	// preceding byte is CR. A lone LF means CRLF was silently converted.
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' && (i == 0 || s[i-1] != '\r') {
			t.Fatalf("found lone LF at byte %d after CRLF-preserving rewrite:\n%q", i, s)
		}
	}
	if !strings.Contains(s, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatalf("expected directive written, got:\n%s", s)
	}
}

// F14: quoted host patterns must tokenize as a single pattern.

func TestHostPatternsTreatsQuotedStringAsSingleToken(t *testing.T) {
	got := hostPatterns(`Host "prod staging"`)
	want := []string{"prod staging"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("hostPatterns = %v, want %v", got, want)
	}
}

func TestHostPatternsSingleQuotedStringAsSingleToken(t *testing.T) {
	got := hostPatterns("Host 'alpha beta'")
	want := []string{"alpha beta"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("hostPatterns = %v, want %v", got, want)
	}
}

func TestHostPatternsMixedQuotedAndPlainTokens(t *testing.T) {
	got := hostPatterns(`Host alpha "with space" gamma`)
	want := []string{"alpha", "with space", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("hostPatterns len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hostPatterns[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestHostPatternsAcceptsSpaceAroundEquals pins ssh_config(5) behavior: the
// keyword/value separator may be whitespace or `=`, with optional whitespace
// on either side of the `=`. All four variants must parse to the same single
// host pattern.
func TestHostPatternsAcceptsSpaceAroundEquals(t *testing.T) {
	cases := []string{
		"Host foo",
		"Host=foo",
		"Host =foo",
		"Host= foo",
		"Host = foo",
	}
	for _, line := range cases {
		got := hostPatterns(line)
		if len(got) != 1 || got[0] != "foo" {
			t.Fatalf("hostPatterns(%q) = %v, want [foo]", line, got)
		}
	}
}

func TestHostPatternsStripInlineComment(t *testing.T) {
	cases := []string{
		"Host foo # note",
		"Host = foo # note",
	}
	for _, line := range cases {
		got := hostPatterns(line)
		if len(got) != 1 || got[0] != "foo" {
			t.Fatalf("hostPatterns(%q) = %v, want [foo]", line, got)
		}
	}
}

func TestReadManagedTunnelPortsReadsManagedBlockWhenHostLineHasInlineComment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := strings.Join([]string{
		"Host example # note",
		"    # >>> cc-clip managed host: example >>>",
		"    RemoteForward 19001 127.0.0.1:18444",
		"    # <<< cc-clip managed host: example <<<",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ports, err := ReadManagedTunnelPorts("example")
	if err != nil {
		t.Fatalf("ReadManagedTunnelPorts: %v", err)
	}
	if ports != (ManagedTunnelPorts{RemotePort: 19001, LocalPort: 18444}) {
		t.Fatalf("ports = %+v, want remote=19001 local=18444", ports)
	}
}

// TestParseSSHDirectiveAcceptsSpaceAroundEquals pins the matching behavior
// for body directives: `ControlMaster = auto` must return the same key/value
// pair as `ControlMaster auto` and `ControlMaster=auto`.
func TestParseSSHDirectiveAcceptsSpaceAroundEquals(t *testing.T) {
	cases := []struct {
		line, key, value string
	}{
		{"ControlMaster auto", "ControlMaster", "auto"},
		{"ControlMaster=auto", "ControlMaster", "auto"},
		{"ControlMaster =auto", "ControlMaster", "auto"},
		{"ControlMaster= auto", "ControlMaster", "auto"},
		{"ControlMaster = auto", "ControlMaster", "auto"},
		// Embedded `=` in the value must be preserved.
		{"ProxyCommand ssh -o Foo=bar host", "ProxyCommand", "ssh -o Foo=bar host"},
		{"RemoteForward 127.0.0.1:18339 127.0.0.1:18339", "RemoteForward", "127.0.0.1:18339 127.0.0.1:18339"},
	}
	for _, tc := range cases {
		key, value := parseSSHDirective(tc.line)
		if key != tc.key || value != tc.value {
			t.Fatalf("parseSSHDirective(%q) = (%q, %q), want (%q, %q)", tc.line, key, value, tc.key, tc.value)
		}
	}
}

// F15: SSH config backup must preserve the pristine pre-cc-clip copy across
// subsequent runs and roll a timestamped copy for later edits.

func TestEnsureSSHConfigAtPreservesPristineBackupAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	original := "Host myserver\n    HostName 10.0.0.1\n"
	if err := os.WriteFile(configPath, []byte(original), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "myserver", 18339); err != nil {
		t.Fatalf("first ensureSSHConfigAt: %v", err)
	}

	pristinePath := configPath + ".cc-clip-backup"
	pristine1, err := os.ReadFile(pristinePath)
	if err != nil {
		t.Fatalf("ReadFile pristine backup: %v", err)
	}
	if string(pristine1) != original {
		t.Fatalf("expected pristine backup to match original, got:\n%s", pristine1)
	}

	// Simulate an external edit after cc-clip has run once, then run
	// ensureSSHConfigAt again. The pristine backup must NOT be overwritten.
	tampered := string(pristine1) + "# manually appended after cc-clip ran\n"
	if err := os.WriteFile(configPath, []byte(tampered), 0644); err != nil {
		t.Fatalf("WriteFile tampered: %v", err)
	}
	if _, err := ensureSSHConfigAt(configPath, "myserver", 22222); err != nil {
		t.Fatalf("second ensureSSHConfigAt: %v", err)
	}
	pristine2, err := os.ReadFile(pristinePath)
	if err != nil {
		t.Fatalf("ReadFile pristine backup (second): %v", err)
	}
	if string(pristine2) != original {
		t.Fatalf("expected pristine backup to remain unchanged, got:\n%s", pristine2)
	}
}

// F28: ~/.ssh/config permissions must be tightened to 0600 even if the
// original file was 0644.

func TestWriteSSHConfigFileForces0600FromLooserMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningfully enforced on Windows; os.Chmod clamps to 0666/0444")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte("Host a\n    HostName 1.2.3.4\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "newhost", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected config perms to be tightened to 0600 (no group/other bits), got %o", info.Mode().Perm())
	}
}

// The .cc-clip-backup sidecar carries the same secrets as ~/.ssh/config
// (IdentityFile, ProxyCommand, RemoteForward targets). Leaving it
// world-readable because of a looser-umask surprise would leak every
// connection parameter that the 0600-on-the-primary enforcement exists to
// protect. Cover the backup path explicitly rather than leaning on the
// main-config coverage above.
func TestWriteSSHConfigBackupForces0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningfully enforced on Windows; os.Chmod clamps to 0666/0444")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	// Seed with a non-0600 file so we can observe whether the backup
	// inherits the loose mode (the bug) or lands at 0600 (the fix).
	if err := os.WriteFile(configPath, []byte("Host a\n    HostName 1.2.3.4\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "newhost", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	backupPath := configPath + ".cc-clip-backup"
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("Stat backup: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected backup perms to be 0600 (no group/other bits), got %o", info.Mode().Perm())
	}
}

func TestEnsureSSHConfigAlreadyConfiguredTightensExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningfully enforced on Windows; os.Chmod clamps to 0666/0444")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	content := strings.Join([]string{
		"Host myserver",
		"    RemoteForward 18339 127.0.0.1:18339",
		"    ControlMaster no",
		"    ControlPath none",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "myserver", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected config perms to be tightened to 0600, got %o", info.Mode().Perm())
	}
}

func TestExistingSSHConfigBackupIsRetightenedTo0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningfully enforced on Windows; os.Chmod clamps to 0666/0444")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := "Host foo\n    HostName 1.2.3.4\n"
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	backupPath := configPath + ".cc-clip-backup"
	if err := os.WriteFile(backupPath, []byte(initial), 0644); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "newhost", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("Stat backup: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected backup perms to be tightened to 0600, got %o", info.Mode().Perm())
	}
}

func TestEnsureSSHConfig_WritesThroughSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real-config")
	linkPath := filepath.Join(dir, "config")

	realContent := "Host foo\n    HostName 1.1.1.1\n"
	if err := os.WriteFile(realPath, []byte(realContent), 0600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		// Windows unprivileged users cannot create symlinks; the refusal
		// path still applies on every OS, but the test needs a symlink to
		// exercise it. Skip rather than fail when the platform refuses.
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	if _, err := ensureSSHConfigAt(linkPath, "bar", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to remain a symlink after refuse, got mode %v", linkPath, info.Mode())
	}

	gotReal, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("ReadFile real: %v", err)
	}
	if !strings.Contains(string(gotReal), "Host bar") {
		t.Fatalf("expected symlink target to be updated, got:\n%s", gotReal)
	}
}

// TestEnsureSSHConfig_StripsUTF8BOM pins that a file starting with a UTF-8
// BOM (produced by some Windows editors / tools that treat it as the "mark
// this as UTF-8" preamble) is parsed correctly — without BOM stripping, the
// first `Host` keyword carries three invisible leading bytes and pattern
// matching fails, so cc-clip would append a duplicate managed block.
func TestEnsureSSHConfig_StripsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "\ufeff" + "Host myserver\n    HostName 1.2.3.4\n"
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureSSHConfigAt(configPath, "myserver", 18339); err != nil {
		t.Fatalf("ensureSSHConfigAt: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(content)

	// Exactly one `Host myserver` line — without BOM stripping, the Host
	// keyword on line 1 carries three invisible prefix bytes, the pattern
	// match fails, and cc-clip appends a duplicate `Host myserver` block.
	if strings.Count(got, "Host myserver") != 1 {
		t.Fatalf("expected exactly one Host myserver line after BOM-prefixed input, got:\n%s", got)
	}
	// BOM itself must be gone — otherwise any tool that re-reads the file
	// will hit the same problem on the next cc-clip invocation.
	if strings.HasPrefix(got, "\ufeff") {
		t.Fatalf("expected BOM to be stripped, got leading BOM in:\n%s", got)
	}
}
