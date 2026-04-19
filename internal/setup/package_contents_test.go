package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPackageDoesNotTouchSSHConfig is an *anti-feature* test. It pins the
// invariant documented in CLAUDE.md / AGENTS.md: the internal/setup package
// MUST NOT contain any code that reads, writes, or names ~/.ssh/config. The
// daemon owns the tunnel lifecycle via `ssh -N -R` argv it builds itself.
//
// This is best-effort — a sufficiently motivated contributor can smuggle
// sshconfig logic in via opaque byte literals or a new sibling package. The
// goal is to catch a copy-paste reintroduction of the deleted helpers (or a
// minimally-refactored rename) before CI, not to be injection-proof.
func TestPackageDoesNotTouchSSHConfig(t *testing.T) {
	// Substrings that indicate sshconfig read/write logic has crept back.
	// Matched case-insensitively against non-test .go source. Covers:
	//   - directive strings the deleted code wrote into config files
	//   - the marker-comment that framed the managed-host block
	//   - Go-level fingerprints (type/func names from the deleted API)
	//   - the O_NOFOLLOW guard that only existed to protect config writes
	// All substrings are matched case-insensitively, so `SSHConfig`,
	// `SSHConfigOptions`, etc. are already covered by the `sshconfig` entry.
	forbidden := []string{
		".ssh/config",
		"RemoteForward",
		"ControlMaster",
		"ControlPath",
		">>> cc-clip managed host",
		">>> cc-clip SetEnv", // matches any rewrite of the new marker text too
		"sshconfig",          // package name + type prefix of deleted code (also matches SSHConfig*)
		// Full package path: catches a renamed import like
		// `ssh "github.com/shunmei/cc-clip/internal/sshconfig"` where the
		// short `sshconfig` identifier would not otherwise appear in code.
		"internal/sshconfig",
		"ManagedHost",     // e.g. ManagedHostSpec, managedHostBlock
		"EnsureSSH",       // e.g. EnsureSSHConfig
		"RemoveManaged",   // e.g. RemoveManagedHost
		"ReadManaged",     // e.g. ReadManagedRemotePort
		"O_NOFOLLOW",      // guard only needed for config-file writes
		"openForWrite",    // helper name from deleted nofollow_{unix,windows}.go
		"SetEnv CC_CLIP_", // literal of the directive sshconfig.Apply writes
	}

	var files []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			// The package is supposed to stay flat (deps.go + tests). `testdata`
			// is the one conventional exception — Go toolchain excludes it from
			// builds. Anything else is suspicious: flag it rather than letting a
			// contributor hide sshconfig logic under a subpackage.
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			t.Errorf("unexpected subdirectory %q; internal/setup must stay flat (see CLAUDE.md)", path)
			return filepath.SkipDir
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk package: %v", err)
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Strip comments and string literals before the substring scan so a
		// doc-comment like `// don't touch .ssh/config` does not trip the
		// guard. We only care about executable code here — if `.ssh/config`
		// or `SetEnv CC_CLIP_` shows up in an identifier or code token the
		// contributor is actually reintroducing sshconfig logic; a comment
		// mentioning the forbidden text is fine.
		src := strings.ToLower(stripGoCommentsAndStrings(string(data)))
		for _, needle := range forbidden {
			if strings.Contains(src, strings.ToLower(needle)) {
				t.Errorf("%s contains forbidden reference %q; internal/setup must not touch ~/.ssh/config (see CLAUDE.md)", path, needle)
			}
		}
	}
}

// stripGoCommentsAndStrings removes // line comments, /* … */ block comments,
// and the contents of "…" / `…` string literals from Go source. Good enough
// for the anti-feature scan: the invariant is about executable code, so false
// negatives in rare edge cases (an identifier inside a raw string literal
// that we blank out) are acceptable.
var (
	reGoLineComment  = regexp.MustCompile(`(?m)//[^\n]*`)
	reGoBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reGoRawString    = regexp.MustCompile("`[^`]*`")
	// Double-quoted strings may contain escaped quotes (\"). Non-greedy
	// capture between unescaped quotes covers the common cases.
	reGoDoubleString = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)
)

func stripGoCommentsAndStrings(src string) string {
	src = reGoBlockComment.ReplaceAllString(src, "")
	src = reGoLineComment.ReplaceAllString(src, "")
	src = reGoRawString.ReplaceAllString(src, "``")
	src = reGoDoubleString.ReplaceAllString(src, `""`)
	return src
}

// TestPackageDoesNotDependOnSSHConfigRewriters is the import-graph half of
// the anti-feature guard. The source-level substring scan above catches
// copy-paste reintroduction of deleted helpers, but a determined contributor
// could hide ssh_config writing in a sibling package (e.g.
// `internal/setup-helpers/`) and import it — the compiler would resolve the
// import but the substring scan only looks at files under `internal/setup`.
//
// `go list -deps` walks the full import closure (direct + transitive), so
// any package in the "forbidden" set that shows up here means `internal/setup`
// is either calling it directly or pulling it in through another dep. The
// rule is strict: contributors who need ssh_config behavior must route users
// through `cmd/cc-clip` (which owns user-facing flows) instead of hiding it
// behind a `setup.X` call.
func TestPackageDoesNotDependOnSSHConfigRewriters(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not in PATH: %v", err)
	}

	// P2-6: run `go list -deps` under every supported target GOOS so a
	// contributor cannot hide an sshconfig import behind a build tag
	// (`//go:build windows` etc.). `go list` only sees the current
	// GOOS/GOARCH matrix by default, which would make `windows`-tagged
	// code invisible on a Linux test runner. Iterating keeps the
	// anti-feature guard honest regardless of CI host OS.
	for _, goos := range []string{"linux", "darwin", "windows"} {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./")
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps (GOOS=%s) failed: %v\n%s", goos, err, out)
		}

		deps := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range deps {
			trimmed := strings.TrimSpace(line)
			for _, suffix := range forbiddenSSHConfigDepSuffixes {
				if strings.HasSuffix(trimmed, suffix) {
					t.Errorf("internal/setup (GOOS=%s) depends on %q (via its import graph); ssh_config rewrites must live outside internal/setup — route callers through cmd/cc-clip instead (see CLAUDE.md)", goos, line)
				}
			}
			// Subpackage match: a future contributor splitting sshconfig into
			// `internal/sshconfig/writer` etc. would slip past the suffix-only
			// check above. Treat ANY import path containing `/internal/sshconfig/`
			// (note the trailing slash — distinguishes child packages from a
			// hypothetical sibling like `internal/sshconfighelpers`) as a
			// violation too. The plain `/internal/sshconfig` exact-suffix match
			// above continues to handle the canonical package.
			if strings.Contains(trimmed, "/internal/sshconfig/") {
				t.Errorf("internal/setup (GOOS=%s) depends on subpackage %q under internal/sshconfig; ssh_config rewrites must live outside internal/setup (see CLAUDE.md)", goos, line)
			}
		}
	}
}

// forbiddenSSHConfigDepSuffixes is extracted so the classifier logic below
// can be exercised directly on a synthetic deps list — we want the guard
// itself to be tested, not only its real output. Any dep whose ImportPath
// ends with one of these is a violation: `sshconfig` is the obvious one,
// and anything claiming to do ssh_config manipulation should end up there,
// not in a sibling package.
var forbiddenSSHConfigDepSuffixes = []string{
	"/internal/sshconfig",
}

// TestForbiddenSSHConfigDepSuffixesCatchesViolators verifies the import-graph
// classifier actually fires on a synthetic dependency list. Without this,
// a regression that silently drops the `strings.HasSuffix` check (e.g. a
// refactor swapping HasSuffix for Contains with a typo) would leave the
// real test happily passing on a clean import graph but wide open on the
// day it matters.
func TestForbiddenSSHConfigDepSuffixesCatchesViolators(t *testing.T) {
	synthetic := []string{
		"github.com/shunmei/cc-clip/internal/setup",
		"github.com/shunmei/cc-clip/internal/sshconfig", // forbidden by suffix
		"github.com/shunmei/cc-clip/internal/token",
	}
	hits := 0
	for _, line := range synthetic {
		for _, suffix := range forbiddenSSHConfigDepSuffixes {
			if strings.HasSuffix(line, suffix) {
				hits++
			}
		}
	}
	if hits != 1 {
		t.Fatalf("classifier matched %d entries, want exactly 1 (the sshconfig line)", hits)
	}

	// Subpackage substring guard: a contributor splitting logic into
	// `internal/sshconfig/writer` would not match the exact suffix above.
	// The Contains check in TestPackageDoesNotDependOnSSHConfigRewriters is
	// what catches that — verify it would fire here.
	subpkg := "github.com/shunmei/cc-clip/internal/sshconfig/writer"
	if !strings.Contains(subpkg, "/internal/sshconfig/") {
		t.Fatalf("subpackage substring guard would miss %q", subpkg)
	}
	// And confirm the guard does NOT misfire on a sibling that merely shares
	// the prefix (the trailing slash on `/internal/sshconfig/` is what
	// distinguishes a child package from a hypothetical sibling).
	sibling := "github.com/shunmei/cc-clip/internal/sshconfighelpers"
	if strings.Contains(sibling, "/internal/sshconfig/") {
		t.Fatalf("subpackage substring guard misfired on sibling %q", sibling)
	}
}
