package main

import (
	"errors"
	"runtime"
	"strings"
	"testing"
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

// TestRunCmdNotifySoundNonDarwinRejects pins the refusal on non-macOS
// platforms. The existing darwin-tagged tests in main_test.go can't
// exercise this branch. Without this, a Linux user who typed
// `cc-clip notify-sound Glass` would get a misleading "success" when in
// fact terminal-notifier never reads the preference on non-macOS.
func TestRunCmdNotifySoundNonDarwinRejects(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-darwin test path")
	}
	err := runCmdNotifySound("Glass", cmdNotifySoundDeps{
		lookPath: func(string) (string, error) {
			t.Fatal("lookPath must not be called on non-darwin")
			return "", nil
		},
		writeSound: func(string) error {
			t.Fatal("writeSound must not be called on non-darwin")
			return nil
		},
		soundPath: func() (string, error) {
			t.Fatal("soundPath must not be called on non-darwin")
			return "", nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "only supported on macOS") {
		t.Fatalf("expected non-darwin refusal, got %v", err)
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
