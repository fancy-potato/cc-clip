package sshconfig

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shunmei/cc-clip/internal/userhome"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeTempConfig: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

// fakeUserhomeResolver is a minimal test double satisfying the
// userhome.Resolver interface. Fields are plain func values so each
// test can configure only what it exercises.
type fakeUserhomeResolver struct {
	lookup func(string) (*user.User, error)
	home   func() (string, error)
	sudo   func() bool
}

func (f fakeUserhomeResolver) LookupUser(name string) (*user.User, error) {
	return f.lookup(name)
}

func (f fakeUserhomeResolver) UserHomeDir() (string, error) {
	return f.home()
}

func (f fakeUserhomeResolver) IsSudoRoot() bool {
	if f.sudo == nil {
		return false
	}
	return f.sudo()
}

func TestLocalConfigPathUsesSUDOUserHome(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")
	t.Setenv("SUDO_UID", "501")
	userhome.SetResolverForTest(t, fakeUserhomeResolver{
		lookup: func(name string) (*user.User, error) {
			if name != "alice" {
				t.Fatalf("lookupUser(%q), want alice", name)
			}
			return &user.User{Username: "alice", Uid: "501", HomeDir: "/Users/alice"}, nil
		},
		home: func() (string, error) {
			return "/var/root", nil
		},
		sudo: func() bool { return true },
	})

	got, err := LocalConfigPath()
	if err != nil {
		t.Fatalf("LocalConfigPath: %v", err)
	}
	if got != filepath.Join("/Users/alice", ".ssh", "config") {
		t.Fatalf("LocalConfigPath = %q, want /Users/alice/.ssh/config", got)
	}
}

func TestApplyInsertsBlockInsideExistingHost(t *testing.T) {
	path := writeTempConfig(t, `Host other
  HostName other.example.com

Host myalias
  HostName srv.example.com
  User shareduser

Host last
  HostName last.example.com
`)

	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/shareduser/.cache/cc-clip/peers/abc123",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readFile(t, path)
	want := `Host other
  HostName other.example.com

Host myalias
  HostName srv.example.com
  User shareduser
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shareduser/.cache/cc-clip/peers/abc123
  # <<< cc-clip SetEnv (do not edit) <<<

Host last
  HostName last.example.com
`
	if got != want {
		t.Fatalf("Apply output mismatch:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName srv
`)
	env := map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/u/.cache/cc-clip/peers/abc",
	}
	if err := ApplyToFile(path, "myalias", env); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	first := readFile(t, path)
	if err := ApplyToFile(path, "myalias", env); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	second := readFile(t, path)
	if first != second {
		t.Fatalf("expected idempotent re-apply; first:\n%s\nsecond:\n%s", first, second)
	}
}

func TestApplyUpdatesExistingMarkerBlock(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName srv
`)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18339"}); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18341",
		"CC_CLIP_STATE_DIR": "/x",
	}); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18341") {
		t.Fatalf("expected updated port; got:\n%s", got)
	}
	if strings.Contains(got, "SetEnv CC_CLIP_PORT=18339") {
		t.Fatalf("old port should be gone; got:\n%s", got)
	}
	if !strings.Contains(got, "CC_CLIP_STATE_DIR=/x") {
		t.Fatalf("expected new state dir; got:\n%s", got)
	}
	if strings.Count(got, "\n  SetEnv ") != 1 {
		t.Fatalf("expected a single SetEnv directive; got:\n%s", got)
	}
	// Marker pair still appears exactly once.
	if strings.Count(got, markerBegin) != 1 || strings.Count(got, markerEnd) != 1 {
		t.Fatalf("expected exactly one marker pair; got:\n%s", got)
	}
}

func TestApplyMultipleHostsOnlyTouchesTarget(t *testing.T) {
	path := writeTempConfig(t, `Host a
  HostName a.example.com

Host b
  HostName b.example.com

Host c
  HostName c.example.com
`)
	if err := ApplyToFile(path, "b", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	want := `Host a
  HostName a.example.com

Host b
  HostName b.example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340
  # <<< cc-clip SetEnv (do not edit) <<<

Host c
  HostName c.example.com
`
	if got != want {
		t.Fatalf("expected only Host b touched:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestApplyMatchesAliasInMultiTokenHostLine(t *testing.T) {
	path := writeTempConfig(t, `Host alpha beta gamma
  HostName shared.example.com
`)
	if err := ApplyToFile(path, "beta", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected SetEnv injected into multi-token Host block; got:\n%s", got)
	}
}

func TestApplyRecognizesIndentedHostStanza(t *testing.T) {
	path := writeTempConfig(t, `  Host other
    HostName other.example.com

  Host myalias
    HostName srv.example.com
    User shareduser

  Host last
    HostName last.example.com
`)

	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readFile(t, path)
	want := `  Host other
    HostName other.example.com

  Host myalias
    HostName srv.example.com
    User shareduser
    # >>> cc-clip SetEnv (do not edit) >>>
    SetEnv CC_CLIP_PORT=18340
    # <<< cc-clip SetEnv (do not edit) <<<

  Host last
    HostName last.example.com
`
	if got != want {
		t.Fatalf("Apply output mismatch for indented Host stanza:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestApplyRejectsGlobOnlyMatch(t *testing.T) {
	cases := []struct {
		name   string
		config string
		alias  string
	}{
		{
			name:   "star_all",
			config: "Host *\n  User shareduser\n",
			alias:  "myalias",
		},
		{
			name:   "star_suffix",
			config: "Host *.example.com\n  User shareduser\n",
			alias:  "myalias.example.com",
		},
		{
			name:   "question_mark",
			config: "Host foo?\n  User shareduser\n",
			alias:  "fooX",
		},
		{
			name:   "multiple_patterns_same_line",
			config: "Host dev-* staging-*\n  User shareduser\n",
			alias:  "dev-box",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.config)
			err := ApplyToFile(path, tc.alias, map[string]string{"CC_CLIP_PORT": "18340"})
			if !errors.Is(err, ErrOnlyGlobMatch) {
				t.Fatalf("expected ErrOnlyGlobMatch, got %v", err)
			}
			// Verify the rejected config was NOT modified.
			if got := readFile(t, path); got != tc.config {
				t.Fatalf("config was mutated on glob rejection:\n--- got\n%s\n--- want\n%s", got, tc.config)
			}
		})
	}
}

func TestApplyTreatsNegatedLiteralAsExcluded(t *testing.T) {
	original := "Host myalias !myalias\n  HostName srv\n"
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing for excluded literal alias, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated despite negated literal exclusion:\n%s", got)
	}
}

func TestApplyTreatsNegatedGlobAsExcluded(t *testing.T) {
	original := "Host *.example.com !bad.example.com\n  User shareduser\n"
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "bad.example.com", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing for excluded glob alias, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated despite negated glob exclusion:\n%s", got)
	}
}

func TestApplySkipsMatchHostBlocks(t *testing.T) {
	path := writeTempConfig(t, `Match host myalias
  ForwardAgent yes
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing for Match-only config, got %v", err)
	}
}

func TestApplyMissingHostBlockReturnsErr(t *testing.T) {
	path := writeTempConfig(t, `Host other
  HostName other.example.com
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing, got %v", err)
	}
}

func TestApplyReportsHostBlockInTopLevelInclude(t *testing.T) {
	path := writeTempConfig(t, `Include ~/.ssh/conf.d/*.conf

Host other
  HostName other.example.com
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude, got %v", err)
	}
}

func TestApplyReportsHostBlockInIndentedTopLevelInclude(t *testing.T) {
	path := writeTempConfig(t, `  Include ~/.ssh/conf.d/*.conf

Host other
  HostName other.example.com
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude, got %v", err)
	}
}

func TestApplyIgnoresIncludeInsideHostBlock(t *testing.T) {
	path := writeTempConfig(t, `Host other
  HostName other.example.com
  Include ~/.ssh/conf.d/*.conf
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing, got %v", err)
	}
}

func TestApplyTreatsIncludeAfterHostAsInsideSameHostBlock(t *testing.T) {
	path := writeTempConfig(t, `Host other
  HostName other.example.com

Include ~/.ssh/conf.d/*.conf
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing, got %v", err)
	}
}

// TestApplyReportsHostBlockInIncludeAfterMatchBlock pins the fix for the
// review P1 case: an `Include` that follows a `Match` directive at column
// 0 is potentially reachable (Match can fire unconditionally, e.g. `Match
// all`), so it MUST surface as ErrHostBlockInInclude — not the default
// ErrHostBlockMissing. The pre-fix `inBlock` flag latched true on the
// first Host/Match and never reset, masking the Include. Without this
// test, a regression that re-introduces the latch goes undetected.
func TestApplyReportsHostBlockInIncludeAfterMatchBlock(t *testing.T) {
	path := writeTempConfig(t, `Match all
  ForwardAgent yes

Include ~/.ssh/conf.d/*.conf
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude, got %v", err)
	}
}

// TestApplyReportsHostBlockInIncludeAfterHostThenMatch covers a slightly
// trickier ordering: Host opens a block (inside-host=true), then Match
// opens a NEW block which resets the inside-host tracking, and a column-0
// Include after that Match is reachable via the Match. Asserts the
// most-recent-opener semantics, not just the most-recent-of-each-kind.
func TestApplyReportsHostBlockInIncludeAfterHostThenMatch(t *testing.T) {
	path := writeTempConfig(t, `Host other
  HostName other.example.com

Match all

Include ~/.ssh/conf.d/*.conf
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude, got %v", err)
	}
}

func TestHostBlockStatusFromBytesReportsUnsupportedIncludeLayout(t *testing.T) {
	data := []byte("Include ~/.ssh/conf.d/*.conf\n")
	err := HostBlockStatusFromBytes(data, "myalias")
	if !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude, got %v", err)
	}
}

// TestApplyReportsHostBlockInIncludeAfterMatchThenHost pins the
// Match → Host → Include monotonic-reachability bit. The nested-state
// approach used to flip `insideHostBlock=true` on the Host directive
// and then skip the Include as "inside a Host that doesn't match our
// alias". But once a Match has been seen earlier in the file it could
// have fired unconditionally (`Match all`) and the Include is still
// reachable from its body — a later Host directive does not erase that.
// Without the monotonic reachableViaMatch bit this returns
// ErrHostBlockMissing (the wrong answer, silently ignoring the Include).
func TestApplyReportsHostBlockInIncludeAfterMatchThenHost(t *testing.T) {
	path := writeTempConfig(t, `Match all

Host other
  # no alias matching myalias here

Include ~/.ssh/conf.d/other.conf
`)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude (Include after Match→Host is reachable), got %v", err)
	}
}

// TestHasIncludeDirectiveHandlesBackslashContinuation pins that the
// scanner honors ssh_config's line-continuation syntax: a physical line
// ending in `\` (ignoring trailing whitespace) extends the logical
// directive. The continuation body must NOT be mis-classified as a new
// top-level directive.
//
// OpenSSH semantics: `Host foo \` + whitespace-indented `extra` = one
// Host directive carrying two aliases (`foo` and `extra`), opening a
// single host block. A subsequent `Include` at column 0 is then INSIDE
// that Host block and conditional on matching foo/extra, so querying
// about an unrelated alias must NOT surface ErrHostBlockInInclude
// (the Include cannot reach us). Querying about `extra` must succeed
// because the continuation adds `extra` as a literal alias. Without
// the continuation-aware scanner, the continuation line `  extra` gets
// treated as a separate token and the classification desyncs.
func TestHasIncludeDirectiveHandlesBackslashContinuation(t *testing.T) {
	data := []byte("Host foo \\\n  extra\n\nInclude ~/.ssh/conf.d/extra.conf\n")
	// `extra` must be a literal match via continuation, not a miss.
	if err := HostBlockStatusFromBytes(data, "extra"); err != nil {
		t.Fatalf("expected `extra` alias to match the Host foo continuation, got %v", err)
	}
	// Unrelated alias: no literal Host matches, but the Include sits
	// inside the `Host foo extra` block so it does NOT reach us. The
	// correct classification is ErrHostBlockMissing, not InInclude.
	if err := HostBlockStatusFromBytes(data, "myalias"); !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing (Include sits inside unrelated Host block via continuation), got %v", err)
	}
}

// TestHasIncludeDirectiveHandlesBackslashContinuationWithTopLevelInclude
// covers the complementary case where the Include is at top level
// (before any Host/Match) and the Host directive later uses a
// continuation. The Include is unconditionally reachable, so the
// classifier must surface ErrHostBlockInInclude. Without continuation
// handling, the `Host foo \` line's continuation body would be
// mis-read as a second top-level directive and the Include scanner's
// reachability bookkeeping desyncs.
func TestHasIncludeDirectiveHandlesBackslashContinuationWithTopLevelInclude(t *testing.T) {
	data := []byte("Include ~/.ssh/conf.d/extra.conf\n\nHost foo \\\n  extra\n  HostName foo.example.com\n")
	if err := HostBlockStatusFromBytes(data, "myalias"); !errors.Is(err, ErrHostBlockInInclude) {
		t.Fatalf("expected ErrHostBlockInInclude (top-level Include before any Host), got %v", err)
	}
}

func TestApplyDoesNotSplitContinuationWrappedHostDirective(t *testing.T) {
	path := writeTempConfig(t, `Host decoy \
Host myalias
  HostName srv.example.com
`)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "Host decoy \\\n  # >>> cc-clip SetEnv") {
		t.Fatalf("marker was inserted in the middle of a continued Host directive:\n%s", got)
	}
	wantOrder := "Host decoy \\\nHost myalias\n  HostName srv.example.com\n  " + markerBegin
	if !strings.Contains(got, wantOrder) {
		t.Fatalf("expected marker after the continued Host block body, got:\n%s", got)
	}
}

func TestApplyPrefersLiteralOverGlob(t *testing.T) {
	path := writeTempConfig(t, `Host *.example.com
  User globaluser

Host srv.example.com
  HostName srv.example.com
  User specific
`)
	if err := ApplyToFile(path, "srv.example.com", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	// SetEnv must land in the literal block, not the glob block.
	literalIdx := strings.Index(got, "Host srv.example.com")
	markerIdx := strings.Index(got, markerBegin)
	if literalIdx == -1 || markerIdx == -1 || markerIdx < literalIdx {
		t.Fatalf("expected marker to land in literal Host block; got:\n%s", got)
	}
}

func TestApplyDetectsTabIndent(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n\tHostName srv\n\tUser shareduser\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "\t"+markerBegin) {
		t.Fatalf("expected tab-indented marker; got:\n%q", got)
	}
	if !strings.Contains(got, "\tSetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected tab-indented SetEnv; got:\n%q", got)
	}
}

func TestApplyDetectsFourSpaceIndent(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n    HostName srv\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "    "+markerBegin) {
		t.Fatalf("expected 4-space indent; got:\n%q", got)
	}
}

func TestApplyDefaultIndentWhenBlockEmpty(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "  "+markerBegin) {
		t.Fatalf("expected default 2-space indent; got:\n%q", got)
	}
}

func TestApplyPreservesFileMode(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected 0600, got %v", got)
	}
}

// TestApplyRejectsOverlongEnvValue pins the defensive cap on SetEnv
// value length. `reg.StateDir` ultimately flows from the remote peer
// registry, so without a cap a compromised remote could grow the
// user's ~/.ssh/config unbounded. Exactly at the boundary must still
// pass; one byte over must fail closed.
func TestApplyRejectsOverlongEnvValue(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	atCap := strings.Repeat("a", maxEnvValueLen)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_STATE_DIR": atCap}); err != nil {
		t.Fatalf("at-cap value should be accepted, got %v", err)
	}
	overCap := strings.Repeat("a", maxEnvValueLen+1)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_STATE_DIR": overCap})
	if !errors.Is(err, ErrInvalidEnvValue) {
		t.Fatalf("over-cap value: expected ErrInvalidEnvValue, got %v", err)
	}
}

func TestApplyRejectsInvalidHost(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	cases := []string{
		"",
		"my alias",
		"my\nalias",
		"my#alias",
		"my*alias",
		"my?alias",
		"!myalias",
		// Non-ASCII: OpenSSH matches Host tokens byte-for-byte and the upstream
		// tunnel.ValidateSSHHost rejects anything outside [A-Za-z0-9._:@-]; the
		// sshconfig-level validator matches that policy so a direct caller can't
		// write a stanza ssh -G would then refuse to resolve.
		"myalias\u00e9",
		"myalias\u4e2d",
		// ASCII control characters under 0x20 must also be rejected. They
		// could otherwise smuggle a CR into a Host stanza or terminate the
		// directive mid-line for an unsuspecting parser.
		"my\x01alias",
		"my\x07alias",
		"my\x1balias",
		"my\x7falias",
	}
	for _, h := range cases {
		err := ApplyToFile(path, h, map[string]string{"CC_CLIP_PORT": "18340"})
		if !errors.Is(err, ErrInvalidHost) {
			t.Fatalf("host %q: expected ErrInvalidHost, got %v", h, err)
		}
	}
}

func TestApplyRejectsInvalidEnvValue(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18\n340"})
	if !errors.Is(err, ErrInvalidEnvValue) {
		t.Fatalf("expected ErrInvalidEnvValue, got %v", err)
	}
}

func TestApplyRejectsInvalidEnvKey(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	err := ApplyToFile(path, "myalias", map[string]string{"bad-key": "x"})
	if !errors.Is(err, ErrInvalidEnvValue) {
		t.Fatalf("expected ErrInvalidEnvValue for bad key, got %v", err)
	}
}

func TestApplyRejectsSymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real-config")
	if err := os.WriteFile(realPath, []byte("Host myalias\n  HostName srv\n"), 0o600); err != nil {
		t.Fatalf("write real config: %v", err)
	}
	linkPath := filepath.Join(dir, "config")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("symlink config: %v", err)
	}

	err := ApplyToFile(linkPath, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSymlinkConfig) {
		t.Fatalf("expected ErrSymlinkConfig, got %v", err)
	}
	got := readFile(t, realPath)
	if got != "Host myalias\n  HostName srv\n" {
		t.Fatalf("real config unexpectedly modified:\n%s", got)
	}
	if info, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("lstat link: %v", err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected config path to remain a symlink, mode=%v", info.Mode())
	}
}

func TestApplyQuotesEnvValueWithSpaces(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_LABEL": "hello world"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `SetEnv CC_CLIP_LABEL="hello world"`) {
		t.Fatalf("expected quoted value; got:\n%s", got)
	}
}

// TestApplyQuotesEnvValueWithHash pins contract 7: `#` in a value triggers
// OpenSSH's trailing-comment tokenizer when emitted unquoted, silently
// truncating the value. CC_CLIP_STATE_DIR flows from the peer registry so
// a remote-supplied path containing `#` must round-trip through ssh -G.
func TestApplyQuotesEnvValueWithHash(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv.example.com\n")
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_STATE_DIR": "/home/u/foo#bar",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `SetEnv CC_CLIP_STATE_DIR="/home/u/foo#bar"`) {
		t.Fatalf("expected `#` to force quoting; got:\n%s", got)
	}
	// Verify round-trip through ssh -G so a future regression that emits
	// the value unquoted would truncate at `#` and fail this assertion.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh not available: %v", err)
	}
	out, err := exec.Command("ssh", "-G", "-F", path, "myalias").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "setenv CC_CLIP_STATE_DIR=/home/u/foo#bar") {
		t.Fatalf("ssh -G truncated value at `#`:\n%s", out)
	}
}

func TestApplyRejectsExistingUserSetEnv(t *testing.T) {
	original := `Host myalias
  HostName srv
  SetEnv FOO=bar
`
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("expected ErrSetEnvConflict, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated on SetEnv conflict:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

// TestApplySucceedsAfterUserRemovesConflictingSetEnv pins the recovery
// path: an Apply that returned ErrSetEnvConflict is not a sticky error.
// Once the operator removes their conflicting SetEnv line, the next Apply
// must install the marker block cleanly. A future refactor that caches
// "this host is poisoned" or that fails to re-scan the file would silently
// break recovery without breaking the original conflict-detection test.
func TestApplySucceedsAfterUserRemovesConflictingSetEnv(t *testing.T) {
	conflicting := `Host myalias
  HostName srv
  SetEnv FOO=bar
`
	path := writeTempConfig(t, conflicting)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("step 1: expected ErrSetEnvConflict, got %v", err)
	}

	cleaned := `Host myalias
  HostName srv
`
	if err := os.WriteFile(path, []byte(cleaned), 0600); err != nil {
		t.Fatalf("rewrite cleaned config: %v", err)
	}

	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/u/.cache/cc-clip/peers/abc",
	}); err != nil {
		t.Fatalf("step 2: Apply after conflict resolution failed: %v", err)
	}

	got := readFile(t, path)
	if !strings.Contains(got, markerBegin) || !strings.Contains(got, markerEnd) {
		t.Fatalf("recovered Apply did not write marker pair:\n%s", got)
	}
	if !strings.Contains(got, "CC_CLIP_PORT=18340") || !strings.Contains(got, "CC_CLIP_STATE_DIR=/home/u/.cache/cc-clip/peers/abc") {
		t.Fatalf("recovered Apply did not write expected env values:\n%s", got)
	}
}

func TestApplyRejectsExistingUserSetEnvInLaterDuplicateHost(t *testing.T) {
	original := `Host myalias
  HostName first.example.com

Host myalias
  HostName second.example.com
  SetEnv FOO=bar
`
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("expected ErrSetEnvConflict, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated on duplicate-host SetEnv conflict:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

func TestApplyRepairsOrphanedBeginMarker(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName srv
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18339
`)

	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/u/.cache/cc-clip/peers/abc",
	}); err != nil {
		t.Fatalf("Apply should repair orphaned begin marker, got %v", err)
	}

	got := readFile(t, path)
	if strings.Count(got, markerBegin) != 1 || strings.Count(got, markerEnd) != 1 {
		t.Fatalf("expected one repaired marker pair, got:\n%s", got)
	}
	if strings.Count(got, "\n  SetEnv ") != 1 || strings.Contains(got, "CC_CLIP_PORT=18339") {
		t.Fatalf("expected orphaned SetEnv line to be replaced cleanly, got:\n%s", got)
	}
}

func TestApplyRepairsOrphanedEndMarker(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName srv
  SetEnv CC_CLIP_PORT=18339
  # <<< cc-clip SetEnv (do not edit) <<<
`)

	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/u/.cache/cc-clip/peers/abc",
	}); err != nil {
		t.Fatalf("Apply should repair orphaned end marker, got %v", err)
	}

	got := readFile(t, path)
	if strings.Count(got, markerBegin) != 1 || strings.Count(got, markerEnd) != 1 {
		t.Fatalf("expected one repaired marker pair, got:\n%s", got)
	}
	if strings.Count(got, "\n  SetEnv ") != 1 || strings.Contains(got, "CC_CLIP_PORT=18339") {
		t.Fatalf("expected orphaned SetEnv line to be replaced cleanly, got:\n%s", got)
	}
}

// TestApplyRejectsBackslashInEnvValue pins the contract that a value
// containing `\` is rejected, because OpenSSH's ssh_config tokenizer does
// not interpret `\\`/`\"` inside quoted SetEnv values the way this package
// round-trips them: a written `"a\\b"` would be delivered to the remote as
// the literal five-byte string `a\\b`, not `a\b`. CC_CLIP_STATE_DIR is
// remote-influenceable, so we fail Apply rather than silently corrupt the
// value on round-trip through ssh -G.
func TestApplyRejectsBackslashInEnvValue(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_WEIRD": `a"b\c`})
	if !errors.Is(err, ErrInvalidEnvValue) {
		t.Fatalf("expected ErrInvalidEnvValue for backslash value; got %v", err)
	}
}

// TestApplyEscapesEnvValueWithQuotesOnly pins the contract that a value
// containing `"` (but no `\`) is still quoted-and-escaped so an existing
// quote cannot prematurely terminate the quoted SetEnv value.
func TestApplyEscapesEnvValueWithQuotesOnly(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_WEIRD": `a"b"c`}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	want := `SetEnv CC_CLIP_WEIRD="a\"b\"c"`
	if !strings.Contains(got, want) {
		t.Fatalf("expected escaped quotes %q; got:\n%s", want, got)
	}
}

func TestApplyRendersBothEnvVarsInSingleSetEnvDirective(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/shared/.cache/cc-clip/peers/peer-a",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readFile(t, path)
	if strings.Count(got, "\n  SetEnv ") != 1 {
		t.Fatalf("expected a single SetEnv directive, got:\n%s", got)
	}
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/peer-a") {
		t.Fatalf("expected both env vars on one SetEnv line; got:\n%s", got)
	}
}

func TestApplySingleSetEnvDirectiveSurvivesSSHG(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh not available: %v", err)
	}
	path := writeTempConfig(t, "Host myalias\n  HostName srv.example.com\n")
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/shared/.cache/cc-clip/peers/peer-a",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	out, err := exec.Command("ssh", "-G", "-F", path, "myalias").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G failed: %v\noutput: %s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"setenv CC_CLIP_PORT=18340",
		"setenv CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/peer-a",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ssh -G output missing %q:\n%s", want, got)
		}
	}
}

func TestRemoveDeletesMarkerBlock(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName srv
  User shareduser
`)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := readFile(t, path)
	want := `Host myalias
  HostName srv
  User shareduser
`
	if got != want {
		t.Fatalf("Remove did not restore original:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestRemoveRecognizesIndentedHostStanza(t *testing.T) {
	original := `  Host myalias
    HostName srv
    User shareduser
`
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("Remove did not restore indented Host stanza:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

func TestRemoveNoOpWhenMarkerAbsent(t *testing.T) {
	original := `Host myalias
  HostName srv
`
	path := writeTempConfig(t, original)
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("expected no-op; got:\n%s", got)
	}
}

func TestRemoveNoOpWhenHostMissing(t *testing.T) {
	original := "Host other\n  HostName other\n"
	path := writeTempConfig(t, original)
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("expected no-op; got:\n%s", got)
	}
}

func TestRemoveNoOpWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("expected nil for missing file, got %v", err)
	}
}

func TestRemoveRejectsSymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real-config")
	original := `Host myalias
  HostName srv
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340
  # <<< cc-clip SetEnv (do not edit) <<<
`
	if err := os.WriteFile(realPath, []byte(original), 0o600); err != nil {
		t.Fatalf("write real config: %v", err)
	}
	linkPath := filepath.Join(dir, "config")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("symlink config: %v", err)
	}

	err := RemoveFromFile(linkPath, "myalias")
	if !errors.Is(err, ErrSymlinkConfig) {
		t.Fatalf("expected ErrSymlinkConfig, got %v", err)
	}
	if got := readFile(t, realPath); got != original {
		t.Fatalf("real config unexpectedly modified:\n%s", got)
	}
	if info, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("lstat link: %v", err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected config path to remain a symlink, mode=%v", info.Mode())
	}
}

func TestRemoveLeavesOtherSetEnvLinesIntact(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName srv
  SetEnv MY_OWN_VAR=keep
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340
  # <<< cc-clip SetEnv (do not edit) <<<
  SetEnv ANOTHER=alsokeep
`)
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := readFile(t, path)
	want := `Host myalias
  HostName srv
  SetEnv MY_OWN_VAR=keep
  SetEnv ANOTHER=alsokeep
`
	if got != want {
		t.Fatalf("Remove damaged user SetEnv lines:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestRemoveOnlyTouchesMatchingHostBlock(t *testing.T) {
	path := writeTempConfig(t, `Host other
  HostName other.example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18339
  # <<< cc-clip SetEnv (do not edit) <<<

Host myalias
  HostName srv
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340
  # <<< cc-clip SetEnv (do not edit) <<<
`)
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18339") {
		t.Fatalf("expected other host's marker untouched; got:\n%s", got)
	}
	if strings.Contains(got, "SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected myalias marker removed; got:\n%s", got)
	}
}

func TestApplyConsolidatesDuplicateLiteralHostBlocks(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName first.example.com

Host myalias
  HostName second.example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=19999 CC_CLIP_STATE_DIR=/old
  # <<< cc-clip SetEnv (do not edit) <<<
`)
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/shared/.cache/cc-clip/peers/peer-a",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readFile(t, path)
	if strings.Count(got, markerBegin) != 1 || strings.Count(got, markerEnd) != 1 {
		t.Fatalf("expected one managed marker pair across duplicate Host blocks; got:\n%s", got)
	}
	firstHost := strings.Index(got, "Host myalias\n  HostName first.example.com")
	secondHost := strings.Index(got, "Host myalias\n  HostName second.example.com")
	marker := strings.Index(got, markerBegin)
	if marker < firstHost || marker > secondHost {
		t.Fatalf("expected managed block in the first matching Host stanza; got:\n%s", got)
	}
	if strings.Contains(got, "19999") || strings.Contains(got, "CC_CLIP_STATE_DIR=/old") {
		t.Fatalf("expected stale managed block removed from later Host stanza; got:\n%s", got)
	}
}

func TestRemoveDeletesManagedBlocksFromAllMatchingLiteralHosts(t *testing.T) {
	path := writeTempConfig(t, `Host myalias
  HostName first.example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/first
  # <<< cc-clip SetEnv (do not edit) <<<

Host myalias
  HostName second.example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=19999 CC_CLIP_STATE_DIR=/second
  # <<< cc-clip SetEnv (do not edit) <<<
`)
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := readFile(t, path)
	if strings.Contains(got, markerBegin) || strings.Contains(got, markerEnd) {
		t.Fatalf("expected all managed blocks removed from duplicate Host stanzas; got:\n%s", got)
	}
	if strings.Contains(got, "CC_CLIP_PORT=") || strings.Contains(got, "CC_CLIP_STATE_DIR=") {
		t.Fatalf("expected managed SetEnv assignments removed from duplicate Host stanzas; got:\n%s", got)
	}
}

func TestApplyToHostBlockAtEOF(t *testing.T) {
	path := writeTempConfig(t, "Host first\n  HostName a\n\nHost myalias\n  HostName srv\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	want := "Host first\n  HostName a\n\nHost myalias\n  HostName srv\n  # >>> cc-clip SetEnv (do not edit) >>>\n  SetEnv CC_CLIP_PORT=18340\n  # <<< cc-clip SetEnv (do not edit) <<<\n"
	if got != want {
		t.Fatalf("EOF block insertion mismatch:\n--- got\n%q\n--- want\n%q", got, want)
	}
}

func TestApplyHandlesHostKeyEqualsForm(t *testing.T) {
	path := writeTempConfig(t, "Host=myalias\n  HostName=srv\n")
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected SetEnv injected into Host=alias form; got:\n%s", got)
	}
}

func TestApplyRoundTripBytesMinusInjection(t *testing.T) {
	original := `# global comment

Host myalias
  HostName srv

# trailing comment
`
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := readFile(t, path)
	if got != original {
		t.Fatalf("Apply→Remove not byte-equivalent:\n--- got\n%q\n--- original\n%q", got, original)
	}
}

// TestApplyHandlesCRLFInput exercises the Windows-authored config path:
// lines end with \r\n. Both the match (Host alias must be found despite
// the trailing \r on the keyword line) and the write (new marker lines
// must re-emit with \r\n, not mix LF and CRLF) are covered. Round-trip
// via Apply→Remove restores the exact CRLF original.
func TestApplyHandlesCRLFInput(t *testing.T) {
	original := "Host myalias\r\n  HostName srv\r\n"
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	want := "Host myalias\r\n  HostName srv\r\n  # >>> cc-clip SetEnv (do not edit) >>>\r\n  SetEnv CC_CLIP_PORT=18340\r\n  # <<< cc-clip SetEnv (do not edit) <<<\r\n"
	if got != want {
		t.Fatalf("CRLF output mismatch:\n--- got\n%q\n--- want\n%q", got, want)
	}
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("CRLF round-trip not byte-equal:\n--- got\n%q\n--- original\n%q", got, original)
	}
}

// TestApplyPreservesBOM covers editors (Notepad, some Windows tools) that
// prepend a UTF-8 BOM. The BOM is preserved verbatim at the file head and
// not carried into any line body, so `Host myalias` still matches on the
// first line.
func TestApplyPreservesBOM(t *testing.T) {
	original := "\xEF\xBB\xBFHost myalias\n  HostName srv\n"
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.HasPrefix(got, "\xEF\xBB\xBF") {
		t.Fatalf("expected BOM preserved at head; got: %q", got)
	}
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected SetEnv injected despite BOM; got:\n%s", got)
	}
	if err := RemoveFromFile(path, "myalias"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("BOM round-trip not byte-equal:\n--- got\n%q\n--- original\n%q", got, original)
	}
}

// TestApplyTreatsUnquotedHashAsCommentInHostLine pins the user-intent
// semantics: `Host a b c # d e` has three aliases (a, b, c), and setup for
// "d" must NOT match — the `# d e` is a trailing comment.
func TestApplyTreatsUnquotedHashAsCommentInHostLine(t *testing.T) {
	path := writeTempConfig(t, "Host a b c # d e\n  HostName shared.example.com\n")
	err := ApplyToFile(path, "d", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing for commented alias, got %v", err)
	}
	// Real aliases still match, and the trailing comment is preserved
	// verbatim because we never rewrite Host lines.
	if err := ApplyToFile(path, "b", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply on real alias: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected injection on real alias; got:\n%s", got)
	}
	if !strings.Contains(got, "Host a b c # d e") {
		t.Fatalf("expected Host comment preserved; got:\n%s", got)
	}
}

// TestApplyDoesNotTreatGluedHashAsComment pins the OpenSSH behavior for
// `Host myalias#comment`: the `#` is part of the literal alias token, not the
// start of a comment, so setup for `myalias` must NOT match this stanza.
func TestApplyDoesNotTreatGluedHashAsComment(t *testing.T) {
	original := "Host myalias#comment\n  HostName srv\n"
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrHostBlockMissing) {
		t.Fatalf("expected ErrHostBlockMissing, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated on glued-hash mismatch:\n%s", got)
	}
}

// TestReadManagedEnvRoundTrip pins that Apply(env) → ReadManagedEnvFromBytes
// returns the same map, including values that had to be emitted quoted
// (`#`, spaces, `"`). Values containing `\` are rejected by Apply (see
// TestApplyRejectsBackslashInEnvValue) because OpenSSH's ssh_config
// parser does not interpret the escape the same way this package
// round-trips it; covering them here would therefore be an incorrect
// contract.
func TestReadManagedEnvRoundTrip(t *testing.T) {
	cases := []map[string]string{
		{"CC_CLIP_PORT": "18340"},
		{"CC_CLIP_PORT": "18340", "CC_CLIP_STATE_DIR": "/home/shared/.cache/cc-clip/peers/peer-a"},
		{"CC_CLIP_PORT": "18340", "CC_CLIP_STATE_DIR": "/home/u with space/peers/peer a"},
		{"CC_CLIP_PORT": "18340", "CC_CLIP_STATE_DIR": "/home/u/foo#bar"},
		{"CC_CLIP_PORT": "18340", "CC_CLIP_STATE_DIR": `a"b_c`},
	}
	for i, env := range cases {
		path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
		if err := ApplyToFile(path, "myalias", env); err != nil {
			t.Fatalf("case %d Apply: %v", i, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("case %d read: %v", i, err)
		}
		got, err := ReadManagedEnvFromBytes(data, "myalias")
		if err != nil {
			t.Fatalf("case %d ReadManagedEnvFromBytes: %v", i, err)
		}
		if len(got) != len(env) {
			t.Fatalf("case %d: got %d keys, want %d; got=%v env=%v", i, len(got), len(env), got, env)
		}
		for k, want := range env {
			if got[k] != want {
				t.Errorf("case %d key %q: got %q, want %q", i, k, got[k], want)
			}
		}
	}
}

// TestReadManagedEnvReturnsNilWhenMarkerAbsent pins that a Host block
// without a managed SetEnv block yields (nil, nil) — the caller (doctor)
// treats that as "not using the multi-laptop path", not a failure.
func TestReadManagedEnvReturnsNilWhenMarkerAbsent(t *testing.T) {
	data := []byte("Host myalias\n  HostName srv.example.com\n")
	got, err := ReadManagedEnvFromBytes(data, "myalias")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil map when no managed block present, got %v", got)
	}
}

// TestReadManagedEnvReturnsNilWhenHostMissing covers the case where the
// user's ~/.ssh/config has no Host <alias> stanza at all — doctor should
// stay silent, not claim a config error.
func TestReadManagedEnvReturnsNilWhenHostMissing(t *testing.T) {
	data := []byte("Host other\n  HostName other\n")
	got, err := ReadManagedEnvFromBytes(data, "myalias")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil map when host missing, got %v", got)
	}
}

func TestReadManagedEnvRejectsMarkerWithoutSetEnvDirective(t *testing.T) {
	data := []byte(
		"Host myalias\n" +
			"  HostName srv.example.com\n" +
			"  # >>> cc-clip SetEnv (do not edit) >>>\n" +
			"  # SetEnv line removed by hand\n" +
			"  # <<< cc-clip SetEnv (do not edit) <<<\n",
	)
	got, err := ReadManagedEnvFromBytes(data, "myalias")
	if err == nil || !strings.Contains(err.Error(), "contains no SetEnv directive") {
		t.Fatalf("err = %v, want corrupted managed-block error", err)
	}
	if got != nil {
		t.Fatalf("expected nil map on parse failure, got %v", got)
	}
}

// TestApplyConcurrentIsSerialized pins that two concurrent ApplyToFile
// calls against the same config do not clobber each other — the advisory
// flock serializes them so the final file either contains one or the
// other port, never a torn mix. Without the lock, two processes reading
// the same snapshot and both writing would produce a file missing one
// of the SetEnv updates.
func TestApplyConcurrentIsSerialized(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")

	var wg sync.WaitGroup
	workers := 8
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			env := map[string]string{
				"CC_CLIP_PORT":      "1834" + string(rune('0'+i%10)),
				"CC_CLIP_STATE_DIR": "/tmp/peer-" + string(rune('a'+i%26)),
			}
			if err := ApplyToFile(path, "myalias", env); err != nil {
				t.Errorf("worker %d: Apply: %v", i, err)
			}
		}()
	}
	wg.Wait()

	got := readFile(t, path)
	// Exactly ONE marker block survives — no stacking, no half-written
	// remnants from a racing writer.
	if strings.Count(got, markerBegin) != 1 || strings.Count(got, markerEnd) != 1 {
		t.Fatalf("expected exactly one marker pair after concurrent Applies; got:\n%s", got)
	}
	// Exactly one SetEnv directive under the marker.
	if strings.Count(got, "\n  SetEnv ") != 1 {
		t.Fatalf("expected exactly one SetEnv directive; got:\n%s", got)
	}
}

// TestApplyRemoveConcurrentIsSerialized pins that concurrent ApplyToFile
// and RemoveFromFile against the same config converge to a well-defined
// state — either the Apply or the Remove wins, never a partial file
// where the marker-begin exists without the marker-end (which would
// poison every subsequent Apply).
func TestApplyRemoveConcurrentIsSerialized(t *testing.T) {
	path := writeTempConfig(t, "Host myalias\n  HostName srv\n")
	// Seed a managed block so Remove has something to do.
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18339"}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				if err := ApplyToFile(path, "myalias", map[string]string{
					"CC_CLIP_PORT": "18340",
				}); err != nil {
					t.Errorf("worker %d Apply: %v", i, err)
				}
			} else {
				if err := RemoveFromFile(path, "myalias"); err != nil {
					t.Errorf("worker %d Remove: %v", i, err)
				}
			}
		}()
	}
	wg.Wait()

	got := readFile(t, path)
	beginCount := strings.Count(got, markerBegin)
	endCount := strings.Count(got, markerEnd)
	if beginCount != endCount {
		t.Fatalf("marker pair count mismatch after concurrent Apply/Remove: begin=%d end=%d; got:\n%s", beginCount, endCount, got)
	}
	if beginCount > 1 {
		t.Fatalf("expected at most one marker pair, got %d; race produced stacked blocks:\n%s", beginCount, got)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*", "anything", true},
		{"*.example.com", "foo.example.com", true},
		{"*.example.com", "example.com", false},
		{"foo?bar", "fooXbar", true},
		{"foo?bar", "fooXXbar", false},
		{"foo*bar", "foobar", true},
		{"foo*bar", "fooXYZbar", true},
		{"foo*bar", "foobaz", false},
		{"abc", "abc", true},
		{"abc", "abcd", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// TestApplyHonorsBackslashNewlineContinuationInHost pins that a Host
// directive spread across multiple physical lines via `\<newline>` is
// parsed as a single logical line. Without continuation handling,
// `Host alpha \\\nmyalias` would tokenize to `["alpha", "\\"]` and the
// `myalias` alias would be silently missed, making Apply return
// ErrHostBlockMissing even though the user DID have a matching block.
// This is the real-world case for users who wrap long Host lines for
// readability.
func TestApplyHonorsBackslashNewlineContinuationInHost(t *testing.T) {
	path := writeTempConfig(t, "Host alpha \\\n  myalias\n  HostName srv.example.com\n  User shareduser\n")
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/u/.cache/cc-clip/peers/abc",
	}); err != nil {
		t.Fatalf("Apply: %v (continuation-wrapped Host alias was not recognized)", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# >>> cc-clip SetEnv") {
		t.Fatalf("marker not inserted for continuation-wrapped Host block:\n%s", got)
	}
}

// TestApplyDoesNotMisclassifyHostContinuationLineAsNewBlock pins that a
// backslash-newline continuation whose continuation line textually starts
// with `Host ` stays tokenised as part of the parent directive. ssh_config
// has already joined those tokens, so the outer findHostBlocks loop must
// skip past the consumed continuation lines — otherwise `Host decoy \<nl>
// Host realalias` would be parsed as two separate Host blocks and Apply
// would target the wrong one (writing the SetEnv marker inside a stanza
// the user never intended to scope to the alias).
func TestApplyDoesNotMisclassifyHostContinuationLineAsNewBlock(t *testing.T) {
	// Line 1 opens `Host decoy` with a trailing backslash; line 2's textual
	// content begins with `Host realalias` but is structurally a continuation
	// of line 1 — both tokens belong to the same logical Host directive.
	// The alias we want to target is `onlyhost`, which has its own block
	// further down. If the continuation is mis-classified as a new top-level
	// Host, `Host realalias` would match literally and the marker would end
	// up in the wrong block.
	cfg := "Host decoy \\\nHost realalias\n  HostName decoy.example.com\n\nHost onlyhost\n  HostName srv.example.com\n  User shareduser\n"
	path := writeTempConfig(t, cfg)
	if err := ApplyToFile(path, "onlyhost", map[string]string{
		"CC_CLIP_PORT":      "18340",
		"CC_CLIP_STATE_DIR": "/home/u/.cache/cc-clip/peers/abc",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	// The marker must land inside the `Host onlyhost` block, not inside the
	// decoy block. A simple way to assert that: the marker must appear AFTER
	// the `Host onlyhost` line and BEFORE EOF, and there must be exactly one.
	markerIdx := strings.Index(got, "# >>> cc-clip SetEnv")
	onlyHostIdx := strings.Index(got, "Host onlyhost")
	if markerIdx < 0 {
		t.Fatalf("marker not inserted at all:\n%s", got)
	}
	if onlyHostIdx < 0 || markerIdx < onlyHostIdx {
		t.Fatalf("marker landed in the wrong block (before Host onlyhost):\n%s", got)
	}
	if strings.Count(got, "# >>> cc-clip SetEnv") != 1 {
		t.Fatalf("expected exactly one marker block, got %d:\n%s", strings.Count(got, "# >>> cc-clip SetEnv"), got)
	}
}

// TestApplyHandlesFileWithoutTrailingNewline pins that a config file whose
// final line has no `\n` still accepts marker insertion cleanly — the
// inserted block must not glue onto the preceding line. Real-world configs
// sometimes end without a trailing newline when edited by tools that don't
// enforce POSIX text-file semantics.
func TestApplyHandlesFileWithoutTrailingNewline(t *testing.T) {
	// No final newline on the last Host line.
	path := writeTempConfig(t, "Host myalias\n  HostName srv.example.com")
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT": "18340",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	// The marker must sit on its own line, not glued onto "HostName srv.example.com".
	if strings.Contains(got, "srv.example.com  #") || strings.Contains(got, "srv.example.com# >>>") {
		t.Fatalf("marker glued onto preceding line:\n%s", got)
	}
	if !strings.Contains(got, "\n  # >>> cc-clip SetEnv") && !strings.Contains(got, "\n\t# >>> cc-clip SetEnv") {
		t.Fatalf("marker not inserted as its own indented line:\n%s", got)
	}
}

// TestApplyHandlesHostEqualsFormWithSpaces pins that Apply tolerates the
// `Host = myalias` form with whitespace around the `=`, which OpenSSH
// accepts but our tokenizer previously had zero direct coverage for.
// Regression guard for the Host=alias parsing path.
func TestApplyHandlesHostEqualsFormWithSpaces(t *testing.T) {
	path := writeTempConfig(t, "Host = myalias\n  HostName srv.example.com\n  User u\n")
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT": "18340",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# >>> cc-clip SetEnv") {
		t.Fatalf("marker not inserted for `Host = myalias` form:\n%s", got)
	}
}

// TestApplyIgnoresSetEnvInsideMatchAllBlock pins that a `SetEnv` directive
// inside a `Match all` (or any `Match`) block does NOT count as the
// target Host's SetEnv — Match blocks have different scoping rules and
// cc-clip explicitly refuses to inject into or conflict-check against
// them. Without this, a user-authored `Match all\n  SetEnv FOO=bar` would
// trip ErrSetEnvConflict for an unrelated literal Host block.
func TestApplyIgnoresSetEnvInsideMatchAllBlock(t *testing.T) {
	path := writeTempConfig(t, `Match all
  SetEnv FOO=bar

Host myalias
  HostName srv.example.com
  User shareduser
`)
	if err := ApplyToFile(path, "myalias", map[string]string{
		"CC_CLIP_PORT": "18340",
	}); err != nil {
		t.Fatalf("Apply returned %v; Match-all SetEnv must not block a literal Host apply", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# >>> cc-clip SetEnv") {
		t.Fatalf("marker not inserted alongside Match block:\n%s", got)
	}
	// The user's Match block's SetEnv must survive untouched.
	if !strings.Contains(got, "Match all") || !strings.Contains(got, "SetEnv FOO=bar") {
		t.Fatalf("user's Match block was mutated:\n%s", got)
	}
}

// TestApplyToFileReturnsErrNotExistWhenConfigMissing pins the documented
// contract: Apply does NOT create ~/.ssh/config. A caller passing a path
// to a file that does not yet exist must see an os.ErrNotExist-wrapping
// error, not a zero-byte file silently appearing at the path. This pins
// the P2-4 review finding — a future refactor that switches readConfig
// to "best-effort, tolerate missing" would flip the contract and the CLI
// would create a config file the user never opted into.
func TestApplyToFileReturnsErrNotExistWhenConfigMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "config")

	err := ApplyToFile(missing, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if err == nil {
		t.Fatal("expected error when config file is missing, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected errors.Is(err, os.ErrNotExist); got %v", err)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ApplyToFile must not create the config on a missing path; stat = %v", statErr)
	}
}

// TestRemoveFromFileIsNoOpWhenConfigMissing pins the parallel contract for
// Remove: unlike Apply, Remove is idempotent and returns nil when the
// file is absent so an uninstall run on a never-connected laptop does
// not flag a spurious error.
func TestRemoveFromFileIsNoOpWhenConfigMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "config")

	if err := RemoveFromFile(missing, "myalias"); err != nil {
		t.Fatalf("RemoveFromFile on missing config should be no-op, got %v", err)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("RemoveFromFile must not create the config on a missing path; stat = %v", statErr)
	}
}

// TestApplyHandlesLargeConfig pins that a pathologically long config (many
// Host blocks, tens of thousands of lines) still applies cleanly. Earlier
// findOrphanMarker is O(n²) on pathological input but should still finish
// in a test-friendly time for realistic sizes. This guards against a
// future refactor that turns the n² path quadratic-in-blocks.
func TestApplyHandlesLargeConfig(t *testing.T) {
	var buf strings.Builder
	for i := 0; i < 2000; i++ {
		buf.WriteString("Host hostname")
		buf.WriteString(strings.Repeat("x", 1))
		buf.WriteString("\n  HostName example.com\n")
	}
	buf.WriteString("Host myalias\n  HostName srv.example.com\n")
	path := writeTempConfig(t, buf.String())
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply on large config: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# >>> cc-clip SetEnv") {
		t.Fatalf("marker not inserted in large config (size=%d)", len(got))
	}
}

// TestApplyDetectsUserSetEnvAfterOrphanBeginMarker pins P2-14: a stray
// `# >>> cc-clip SetEnv (do not edit) >>>` with NO matching end marker
// must not suppress user-SetEnv conflict detection on subsequent lines.
// The previous implementation used a simple insideManagedMarker flag
// that latched on begin and only cleared on end, so any user SetEnv
// after an orphan begin was silently classified as managed.
func TestApplyDetectsUserSetEnvAfterOrphanBeginMarker(t *testing.T) {
	original := `Host myalias
  HostName srv
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv FOO=bar
`
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("expected ErrSetEnvConflict even after orphan begin marker, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated while rejecting conflict:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

// TestApplyDetectsUserSetEnvAfterOrphanEndMarker covers the symmetric
// case: a stray end marker without a matching begin must also not
// confuse conflict detection.
func TestApplyDetectsUserSetEnvAfterOrphanEndMarker(t *testing.T) {
	original := `Host myalias
  HostName srv
  # <<< cc-clip SetEnv (do not edit) <<<
  SetEnv FOO=bar
`
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("expected ErrSetEnvConflict with orphan end marker, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated while rejecting conflict:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

// TestApplyDetectsUserSetEnvWithMismatchedMarkerCounts covers the
// broader P2-14 case: two begin markers but only one end should NOT
// produce a latched "always inside" region that swallows user SetEnv.
func TestApplyDetectsUserSetEnvWithMismatchedMarkerCounts(t *testing.T) {
	original := `Host myalias
  HostName srv
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340
  # <<< cc-clip SetEnv (do not edit) <<<
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv FOO=bar
`
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18341"})
	if !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("expected ErrSetEnvConflict with mismatched marker counts, got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("config was mutated while rejecting conflict:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

// TestApplyOrphanSweepPreservesBlanksAndCommentsBetweenOrphanAndLegacySetEnv
// pins the surgical-removal fix. `findAdjacentManagedSetEnv` skips over
// blank lines and comments when scanning, so an orphan followed by
// "blank, blank, comment, managed-SetEnv" is treated as adjacent. The
// previous removal was an inclusive `[min..max]` span — it deleted the
// blanks and comment between them as a side effect, violating
// "never touch user content". The fix removes ONLY the two specific
// indices (orphan + SetEnv), preserving everything in between.
func TestApplyOrphanSweepPreservesBlanksAndCommentsBetweenOrphanAndLegacySetEnv(t *testing.T) {
	// Orphan begin marker (no matching end), then two blank lines, then
	// a user-authored comment, then the legacy managed SetEnv that the
	// orphan-adjacent sweep reaps. After Apply, the comment and one or
	// both blank lines must remain in the rewritten file.
	original := "Host myalias\n" +
		"  HostName srv\n" +
		"  " + markerBegin + "\n" +
		"\n" +
		"\n" +
		"  # user-authored comment that must survive\n" +
		"  SetEnv CC_CLIP_PORT=18339\n"
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# user-authored comment that must survive") {
		t.Fatalf("user comment was deleted by orphan sweep:\n%s", got)
	}
	// Old SetEnv with port 18339 must be gone, replaced by 18340 inside
	// a fresh marker pair.
	if strings.Contains(got, "CC_CLIP_PORT=18339") {
		t.Fatalf("expected legacy SetEnv to be reaped; got:\n%s", got)
	}
	if !strings.Contains(got, "CC_CLIP_PORT=18340") {
		t.Fatalf("expected new SetEnv to be written; got:\n%s", got)
	}
	// Exactly one new begin marker; the orphan should be gone.
	if got, want := strings.Count(got, markerBegin), 1; got != want {
		t.Fatalf("expected exactly %d begin marker after rewrite, got %d", want, got)
	}
}

// TestApplyPreservesUserSetEnvContainingCCClipPortSubstring pins P2-15:
// a user-authored SetEnv whose VALUE happens to contain the literal
// substring `CC_CLIP_PORT=` must NOT be misclassified as managed and
// deleted by the orphan-adjacent sweep. The previous substring-based
// findAdjacentManagedSetEnv would match on any occurrence, regardless
// of whether the key itself was CC_CLIP_PORT.
func TestApplyPreservesUserSetEnvContainingCCClipPortSubstring(t *testing.T) {
	// A user SetEnv whose KEY is MY_VAR but whose VALUE contains the
	// literal substring `CC_CLIP_PORT=`. We verify it's rejected as a
	// user-authored conflict (not silently deleted) on Apply.
	original := `Host myalias
  HostName srv
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv MY_VAR="prefix-CC_CLIP_PORT=foo"
`
	path := writeTempConfig(t, original)
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSetEnvConflict) {
		t.Fatalf("expected ErrSetEnvConflict (user SetEnv w/ substring must not be masked as managed), got %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("user SetEnv with CC_CLIP_PORT= substring was mutated:\n--- got\n%s\n--- want\n%s", got, original)
	}
}

// TestApplyPreservesCRLFMajorityOverStrayCR pins P2-17: a mostly-LF
// file with one stray `\r` must stay LF on rewrite. The previous
// "any line with \r wins" logic would flip the whole file to CRLF,
// turning a one-line hand-edit artifact into a platform-style
// conversion.
func TestApplyPreservesCRLFMajorityOverStrayCR(t *testing.T) {
	// Four LF lines, one with a stray \r before the \n.
	original := "Host myalias\n  HostName srv\r\n  User alice\n  Port 22\n"
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	// Majority was LF; the output must not contain \r\n.
	if strings.Contains(got, "\r\n") {
		t.Fatalf("expected LF output despite minority CRLF line; got:\n%q", got)
	}
	// Sanity: the marker pair was still written.
	if !strings.Contains(got, markerBegin) {
		t.Fatalf("expected marker inserted; got:\n%s", got)
	}
}

// TestApplyPreservesLFWhenCRLFCountTies covers the tie-break rule: when
// CRLF and LF line counts are equal, LF wins (matches the Unix-authored
// common case and keeps behavior deterministic).
func TestApplyPreservesLFWhenCRLFCountTies(t *testing.T) {
	// Two LF lines and two CRLF lines: counts tie, fall back to LF.
	original := "Host myalias\n  HostName srv\r\n  User alice\r\n  Port 22\n"
	path := writeTempConfig(t, original)
	if err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "\r\n") {
		t.Fatalf("expected LF output on tie; got:\n%q", got)
	}
}

// TestApplyRejectsMixedLiteralWildcardHostBlock pins P2-A: a Host block
// whose token list contains BOTH a literal matching the alias AND a
// non-negated wildcard (e.g. `Host myalias *`) must be rejected with
// ErrOnlyGlobMatch. Accepting it would inject CC_CLIP_* into a block
// OpenSSH applies to every host, leaking per-laptop env to every ssh
// target.
func TestApplyRejectsMixedLiteralWildcardHostBlock(t *testing.T) {
	cases := []struct {
		name   string
		config string
		alias  string
	}{
		{
			name:   "literal_then_star",
			config: "Host myalias *\n  HostName srv\n",
			alias:  "myalias",
		},
		{
			name:   "star_then_literal",
			config: "Host * myalias\n  HostName srv\n",
			alias:  "myalias",
		},
		{
			name:   "literal_between_two_globs",
			config: "Host dev-* myalias prod-*\n  HostName srv\n",
			alias:  "myalias",
		},
		{
			name:   "literal_with_qmark_glob",
			config: "Host myalias foo?\n  HostName srv\n",
			alias:  "myalias",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.config)
			err := ApplyToFile(path, tc.alias, map[string]string{"CC_CLIP_PORT": "18340"})
			if !errors.Is(err, ErrOnlyGlobMatch) {
				t.Fatalf("expected ErrOnlyGlobMatch, got %v", err)
			}
			if got := readFile(t, path); got != tc.config {
				t.Fatalf("config was mutated on mixed literal+wildcard rejection:\n--- got\n%s\n--- want\n%s", got, tc.config)
			}
		})
	}
}

// TestApplyAcceptsLiteralWithNegatedWildcard confirms the flipside of
// P2-A: a Host token list with a literal match AND a NEGATED wildcard
// (e.g. `Host myalias !*.example.com`) does not broaden the match,
// because the negated wildcard only excludes hosts — it doesn't add any.
// Treating this as mixed-glob-reject would surprise users whose stanza
// uses negation to narrow rather than expand scope.
func TestApplyAcceptsLiteralWithNegatedWildcard(t *testing.T) {
	path := writeTempConfig(t, "Host myalias !*.example.com\n  HostName srv\n")
	err := ApplyToFile(path, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if err != nil {
		t.Fatalf("Apply should accept literal+negated-wildcard, got %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# >>> cc-clip SetEnv") {
		t.Fatalf("expected marker inserted; got:\n%s", got)
	}
}

// TestReadConfigRejectsOversizeFile pins P2-B: ~/.ssh/config must be
// capped so a runaway or hostile writer cannot force unbounded memory
// allocation in readConfig. Exactly at the cap must still parse; one
// byte over must fail closed with ErrSSHConfigTooLarge.
func TestReadConfigRejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	overPath := filepath.Join(dir, "over")
	// maxSSHConfigSize+1 bytes: clearly over the cap.
	if err := os.WriteFile(overPath, make([]byte, maxSSHConfigSize+1), 0o600); err != nil {
		t.Fatalf("seed over: %v", err)
	}
	err := ApplyToFile(overPath, "myalias", map[string]string{"CC_CLIP_PORT": "18340"})
	if !errors.Is(err, ErrSSHConfigTooLarge) {
		t.Fatalf("expected ErrSSHConfigTooLarge for over-cap file, got %v", err)
	}

	// At exactly the cap, the size gate must NOT fire. We pad with
	// comment lines so the content is still syntactically valid
	// ssh_config (an all-NUL blob would trip other validation).
	atCapPath := filepath.Join(dir, "atcap")
	header := "Host myalias\n  HostName srv\n"
	padding := strings.Repeat("# p\n", (maxSSHConfigSize-len(header))/4)
	body := header + padding
	// Pad the remainder with spaces so we land exactly on the cap.
	if rem := maxSSHConfigSize - len(body); rem > 0 {
		body += strings.Repeat(" ", rem-1) + "\n"
	}
	if len(body) != maxSSHConfigSize {
		t.Fatalf("seed size = %d, want %d", len(body), maxSSHConfigSize)
	}
	if err := os.WriteFile(atCapPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed at-cap: %v", err)
	}
	if err := ApplyToFile(atCapPath, "myalias", map[string]string{"CC_CLIP_PORT": "18340"}); err != nil {
		if errors.Is(err, ErrSSHConfigTooLarge) {
			t.Fatalf("at-cap file (%d bytes) should not trigger size cap, got %v", len(body), err)
		}
		// Other errors (e.g. parse) are acceptable here; the test's
		// contract is specifically about the size gate.
	}
}

// TestSetEnvIsManagedOnly pins P3-F: the helper identifies lines that
// cc-clip itself wrote, used by the orphan-adjacent sweep to know what
// it is allowed to delete. A user SetEnv whose VALUE happens to contain
// the literal substring `CC_CLIP_PORT=` must NOT be classified as
// managed — key equality is what matters, not a substring match.
func TestSetEnvIsManagedOnly(t *testing.T) {
	cases := []struct {
		name string
		rest string
		want bool
	}{
		{
			name: "single_managed_key",
			rest: "CC_CLIP_PORT=18340",
			want: true,
		},
		{
			name: "both_managed_keys",
			rest: "CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/u/.cache/cc-clip/peers/abc",
			want: true,
		},
		{
			name: "managed_key_plus_user_key",
			rest: "CC_CLIP_PORT=18340 MY_VAR=foo",
			want: false,
		},
		{
			name: "user_only",
			rest: "MY_VAR=foo",
			want: false,
		},
		{
			name: "quoted_value_with_embedded_managed_substring",
			rest: `MY_VAR="prefix-CC_CLIP_PORT=foo"`,
			want: false,
		},
		{
			name: "quoted_value_with_embedded_equals",
			rest: `CC_CLIP_STATE_DIR="/path=weird" CC_CLIP_PORT=18340`,
			want: true,
		},
		{
			name: "empty_rest",
			rest: "",
			want: false,
		},
		{
			name: "token_without_equals",
			rest: "CC_CLIP_PORT",
			want: false,
		},
		{
			name: "unterminated_quote",
			rest: `CC_CLIP_PORT="18340`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := setEnvIsManagedOnly(tc.rest)
			if got != tc.want {
				t.Fatalf("setEnvIsManagedOnly(%q) = %v, want %v", tc.rest, got, tc.want)
			}
		})
	}
}

// TestCollectManagedMarkerPairs pins P3-F: the helper walks a block and
// returns only properly-paired marker ranges. Orphan begins / ends and
// mismatched counts must be omitted so callers can treat them as
// non-managed regions and sweep them via findOrphanMarker.
func TestCollectManagedMarkerPairs(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		// pairs are expressed as [begin, end] indices into lines.
		wantPairs [][2]int
	}{
		{
			name: "B1_B2_E1_E2",
			// Two begins then two ends. Per findMarkerPair's first-begin →
			// first-following-end pairing, this is one pair (B1,E1),
			// which consumes the inner B2 — cursor advances past E1 and
			// the orphan E2 that remains has no partner.
			lines: []string{
				"Host myalias",
				markerBegin,
				markerBegin,
				markerEnd,
				markerEnd,
			},
			wantPairs: [][2]int{{1, 3}},
		},
		{
			name: "E1_B1_E2",
			// Orphan end, then a real begin with a following end. Only
			// the (B1,E2) pair should be returned.
			lines: []string{
				"Host myalias",
				markerEnd,
				markerBegin,
				markerEnd,
			},
			wantPairs: [][2]int{{2, 3}},
		},
		{
			name: "B1_E1_B2_E2",
			// Two disjoint pairs: both should be returned in order.
			lines: []string{
				"Host myalias",
				markerBegin,
				markerEnd,
				markerBegin,
				markerEnd,
			},
			wantPairs: [][2]int{{1, 2}, {3, 4}},
		},
		{
			name: "B1_setenv_B2_setenv_E1",
			// One nested begin whose end is shared with the outer: pair
			// (B1,E1) is returned, covering both managed SetEnv lines.
			lines: []string{
				"Host myalias",
				markerBegin,
				"  SetEnv CC_CLIP_PORT=1",
				markerBegin,
				"  SetEnv CC_CLIP_PORT=2",
				markerEnd,
			},
			wantPairs: [][2]int{{1, 5}},
		},
		{
			name: "no_markers",
			lines: []string{
				"Host myalias",
				"  HostName srv",
			},
			wantPairs: nil,
		},
		{
			name: "orphan_begin_only",
			lines: []string{
				"Host myalias",
				markerBegin,
				"  SetEnv CC_CLIP_PORT=1",
			},
			wantPairs: nil,
		},
		{
			name: "orphan_end_only",
			lines: []string{
				"Host myalias",
				"  SetEnv CC_CLIP_PORT=1",
				markerEnd,
			},
			wantPairs: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectManagedMarkerPairs(tc.lines, 0, len(tc.lines))
			if len(got) != len(tc.wantPairs) {
				t.Fatalf("got %d pairs, want %d (got=%v want=%v)", len(got), len(tc.wantPairs), got, tc.wantPairs)
			}
			for i, want := range tc.wantPairs {
				if got[i] != want {
					t.Errorf("pair %d: got %v, want %v", i, got[i], want)
				}
			}
		})
	}
}
