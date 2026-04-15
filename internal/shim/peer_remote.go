package shim

import (
	"encoding/json"
	"fmt"

	"github.com/shunmei/cc-clip/internal/peer"
)

func ReservePeerViaSession(session *SSHSession, remoteBin, peerID, label string, rangeStart, rangeEnd int) (peer.Registration, error) {
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
	cmd := fmt.Sprintf("%s peer release --peer-id %s", remoteBin, shellQuote(peerID))
	out, err := session.Exec(cmd)
	if err != nil {
		return peer.Registration{}, fmt.Errorf("failed to release peer port: %w", err)
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return peer.Registration{}, fmt.Errorf("failed to decode peer release: %w", err)
	}
	return reg, nil
}

func LookupPeerViaSession(session *SSHSession, remoteBin, peerID string) (peer.Registration, error) {
	cmd := fmt.Sprintf("%s peer show --peer-id %s", remoteBin, shellQuote(peerID))
	out, err := session.Exec(cmd)
	if err != nil {
		return peer.Registration{}, fmt.Errorf("failed to read peer registry entry: %w", err)
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return peer.Registration{}, fmt.Errorf("failed to decode peer registry entry: %w", err)
	}
	return reg, nil
}
