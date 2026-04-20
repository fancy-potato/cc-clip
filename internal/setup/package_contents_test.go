package setup

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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
	//
	// IMPORTANT for future contributors: identifiers like `O_NOFOLLOW` and
	// `openForWrite` are on this list because they were the exact names used
	// by the deleted ssh-config writer. If you are adding an UNRELATED
	// nofollow helper to `internal/setup` (say, a generic hardened-open for
	// some new non-ssh-config file), do NOT reuse these names — pick a
	// different identifier, OR remove the stale entry here and document the
	// rationale in the commit message. This list is a spelling-based guard;
	// collisions with unrelated features silently expand its blast radius.
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
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		// Only this file (the anti-feature test itself) is allowed to
		// mention the forbidden substrings: that is where they are listed.
		// Scan every other file — production AND test — because test files
		// can equally well shell out or use os-level APIs to rewrite
		// ~/.ssh/config. A previous revision skipped `_test.go`, which
		// left a trivial bypass for a regression: `helper_test.go` that
		// exec's `sed -i /...ssh/config` would have passed silently.
		if name == "package_contents_test.go" {
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
		stripped, scanErrs := stripGoCommentsAndStringsWithErrors(string(data))
		if len(scanErrs) > 0 {
			// A syntax error in a scanned file is fatal to this
			// anti-feature test: without a clean tokenization, the
			// scanner silently discards tokens past the error point,
			// giving a contributor a free window to smuggle
			// `sshconfig` identifiers into a file that happens to
			// have a mid-edit typo. Surface the first error and
			// instruct the contributor to fix the syntax before
			// the guard can run.
			t.Errorf("%s: fix syntax error(s) before anti-feature scan can run: %v", path, scanErrs[0])
			continue
		}
		src := strings.ToLower(stripped)
		for _, needle := range forbidden {
			if strings.Contains(src, strings.ToLower(needle)) {
				t.Errorf("%s contains forbidden reference %q; internal/setup must not touch ~/.ssh/config (see CLAUDE.md)", path, needle)
			}
		}
	}
}

// stripGoCommentsAndStrings removes // line comments, /* … */ block comments,
// and the contents of "…" / `…` / '…' string/rune literals from Go source,
// leaving only executable tokens (identifiers, operators, keywords,
// numeric literals). Previous revisions used an ordered sequence of regexps
// (strip comments, then strings). That was unsafe because a line like
//
//	foo := "http://x"; bar := ".ssh/config"
//
// would have its `//x"; bar := ".ssh/config` range eaten by the line-comment
// regex before the string-literal regex ever got to it — bypassing the
// `.ssh/config` guard entirely. Using `go/scanner` gives us the real Go
// lexer, so string and comment boundaries are respected regardless of order.
func stripGoCommentsAndStrings(src string) string {
	stripped, _ := stripGoCommentsAndStringsWithErrors(src)
	return stripped
}

// stripGoCommentsAndStringsWithErrors is the error-surfacing sibling of
// stripGoCommentsAndStrings. Callers that need to detect a truncated
// tokenization (the anti-feature test, specifically) must use this
// variant — a silently-dropped scanner error could otherwise let a
// contributor smuggle forbidden identifiers past the substring scan by
// introducing a syntax error earlier in the file. Non-fatal callers can
// keep using stripGoCommentsAndStrings.
func stripGoCommentsAndStringsWithErrors(src string) (string, []error) {
	var (
		s       scanner.Scanner
		scanErr []error
	)
	fset := token.NewFileSet()
	file := fset.AddFile("scan.go", fset.Base(), len(src))
	// Mode 0 means: do NOT report comments as tokens (they are skipped).
	// Errors are captured into scanErr so the caller can fail loudly
	// rather than silently accepting a half-scanned file. A previous
	// revision used an empty error handler — that let a truncated
	// tokenization silently bypass the anti-feature substring scan.
	s.Init(file, []byte(src), func(pos token.Position, msg string) {
		scanErr = append(scanErr, fmt.Errorf("%s: %s", pos, msg))
	}, 0)

	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		switch tok {
		case token.COMMENT:
			// scanner.Mode 0 elides comments, but keep this arm for clarity
			// if we ever set ScanComments in the future.
			continue
		case token.STRING, token.CHAR:
			// Drop the literal contents entirely. The guard cares whether
			// forbidden substrings appear in executable code; string /
			// rune literals are data, not code, and a comment or doc that
			// mentions `.ssh/config` should not trip the scan.
			continue
		case token.IDENT, token.INT, token.FLOAT, token.IMAG:
			b.WriteString(lit)
			b.WriteByte(' ')
		default:
			// Keywords and operators surface as their token literal form.
			// We still emit them so identifier-like keywords (e.g. `func`)
			// are visible to the substring scan — they never match any of
			// the forbidden entries, but we want the output to look like
			// code rather than a run-together blob.
			b.WriteString(tok.String())
			b.WriteByte(' ')
		}
	}
	return b.String(), scanErr
}

// TestStripGoCommentsAndStringsHandlesMixedCommentAndStringOnOneLine pins the
// P1-7 regression: a previous implementation stripped `//` line comments
// before string literals, so a line containing both would have its comment
// range eaten past the closing quote of the following string literal, which
// in turn made `.ssh/config` hide inside a comment the scanner never
// examined. The go/scanner rewrite tokenises correctly regardless of order;
// this test fails loud if someone swaps it back for a regex sequence.
func TestStripGoCommentsAndStringsHandlesMixedCommentAndStringOnOneLine(t *testing.T) {
	// Wrap the problematic line in a valid Go source snippet so the lexer
	// accepts it. If the strip function bails on the first error, the
	// forbidden substring would leak through and the test would pass
	// misleadingly — hence asserting the presence of benign identifiers
	// (`foo`, `bar`) alongside the absence of the forbidden substring.
	src := "package p\n" +
		"func _f() { foo := \"http://x\"; bar := \".ssh/config\"; _ = foo; _ = bar }\n"
	stripped := strings.ToLower(stripGoCommentsAndStrings(src))
	if strings.Contains(stripped, ".ssh/config") {
		t.Fatalf("string literal contents leaked into stripped code output: %q", stripped)
	}
	if strings.Contains(stripped, "http://x") {
		t.Fatalf("string literal contents leaked into stripped code output: %q", stripped)
	}
	// Identifiers should survive so the forbidden-substring scan still sees
	// any reintroduced deleted-helper names that happen to co-occur with a
	// string literal on the same source line.
	if !strings.Contains(stripped, "foo") || !strings.Contains(stripped, "bar") {
		t.Fatalf("identifiers dropped from stripped output: %q", stripped)
	}
}

// TestStripGoCommentsAndStringsDoesNotTripOnForbiddenInsideCommentOrString
// is the positive counterpart: the forbidden substrings SHOULD be eaten
// when they sit inside comments or string literals, otherwise harmless
// docstrings mentioning the legacy directive names would trip the guard.
func TestStripGoCommentsAndStringsDoesNotTripOnForbiddenInsideCommentOrString(t *testing.T) {
	src := "package p\n" +
		"// RemoteForward only lives in docstrings here; should be stripped\n" +
		"const _ = \"RemoteForward literal\"\n"
	stripped := strings.ToLower(stripGoCommentsAndStrings(src))
	if strings.Contains(stripped, "remoteforward") {
		t.Fatalf("expected RemoteForward in comment/string to be stripped, got %q", stripped)
	}
}

// TestPackageDoesNotDependOnSSHConfigRewriters is the import-graph half of
// the anti-feature guard. The source-level substring scan above catches
// copy-paste reintroduction of deleted helpers, but a determined contributor
// could hide ssh_config writing in a sibling package (e.g.
// `internal/setup-helpers/`) and import it — the compiler would resolve the
// import but the substring scan only looks at files under `internal/setup`.
//
// `go list -deps` walks the full import closure (direct + transitive) for
// the `./` argument (this package, `internal/setup`). Any package in the
// "forbidden" set that shows up here means `internal/setup` is either
// calling it directly or pulling it in through another dep. The rule is
// strict: contributors who need ssh_config behavior must route users
// through `cmd/cc-clip` (which owns user-facing flows) instead of hiding
// it behind a `setup.X` call.
//
// Scope of this guarantee: it covers `internal/setup`'s OWN transitive
// deps only. It deliberately does NOT walk `cmd/cc-clip`'s dep graph —
// that binary legitimately imports `internal/sshconfig` and an expanded
// scan would fire on every compile. The anti-feature invariant we are
// defending is "`internal/setup` stays flat and ssh-config-free", not
// "the whole repo pretends sshconfig does not exist". If a future
// contributor wires ssh-config logic into `cmd/cc-clip` itself, that has
// to be caught by the per-package invariants documented in AGENTS.md,
// not by this test.
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
