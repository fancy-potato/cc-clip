package shim

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/peer"
)

// exitErrorWithSentinel runs a shell snippet that both emits the
// PeerNotFoundSentinel on stderr and exits with `code`. exec.Cmd.Output()
// populates ExitError.Stderr automatically, mirroring how SSHSession.Exec
// captures errors in production.
func exitErrorWithSentinel(t *testing.T, code int, stderrBody string) *exec.ExitError {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestPeerRemoteExitHelper")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		fmt.Sprintf("CC_CLIP_HELPER_EXIT_CODE=%d", code),
		"CC_CLIP_HELPER_STDERR_B64="+base64.StdEncoding.EncodeToString([]byte(stderrBody)),
	)
	_, runErr := cmd.Output()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T (%v)", runErr, runErr)
	}
	return exitErr
}

func TestPeerRemoteExitHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	stderrBody, err := base64.StdEncoding.DecodeString(os.Getenv("CC_CLIP_HELPER_STDERR_B64"))
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := os.Stderr.Write(stderrBody); err != nil {
		os.Exit(2)
	}
	code, err := strconv.Atoi(os.Getenv("CC_CLIP_HELPER_EXIT_CODE"))
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(code)
}

// TestClassifyPeerNotFoundTranslatesExitCode pins the contract that
// exitcode.PeerNotFound from the remote `cc-clip peer` subprocess is
// translated into peer.ErrPeerNotFound when the stderr sentinel is also
// present. Without this, the idempotent cleanup path in
// cleanupAndReleasePeer has to grep stderr for "peer not found" — which
// could swallow unrelated errors happening to contain the same substring.
func TestClassifyPeerNotFoundTranslatesExitCode(t *testing.T) {
	exitErr := exitErrorWithSentinel(t, exitcode.PeerNotFound, exitcode.PeerNotFoundSentinel+"\npeer release failed: peer foo: peer not found\n")

	classified := classifyPeerNotFound(exitErr)
	if !errors.Is(classified, peer.ErrPeerNotFound) {
		t.Fatalf("expected errors.Is(err, peer.ErrPeerNotFound); got %v", classified)
	}
}

// TestClassifyPeerNotFoundLeavesOtherExitCodesAlone pins that we only
// translate the dedicated PeerNotFound exit code. A generic exit 1 (e.g.
// remote binary panicked, ssh transport error) must surface as-is so
// cleanup paths do not mistake it for an idempotent success.
func TestClassifyPeerNotFoundLeavesOtherExitCodesAlone(t *testing.T) {
	exitErr := exitErrorWithSentinel(t, 1, "")

	classified := classifyPeerNotFound(exitErr)
	if errors.Is(classified, peer.ErrPeerNotFound) {
		t.Fatalf("exit 1 must not be classified as peer-not-found; got %v", classified)
	}
	if classified != exitErr {
		t.Fatalf("expected original error to pass through unchanged")
	}
}

// TestClassifyPeerNotFoundRequiresStderrSentinel pins the defense-in-depth
// contract: a bare exit-22 without the cc-clip sentinel on stderr (e.g. a
// ssh transport wrapper that happens to propagate 22 for an unrelated
// failure) must NOT be classified as peer-not-found. Otherwise uninstall
// would treat a broken remote session as an idempotent success and leak
// the peer lease in the remote registry.
func TestClassifyPeerNotFoundRequiresStderrSentinel(t *testing.T) {
	// Exit 22 with NO sentinel on stderr — this is the hostile case.
	exitErr := exitErrorWithSentinel(t, exitcode.PeerNotFound, "ssh: connection reset\n")

	classified := classifyPeerNotFound(exitErr)
	if errors.Is(classified, peer.ErrPeerNotFound) {
		t.Fatalf("exit 22 without sentinel must not classify as peer-not-found; got %v", classified)
	}
	if classified != exitErr {
		t.Fatalf("expected original error to pass through unchanged")
	}
}

// TestClassifyPeerNotFoundNilPassthrough ensures a nil error stays nil —
// callers wrap the result in fmt.Errorf only when err != nil, so the
// classifier must preserve that precondition.
func TestClassifyPeerNotFoundNilPassthrough(t *testing.T) {
	if got := classifyPeerNotFound(nil); got != nil {
		t.Fatalf("classifyPeerNotFound(nil) = %v; want nil", got)
	}
}

// TestClassifyPeerNotFoundIgnoresNonExitError pins the defensive
// non-*exec.ExitError branch (e.g. context deadline, missing binary in
// PATH, permission denied): we must not pretend the peer is missing
// just because classification failed.
func TestClassifyPeerNotFoundIgnoresNonExitError(t *testing.T) {
	err := errors.New("transport: connection reset")
	got := classifyPeerNotFound(err)
	if errors.Is(got, peer.ErrPeerNotFound) {
		t.Fatalf("non-ExitError must not be classified as peer-not-found; got %v", got)
	}
}

// TestReleasePeerViaSessionDoubleWrapPreservesErrPeerNotFound pins the
// documented contract in peer_remote.go: the double-%w wrap between
// classifyPeerNotFound (inner "%w: %v") and ReleasePeerViaSession (outer
// "failed to release peer port: %w") must keep
// errors.Is(err, peer.ErrPeerNotFound) reachable. cleanupAndReleasePeerWith
// in cmd/cc-clip/main.go relies on this for its idempotent cleanup branch.
// A future refactor that collapses one layer to %v or %s would silently
// break that branch — this test catches the regression.
func TestReleasePeerViaSessionDoubleWrapPreservesErrPeerNotFound(t *testing.T) {
	innerExitErr := exitErrorWithSentinel(t, exitcode.PeerNotFound, exitcode.PeerNotFoundSentinel+"\n")
	classified := classifyPeerNotFound(innerExitErr)
	wrapped := fmt.Errorf("failed to release peer port: %w", classified)
	if !errors.Is(wrapped, peer.ErrPeerNotFound) {
		t.Fatalf("double-wrapped error no longer unwraps to peer.ErrPeerNotFound; got %v", wrapped)
	}
}

// TestValidateRemoteBinAllowsCanonicalPath pins that the default
// `~/.local/bin/cc-clip` value used in cmd/cc-clip/main.go is accepted —
// otherwise every connect/uninstall would regress on first call.
func TestValidateRemoteBinAllowsCanonicalPath(t *testing.T) {
	if err := ValidateRemoteBin("~/.local/bin/cc-clip"); err != nil {
		t.Fatalf("canonical remote binary rejected: %v", err)
	}
	for _, ok := range []string{
		"/usr/local/bin/cc-clip",
		"~/alt/cc-clip-0.7.0",
		"cc-clip",
	} {
		if err := ValidateRemoteBin(ok); err != nil {
			t.Errorf("expected %q to be accepted, got %v", ok, err)
		}
	}
}

// TestValidateRemoteBinRejectsInjection pins the P1-3 review fix. The peer
// helpers Sprintf `remoteBin` raw (because `~` must expand), so any shell
// metacharacter in the path would be interpreted by the remote shell. The
// validator must reject whitespace, quote, command-substitution, redirection,
// and expansion metacharacters so a future caller can't hand through an
// operator-supplied value like `--local-bin` and open injection.
func TestValidateRemoteBinRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"cc-clip with space",
		"cc-clip\tevil",
		"cc-clip\nevil",
		`cc-clip"evil"`,
		"cc-clip'evil'",
		"cc-clip`evil`",
		"cc-clip$(evil)",
		"cc-clip${evil}",
		"cc-clip;evil",
		"cc-clip&evil",
		"cc-clip|evil",
		"cc-clip<evil",
		"cc-clip>evil",
		`cc-clip\evil`,
		"cc-clip*evil",
		"cc-clip?evil",
		"cc-clip[abc]",
	}
	for _, b := range bad {
		if err := ValidateRemoteBin(b); err == nil {
			t.Errorf("expected %q to be rejected, got nil", b)
		}
	}
}
