package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureSSHConfig_NewHostBeforeHostStar(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host *\n    ServerAliveInterval 30\n"
	os.WriteFile(configPath, []byte(initial), 0644)

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
	os.WriteFile(configPath, []byte(initial), 0644)

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
	os.WriteFile(configPath, []byte(initial), 0644)

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

	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("expected 1 created change, got %v", changes)
	}
}

func TestEnsureSSHConfig_CreatesBackup(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	initial := "Host myserver\n    HostName 10.0.0.1\n"
	os.WriteFile(configPath, []byte(initial), 0644)

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
	os.WriteFile(configPath, []byte(initial), 0644)

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

func TestEnsureManagedAliasConfigAtCreatesManagedBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := "Host myserver\n    HostName 10.0.0.1\n    User admin\n    IdentityFile ~/.ssh/id_test\n"
	os.WriteFile(configPath, []byte(initial), 0644)

	changes, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
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
		"Host myserver-cc-clip-macbook",
		"HostName 10.0.0.1",
		"User admin",
		"IdentityFile ~/.ssh/id_test",
		"RemoteForward 18340 127.0.0.1:18339",
		"RemoteCommand test -x ~/.local/bin/cc-clip-shell-enter && exec ~/.local/bin/cc-clip-shell-enter peer-abc 18340 macbook",
		"# >>> cc-clip managed: myserver-cc-clip-macbook >>>",
		"# <<< cc-clip managed: myserver-cc-clip-macbook <<<",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}

func TestEnsureManagedAliasConfigAtCreatesManagedBlockBeforeHostStar(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"",
		"Host *",
		"    ServerAliveInterval 30",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	aliasIdx := strings.Index(s, "Host myserver-cc-clip-macbook")
	starIdx := strings.Index(s, "Host *")
	if aliasIdx < 0 || starIdx < 0 {
		t.Fatalf("expected alias and Host * blocks, got:\n%s", s)
	}
	if aliasIdx >= starIdx {
		t.Fatalf("expected managed alias before Host *, got:\n%s", s)
	}
}

func TestEnsureManagedAliasConfigAtCreatesManagedBlockBeforeMatchingWildcardHost(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host dev-*",
		"    ServerAliveInterval 30",
		"",
		"Host *",
		"    Compression yes",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return nil, errors.New("ssh unavailable")
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "dev-api",
		Alias:      "dev-api-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	aliasIdx := strings.Index(s, "Host dev-api-cc-clip-macbook")
	wildcardIdx := strings.Index(s, "Host dev-*")
	if aliasIdx < 0 || wildcardIdx < 0 {
		t.Fatalf("expected alias and wildcard blocks, got:\n%s", s)
	}
	if aliasIdx >= wildcardIdx {
		t.Fatalf("expected managed alias before matching wildcard Host block, got:\n%s", s)
	}
}

func TestEnsureManagedAliasConfigAtCopiesPatternMatchedDirectives(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host *.corp",
		"    User admin",
		"    IdentityFile ~/.ssh/id_corp",
		"    ProxyJump bastion",
		"",
		"Host *",
		"    ServerAliveInterval 30",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "db01.corp",
		Alias:      "db01-corp-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"HostName db01.corp",
		"User admin",
		"IdentityFile ~/.ssh/id_corp",
		"ProxyJump bastion",
		"ServerAliveInterval 30",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}

func TestEnsureManagedAliasConfigAtUpdatesExistingBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"",
		"# >>> cc-clip managed: myserver-cc-clip-macbook >>>",
		"Host myserver-cc-clip-macbook",
		"    HostName 10.0.0.1",
		"    RemoteForward 18339 127.0.0.1:18339",
		"# <<< cc-clip managed: myserver-cc-clip-macbook <<<",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	changes, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "updated" {
		t.Fatalf("unexpected changes: %v", changes)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Count(s, "Host myserver-cc-clip-macbook") != 1 {
		t.Fatalf("expected exactly one managed alias block, got:\n%s", s)
	}
	if !strings.Contains(s, "RemoteForward 18340 127.0.0.1:18339") {
		t.Fatalf("expected updated remote forward, got:\n%s", s)
	}
}

func TestEnsureManagedAliasConfigAtMovesExistingBlockBeforeMatchingWildcardHost(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	spec := ManagedAliasSpec{
		BaseHost:   "dev-api",
		Alias:      "dev-api-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	}

	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return nil, errors.New("ssh unavailable")
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	wildcardBlock := strings.Join([]string{
		"Host dev-*",
		"    ServerAliveInterval 30",
		"",
	}, "\n")
	existingBlock := strings.Join(managedAliasBlock(strings.Split(wildcardBlock, "\n"), configPath, spec), "\n")
	initial := wildcardBlock + existingBlock
	os.WriteFile(configPath, []byte(initial), 0644)

	changes, err := ensureManagedAliasConfigAt(configPath, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "updated" {
		t.Fatalf("unexpected changes: %v", changes)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	aliasIdx := strings.Index(s, "Host dev-api-cc-clip-macbook")
	wildcardIdx := strings.Index(s, "Host dev-*")
	if aliasIdx < 0 || wildcardIdx < 0 {
		t.Fatalf("expected alias and wildcard blocks, got:\n%s", s)
	}
	if aliasIdx >= wildcardIdx {
		t.Fatalf("expected existing alias block to move before matching wildcard, got:\n%s", s)
	}
}

func TestEnsureManagedAliasConfigAtPreservesBaseRemoteForwards(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    RemoteForward 2200 127.0.0.1:22",
		"    RemoteForward 9000 127.0.0.1:9000",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return nil, errors.New("ssh unavailable")
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"RemoteForward 2200 127.0.0.1:22",
		"RemoteForward 9000 127.0.0.1:9000",
		"RemoteForward 18340 127.0.0.1:18339",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}

func TestEnsureManagedAliasConfigAtDropsInheritedConflictingRemoteForward(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"    RemoteForward 18340 127.0.0.1:22",
		"    RemoteForward 2200 127.0.0.1:22",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return nil, errors.New("ssh unavailable")
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	start := strings.Index(s, "# >>> cc-clip managed: myserver-cc-clip-macbook >>>")
	end := strings.Index(s, "# <<< cc-clip managed: myserver-cc-clip-macbook <<<")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("expected managed alias block, got:\n%s", s)
	}
	aliasBlock := s[start:end]
	if strings.Contains(aliasBlock, "RemoteForward 18340 127.0.0.1:22") {
		t.Fatalf("expected conflicting inherited RemoteForward to be removed from alias block, got:\n%s", aliasBlock)
	}
	if strings.Count(aliasBlock, "RemoteForward 18340") != 1 {
		t.Fatalf("expected exactly one RemoteForward on port 18340 in alias block, got:\n%s", aliasBlock)
	}
	if !strings.Contains(s, "RemoteForward 2200 127.0.0.1:22") {
		t.Fatalf("expected unrelated RemoteForward to be preserved, got:\n%s", s)
	}
}

func TestEnsureManagedAliasConfigAtDropsInheritedLegacyCCClipRemoteForward(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return []sshDirective{
			{key: "hostname", value: "10.0.0.1"},
			{key: "remoteforward", value: "18339 127.0.0.1:18339"},
			{key: "remoteforward", value: "2200 127.0.0.1:22"},
		}, nil
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	start := strings.Index(s, "# >>> cc-clip managed: myserver-cc-clip-macbook >>>")
	end := strings.Index(s, "# <<< cc-clip managed: myserver-cc-clip-macbook <<<")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("expected managed alias block, got:\n%s", s)
	}
	aliasBlock := s[start:end]
	if strings.Contains(aliasBlock, "RemoteForward 18339 127.0.0.1:18339") {
		t.Fatalf("expected legacy cc-clip RemoteForward to be removed from alias block, got:\n%s", aliasBlock)
	}
	if !strings.Contains(aliasBlock, "RemoteForward 18340 127.0.0.1:18339") {
		t.Fatalf("expected managed alias RemoteForward, got:\n%s", aliasBlock)
	}
	if !strings.Contains(aliasBlock, "RemoteForward 2200 127.0.0.1:22") {
		t.Fatalf("expected unrelated RemoteForward to be preserved, got:\n%s", aliasBlock)
	}
}

func TestEnsureManagedAliasConfigAtDropsInheritedPeerRangeLocalhostForwardAfterPortChange(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")

	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return []sshDirective{
			{key: "hostname", value: "10.0.0.1"},
			{key: "remoteforward", value: "18339 127.0.0.1:19999"},
			{key: "remoteforward", value: "2200 127.0.0.1:22"},
		}, nil
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	start := strings.Index(s, "# >>> cc-clip managed: myserver-cc-clip-macbook >>>")
	end := strings.Index(s, "# <<< cc-clip managed: myserver-cc-clip-macbook <<<")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("expected managed alias block, got:\n%s", s)
	}
	aliasBlock := s[start:end]
	if strings.Contains(aliasBlock, "RemoteForward 18339 127.0.0.1:19999") {
		t.Fatalf("expected peer-range localhost RemoteForward to be removed from alias block, got:\n%s", aliasBlock)
	}
	if !strings.Contains(aliasBlock, "RemoteForward 18340 127.0.0.1:18339") {
		t.Fatalf("expected managed alias RemoteForward, got:\n%s", aliasBlock)
	}
	if !strings.Contains(aliasBlock, "RemoteForward 2200 127.0.0.1:22") {
		t.Fatalf("expected unrelated RemoteForward to be preserved, got:\n%s", aliasBlock)
	}
}

func TestEnsureManagedAliasConfigAtRejectsUnmanagedAliasBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver-cc-clip-macbook",
		"    HostName 10.0.0.1",
		"    RemoteCommand /bin/bash",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err == nil || !strings.Contains(err.Error(), "existing unmanaged Host entry") {
		t.Fatalf("expected unmanaged alias conflict, got %v", err)
	}
}

func TestEnsureManagedAliasConfigAtRejectsDuplicateUnmanagedAliasAlongsideManagedBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"# >>> cc-clip managed: myserver-cc-clip-macbook >>>",
		"Host myserver-cc-clip-macbook",
		"    HostName 10.0.0.1",
		"# <<< cc-clip managed: myserver-cc-clip-macbook <<<",
		"",
		"Host myserver-cc-clip-macbook",
		"    HostName other.example.com",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err == nil || !strings.Contains(err.Error(), "existing unmanaged Host entry") {
		t.Fatalf("expected duplicate unmanaged alias conflict, got %v", err)
	}
}

func TestRemoveManagedAliasConfigAt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"",
		"# >>> cc-clip managed: myserver-cc-clip-macbook >>>",
		"Host myserver-cc-clip-macbook",
		"    HostName 10.0.0.1",
		"# <<< cc-clip managed: myserver-cc-clip-macbook <<<",
		"",
	}, "\n")
	os.WriteFile(configPath, []byte(initial), 0644)

	if err := removeManagedAliasConfigAt(configPath, "myserver-cc-clip-macbook"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	if strings.Contains(s, "myserver-cc-clip-macbook") {
		t.Fatalf("expected alias block removal, got:\n%s", s)
	}
	if !strings.Contains(s, "Host myserver") {
		t.Fatalf("expected base host preserved, got:\n%s", s)
	}
}

func TestEnsureManagedAliasConfigAtUsesResolvedSSHConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		if host != "myserver" {
			t.Fatalf("unexpected host %q", host)
		}
		return []sshDirective{
			{key: "hostname", value: "10.0.0.1"},
			{key: "user", value: "admin"},
			{key: "proxyjump", value: "bastion"},
			{key: "identityfile", value: "~/.ssh/id_test"},
		}, nil
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"HostName 10.0.0.1",
		"User admin",
		"ProxyJump bastion",
		"IdentityFile ~/.ssh/id_test",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}

func TestEnsureManagedAliasConfigAtPreservesHostKeyDirectivesFromResolvedConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return []sshDirective{
			{key: "hostname", value: "10.0.0.1"},
			{key: "hostkeyalias", value: "prod-host"},
			{key: "stricthostkeychecking", value: "yes"},
			{key: "userknownhostsfile", value: "~/.ssh/known_hosts_prod"},
			{key: "globalknownhostsfile", value: "/etc/ssh/work_known_hosts"},
		}, nil
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "myserver",
		Alias:      "myserver-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"HostKeyAlias prod-host",
		"StrictHostKeyChecking yes",
		"UserKnownHostsFile ~/.ssh/known_hosts_prod",
		"GlobalKnownHostsFile /etc/ssh/work_known_hosts",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}

func TestEnsureManagedAliasConfigAtSplitsUserAtHostFallback(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	oldResolver := resolveManagedAliasDirectives
	resolveManagedAliasDirectives = func(configPath, host string) ([]sshDirective, error) {
		return nil, errors.New("ssh unavailable")
	}
	defer func() { resolveManagedAliasDirectives = oldResolver }()

	_, err := ensureManagedAliasConfigAt(configPath, ManagedAliasSpec{
		BaseHost:   "alice@example.com",
		Alias:      "example-cc-clip-macbook",
		RemotePort: 18340,
		LocalPort:  18339,
		PeerID:     "peer-abc",
		PeerLabel:  "macbook",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(configPath)
	s := string(content)
	for _, needle := range []string{
		"HostName example.com",
		"User alice",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("expected config to contain %q, got:\n%s", needle, s)
		}
	}
}
