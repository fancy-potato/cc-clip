package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

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
// leaking the peer's reserved port in the registry. `exec.Cmd.Output()`
// populates ExitError.Stderr automatically, so the sentinel check comes
// for free wherever the caller used `session.Exec()` (which uses Output()
// internally).
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
		return peer.Registration{}, fmt.Errorf("failed to decode peer reservation: %w", err)
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
		return peer.Registration{}, fmt.Errorf("failed to decode peer release: %w", err)
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
		return peer.Registration{}, fmt.Errorf("failed to decode peer registry entry: %w", err)
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
	trimmed := bytes.TrimSpace([]byte(out))
	if len(trimmed) == 0 {
		return nil, nil
	}
	var regs []peer.Registration
	if err := json.Unmarshal(trimmed, &regs); err != nil {
		return nil, fmt.Errorf("failed to decode peer list: %w", err)
	}
	return regs, nil
}
