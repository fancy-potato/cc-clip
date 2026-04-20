//go:build darwin

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shunmei/cc-clip/internal/userhome"
)

type failingHomeResolver struct{}

func (failingHomeResolver) LookupUser(string) (*user.User, error) {
	return nil, errors.New("unexpected lookup")
}

func (failingHomeResolver) UserHomeDir() (string, error) {
	return "", errors.New("boom")
}

func (failingHomeResolver) IsSudoRoot() bool { return false }

// TestSanitizePreviewSessionIDRejectsTraversal pins the filename-safety
// contract: a malicious sessionID like "../evil" MUST NOT be able to redirect
// the filepath.Join'd preview path outside previewDir. The 8-char truncation
// alone is insufficient (len("../evil")==7 passes it), so the rune-allowlist
// defense is what actually guarantees containment.
func TestSanitizePreviewSessionIDRejectsTraversal(t *testing.T) {
	previewDir := "/tmp/previewdir"
	cases := []struct {
		name string
		sid  string
	}{
		{"dot-dot-slash traversal", "../evil"},
		{"absolute-ish", "/etc/passwd"},
		{"null byte", "a\x00b"},
		{"unicode trickery", "\u2028hax"},
		{"long malicious", "../../../etc/shadow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := sanitizePreviewSessionID(tc.sid)
			if strings.ContainsAny(sid, "/\\") {
				t.Fatalf("sanitized sid %q still contains path separator", sid)
			}
			if strings.Contains(sid, "..") {
				t.Fatalf("sanitized sid %q still contains dot-dot", sid)
			}
			path := filepath.Join(previewDir, fmt.Sprintf("preview-%s-%d.png", sid, 1))
			rel, err := filepath.Rel(previewDir, path)
			if err != nil {
				t.Fatalf("filepath.Rel: %v", err)
			}
			if strings.HasPrefix(rel, "..") {
				t.Fatalf("sanitized path %q escapes previewDir %q (rel=%q)", path, previewDir, rel)
			}
		})
	}
}

func TestSanitizePreviewSessionIDEmptyYieldsUnknown(t *testing.T) {
	if got := sanitizePreviewSessionID(""); got != "unknown" {
		t.Fatalf("got %q, want unknown", got)
	}
	// sid made entirely of disallowed bytes still yields non-empty output
	// (each byte becomes '_') so we do not fall back to "unknown".
	if got := sanitizePreviewSessionID("///"); got != "___" {
		t.Fatalf("got %q, want ___", got)
	}
}

func TestSanitizePreviewSessionIDTruncates(t *testing.T) {
	sid := sanitizePreviewSessionID("abcdefghijklmnop")
	if len(sid) != 8 {
		t.Fatalf("len(%q)=%d, want 8", sid, len(sid))
	}
	if sid != "abcdefgh" {
		t.Fatalf("got %q, want abcdefgh", sid)
	}
}

func TestSanitizeForAppleScriptStripsHostileRunes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello world", "hello world"},
		{"bye\"quote", "bye\"quote"},
		{"line\nbreak", "line_break"},
		{"carriage\rreturn", "carriage_return"},
		{"null\x00byte", "null_byte"},
		{"line\u2028sep", "line_sep"},
		{"para\u2029sep", "para_sep"},
		{"unicode\u00e9", "unicode_"},
		{"tab\there", "tab\there"},
	}
	for _, tc := range cases {
		if got := sanitizeForAppleScript(tc.in); got != tc.want {
			t.Errorf("sanitizeForAppleScript(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDarwinPreviewDirFallsBackToAbsoluteTempPathWhenHomeLookupFails(t *testing.T) {
	userhome.SetResolverForTest(t, failingHomeResolver{})
	t.Setenv("SUDO_USER", "")

	got := darwinPreviewDir()
	want := filepath.Join(os.TempDir(), "cc-clip", "previews")
	if got != want {
		t.Fatalf("darwinPreviewDir() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("darwinPreviewDir() = %q, want absolute path", got)
	}
	if strings.HasPrefix(got, "~") {
		t.Fatalf("darwinPreviewDir() = %q, must not use ~ fallback", got)
	}
}

func TestNewDarwinNotifierUsesAbsoluteFallbackPreviewDirWhenHomeLookupFails(t *testing.T) {
	userhome.SetResolverForTest(t, failingHomeResolver{})
	t.Setenv("SUDO_USER", "")

	n := NewDarwinNotifier()
	want := filepath.Join(os.TempDir(), "cc-clip", "previews")
	if n.previewDir != want {
		t.Fatalf("previewDir = %q, want %q", n.previewDir, want)
	}
	if !filepath.IsAbs(n.previewDir) {
		t.Fatalf("previewDir = %q, want absolute path", n.previewDir)
	}
}
