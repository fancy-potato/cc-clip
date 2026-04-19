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

// TestHookScriptStrictModePrefixIsExportedConstant cross-pins the strict-
// mode error wording with the exported HookHealthFailurePrefix constant
// that runRemoteNotificationHealthProbe (cmd/cc-clip/main.go) keys off.
// Without this test, a future template refactor that reworded the printf
// could pass the loose substring test above (which only checks for the
// shell-variable form) and the probe-side test (which checks for a
// literal "http=500" string) independently, while silently breaking the
// end-to-end probe for any OTHER status code or reword variant.
func TestHookScriptStrictModePrefixIsExportedConstant(t *testing.T) {
	got := HookScript(18339)
	if !strings.Contains(got, HookHealthFailurePrefix) {
		t.Fatalf("rendered hook script does not contain HookHealthFailurePrefix %q; the constant must match the bash template wording so runRemoteNotificationHealthProbe can recognise the probe failure", HookHealthFailurePrefix)
	}
	// Defense in depth: assert no near-miss variant has crept in. If a
	// future refactor introduces a second prefix (e.g. plural "probes"),
	// this catches the divergence.
	if strings.Count(got, "cc-clip-hook health probe") != 2 {
		// One occurrence in the with-curl-err branch, one in the without-
		// curl-err branch. Both must use the canonical wording.
		t.Fatalf("expected exactly 2 occurrences of canonical 'cc-clip-hook health probe' wording; got %d", strings.Count(got, "cc-clip-hook health probe"))
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
// Python interpolation (`'`, `"`, `$`, “ ` “) does NOT cause the script
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
		mu      sync.Mutex
		gotBody []byte
		gotAuth string
		gotHits int
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

	// Precondition: python3 must NOT be resolvable from the shimmed PATH,
	// otherwise the test would be exercising the happy path instead of the
	// fallback it claims to pin.
	if _, err := os.Stat(filepath.Join(binDir, "python3")); err == nil {
		t.Fatalf("test setup bug: python3 present in shimmed PATH (%s); cannot exercise fallback", binDir)
	}
	// Log the preconditions we rely on so any future skip or precondition
	// failure is visible in test output — the fallback branch is valuable
	// coverage, and a silent skip would let a real regression hide.
	t.Logf("precondition: shimmed PATH=%s (python3 deliberately absent)", binDir)

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

func TestHookScriptTrimsCRFromNonceFile(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	dir := t.TempDir()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("crlf-nonce\r\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}
	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"CC_CLIP_STATE_DIR="+dir,
		"CC_CLIP_PORT="+u.Port(),
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook script exited nonzero with CRLF nonce: %v\noutput: %s", err, out)
	}
	if gotAuth != "Bearer crlf-nonce" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer crlf-nonce")
	}
}

// TestHookScriptFireAndForgetOnHTTP500 pins the fire-and-forget contract:
// when CC_CLIP_STRICT is unset (the default installed behavior), a remote
// daemon returning HTTP 500 must still cause the hook to exit 0 and append
// the real status code to the health log. A regression that starts
// propagating non-2xx would silently block Claude Code.
func TestHookScriptFireAndForgetOnHTTP500(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}
	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"CC_CLIP_STATE_DIR="+dir,
		"CC_CLIP_PORT="+u.Port(),
		// CC_CLIP_STRICT intentionally unset — fire-and-forget branch.
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script exited nonzero on HTTP 500 without strict mode: %v\noutput: %s", err, out)
	}

	logPath := filepath.Join(dir, "notify-health.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("health log not written: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "FAIL http=500") {
		t.Fatalf("health log missing real status code; got: %q", body)
	}
	if strings.Contains(body, "FAIL http=000") {
		t.Fatalf("health log reports http=000 instead of real 500 — curl -f clobber regression: %q", body)
	}
}

// TestHookScriptStrictModePropagatesHTTP500 pins the strict-mode branch:
// when CC_CLIP_STRICT=1 and the daemon returns non-2xx, the hook must exit 1
// and print the real status code to stdout so `cc-clip connect`'s health
// probe can surface it. Complements the fire-and-forget test above.
func TestHookScriptStrictModePropagatesHTTP500(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}
	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"CC_CLIP_STATE_DIR="+dir,
		"CC_CLIP_PORT="+u.Port(),
		"CC_CLIP_STRICT=1",
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("strict mode must exit nonzero on HTTP 500; got exit 0\noutput: %s", out)
	}
	// Exact literal, not just substring: runRemoteNotificationHealthProbe
	// greps this wording to surface the failure to the user. A reworded
	// printf with a matching template string would pass a loose substring
	// check but silently break the end-to-end probe.
	const wantLiteral = "cc-clip-hook health probe failed: http=500"
	if !strings.Contains(string(out), wantLiteral) {
		t.Fatalf("strict-mode probe stdout missing exact literal %q; got: %q", wantLiteral, out)
	}
	if strings.Contains(string(out), "http=000") {
		t.Fatalf("probe reports http=000 instead of real 500 — curl -f clobber regression: %q", out)
	}
}

func TestHookScriptFireAndForgetOnCurlFailureWritesHTTP000(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for _, tool := range []string{"bash", "cat", "head", "date"} {
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
		"CC_CLIP_PORT=65534",
	}
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook script exited nonzero on curl failure path: %v\noutput: %s", err, out)
	}

	logPath := filepath.Join(dir, "notify-health.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("health log not written: %v", err)
	}
	if !strings.Contains(string(data), "FAIL http=000") {
		t.Fatalf("health log missing curl-failure marker: %q", data)
	}
}

// TestHookScriptHostAliasSurvivesPathologicalInput covers CC_CLIP_HOST_ALIAS
// values that would break naive string-interpolation of the alias into the
// Python payload-rewriter: newlines (Python statement separator), trailing
// backslashes (string-literal continuation in some languages), and mixed
// single/double quotes. `hostname -s` will never produce these, but an
// operator could set the env var manually. For each case the emitted script
// must still exit 0, refuse to execute host-supplied code, and deliver the
// literal alias as the `_cc_clip_host` JSON field (proving the env-var path
// is actually read end-to-end, not just silently stripped).
func TestHookScriptHostAliasSurvivesPathologicalInput(t *testing.T) {
	// Log the precondition outcome so a silent skip in minimal CI images
	// (no python3 / curl / bash) is traceable without re-running with -v
	// on every test ID. Mirrors the sibling test at
	// TestHookScriptHostAliasMissingPython3FallbackPreservesPayload which
	// already logs its precondition.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Logf("precondition: bash not available (%v); skipping pathological-input coverage", err)
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Logf("precondition: curl not available (%v); skipping pathological-input coverage", err)
		t.Skipf("curl not available: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Logf("precondition: python3 not available (%v); skipping pathological-input coverage — this is the Python-injection defense test; ensure a CI image with python3 covers it", err)
		t.Skipf("python3 not available: %v", err)
	}

	cases := []struct {
		name   string
		poison string
	}{
		{"newline", "evil\nimport os; os.system('id')\n#"},
		{"trailing_backslash", `evil\`},
		{"double_backslash_and_quote", `evil\\"`},
		{"mixed_quotes", `x"y'z`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
				t.Fatalf("write nonce: %v", err)
			}
			scriptPath := filepath.Join(dir, "cc-clip-hook")
			if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			cmd := exec.Command("bash", scriptPath)
			cmd.Env = append(os.Environ(),
				"CC_CLIP_HOST_ALIAS="+tc.poison,
				"CC_CLIP_STATE_DIR="+dir,
				"CC_CLIP_PORT="+u.Port(),
			)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hook script exited nonzero with %s alias: %v\noutput: %s", tc.name, err, out)
			}
			if strings.Contains(string(out), "uid=") {
				t.Fatalf("%s alias executed as Python: %s", tc.name, out)
			}
			mu.Lock()
			body := gotBody
			mu.Unlock()
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("payload is not valid JSON: %v\nbody=%s", err, body)
			}
			if got, _ := payload["_cc_clip_host"].(string); got != tc.poison {
				t.Fatalf("_cc_clip_host = %q; want literal %q (case %s)", got, tc.poison, tc.name)
			}
		})
	}
}
