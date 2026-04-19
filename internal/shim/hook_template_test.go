package shim

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHookTemplateUsesNotificationNonceAndHealthLog(t *testing.T) {
	got := HookScript(18339)
	for _, needle := range []string{
		"notify.nonce",
		"notify-health.log",
		"application/x-claude-hook",
		"Authorization: Bearer $_nonce",
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("expected template to contain %q", needle)
		}
	}
}

func TestHookScriptSubstitutesPort(t *testing.T) {
	got := HookScript(19999)
	if !strings.Contains(got, "19999") {
		t.Fatal("expected template to contain the substituted port 19999")
	}
	// The default port line should have the substituted value, not a format directive.
	// Note: %d also appears in date formats (%Y-%m-%dT) which is expected.
	if strings.Contains(got, "${CC_CLIP_PORT:-%"+"d}") {
		t.Fatal("template still contains unsubstituted port format directive")
	}
}

func TestHookScriptDoesNotUseSessionToken(t *testing.T) {
	got := HookScript(18339)
	if strings.Contains(got, "session.token") {
		t.Fatal("hook template must use notify.nonce, not session.token")
	}
}

func TestHookScriptAlwaysExitsZero(t *testing.T) {
	got := HookScript(18339)
	if !strings.Contains(got, "exit 0") {
		t.Fatal("hook script must always exit 0 to avoid blocking Claude Code")
	}
}

func TestHookScriptSupportsStrictHealthChecks(t *testing.T) {
	got := HookScript(18339)
	for _, needle := range []string{
		"CC_CLIP_STRICT",
		`cc-clip-hook health probe failed: http=$_http_code`,
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("expected template to contain %q", needle)
		}
	}
}

func TestHookScriptPassesHostAliasViaEnvNotShellInterpolation(t *testing.T) {
	got := HookScript(18339)
	// Host alias is attacker-influenced (hostname -s output, or a user-set
	// env var). Interpolating it raw into Python source allows a hostname
	// containing `'` to break out of the string literal and inject Python.
	// The safe path is to pass it via an environment variable and read it
	// with os.environ inside Python.
	if strings.Contains(got, "'${_CC_CLIP_HOST_ALIAS}'") {
		t.Fatal("hook script must not interpolate _CC_CLIP_HOST_ALIAS into Python source; pass via env instead")
	}
	for _, needle := range []string{
		`CC_CLIP_HOST_ALIAS="$_CC_CLIP_HOST_ALIAS" python3`,
		`os.environ.get("CC_CLIP_HOST_ALIAS"`,
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("expected template to contain %q", needle)
		}
	}
}

func TestHookScriptIsValidBash(t *testing.T) {
	got := HookScript(18339)
	if !strings.HasPrefix(got, "#!/usr/bin/env bash") {
		t.Fatal("hook script must start with bash shebang")
	}
	if !strings.Contains(got, "set -euo pipefail") {
		t.Fatal("hook script must use strict mode")
	}
}

// TestHookScriptPassesBashSyntaxCheck runs the emitted script through
// `bash -n` so any future refactor that accidentally introduces a syntax
// error (unbalanced quoting, broken heredoc, etc.) fails in CI instead of
// on a remote server at notify time. This complements the string-match
// tests: a substring-only assertion can miss shell-level breakage.
func TestHookScriptPassesBashSyntaxCheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	got := HookScript(18339)
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(got)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n rejected hook script: %v\noutput: %s", err, out)
	}
}

// TestHookScriptHostAliasSurvivesQuoteInjection verifies that a hostname
// containing characters that would break out of a naive single-quoted
// Python interpolation (`'`, `"`, `$`, `` ` ``) does NOT cause the script
// to crash and does NOT execute host-supplied code. Runs the emitted
// script as a standalone file with a poisoned CC_CLIP_HOST_ALIAS and a
// valid stdin payload; asserts the script exits 0 (fire-and-forget contract),
// that `id`'s output never appears in the combined output (which would
// signal the alias was interpreted as Python), and that the payload POSTed
// to a local receiver carries `_cc_clip_host` equal to the poisoned alias
// as a literal string — proving the Python branch actually reads the env
// var (not just that injection is silently dropped).
func TestHookScriptHostAliasSurvivesQuoteInjection(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()

	var (
		mu       sync.Mutex
		gotBody  []byte
		gotAuth  string
		gotHits  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotAuth = r.Header.Get("Authorization")
		gotHits++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}
	port := u.Port()

	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("test-nonce\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Hostname that used to be able to inject Python via `'` + newline:
	//   ','); import os; os.system('id'); #
	poison := `'); import os; os.system('id');#"$` + "`foo`"
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"CC_CLIP_HOST_ALIAS="+poison,
		"CC_CLIP_STATE_DIR="+dir,
		"CC_CLIP_PORT="+port,
		// Leave CC_CLIP_STRICT unset so any POST hiccup still exits 0.
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification","type":"idle_prompt"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script exited nonzero with poisoned host alias: %v\noutput: %s", err, out)
	}
	if strings.Contains(string(out), "uid=") {
		t.Fatalf("poisoned host alias executed as Python: %s", out)
	}

	mu.Lock()
	hits, body, auth := gotHits, gotBody, gotAuth
	mu.Unlock()

	if hits != 1 {
		t.Fatalf("expected exactly 1 POST to /notify, got %d", hits)
	}
	if auth != "Bearer test-nonce" {
		t.Fatalf("Authorization header = %q, want %q", auth, "Bearer test-nonce")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v\nbody=%s", err, body)
	}
	got, ok := payload["_cc_clip_host"].(string)
	if !ok {
		t.Fatalf("_cc_clip_host missing or not a string: %v", payload["_cc_clip_host"])
	}
	if got != poison {
		t.Fatalf("_cc_clip_host = %q; want literal poison %q (proves env var is read, not stripped)", got, poison)
	}
}

// TestHookScriptHostAliasMissingPython3FallbackPreservesPayload pins the
// degraded behavior described in the template comment: when python3 is
// absent, the script posts the original payload without `_cc_clip_host`
// instead of failing. Complements TestHookScriptHostAliasSurvivesQuoteInjection,
// which exercises the happy path.
func TestHookScriptHostAliasMissingPython3FallbackPreservesPayload(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	dir := t.TempDir()

	var (
		mu      sync.Mutex
		gotBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port := u.Port()

	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	// Shim PATH to resolve `cat`, `head`, `date`, `curl` but never `python3`.
	// We discover each tool's real location and symlink it into a tempdir.
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for _, tool := range []string{"bash", "cat", "head", "date", "curl"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(binDir, tool)); err != nil {
			t.Fatalf("symlink %s: %v", tool, err)
		}
	}

	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := exec.Command(filepath.Join(binDir, "bash"), scriptPath)
	cmd.Env = []string{
		"PATH=" + binDir,
		"HOME=" + dir,
		"CC_CLIP_STATE_DIR=" + dir,
		"CC_CLIP_PORT=" + port,
		"CC_CLIP_HOST_ALIAS=pythonless.example",
	}
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook script exited nonzero without python3: %v\noutput: %s", err, out)
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()

	if len(body) == 0 {
		t.Fatal("receiver got no body; expected original payload to be posted")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("fallback payload is not valid JSON: %v\nbody=%s", err, body)
	}
	if _, ok := payload["_cc_clip_host"]; ok {
		t.Fatalf("_cc_clip_host unexpectedly present in python3-less fallback: %s", body)
	}
	if got, _ := payload["hook_event_name"].(string); got != "notification" {
		t.Fatalf("fallback dropped original fields: hook_event_name=%q (want %q)", got, "notification")
	}
}

// TestHookScriptFallbackPreservesExitCodeWithoutPython pins the degraded
// behavior when python3 is missing: the hook must still exit 0. Runs with
// PATH scoped to a directory that has neither python3 nor curl; the POST
// will fail (no curl) but, because CC_CLIP_STRICT is not set, the script's
// fire-and-forget contract says it exits 0 anyway.
func TestHookScriptFallbackPreservesExitCodeWithoutPython(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Provide a minimal PATH that resolves `head`, `date`, `cat`, but not
	// python3 or curl. The script uses `set -euo pipefail`; the python3
	// branch's `|| echo ...` keeps the pipeline alive, and the curl branch's
	// `|| _http_code="000"` absorbs the missing binary.
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH=/usr/bin:/bin",
		"CC_CLIP_HOST_ALIAS=pythonless.example",
		"CC_CLIP_STATE_DIR="+dir,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Note: this test will only catch a regression on systems where the
		// minimal PATH genuinely lacks python3. On systems where python3 is
		// at /usr/bin/python3 it still passes (python3 handles the payload).
		// Either way, exit != 0 is a fire-and-forget contract violation.
		t.Fatalf("hook script exited nonzero when python3/curl absent: %v\noutput: %s", err, out)
	}
}
