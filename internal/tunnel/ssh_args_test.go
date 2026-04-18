package tunnel

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateSSHHost(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "plain alias", host: "myhost", wantErr: false},
		{name: "dotted hostname", host: "example.internal", wantErr: false},
		{name: "user@host", host: "user@host.example", wantErr: false},
		{name: "host:port", host: "host.example:2222", wantErr: false},
		{name: "underscore and dash", host: "my_host-01", wantErr: false},
		{name: "empty rejected", host: "", wantErr: true},
		{name: "leading dash flag-injection", host: "-oProxyCommand=evil", wantErr: true},
		{name: "just dash", host: "-", wantErr: true},
		{name: "embedded space", host: "host name", wantErr: true},
		{name: "tab", host: "host\tname", wantErr: true},
		{name: "newline", host: "host\nname", wantErr: true},
		{name: "NUL byte", host: "host\x00name", wantErr: true},
		{name: "control char", host: "host\x07bell", wantErr: true},
		{name: "shell metacharacter", host: "host;rm -rf /", wantErr: true},
		{name: "pipe", host: "host|evil", wantErr: true},
		{name: "backtick", host: "host`evil`", wantErr: true},
		{name: "dollar", host: "host$evil", wantErr: true},
		{name: "slash", host: "host/evil", wantErr: true},
		{name: "too long", host: strings.Repeat("a", maxSSHHostLength+1), wantErr: true},
		{name: "at max length", host: strings.Repeat("a", maxSSHHostLength), wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSSHHost(tt.host)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateSSHHost(%q) = nil, want error", tt.host)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateSSHHost(%q) = %v, want nil", tt.host, err)
			}
		})
	}
}

func TestSSHTunnelArgsRejectsInvalidHost(t *testing.T) {
	cfg := TunnelConfig{
		Host:              "-oProxyCommand=evil",
		LocalPort:         18339,
		RemotePort:        19001,
		SSHConfigResolved: true,
	}

	// Even if sshConfigQueryFunc is stubbed, argv construction should
	// refuse a host that would be interpreted as a flag before ever
	// invoking the query function.
	stubCalls := 0
	prev := sshConfigQueryFunc
	sshConfigQueryFunc = func(ctx context.Context, host string) (string, error) {
		stubCalls++
		return "", nil
	}
	t.Cleanup(func() { sshConfigQueryFunc = prev })

	args, cleanup, err := sshTunnelArgs(context.Background(), cfg)
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("sshTunnelArgs = %v, want validation error", args)
	}
	if !strings.Contains(err.Error(), "invalid ssh host") {
		t.Fatalf("err = %v, want 'invalid ssh host'", err)
	}
	if stubCalls != 0 {
		t.Fatalf("stubCalls = %d, want 0 (validation should short-circuit)", stubCalls)
	}
}

func TestResolveSSHTunnelConfigUsesConfigQueryStub(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "example",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	prev := sshConfigQueryFunc
	sshConfigQueryFunc = func(ctx context.Context, host string) (string, error) {
		if host != "example" {
			t.Fatalf("host = %q, want example", host)
		}
		return "hostname example.internal\nuser demo\n", nil
	}
	t.Cleanup(func() { sshConfigQueryFunc = prev })

	cfg, err := resolveSSHTunnelConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveSSHTunnelConfig: %v", err)
	}
	if !cfg.SSHConfigResolved {
		t.Fatal("expected ssh config to be marked resolved")
	}
	joined := strings.Join(cfg.SSHOptions, " ")
	if !strings.Contains(joined, "hostname=example.internal") {
		t.Fatalf("options missing stubbed hostname: %v", cfg.SSHOptions)
	}
	if !strings.Contains(joined, "user=demo") {
		t.Fatalf("options missing stubbed user: %v", cfg.SSHOptions)
	}
}

func TestResolveSSHTunnelConfigPropagatesConfigQueryError(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "example",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	stubErr := errors.New("ssh -G failed: boom")
	prev := sshConfigQueryFunc
	sshConfigQueryFunc = func(ctx context.Context, host string) (string, error) {
		return "", stubErr
	}
	t.Cleanup(func() { sshConfigQueryFunc = prev })

	_, err := resolveSSHTunnelConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("resolveSSHTunnelConfig = nil, want error from stub")
	}
	if !errors.Is(err, stubErr) {
		t.Fatalf("err = %v, want stub error to propagate", err)
	}
}

// TestResolveSSHTunnelConfigReQueriesWhenResolvedFalse pins the contract
// that the /tunnels/up HTTP handler relies on: a cfg with
// SSHConfigResolved=false always re-runs `ssh -G`, even when the caller
// previously stashed options. This is what makes `cc-clip tunnel up <host>`
// the canonical "pick up my latest ~/.ssh/config" command after an edit.
//
// Breaking this would re-introduce the "fixed my ssh config but the tunnel
// still uses stale HostName/User/IdentityFile" class of bug. The reconnect
// loop intentionally does NOT re-resolve (cache is load-bearing for
// attack-surface pinning and performance) — only user-initiated `tunnel up`
// refreshes.
func TestResolveSSHTunnelConfigReQueriesWhenResolvedFalse(t *testing.T) {
	cfg := TunnelConfig{
		Host:              "example",
		LocalPort:         18339,
		RemotePort:        19001,
		SSHOptions:        []string{"hostname=stale.example.com", "user=olduser"},
		SSHConfigResolved: false, // explicit: fresh cfg from HTTP handler
	}

	var queryCalls int
	prev := sshConfigQueryFunc
	sshConfigQueryFunc = func(ctx context.Context, host string) (string, error) {
		queryCalls++
		return "hostname fresh.example.com\nuser newuser\n", nil
	}
	t.Cleanup(func() { sshConfigQueryFunc = prev })

	got, err := resolveSSHTunnelConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveSSHTunnelConfig: %v", err)
	}
	if queryCalls != 1 {
		t.Fatalf("queryCalls = %d, want 1 (fresh cfg must re-run ssh -G even when SSHOptions is populated)", queryCalls)
	}
	joined := strings.Join(got.SSHOptions, " ")
	if strings.Contains(joined, "stale.example.com") {
		t.Fatalf("SSHOptions still contains stale hostname: %v", got.SSHOptions)
	}
	if !strings.Contains(joined, "hostname=fresh.example.com") {
		t.Fatalf("SSHOptions missing fresh hostname: %v", got.SSHOptions)
	}
	if !got.SSHConfigResolved {
		t.Fatal("SSHConfigResolved = false after successful resolve")
	}
}

// TestResolveSSHTunnelConfigSkipsQueryWhenResolvedTrue pins the
// complementary contract: the reconnect loop (which holds a cached cfg
// with SSHConfigResolved=true) never triggers an `ssh -G` subprocess.
// Breaking this would run ssh -G on every network flap, defeating both
// the performance cache and the attack-surface pin described in
// resolveSSHTunnelConfig.
func TestResolveSSHTunnelConfigSkipsQueryWhenResolvedTrue(t *testing.T) {
	cfg := TunnelConfig{
		Host:              "example",
		LocalPort:         18339,
		RemotePort:        19001,
		SSHOptions:        []string{"hostname=cached.example.com"},
		SSHConfigResolved: true,
	}

	prev := sshConfigQueryFunc
	sshConfigQueryFunc = func(ctx context.Context, host string) (string, error) {
		t.Fatalf("sshConfigQueryFunc must not be called when cfg.SSHConfigResolved=true (host=%s)", host)
		return "", nil
	}
	t.Cleanup(func() { sshConfigQueryFunc = prev })

	got, err := resolveSSHTunnelConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveSSHTunnelConfig: %v", err)
	}
	joined := strings.Join(got.SSHOptions, " ")
	if !strings.Contains(joined, "hostname=cached.example.com") {
		t.Fatalf("cached SSHOptions were mutated: %v", got.SSHOptions)
	}
}

// TestSSHTunnelArgsConfigPathMatchesManagedMatcher pins the invariant that
// the `-F` config file produced by sshTunnelArgs has a basename the process
// matcher recognizes as cc-clip-managed. Breaking it silently defeats
// stale-PID cleanup, adoption, and duplicate-spawn prevention.
func TestSSHTunnelArgsConfigPathMatchesManagedMatcher(t *testing.T) {
	cfg := TunnelConfig{
		Host:              "alias-host",
		LocalPort:         18339,
		RemotePort:        19001,
		SSHOptions:        []string{"hostname=example.internal"},
		SSHConfigResolved: true,
	}
	args, cleanup, err := sshTunnelArgs(context.Background(), cfg)
	if err != nil {
		t.Fatalf("sshTunnelArgs err = %v", err)
	}
	t.Cleanup(cleanup)

	var configPath string
	for i, a := range args {
		if a == "-F" && i+1 < len(args) {
			configPath = args[i+1]
			break
		}
	}
	if configPath == "" {
		t.Fatalf("sshTunnelArgs did not emit -F <path>: %v", args)
	}
	if !isManagedSSHConfigPath(configPath) {
		t.Fatalf("isManagedSSHConfigPath(%q) = false; basename %q must match cc-clip-ssh-config-*.conf so matchesManagedTunnelProcess can find the process", configPath, filepath.Base(configPath))
	}
	// Full-matcher roundtrip: build a cmdline the way /proc/<pid>/cmdline
	// would present it and confirm matchesManagedTunnelProcess accepts it.
	cmdline := "/usr/bin/ssh\x00" + strings.Join(args, "\x00")
	if !matchesTunnelProcess(cmdline, cfg) {
		t.Fatalf("matchesTunnelProcess rejected its own argv: %v", args)
	}
}

func TestSSHTunnelArgsRequiresResolvedConfig(t *testing.T) {
	stubCalls := 0
	prev := sshConfigQueryFunc
	sshConfigQueryFunc = func(ctx context.Context, host string) (string, error) {
		stubCalls++
		return "", nil
	}
	t.Cleanup(func() { sshConfigQueryFunc = prev })

	_, cleanup, err := sshTunnelArgs(context.Background(), TunnelConfig{
		Host:       "example",
		LocalPort:  18339,
		RemotePort: 19001,
	})
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("sshTunnelArgs = nil, want unresolved-options error")
	}
	if !errors.Is(err, ErrTunnelSSHOptionsUnresolved) {
		t.Fatalf("err = %v, want ErrTunnelSSHOptionsUnresolved", err)
	}
	if stubCalls != 0 {
		t.Fatalf("stubCalls = %d, want 0", stubCalls)
	}
}

func TestRequireTrustedSSHBinaryPrefixAcceptsWindowsSystemSSH(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only path policy")
	}
	if err := requireTrustedSSHBinaryPrefix(`C:\Windows\System32\OpenSSH\ssh.exe`); err != nil {
		t.Fatalf("requireTrustedSSHBinaryPrefix: %v", err)
	}
}

func TestBuildSSHTunnelArgsFiltersForwardsFromResolvedConfig(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "alias-host",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	resolved := strings.Join([]string{
		"host alias-host",
		"hostname example.internal",
		"user demo",
		"port 2222",
		"identityfile ~/.ssh/id_ed25519",
		"proxyjump bastion",
		"localforward 8080 [127.0.0.1]:8080",
		"remoteforward 18080 [127.0.0.1]:18080",
		"dynamicforward 1080",
		"requesttty auto",
		"remotecommand none",
	}, "\n")

	cfg.SSHOptions = resolvedTunnelOptionsFromSSHConfig(resolved)
	cfg.SSHConfigResolved = true
	args := buildSSHTunnelArgs(cfg, "/tmp/empty-ssh-config")
	joined := strings.Join(args, " ")

	for _, unexpected := range []string{
		"-o host=alias-host",
		"localforward",
		"remoteforward 18080",
		"dynamicforward",
		"requesttty",
		"remotecommand",
	} {
		if strings.Contains(strings.ToLower(joined), unexpected) {
			t.Fatalf("ssh args unexpectedly preserved %q: %v", unexpected, args)
		}
	}

	for _, expected := range []string{
		"-F /tmp/empty-ssh-config",
		"-o hostname=example.internal",
		"-o user=demo",
		"-o port=2222",
		"-o identityfile=~/.ssh/id_ed25519",
		"-o proxyjump=bastion",
		"-R 19001:127.0.0.1:18339",
		"alias-host",
	} {
		if !strings.Contains(strings.ToLower(joined), strings.ToLower(expected)) {
			t.Fatalf("ssh args missing %q: %v", expected, args)
		}
	}
}

func TestBuildSSHTunnelArgsOverridesControlMasterAndBatchMode(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "example",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	resolved := strings.Join([]string{
		"hostname example.com",
		"batchmode no",
		"controlmaster auto",
		"controlpath ~/.ssh/master-%r@%h:%p",
		"exitonforwardfailure no",
		"serveraliveinterval 0",
		"serveralivecountmax 99",
	}, "\n")

	cfg.SSHOptions = resolvedTunnelOptionsFromSSHConfig(resolved)
	cfg.SSHConfigResolved = true
	args := buildSSHTunnelArgs(cfg, "/tmp/empty-ssh-config")
	joined := strings.Join(args, " ")

	if strings.Contains(strings.ToLower(joined), "controlmaster=auto") {
		t.Fatalf("ssh args should not preserve controlmaster from resolved config: %v", args)
	}
	if strings.Contains(strings.ToLower(joined), "controlpath=~/.ssh/master-%r@%h:%p") {
		t.Fatalf("ssh args should not preserve controlpath from resolved config: %v", args)
	}
	for _, unexpected := range []string{
		"-o batchmode=no",
		"-o exitonforwardfailure=no",
		"-o serveraliveinterval=0",
		"-o serveralivecountmax=99",
	} {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(unexpected)) {
			t.Fatalf("ssh args should not preserve %q from resolved config: %v", unexpected, args)
		}
	}

	for _, expected := range []string{
		"-o BatchMode=yes",
		"-o ExitOnForwardFailure=yes",
		"-o ServerAliveInterval=15",
		"-o ServerAliveCountMax=3",
		"-o ControlMaster=no",
		"-o ControlPath=none",
		"-v",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("ssh args missing %q: %v", expected, args)
		}
	}
}
