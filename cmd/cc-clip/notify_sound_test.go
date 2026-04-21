package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shunmei/cc-clip/internal/daemon"
)

// TestIsDisabledNotificationSoundAliases pins the off/none/silent aliases
// (case-insensitive, trimmed) that route to WriteNotificationSound("").
// A regression that drops one of the synonyms would silently fail closed
// for operators who typed the previous spelling.
//
// Complements the darwin-only tests in main_test.go, which cover the
// full happy path but can't run in CI for non-darwin platforms. This
// alias matrix is pure and runs everywhere.
func TestIsDisabledNotificationSoundAliases(t *testing.T) {
	enabled := []string{"Glass", "Ping", "Tink", "off-brand"}
	for _, s := range enabled {
		if isDisabledNotificationSound(s) {
			t.Errorf("isDisabledNotificationSound(%q) = true, want false", s)
		}
	}
	disabled := []string{"off", "OFF", "  off  ", "none", "None", "silent", "Silent"}
	for _, s := range disabled {
		if !isDisabledNotificationSound(s) {
			t.Errorf("isDisabledNotificationSound(%q) = false, want true", s)
		}
	}
}

// TestRunCmdNotifySoundNonDarwinRejectsName pins the refusal of Apple
// built-in *sound names* on non-macOS platforms. Names route through
// terminal-notifier's `-sound` flag which only exists on macOS, so
// persisting a name on Linux/Windows would be a silent no-op.
// Path-type values are handled by a different test (they DO work
// cross-platform via afplay/paplay/PowerShell).
func TestRunCmdNotifySoundNonDarwinRejectsName(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-darwin test path")
	}
	err := runCmdNotifySound("Glass", cmdNotifySoundDeps{
		lookPath: func(string) (string, error) {
			t.Fatal("lookPath must not be called when rejecting name on non-darwin")
			return "", nil
		},
		writeSound: func(string) error {
			t.Fatal("writeSound must not be called when rejecting name on non-darwin")
			return nil
		},
		soundPath: func() (string, error) {
			return "/tmp/sound-pref", nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "only supported on macOS") {
		t.Fatalf("expected non-darwin name refusal, got %v", err)
	}
}

// TestRunCmdNotifySoundAcceptsPathOnSupportedPlatforms pins that
// *filesystem paths* route through WriteNotificationSound without
// requiring terminal-notifier, on the only two OSes cc-clip's laptop
// side officially supports (macOS + Windows). Playback on the daemon
// side uses afplay or PowerShell SoundPlayer; the CLI just persists
// the path.
func TestRunCmdNotifySoundAcceptsPathOnSupportedPlatforms(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("notify-sound paths are only supported on macOS and Windows laptops")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	soundFile := filepath.Join(home, "ping.wav")
	if err := os.WriteFile(soundFile, []byte("RIFFWAVEfmt "), 0644); err != nil {
		t.Fatalf("create sound file: %v", err)
	}

	lookPathCalled := false
	err := runCmdNotifySound(soundFile, cmdNotifySoundDeps{
		lookPath: func(string) (string, error) {
			lookPathCalled = true
			return "", errors.New("not found") // must NOT be called for paths
		},
		writeSound: daemon.WriteNotificationSound,
		soundPath:  daemon.NotificationSoundPath,
	})
	if err != nil {
		t.Fatalf("runCmdNotifySound(path) = %v, want nil", err)
	}
	if lookPathCalled {
		t.Fatal("lookPath should not be called for path-type sounds (terminal-notifier not needed)")
	}
	got, err := daemon.ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != soundFile {
		t.Fatalf("stored sound = %q, want %q", got, soundFile)
	}
}

// TestRunCmdNotifySoundRejectsPathOnUnsupportedPlatform pins the
// fail-fast guard: on any GOOS other than darwin or windows, the CLI
// refuses to persist a path-type sound. Silently writing it would
// surprise operators because the daemon's PlayAudioFile would just
// log "no audio player available" at delivery time. writeSound is
// wired to fail loudly if ever called.
func TestRunCmdNotifySoundRejectsPathOnUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("platform IS supported; see TestRunCmdNotifySoundAcceptsPathOnSupportedPlatforms")
	}
	home := t.TempDir()
	soundFile := filepath.Join(home, "ping.wav")
	if err := os.WriteFile(soundFile, []byte("RIFFWAVEfmt "), 0644); err != nil {
		t.Fatalf("create sound file: %v", err)
	}

	err := runCmdNotifySound(soundFile, cmdNotifySoundDeps{
		lookPath: func(string) (string, error) {
			t.Fatal("lookPath must not be called when rejecting path on unsupported OS")
			return "", nil
		},
		writeSound: func(string) error {
			t.Fatal("writeSound must not be called when rejecting path on unsupported OS")
			return nil
		},
		soundPath: func() (string, error) { return "/tmp/sound-pref", nil },
	})
	if err == nil {
		t.Fatal("expected rejection of path on unsupported OS, got nil")
	}
	if !strings.Contains(err.Error(), "only supported on macOS and Windows") {
		t.Fatalf("expected unsupported-OS error, got %v", err)
	}
}

// TestRunCmdNotifySoundWriteErrorIsPropagated pins that a validation
// failure from WriteNotificationSound (invalid sound name) surfaces to
// the operator rather than being swallowed. The existing darwin tests
// use the real WriteNotificationSound via daemon.*, so they cover the
// happy path but never exercise a write failure.
func TestRunCmdNotifySoundWriteErrorIsPropagated(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only code path")
	}
	sentinel := errors.New("invalid: bad-sound-name")
	err := runCmdNotifySound("bad-name", cmdNotifySoundDeps{
		lookPath:   func(string) (string, error) { return "/opt/homebrew/bin/terminal-notifier", nil },
		writeSound: func(string) error { return sentinel },
		soundPath:  func() (string, error) { return "/tmp/sound-pref", nil },
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got %v", err)
	}
}
