package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/peer"
)

// classifyPeerNotFound returns peer.ErrPeerNotFound (wrapped with the
// original error for context) when the remote `cc-clip peer` subcommand
// exited with exitcode.PeerNotFound AND emitted exitcode.PeerNotFoundSentinel
// on stderr. Any other error is returned unchanged, so real
// transport/JSON/auth failures still surface normally.
//
// Requiring both the exit code and the sentinel is defense in depth: a
// transport-layer shim (sandbox, wrapper, ssh plugin) that happens to
// propagate or synthesise an exit status of 22 would otherwise be
// misclassified as an idempotent "peer already released" — causing
// uninstall to treat a broken remote session as a clean success and
// leaking the peer's reserved port in the registry.
//
// Load-bearing exec-path invariant (P3-H): this function reads
// exitErr.Stderr directly. Go's *exec.ExitError.Stderr is ONLY populated
// when the command was started with cmd.Output() (or equivalent wrappers
// that internally call Output); cmd.Run() with an explicit
// cmd.Stderr = &buf leaves exitErr.Stderr nil. This classifier is safe
// today because SSHSession.Exec — the sole production caller — uses
// cmd.Output() internally (see internal/shim/ssh.go). If a future
// refactor changes SSHSession.Exec to capture stderr via an explicit
// sink, the sentinel check here will silently start returning false and
// idempotent uninstall will mis-diagnose transport failures as clean
// "peer already released" successes. In that case, switch this classifier
// to take an explicit stderr []byte argument (mirroring
// classifyDoctorPeerNotFound in internal/doctor/remote.go). Pinned
// indirectly by TestClassifyPeerNotFoundTranslatesExitCode in
// peer_remote_test.go, which uses cmd.Output() via exitErrorWithSentinel
// to mirror the production capture.
func classifyPeerNotFound(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}
	if exitErr.ExitCode() != exitcode.PeerNotFound {
		return err
	}
	if !bytes.Contains(exitErr.Stderr, []byte(exitcode.PeerNotFoundSentinel)) {
		return err
	}
	return fmt.Errorf("%w: %v", peer.ErrPeerNotFound, err)
}

// ValidateRemoteBin rejects any remote binary path that would require
// shell-quoting to pass safely as the command prefix in Sprintf("%s peer …",
// remoteBin, …). The canonical value is `~/.local/bin/cc-clip`; we cannot
// shell-quote it because `~` must expand, so the path is interpolated raw
// and we instead constrain its byte set to characters that need no quoting.
// Rejecting whitespace / shell metacharacters defends against a future
// caller passing an operator-supplied --local-bin value containing e.g. a
// space, `$(...)`, or `;`. The peerID/label args on each call site are
// already shellQuote'd; this closes the remaining unquoted hole.
//
// Tilde handling: `~` is accepted ANYWHERE in the path, not just as a
// prefix. POSIX shells only expand `~` at the start of a word or
// immediately after a `:` in certain assignments, so a `~` embedded
// mid-path (e.g. "/opt/foo~1/cc-clip") passes through literally rather
// than expanding. The permissive acceptance matches the current callers
// (cmdConnect / cmdUninstall pass `~/.local/bin/cc-clip`; tests cover
// `~/alt/cc-clip-0.7.0`, `/usr/local/bin/cc-clip`, and bare `cc-clip`)
// and avoids a second regex layer for a character that is already safe
// in the accepted set. If a future caller needs stricter prefix-only
// tilde handling, add that at the call site rather than tightening this
// validator for everyone.
func ValidateRemoteBin(b string) error {
	if b == "" {
		return errors.New("remote binary path is empty")
	}
	for _, r := range b {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '/', '.', '-', '_', '~':
			continue
		}
		return fmt.Errorf("remote binary path %q contains forbidden character %q", b, r)
	}
	return nil
}

func ReservePeerViaSession(session *SSHSession, remoteBin, peerID, label string, rangeStart, rangeEnd int) (peer.Registration, error) {
	if err := ValidateRemoteBin(remoteBin); err != nil {
		return peer.Registration{}, err
	}
	cmd := fmt.Sprintf("%s peer reserve --peer-id %s --label %s --range-start %d --range-end %d",
		remoteBin, shellQuote(peerID), shellQuote(label), rangeStart, rangeEnd)
	out, err := session.Exec(cmd)
	if err != nil {
		return peer.Registration{}, fmt.Errorf("failed to reserve peer port: %w", err)
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return peer.Registration{}, fmt.Errorf("failed to decode peer reservation: %w (got: %s)", err, truncateForError(out))
	}
	return reg, nil
}

func ReleasePeerViaSession(session *SSHSession, remoteBin, peerID string) (peer.Registration, error) {
	if err := ValidateRemoteBin(remoteBin); err != nil {
		return peer.Registration{}, err
	}
	cmd := fmt.Sprintf("%s peer release --peer-id %s", remoteBin, shellQuote(peerID))
	out, err := session.Exec(cmd)
	if err != nil {
		// Double-wrap is load-bearing: classifyPeerNotFound already wraps
		// `%w: %v` carrying peer.ErrPeerNotFound, and the outer `%w` here
		// keeps errors.Is(..., peer.ErrPeerNotFound) reachable through the
		// Unwrap chain. Pinned by TestReleasePeerViaSessionDoubleWrapPreservesErrPeerNotFound.
		// Do NOT collapse to a single `%s` formatter — the idempotent
		// cleanup path in cmd/cc-clip/main.go:cleanupAndReleasePeerWith
		// relies on errors.Is on the returned error.
		return peer.Registration{}, fmt.Errorf("failed to release peer port: %w", classifyPeerNotFound(err))
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return peer.Registration{}, fmt.Errorf("failed to decode peer release: %w (got: %s)", err, truncateForError(out))
	}
	return reg, nil
}

func LookupPeerViaSession(session *SSHSession, remoteBin, peerID string) (peer.Registration, error) {
	if err := ValidateRemoteBin(remoteBin); err != nil {
		return peer.Registration{}, err
	}
	cmd := fmt.Sprintf("%s peer show --peer-id %s", remoteBin, shellQuote(peerID))
	out, err := session.Exec(cmd)
	if err != nil {
		return peer.Registration{}, fmt.Errorf("failed to read peer registry entry: %w", classifyPeerNotFound(err))
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return peer.Registration{}, fmt.Errorf("failed to decode peer registry entry: %w (got: %s)", err, truncateForError(out))
	}
	return reg, nil
}

// ListPeersViaSession invokes `cc-clip peer list` on the remote host and
// decodes the JSON array. Used by the uninstall path to check whether
// other peers (e.g. other laptops sharing the same remote Unix account)
// still hold a reservation — if any do, shared assets like
// `~/.local/bin/clipcc`, `cc-clip-hook`, the Codex notify config, and the
// `~/.bashrc` PATH marker must be preserved.
//
// Wraps *SSHSession instead of RemoteExecutor to mirror the other Via
// helpers in this file; tests drive the equivalent code path against a
// lower-level executor via peerListInspector (cmd/cc-clip/main.go).
func ListPeersViaSession(session *SSHSession, remoteBin string) ([]peer.Registration, error) {
	if err := ValidateRemoteBin(remoteBin); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("%s peer list", remoteBin)
	out, err := session.Exec(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list remote peer registry: %w", err)
	}
	return parsePeerListOutput(out)
}

// parsePeerListOutput decodes the stdout of `cc-clip peer list` into a
// registration slice, failing closed on any ambiguity so the uninstall
// fail-safe contract (AGENTS.md: "Uninstall is multi-peer safe") holds:
// an unparseable reply MUST become an error so callers preserve shared
// assets instead of misreading it as "zero peers, safe to clean up".
//
// `cc-clip peer list` always prints `[...JSON array...]` (see cmdPeer in
// cmd/cc-clip/main.go). We require the trimmed output to START with `[`
// before handing it to json.Unmarshal so a rc-file echo, MOTD banner, or
// wrapper log-line preceding the payload can't coax the parser into
// treating `"some banner\n[]"` as a successful empty-registry read (it
// wouldn't — json.Unmarshal is strict — but a stricter structural gate
// up front keeps the error message actionable and defends against future
// parser leniency).
func parsePeerListOutput(out string) ([]peer.Registration, error) {
	trimmed := bytes.TrimSpace([]byte(out))
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("remote peer list returned empty output (expected JSON array, got 0 bytes)")
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("remote peer list did not start with '[' (got: %s) — likely a shell-rc banner or wrapper log leaked before the payload", truncateForError(string(trimmed)))
	}
	var regs []peer.Registration
	if err := json.Unmarshal(trimmed, &regs); err != nil {
		return nil, fmt.Errorf("failed to decode peer list: %w (got: %s)", err, truncateForError(string(trimmed)))
	}
	if regs == nil {
		return nil, fmt.Errorf("failed to decode peer list: expected JSON array, got null")
	}
	return regs, nil
}

// truncateForError returns a bounded prefix of `out` suitable for embedding
// in error messages. rc-file prompt fragments and stray echoes on the
// remote side routinely land before the intended JSON body, so surfacing
// a short quoted snippet (rather than the raw `%w: <entire output>`)
// keeps the message readable while still giving the operator enough
// context to recognise a prompt leak or wrapper shim without having to
// rerun with ssh -v.
func truncateForError(out string) string {
	const maxLen = 120
	out = strings.TrimSpace(out)
	if len(out) <= maxLen {
		return fmt.Sprintf("%q", out)
	}
	// Rune-safe cut: back up to the last rune boundary before appending
	// the truncation marker so multi-byte characters are not split.
	return fmt.Sprintf("%q …[truncated]", runeSafePrefix(out, maxLen))
}

// runeSafePrefix returns out[:n] adjusted back to the nearest rune
// boundary. The fmt.Sprintf("%q", …) caller renders the result with Go
// escape syntax, so any control bytes in the prefix are still safe to
// print; this function's job is only to avoid cutting inside a rune.
func runeSafePrefix(out string, n int) string {
	if n > len(out) {
		n = len(out)
	}
	for n > 0 {
		r, size := utf8.DecodeLastRuneInString(out[:n])
		if r == utf8.RuneError && size == 1 {
			n--
			continue
		}
		break
	}
	return out[:n]
}
