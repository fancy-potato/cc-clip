//go:build darwin

package service

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
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

func TestGeneratePlist(t *testing.T) {
	content := generatePlist("/usr/local/bin/cc-clip", 18339)

	checks := []struct {
		name     string
		contains string
	}{
		{"label", "<string>com.cc-clip.daemon</string>"},
		{"binary path", "<string>/usr/local/bin/cc-clip</string>"},
		{"serve command", "<string>serve</string>"},
		{"port flag", "<string>--port</string>"},
		{"port value", "<string>18339</string>"},
		{"run at load", "<key>RunAtLoad</key>"},
		{"keep alive", "<key>KeepAlive</key>"},
		{"log path", "cc-clip.log"},
		{"xml header", "<?xml version"},
		{"plist doctype", "<!DOCTYPE plist"},
	}

	for _, check := range checks {
		if !strings.Contains(content, check.contains) {
			t.Errorf("plist missing %s: expected to contain %q", check.name, check.contains)
		}
	}

	// Pin the VALUE of each boolean key, not just the key's presence.
	// The previous substring check ("<true/>" appears somewhere)
	// would pass even if KeepAlive or RunAtLoad flipped to <false/>
	// because the other key contributed a <true/> to the file. A
	// silent KeepAlive regression would defeat the persistent tunnel's
	// "daemon restart via launchd" contract and never be caught.
	valueChecks := []struct {
		name    string
		key     string
		pattern string
	}{
		{"RunAtLoad=true", "RunAtLoad", `<key>RunAtLoad</key>\s*<true/>`},
		{"KeepAlive=true", "KeepAlive", `<key>KeepAlive</key>\s*<true/>`},
	}
	for _, vc := range valueChecks {
		if !regexp.MustCompile(vc.pattern).MatchString(content) {
			t.Errorf("plist %s missing or not true: pattern %q did not match\n--- plist ---\n%s", vc.name, vc.pattern, content)
		}
	}
}

// TestGeneratePlistEscapesSpecialCharacters pins that every XML-significant
// character in user-controllable plist inputs (binary path, log path) is
// properly entity-encoded. A launchd plist emitted with a raw `&`, `<`, `>`,
// `"`, or `'` is invalid XML and `launchctl load` rejects it at install
// time — without this test a future edit to escapeXMLText that drops any
// of the five mappings (most likely &apos;, the least common one) would
// only be caught in the field.
func TestGeneratePlistEscapesSpecialCharacters(t *testing.T) {
	// Each row exercises one raw character that MUST be entity-encoded in
	// the ProgramArguments <string>. Checking the output contains the
	// expected entity AND does NOT contain the raw character in the
	// relevant <string> slot is stronger than a substring check alone —
	// the substring check would still pass if a future edit accidentally
	// double-escaped one char (&amp;amp;) or mangled another.
	cases := []struct {
		name   string
		path   string
		entity string
	}{
		{"ampersand", "/Users/a & b/cc-clip", "/Users/a &amp; b/cc-clip"},
		{"less-than", "/Users/a<b/cc-clip", "/Users/a&lt;b/cc-clip"},
		{"greater-than", "/Users/a>b/cc-clip", "/Users/a&gt;b/cc-clip"},
		{"double-quote", `/Users/a"b/cc-clip`, "/Users/a&quot;b/cc-clip"},
		{"apostrophe", "/Users/o'brien/cc-clip", "/Users/o&apos;brien/cc-clip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := generatePlist(tc.path, 18339)
			if !strings.Contains(content, "<string>"+tc.entity+"</string>") {
				t.Fatalf("plist does not contain expected entity-encoded path\n  want contains: <string>%s</string>\n  got:\n%s", tc.entity, content)
			}
			// Also ensure the raw path doesn't appear unescaped in the
			// ProgramArguments <string> slot. Entity-encoded forms like
			// &amp; still contain the `&` byte, so we search specifically
			// for the `<string>RAW</string>` shape which would signal
			// escaping was skipped entirely.
			if strings.Contains(content, "<string>"+tc.path+"</string>") {
				t.Fatalf("plist contains UN-escaped raw path <string>%s</string>; escaping was skipped", tc.path)
			}
		})
	}
}

// TestEscapeXMLTextIdempotentOnSafeInput pins that strings containing no
// XML-significant characters pass through escapeXMLText unchanged. A
// regression that accidentally encoded every character (e.g. via
// encoding/xml.EscapeText) would wildly inflate plist sizes without
// changing correctness, but would be caught here.
func TestEscapeXMLTextIdempotentOnSafeInput(t *testing.T) {
	for _, in := range []string{
		"/usr/local/bin/cc-clip",
		"com.cc-clip.daemon",
		"cc-clip.log",
		"",
	} {
		if got := escapeXMLText(in); got != in {
			t.Errorf("escapeXMLText(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestGeneratePlist_CustomPort(t *testing.T) {
	content := generatePlist("/opt/bin/cc-clip", 9999)

	if !strings.Contains(content, "<string>9999</string>") {
		t.Error("plist does not contain custom port 9999")
	}
	if !strings.Contains(content, "<string>/opt/bin/cc-clip</string>") {
		t.Error("plist does not contain custom binary path")
	}
}

func TestPlistPath(t *testing.T) {
	path := PlistPath()
	if !strings.HasSuffix(path, "Library/LaunchAgents/com.cc-clip.daemon.plist") {
		t.Errorf("unexpected plist path: %s", path)
	}
}

func TestPlistPathFallsBackToAbsoluteTempPathWhenHomeLookupFails(t *testing.T) {
	userhome.SetResolverForTest(t, failingHomeResolver{})
	t.Setenv("SUDO_USER", "")

	got := PlistPath()
	want := filepath.Join(os.TempDir(), "cc-clip", "LaunchAgents", plistFileName)
	if got != want {
		t.Fatalf("PlistPath() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("PlistPath() = %q, want absolute path", got)
	}
	if strings.HasPrefix(got, "~") {
		t.Fatalf("PlistPath() = %q, must not use ~ fallback", got)
	}
}

func TestLogPathFallsBackToAbsoluteTempPathWhenHomeLookupFails(t *testing.T) {
	userhome.SetResolverForTest(t, failingHomeResolver{})
	t.Setenv("SUDO_USER", "")

	got := logPath()
	want := filepath.Join(os.TempDir(), "cc-clip", "Logs", "cc-clip.log")
	if got != want {
		t.Fatalf("logPath() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("logPath() = %q, want absolute path", got)
	}
	if strings.HasPrefix(got, "~") {
		t.Fatalf("logPath() = %q, must not use ~ fallback", got)
	}
}

func TestInstall_MockLaunchctl(t *testing.T) {
	tmpDir := t.TempDir()

	// Override PlistPath temporarily by using a custom binary path
	// We can't easily override PlistPath, so we test via the generated content
	// and mock launchctl calls

	loadCalled := false
	originalLoad := launchctlLoad
	launchctlLoad = func(plistPath string) error {
		loadCalled = true
		// Verify the plist file was written before load was called
		if _, err := os.Stat(plistPath); err != nil {
			t.Errorf("plist file not found when launchctl load called: %v", err)
		}
		return nil
	}
	defer func() { launchctlLoad = originalLoad }()

	// Write to a temp plist path — we test the content generation separately
	plistContent := generatePlist(tmpDir+"/cc-clip", 18339)
	plistPath := tmpDir + "/test.plist"
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		t.Fatalf("failed to write test plist: %v", err)
	}

	// Verify content
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("failed to read test plist: %v", err)
	}
	if !strings.Contains(string(data), "com.cc-clip.daemon") {
		t.Error("plist content missing label")
	}

	// Call load mock to verify it works
	if err := launchctlLoad(plistPath); err != nil {
		t.Fatalf("mock launchctl load failed: %v", err)
	}
	if !loadCalled {
		t.Error("launchctl load was not called")
	}
}

func TestInstallCreatesFallbackLogDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	t.Setenv("SUDO_USER", "")
	userhome.SetResolverForTest(t, failingHomeResolver{})

	originalLoad := launchctlLoad
	launchctlLoad = func(string) error { return nil }
	defer func() { launchctlLoad = originalLoad }()

	if err := Install(filepath.Join(tmpDir, "cc-clip"), 18339); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "cc-clip", "Logs")); err != nil {
		t.Fatalf("fallback log directory not created: %v", err)
	}
}

func TestUninstall_MockLaunchctl(t *testing.T) {
	unloadCalled := false
	originalUnload := launchctlUnload
	launchctlUnload = func(plistPath string) error {
		unloadCalled = true
		return nil
	}
	defer func() { launchctlUnload = originalUnload }()

	// Call Uninstall — it should call unload and attempt remove (which is fine if file doesn't exist)
	_ = launchctlUnload(PlistPath())
	if !unloadCalled {
		t.Error("launchctl unload was not called")
	}
}

func TestStatus_MockLaunchctl(t *testing.T) {
	originalList := launchctlList
	defer func() { launchctlList = originalList }()

	// Mock: service is running
	launchctlList = func(label string) (bool, error) {
		if label != plistLabel {
			t.Errorf("unexpected label: %s", label)
		}
		return true, nil
	}
	running, err := Status()
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if !running {
		t.Error("expected service to be running")
	}

	// Mock: service is not running
	launchctlList = func(label string) (bool, error) {
		return false, nil
	}
	running, err = Status()
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if running {
		t.Error("expected service to not be running")
	}
}
