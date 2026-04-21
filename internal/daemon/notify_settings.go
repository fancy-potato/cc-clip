package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// ErrInvalidNotificationSound is returned when a sound value fails
// validation (bad name pattern, non-existent path, non-regular file, etc).
// Exposed for CLI callers that want to emit an actionable error instead of
// "save notification sound: %w".
var ErrInvalidNotificationSound = errors.New("invalid notification sound")

// SoundKind classifies a stored notify-sound value. Callers dispatch
// delivery differently for each kind:
//   - SoundKindEmpty  → no sound configured
//   - SoundKindName   → macOS built-in sound name (terminal-notifier -sound)
//   - SoundKindPath   → absolute audio-file path (afplay / paplay / ...)
type SoundKind int

const (
	SoundKindEmpty SoundKind = iota
	SoundKindName
	SoundKindPath
)

// maxSoundPathLen caps the persisted path length. 4096 matches PATH_MAX on
// most unixes and is wildly more than anyone needs; the cap just prevents
// a hand-edited config from ballooning the preference file.
const maxSoundPathLen = 4096

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

// looksLikePath returns true when the raw value should be interpreted as
// a filesystem path rather than a terminal-notifier sound name. Detection
// is lexical (presence of a separator or `~` prefix) so callers don't
// stat the filesystem just to classify.
func looksLikePath(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "~") {
		return true
	}
	if strings.ContainsAny(s, "/\\") {
		return true
	}
	return false
}

// ClassifySoundValue returns the SoundKind of a stored notify-sound value
// (empty / name / path). The input is trimmed before classification.
func ClassifySoundValue(s string) SoundKind {
	v := strings.TrimSpace(s)
	if v == "" {
		return SoundKindEmpty
	}
	if looksLikePath(v) {
		return SoundKindPath
	}
	return SoundKindName
}

// expandHomePath expands a leading ~ or ~/ into the current user's home.
// Returns the input unchanged when it has no tilde prefix. Errors bubble
// up when HOME is unset — we never silently drop the tilde, which would
// let a config like "~/sounds/ping.wav" resolve to "./sounds/ping.wav"
// relative to the daemon's working directory.
func expandHomePath(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := userhome.Dir()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotificationSoundHomeUnset, err)
	}
	if strings.TrimSpace(home) == "" {
		return "", ErrNotificationSoundHomeUnset
	}
	if p == "~" {
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}
	// `~user/...` is NOT expanded — this keeps behaviour predictable and
	// prevents a config value from probing other users' homedirs.
	return "", fmt.Errorf("%w: ~user/... form is not supported, use an absolute path", ErrInvalidNotificationSound)
}

// validateNotificationSoundName guards the CLI writer and the file reader
// for the legacy "sound name" form. A value that fails here would
// otherwise survive a round-trip through disk and ship to
// terminal-notifier as an argv token carrying newlines or a leading dash
// (argv injection via `--foo`).
func validateNotificationSoundName(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidNotificationSound)
	}
	if !notificationSoundPattern.MatchString(trimmed) {
		return fmt.Errorf("%w: %q (allowed: ASCII letters/digits/underscore/dash, 1-64 chars, no leading dash)", ErrInvalidNotificationSound, s)
	}
	return nil
}

// validateNotificationSoundPath guards the CLI writer for the path form.
// The expanded absolute path is returned so the caller can persist the
// resolved value rather than the raw `~/...` input (which would depend
// on HOME at read time). A missing file is rejected up front so the
// operator sees the error immediately rather than at first notification.
func validateNotificationSoundPath(raw string) (string, error) {
	return validateNotificationSoundPathForGOOS(raw, runtime.GOOS)
}

func validateNotificationSoundPathForGOOS(raw, goos string) (string, error) {
	if strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("%w: path contains control characters", ErrInvalidNotificationSound)
	}
	if len(raw) > maxSoundPathLen {
		return "", fmt.Errorf("%w: path exceeds %d bytes", ErrInvalidNotificationSound, maxSoundPathLen)
	}
	expanded, err := expandHomePath(raw)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("%w: %q must be an absolute path (got relative after expansion)", ErrInvalidNotificationSound, raw)
	}
	cleaned := filepath.Clean(expanded)
	info, err := os.Stat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %q does not exist", ErrInvalidNotificationSound, cleaned)
		}
		return "", fmt.Errorf("%w: stat %q: %v", ErrInvalidNotificationSound, cleaned, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q is not a regular file", ErrInvalidNotificationSound, cleaned)
	}
	if goos == "windows" && !strings.EqualFold(filepath.Ext(cleaned), ".wav") {
		return "", fmt.Errorf("%w: %q must be a .wav file on Windows", ErrInvalidNotificationSound, cleaned)
	}
	return cleaned, nil
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

// ReadNotificationSound returns the configured sound value — either a
// terminal-notifier sound name or an absolute audio-file path. Callers
// should use ClassifySoundValue to dispatch. A missing file means "no
// custom sound configured". A file whose contents fail validation is
// also treated as "no custom sound configured" — delivery is non-
// critical and should never fail a notification because of a hand-
// edited config file. The CLI writer rejects bad input up front, so a
// stored invalid value almost certainly means operator tinkering.
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
	switch ClassifySoundValue(candidate) {
	case SoundKindName:
		if err := validateNotificationSoundName(candidate); err != nil {
			return "", nil
		}
		return candidate, nil
	case SoundKindPath:
		// On read we only check that the stored path still points at a
		// regular file — a missing file at read time means the user
		// deleted the asset; silently ignore rather than fail delivery.
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			return "", nil
		}
		return candidate, nil
	default:
		return "", nil
	}
}

// WriteNotificationSound persists the configured sound value. An empty
// value clears the setting. Non-empty values are classified and
// validated: names go through the Apple regex; paths must expand to an
// absolute path pointing at a regular file that currently exists. Bad
// values return ErrInvalidNotificationSound so operators see an
// actionable error rather than shipping a poisoned argv / missing file
// to the audio player later.
func WriteNotificationSound(sound string) error {
	path, err := NotificationSoundPath()
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(sound)
	if trimmed == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var stored string
	switch ClassifySoundValue(trimmed) {
	case SoundKindPath:
		resolved, err := validateNotificationSoundPath(trimmed)
		if err != nil {
			return err
		}
		stored = resolved
	case SoundKindName:
		if err := validateNotificationSoundName(trimmed); err != nil {
			return err
		}
		stored = trimmed
	default:
		return fmt.Errorf("%w: empty", ErrInvalidNotificationSound)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(stored+"\n"), 0600)
}
