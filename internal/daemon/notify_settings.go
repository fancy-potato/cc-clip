package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shunmei/cc-clip/internal/userhome"
)

const notificationSoundFileName = "notify-sound"

// ErrNotificationSoundHomeUnset is returned when HOME is unset (or user
// homedir lookup fails) and we cannot construct the canonical path for the
// sound-preference file. The old behaviour silently fell back to a CWD-
// relative `.cache/cc-clip/notify-sound`, which could land the file in
// whatever directory the daemon happened to be started from — confusing at
// best, a silent config-drift bug at worst.
var ErrNotificationSoundHomeUnset = errors.New("HOME is unset; cannot locate notification sound preference")

// ErrInvalidNotificationSound is returned when a sound name fails the
// validation regex. Exposed for CLI callers that want to emit an
// actionable error instead of "save notification sound: %w".
var ErrInvalidNotificationSound = errors.New("invalid notification sound name")

// notificationSoundPattern accepts Apple's documented terminal-notifier
// sound names (ASCII alphanumeric plus underscore and dash) without
// allowing a leading dash (which terminal-notifier would interpret as a
// flag) and without control chars / newlines. Spaces are rejected:
// every documented macOS system sound (Basso, Blow, Glass, Ping, …) is
// a single token, and accepting "My Sound" would pass validation but
// fail at notification time when terminal-notifier rejected it — so
// better to fail loud here than with a silent no-op later. 64 chars is
// an intentional upper bound — real sound names are <20 — so a typo
// can't dump arbitrary text into the config.
var notificationSoundPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

// validateNotificationSoundName guards both the CLI writer and the file
// reader. A sound value that fails here would otherwise survive a
// round-trip through disk and ship to terminal-notifier as an argv token
// carrying newlines or a leading dash (argv injection via `--foo`).
func validateNotificationSoundName(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", ErrInvalidNotificationSound)
	}
	if !notificationSoundPattern.MatchString(trimmed) {
		return fmt.Errorf("%w: %q (allowed: ASCII letters/digits/underscore/space/dash, 1-64 chars, no leading dash)", ErrInvalidNotificationSound, s)
	}
	return nil
}

// NotificationSoundPath returns the local persisted sound preference used
// when terminal-notifier delivers cc-clip notifications.
//
// Unlike the previous version, this function now surfaces an error when
// HOME is unset — silently falling back to a CWD-relative path could put
// the config file wherever the daemon's working directory happened to be
// when `cc-clip serve` launched. Callers must decide whether that is fatal
// (CLI writer: yes) or benign (delivery path: treat as "no sound
// configured" and skip the `-sound` argv).
func NotificationSoundPath() (string, error) {
	home, err := userhome.Dir()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotificationSoundHomeUnset, err)
	}
	if strings.TrimSpace(home) == "" {
		return "", ErrNotificationSoundHomeUnset
	}
	return filepath.Join(home, ".cache", "cc-clip", notificationSoundFileName), nil
}

// ReadNotificationSound returns the configured terminal-notifier sound name.
// A missing file means "no custom sound configured". A file whose contents
// fail validation is also treated as "no custom sound configured" — the
// rationale is that the delivery path is non-critical (fall-through to the
// default macOS notification sound) and should never fail a notification
// because of a hand-edited config file. The CLI writer rejects bad input
// up front, so a stored invalid value almost certainly means operator
// tinkering.
func ReadNotificationSound() (string, error) {
	path, err := NotificationSoundPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	candidate := strings.TrimSpace(string(data))
	if candidate == "" {
		return "", nil
	}
	if err := validateNotificationSoundName(candidate); err != nil {
		// Silently ignore — falling back to the default sound is safer
		// than surfacing a read error to every notification attempt.
		return "", nil
	}
	return candidate, nil
}

// WriteNotificationSound persists the configured terminal-notifier sound name.
// An empty sound clears the setting. Non-empty values are validated — a
// name with control chars, newlines, or a leading dash is rejected with
// ErrInvalidNotificationSound so the operator sees an actionable error
// rather than shipping a poisoned argv to terminal-notifier later.
func WriteNotificationSound(sound string) error {
	path, err := NotificationSoundPath()
	if err != nil {
		return err
	}
	if strings.TrimSpace(sound) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := validateNotificationSoundName(sound); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(sound)+"\n"), 0600)
}
