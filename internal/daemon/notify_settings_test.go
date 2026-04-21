package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndReadNotificationSound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteNotificationSound("Glass"); err != nil {
		t.Fatalf("WriteNotificationSound: %v", err)
	}

	got, err := ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "Glass" {
		t.Fatalf("sound = %q, want %q", got, "Glass")
	}

	data, err := os.ReadFile(filepath.Join(home, ".cache", "cc-clip", notificationSoundFileName))
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if string(data) != "Glass\n" {
		t.Fatalf("file contents = %q, want %q", string(data), "Glass\n")
	}
}

func TestWriteNotificationSoundClearsSetting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteNotificationSound("Ping"); err != nil {
		t.Fatalf("seed WriteNotificationSound: %v", err)
	}
	if err := WriteNotificationSound(""); err != nil {
		t.Fatalf("clear WriteNotificationSound: %v", err)
	}

	got, err := ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "" {
		t.Fatalf("sound = %q, want empty", got)
	}

	path, err := NotificationSoundPath()
	if err != nil {
		t.Fatalf("NotificationSoundPath: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected config file removed, stat err = %v", err)
	}
}

// TestNotificationSoundPathErrorsWhenHomeUnset pins the P2-E fix: when
// HOME is unset, the lib must return a typed error instead of silently
// falling back to a CWD-relative path. A previous revision constructed
// `.cache/cc-clip/notify-sound` against the working directory, which could
// scatter the config across whatever directory the daemon happened to be
// launched from.
func TestNotificationSoundPathErrorsWhenHomeUnset(t *testing.T) {
	// t.Setenv + empty value unsets HOME for the test's lifetime. Also
	// clear USERPROFILE in case the test runs on Windows (os.UserHomeDir
	// consults both).
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	// Some platforms (macOS under CGo) use getpwuid when HOME is empty.
	// Point that fallback at a non-existent uid-less config with XDG_*.
	t.Setenv("XDG_CACHE_HOME", "")

	path, err := NotificationSoundPath()
	if err == nil {
		// os.UserHomeDir may still succeed on some CI configurations by
		// reading /etc/passwd. Skip rather than false-fail in that case —
		// the invariant we care about is "if homedir lookup fails, the
		// caller gets a typed error", not "HOME=\"\" always fails".
		t.Skipf("os.UserHomeDir still resolved a home path on this platform (got %q); cannot exercise the HOME-unset branch", path)
	}
	if !errors.Is(err, ErrNotificationSoundHomeUnset) {
		t.Fatalf("err = %v, want ErrNotificationSoundHomeUnset", err)
	}
}

// TestValidateNotificationSoundName pins the regex used by the lib writer
// AND the CLI validator. Allowed: ASCII letters/digits/underscore/dash,
// 1-64 chars, no leading dash. Rejected: empty, any whitespace, newline/
// control, leading dash (argv injection), over-long. Spaces are rejected
// because every Apple system sound is a single token; allowing spaces
// lets validation pass for names that terminal-notifier silently rejects
// at delivery time.
func TestValidateNotificationSoundName(t *testing.T) {
	valid := []string{
		"Glass",
		"Tink",
		"Ping",
		"A",
		"A-B_C1",
		"name_with_underscore",
	}
	for _, s := range valid {
		if err := validateNotificationSoundName(s); err != nil {
			t.Errorf("validateNotificationSoundName(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		"Glass\nextra",
		"Sound With Space",
		"-dash",
		"-",
		"with\ttab",
		"bell\x07",
		"double--dash-" + string(make([]byte, 70)),
		"unicode-ë",
		"semicolon;ok",
	}
	for _, s := range invalid {
		if err := validateNotificationSoundName(s); err == nil {
			t.Errorf("validateNotificationSoundName(%q) = nil, want error", s)
		} else if !errors.Is(err, ErrInvalidNotificationSound) {
			t.Errorf("validateNotificationSoundName(%q) = %v, want ErrInvalidNotificationSound", s, err)
		}
	}
}

// TestWriteNotificationSoundRejectsInvalidInput verifies the writer
// propagates validation errors without touching the config file.
func TestWriteNotificationSoundRejectsInvalidInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, bad := range []string{"Glass\nextra", "-dash", "", "  "} {
		err := WriteNotificationSound(bad)
		switch bad {
		case "", "  ":
			// empty / whitespace-only means "clear" — succeeds and
			// removes any existing file. The round-trip above already
			// covers this path; just assert no error here.
			if err != nil {
				t.Errorf("WriteNotificationSound(%q) = %v, want nil (clear)", bad, err)
			}
		default:
			if err == nil {
				t.Errorf("WriteNotificationSound(%q) = nil, want error", bad)
			} else if !errors.Is(err, ErrInvalidNotificationSound) {
				t.Errorf("WriteNotificationSound(%q) = %v, want ErrInvalidNotificationSound", bad, err)
			}
		}
	}

	path, err := NotificationSoundPath()
	if err != nil {
		t.Fatalf("NotificationSoundPath: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no config file written for invalid input, stat err = %v", err)
	}
}

// TestClassifySoundValue pins the name-vs-path dispatch. Operators
// rely on this to predict which code path their config hits: names go
// to terminal-notifier, paths go to afplay/paplay/PowerShell.
func TestClassifySoundValue(t *testing.T) {
	cases := []struct {
		in   string
		want SoundKind
	}{
		{"", SoundKindEmpty},
		{"   ", SoundKindEmpty},
		{"Glass", SoundKindName},
		{"Ping_1", SoundKindName},
		{"/abs/path.wav", SoundKindPath},
		{"~/sounds/ping.wav", SoundKindPath},
		{"relative/path.wav", SoundKindPath},
		{`C:\sounds\ping.wav`, SoundKindPath},
		// Leading `~` alone → path form (caller stats it; fails validation later).
		{"~", SoundKindPath},
	}
	for _, c := range cases {
		if got := ClassifySoundValue(c.in); got != c.want {
			t.Errorf("ClassifySoundValue(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestWriteAndReadNotificationSoundPath roundtrips a path value. The
// writer must persist the expanded absolute path (so reads don't depend
// on HOME at notification time) and must reject a path that doesn't
// exist.
func TestWriteAndReadNotificationSoundPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create a real audio file.
	soundFile := filepath.Join(home, "ping.wav")
	if err := os.WriteFile(soundFile, []byte("RIFF....WAVEfmt "), 0644); err != nil {
		t.Fatalf("create sound file: %v", err)
	}

	// Write using ~-prefixed path; reader should see the expanded form.
	if err := WriteNotificationSound("~/ping.wav"); err != nil {
		t.Fatalf("WriteNotificationSound: %v", err)
	}
	got, err := ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != soundFile {
		t.Fatalf("sound = %q, want %q (expanded)", got, soundFile)
	}
	if ClassifySoundValue(got) != SoundKindPath {
		t.Fatalf("stored value not classified as path: %q", got)
	}
}

// TestWriteNotificationSoundRejectsMissingPath guards against persisting
// a path that doesn't exist — the operator typed a typo, and we'd
// rather surface it now than at first notification delivery.
func TestWriteNotificationSoundRejectsMissingPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := WriteNotificationSound(filepath.Join(home, "does-not-exist.wav"))
	if err == nil {
		t.Fatal("expected error for missing path, got nil")
	}
	if !errors.Is(err, ErrInvalidNotificationSound) {
		t.Fatalf("err = %v, want ErrInvalidNotificationSound", err)
	}
}

// TestWriteNotificationSoundRejectsDirectory — a path to a directory
// should not pass validation even if it exists.
func TestWriteNotificationSoundRejectsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := WriteNotificationSound(home)
	if err == nil {
		t.Fatal("expected error for directory path, got nil")
	}
	if !errors.Is(err, ErrInvalidNotificationSound) {
		t.Fatalf("err = %v, want ErrInvalidNotificationSound", err)
	}
}

func TestValidateNotificationSoundPathForGOOSRejectsNonWAVOnWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	soundFile := filepath.Join(home, "ping.mp3")
	if err := os.WriteFile(soundFile, []byte("ID3"), 0644); err != nil {
		t.Fatalf("create sound file: %v", err)
	}

	_, err := validateNotificationSoundPathForGOOS(soundFile, "windows")
	if err == nil {
		t.Fatal("expected error for non-wav file on windows, got nil")
	}
	if !errors.Is(err, ErrInvalidNotificationSound) {
		t.Fatalf("err = %v, want ErrInvalidNotificationSound", err)
	}
}

func TestValidateNotificationSoundPathForGOOSAcceptsWAVOnWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	soundFile := filepath.Join(home, "ping.wav")
	if err := os.WriteFile(soundFile, []byte("RIFF....WAVEfmt "), 0644); err != nil {
		t.Fatalf("create sound file: %v", err)
	}

	got, err := validateNotificationSoundPathForGOOS(soundFile, "windows")
	if err != nil {
		t.Fatalf("validateNotificationSoundPathForGOOS: %v", err)
	}
	if got != soundFile {
		t.Fatalf("path = %q, want %q", got, soundFile)
	}
}

// TestReadNotificationSoundIgnoresMissingStoredPath — if the user
// deletes the audio file after setting it, reads must silently return
// empty rather than fail every delivery. (The writer catches
// non-existent paths up front; this path handles post-hoc removal.)
func TestReadNotificationSoundIgnoresMissingStoredPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := NotificationSoundPath()
	if err != nil {
		t.Fatalf("NotificationSoundPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Simulate a hand-edited config pointing at a now-missing file.
	if err := os.WriteFile(path, []byte("/tmp/definitely/does/not/exist-cc-clip.wav\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "" {
		t.Fatalf("sound = %q, want empty (missing path ignored)", got)
	}
}

// TestReadNotificationSoundFallsBackOnInvalidStoredValue confirms the
// read path treats an operator-corrupted file as "no sound configured"
// rather than propagating the validation error. Delivery is non-critical
// (fall-through to the default sound) so failing every notification on a
// hand-edited config would be worse than ignoring it.
func TestReadNotificationSoundFallsBackOnInvalidStoredValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := NotificationSoundPath()
	if err != nil {
		t.Fatalf("NotificationSoundPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Write a value that bypasses the writer's validation (simulating a
	// hand-edited file with a newline payload).
	if err := os.WriteFile(path, []byte("Glass\nextra\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "" {
		t.Fatalf("sound = %q, want empty (invalid stored value ignored)", got)
	}
}
