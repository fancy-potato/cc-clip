package tunnel

import (
	"fmt"
	"strings"
)

// matchesTunnelProcess checks whether a process command line looks like an
// SSH tunnel that this cc-clip manager launched. Match is deliberately
// strict: the invocation must carry the `-F <cc-clip-ssh-config-*>` anchor,
// the `-N`/`-v` flags, and every required `-o` option that sshTunnelArgs
// emits. Cleanup callers rely on this strictness: a loose match would let
// a PID recycled to an unrelated `ssh -N -R …` invocation be killed as if
// it were ours.
func matchesTunnelProcess(cmdline string, cfg TunnelConfig) bool {
	fields := splitCommandLineTokens(cmdline)
	if len(fields) == 0 {
		return false
	}
	switch normalizedProcessToken(fields[0]) {
	case "ssh", "ssh.exe":
	default:
		return false
	}
	return matchesManagedTunnelProcess(fields, cfg)
}

func matchesManagedTunnelProcess(fields []string, cfg TunnelConfig) bool {
	// The cmdline must end in `-R <expected-spec> <host>`. Anchoring on the
	// tail makes the match robust against reordering or extra `-o key=value`
	// options earlier in the argv — e.g. a future ssh client that emits an
	// extra option, or a wrapper that prepends user-defined `-o` flags. The
	// required set of cc-clip-emitted options must still all be present
	// somewhere before the tail; we just no longer demand them in a fixed
	// order or count.
	if len(fields) < 4 {
		return false
	}
	expected := fmt.Sprintf("%d:127.0.0.1:%d", cfg.RemotePort, cfg.LocalPort)
	tail := fields[len(fields)-3:]
	if strings.Trim(tail[0], `"'`) != "-R" {
		return false
	}
	if strings.Trim(tail[1], `"'`) != expected {
		return false
	}
	if strings.Trim(tail[2], `"'`) != cfg.Host {
		return false
	}

	body := fields[1 : len(fields)-3]
	if !managedTunnelArgsContain(body, "-N") || !managedTunnelArgsContain(body, "-v") {
		return false
	}
	// Require the `-F <cc-clip-ssh-config-*>` prefix cc-clip emits from
	// sshTunnelArgs. A user-started ssh with the same -o options and -R
	// spec would otherwise be mis-adopted as cc-clip-managed.
	if !managedTunnelArgsHaveCCClipConfigPath(body) {
		return false
	}
	requiredOpts := []string{
		"BatchMode=yes",
		"ExitOnForwardFailure=yes",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
		"ControlMaster=no",
		"ControlPath=none",
	}
	seen := collectDashOValues(body)
	for _, want := range requiredOpts {
		if !seen[want] {
			return false
		}
	}
	return true
}

func managedTunnelArgsContain(body []string, want string) bool {
	for _, f := range body {
		if strings.Trim(f, `"'`) == want {
			return true
		}
	}
	return false
}

// managedTunnelArgsHaveCCClipConfigPath looks for the `-F <path>` pair that
// sshTunnelArgs always emits where the path's basename matches the exact
// `cc-clip-ssh-config-*.conf` shape `os.CreateTemp` produces. Matching on the
// basename (rather than a free-form substring) keeps the check robust across
// tempdir variation while refusing paths that merely happen to contain the
// marker somewhere (e.g. `/tmp/my-cc-clip-ssh-config-notes.txt` or a
// directory named `cc-clip-ssh-config-scratch/`). The basename must start
// with the literal prefix and carry a `.conf` extension so a random
// attacker-controlled file cannot pose as the managed config.
func managedTunnelArgsHaveCCClipConfigPath(body []string) bool {
	for i := 0; i < len(body)-1; i++ {
		if strings.Trim(body[i], `"'`) != "-F" {
			continue
		}
		raw := strings.Trim(body[i+1], `"'`)
		if isManagedSSHConfigPath(raw) {
			return true
		}
	}
	return false
}

// isManagedSSHConfigPath reports whether path looks like a file
// `os.CreateTemp(tmpDir, "cc-clip-ssh-config-*.conf")` would have produced:
// basename starts with `cc-clip-ssh-config-`, has at least one byte between
// the prefix and the `.conf` suffix, and ends with `.conf`. Splits on both
// `/` and `\` regardless of GOOS so Unix ps output and Windows CIM output
// both resolve to the same basename on either test host.
//
// The "at least one byte" check rejects `cc-clip-ssh-config-.conf` (empty
// random middle). Real `os.CreateTemp` always substitutes a non-empty
// random token, so a path matching the empty-middle shape could only come
// from a hostile neighbor trying to self-identify as cc-clip-owned.
func isManagedSSHConfigPath(raw string) bool {
	if raw == "" {
		return false
	}
	base := raw
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	const prefix = "cc-clip-ssh-config-"
	const suffix = ".conf"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
		return false
	}
	return len(base) > len(prefix)+len(suffix)
}

func collectDashOValues(body []string) map[string]bool {
	seen := make(map[string]bool)
	for i := 0; i < len(body)-1; i++ {
		if strings.Trim(body[i], `"'`) == "-o" {
			seen[strings.Trim(body[i+1], `"'`)] = true
		}
	}
	return seen
}

func splitCommandLineTokens(cmdline string) []string {
	// When the input carries NUL bytes it came from a source that preserves
	// argv boundaries exactly (e.g. /proc/<pid>/cmdline on Linux). In that
	// case use NUL as the *only* separator so argv tokens that themselves
	// contain whitespace (a `-F` tempfile path under a home directory with a
	// space, an ssh alias with quoted spaces) survive tokenization intact.
	// Falling through to the whitespace tokenizer would re-split such tokens
	// and break the `-F` / host anchor checks.
	if strings.ContainsRune(cmdline, 0) {
		parts := strings.Split(cmdline, "\x00")
		tokens := make([]string, 0, len(parts))
		for _, p := range parts {
			if p == "" {
				continue
			}
			tokens = append(tokens, p)
		}
		return tokens
	}

	var (
		tokens  []string
		current strings.Builder
		quote   rune
	)

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, r := range cmdline {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return tokens
}
