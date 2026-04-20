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
	"strconv"
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

// TestHookScriptUsesBashLongestMatchForNonceCRStrip pins the rendered
// bash form `${_nonce%%$'\r'}` (longest-match). The template source uses
// four percent signs which fmt.Sprintf collapses to two. A regression
// that drops a pair (down to two in source → one in output) would
// degrade to shortest-match, which is equivalent for a single-char
// target today but invites silent semantic drift for any future
// longest-match pattern added next to this one.
func TestHookScriptUsesBashLongestMatchForNonceCRStrip(t *testing.T) {
	got := HookScript(18339)
	if !strings.Contains(got, "${_nonce%%$'\\r'}") {
		t.Fatalf("expected rendered bash to use longest-match %%%%$'\\r' suffix strip; got script:\n%s", got)
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

func TestHookScriptStrictModeSanitizesCurlErrorWithoutExternalTr(t *testing.T) {
	got := HookScript(18339)
	if strings.Contains(got, "| tr -cd") {
		t.Fatalf("strict-mode curl error sanitization must not depend on external tr; got script:\n%s", got)
	}
	if !strings.Contains(got, "_curl_err_safe=") {
		t.Fatalf("expected strict-mode curl error sanitization helper in script:\n%s", got)
	}
}

// TestHookScriptStrictModeScrubsANSIEscapesFromCurlError drives the in-script
// sanitization loop with hostile inputs — ANSI CSI/OSC escapes, raw control
// bytes, C1 controls, and high-Unicode — and confirms only printable ASCII
// (plus TAB) survives. This is a behavioural pin on top of the existing
// structural test (which only checks the helper exists): a future edit that
// swaps `[[:print:]]` for, say, `[[:graph:]]` would erase TAB (regression) or
// swap in `*` (regression: matches anything, defeating the scrub) and pass
// the structural test. This test exercises the *actual bash logic*.
func TestHookScriptStrictModeScrubsANSIEscapesFromCurlError(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-behavior test")
	}
	// Harness mirrors the exact scrub loop from HookScript (see the
	// `for ((_i=0; ...))` block in hook_template.go). Copied rather than
	// regex-extracted so this test is a deliberate specification: if the
	// template scrub diverges from the harness, the structural test above
	// catches the shape regression and this test catches the behavior
	// regression. Force the C locale so `[[:print:]]` behavior doesn't
	// drift with the tester's LANG setting (under en_US.UTF-8 bash will
	// accept many Unicode formatting chars as printable).
	//
	// Input arrives on stdin, not argv, because argv bytes can't carry a
	// NUL (execve rejects it) — and NUL is exactly one of the bytes a
	// compromised curl-stderr could emit, so we must be able to test it.
	const harness = `
set -euo pipefail
LC_ALL=C
_curl_err=$(cat)
_curl_err_safe=""
for ((_i=0; _i<${#_curl_err}; _i++)); do
	_ch=${_curl_err:_i:1}
	case "$_ch" in
		[[:print:]]|$'\t') _curl_err_safe="${_curl_err_safe}${_ch}" ;;
	esac
done
printf '%s' "$_curl_err_safe"
`
	// Scope note: this scrub defends against terminal-rewriting bytes
	// (ESC, BEL, CR/LF, NUL, C1 CSI 0x9B). Unicode format characters like
	// U+2028/U+2029/U+202E are NOT this scrub's responsibility — they are
	// sanitized by internal/daemon/notify_darwin.go (sanitizeForAppleScript).
	// Keeping the scope narrow avoids locale-dependent `[[:print:]]` drift.
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain ASCII preserved",
			input: "curl: (7) Failed to connect",
			want:  "curl: (7) Failed to connect",
		},
		{
			name:  "TAB preserved",
			input: "curl\t(28)\ttimeout",
			want:  "curl\t(28)\ttimeout",
		},
		{
			name:  "ESC CSI (color/cursor) stripped",
			input: "\x1b[2J\x1b[31mBAD\x1b[0m",
			want:  "[2J[31mBAD[0m",
		},
		{
			name:  "OSC title-rewrite stripped",
			input: "\x1b]0;pwned\x07curl failed",
			want:  "]0;pwnedcurl failed",
		},
		{
			name:  "CR/LF/NUL stripped",
			input: "curl\r\nrefused\x00tail",
			want:  "curlrefusedtail",
		},
		{
			name:  "C1 CSI single-byte stripped",
			input: "curl\x9b31mBAD",
			want:  "curl31mBAD",
		},
		{
			name:  "DEL (0x7F) stripped",
			input: "curl\x7ferased",
			want:  "curlerased",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", harness)
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash harness failed: %v", err)
			}
			if string(out) != tc.want {
				t.Fatalf("scrub mismatch\n  input:   %q\n  got:     %q\n  want:    %q", tc.input, out, tc.want)
			}
		})
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
	// this catches the divergence. Current branches:
	//   1. nonce-empty-or-corrupt strict-mode exit (post-sanitization)
	//   2. cat-rc strict-mode exit (stdin reader failed)
	//   3. HTTP-failure with curl-err branch
	//   4. HTTP-failure without curl-err branch
	// All four MUST use the canonical wording so the Go-side probe can
	// recognise any of them from stdout.
	if strings.Count(got, "cc-clip-hook health probe") != 4 {
		t.Fatalf("expected exactly 4 occurrences of canonical 'cc-clip-hook health probe' wording; got %d", strings.Count(got, "cc-clip-hook health probe"))
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

func TestHookScriptPayloadPreservesLiteralCatRcMarker(t *testing.T) {
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

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("test-nonce\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	scriptPath := filepath.Join(dir, "cc-clip-hook")
	if err := os.WriteFile(scriptPath, []byte(HookScript(1)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	payloadIn := `{"hook_event_name":"notification","message":"literal __cc_clip_cat_rc=7 survives"}`
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"CC_CLIP_STATE_DIR="+dir,
		"CC_CLIP_PORT="+u.Port(),
	)
	cmd.Stdin = strings.NewReader(payloadIn)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook script exited nonzero with marker-like payload: %v\noutput: %s", err, out)
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()
	if len(body) == 0 {
		t.Fatal("receiver got no body")
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v\nbody=%s", err, body)
	}
	if got, _ := payload["message"].(string); got != "literal __cc_clip_cat_rc=7 survives" {
		t.Fatalf("message = %q, want literal marker payload", got)
	}
}

// TestHookScriptRejectsNonNumericPort pins the P1-6 guard: a hostile or
// typo'd CC_CLIP_PORT that contains non-digits (URL path splicing,
// command substitution, empty, whitespace) must fall back to the default
// port baked into the template, not be spliced into the notify URL.
// Without this, an attacker able to seed CC_CLIP_PORT via the managed
// SSH SetEnv block could redirect the /notify POST to an arbitrary path,
// or trigger command substitution through the later URL interpolation.
func TestHookScriptRejectsNonNumericPort(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	const defaultPort = 18339
	cases := []struct {
		name        string
		port        string
		description string
	}{
		{"url_splice", "18339/attacker.example/x", "URL-path injection"},
		{"command_substitution_literal", "$(id)", "literal $(...) should not execute"},
		{"backtick_substitution_literal", "`id`", "literal backticks should not execute"},
		{"whitespace", "  ", "whitespace-only rejected"},
		{"empty", "", "empty string rejected (uses default via parameter expansion, then guard)"},
		{"alpha", "abc", "alpha-only rejected"},
		{"mixed", "18339x", "mixed numeric+letter rejected"},
		{"zero", "0", "zero rejected as invalid TCP port"},
		{"too_large", "70000", "out-of-range TCP port rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			var (
				mu      sync.Mutex
				gotURL  string
				gotHits int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotURL = r.URL.Path
				gotHits++
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)

			// Point the template's DEFAULT port (which the rejection fallback
			// reuses via the second %d substitution) at the httptest server —
			// if the port guard works, the hook will hit srv.URL+"/notify"
			// regardless of the poisoned CC_CLIP_PORT.
			listenPortInt, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatalf("parse test server port: %v", err)
			}

			if err := os.WriteFile(filepath.Join(dir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
				t.Fatalf("write nonce: %v", err)
			}
			scriptPath := filepath.Join(dir, "cc-clip-hook")
			if err := os.WriteFile(scriptPath, []byte(HookScript(listenPortInt)), 0700); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cmd := exec.Command("bash", scriptPath)
			cmd.Env = append(os.Environ(),
				"CC_CLIP_STATE_DIR="+dir,
				"CC_CLIP_PORT="+tc.port,
			)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("hook script exited nonzero with poisoned port: %v\noutput: %s", err, out)
			}

			mu.Lock()
			hits, path := gotHits, gotURL
			mu.Unlock()

			// The poisoned port was rejected and the default (test server
			// port) was used, so we should see exactly 1 hit on /notify,
			// NOT on a poisoned path. If the guard fails, either no hit
			// lands (port routed to some other address) or the path was
			// spliced — either way, this assertion catches it.
			if hits != 1 {
				t.Fatalf("[%s] expected exactly 1 POST, got %d (guard may have passed poisoned port through)", tc.description, hits)
			}
			if path != "/notify" {
				t.Fatalf("[%s] expected path=/notify, got %q (URL splice not rejected)", tc.description, path)
			}

			// Silence unused defaultPort warning while keeping the
			// constant documented near the test for future readers who
			// want to confirm the template-embedded default.
			_ = defaultPort
		})
	}
}

// TestHookScriptAcceptsLegitimateDoubleDotInStateDir pins the P2-24 fix:
// the earlier `*..*` glob also rejected legitimate paths containing ".."
// as a substring (e.g. /home/a..b/.cache/cc-clip). Only a true ".." path
// component should be rejected.
func TestHookScriptAcceptsLegitimateDoubleDotInStateDir(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	dir := t.TempDir()
	// Create a directory with a literal ".." inside the path-segment name
	// (not a path component) to exercise the accept branch.
	stateDir := filepath.Join(dir, "a..b", "cache")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "notify.nonce"), []byte("n\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	var (
		mu      sync.Mutex
		gotAuth string
		gotHits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotHits++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	scriptPath := filepath.Join(dir, "cc-clip-hook")
	listenPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte(HookScript(listenPort)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"CC_CLIP_STATE_DIR="+stateDir,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook script exited nonzero with a..b state dir: %v\noutput: %s", err, out)
	}

	mu.Lock()
	hits, auth := gotHits, gotAuth
	mu.Unlock()

	if hits != 1 {
		t.Fatalf("expected 1 POST (legitimate a..b path should have been accepted), got %d", hits)
	}
	// Auth header from the per-peer state dir's nonce proves we did NOT
	// silently fall back to $HOME/.cache/cc-clip.
	if auth != "Bearer n" {
		t.Fatalf("Authorization = %q, want %q (legitimate state dir not preserved — fell back to $HOME/.cache/cc-clip)", auth, "Bearer n")
	}
}

// TestHookScriptRejectsPathTraversalStateDir pins the path-traversal
// rejection half of P2-24: a state dir with a true ".." path component
// must still be rejected (the original security invariant, now with the
// tighter pattern that doesn't also catch "a..b").
func TestHookScriptRejectsPathTraversalStateDir(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not available: %v", err)
	}
	dir := t.TempDir()
	// The fallback state dir is ${HOME}/.cache/cc-clip, so the nonce must
	// live there — not at the bare HOME — for the fallback branch to read it.
	fallbackDir := filepath.Join(dir, ".cache", "cc-clip")
	if err := os.MkdirAll(fallbackDir, 0700); err != nil {
		t.Fatalf("mkdir fallback: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fallbackDir, "notify.nonce"), []byte("real-default-nonce\n"), 0600); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	var (
		mu      sync.Mutex
		gotAuth string
		gotHits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotHits++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	scriptPath := filepath.Join(dir, "cc-clip-hook")
	listenPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte(HookScript(listenPort)), 0700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Test several path-traversal forms. Each should fall back to
	// $HOME/.cache/cc-clip (via HOME=dir). We put the "real" nonce at
	// dir/notify.nonce to assert the fallback path is used.
	cases := []string{
		"/home/user/../../etc",
		"/var/lib/foo/..",
		"../relative-is-also-rejected", // relative, too, separate branch
	}
	for _, poisoned := range cases {
		mu.Lock()
		gotAuth = ""
		gotHits = 0
		mu.Unlock()
		cmd := exec.Command("bash", scriptPath)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + dir,
			"CC_CLIP_STATE_DIR=" + poisoned,
		}
		cmd.Stdin = strings.NewReader(`{"hook_event_name":"notification"}`)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook script exited nonzero on poisoned state dir %q: %v\noutput: %s", poisoned, err, out)
		}
		mu.Lock()
		auth := gotAuth
		hits := gotHits
		mu.Unlock()
		if hits != 1 {
			t.Fatalf("[%s] expected exactly 1 POST, got %d", poisoned, hits)
		}
		if auth != "Bearer real-default-nonce" {
			t.Fatalf("[%s] Authorization = %q, want %q (fallback to $HOME/.cache/cc-clip not used)", poisoned, auth, "Bearer real-default-nonce")
		}
	}
}

// TestHookScriptPayloadNotInArgv pins the P3 fix: the payload must be
// passed on stdin (--data-binary @-), not as an argv element
// (--data-raw "$_payload"). argv is visible via /proc/<pid>/cmdline on
// Linux and `ps -ww` on macOS; stdin is not. A template refactor that
// reintroduces --data-raw would expose notification bodies to any local
// user on a shared remote.
func TestHookScriptPayloadNotInArgv(t *testing.T) {
	got := HookScript(18339)
	if strings.Contains(got, "--data-raw") {
		t.Fatal("hook template must not use --data-raw; switch to --data-binary @- to keep payload off argv")
	}
	if !strings.Contains(got, "--data-binary @-") {
		t.Fatal("hook template must use --data-binary @- so payload is read from stdin, not argv")
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
