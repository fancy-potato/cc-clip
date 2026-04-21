package daemon

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildWindowsSoundPlayerScriptEscapesLiteralPath(t *testing.T) {
	path := `C:\sounds\it's-$env:TEMP-$(calc).wav`
	got := buildWindowsSoundPlayerScript(path)
	want := `$p = New-Object System.Media.SoundPlayer 'C:\sounds\it''s-$env:TEMP-$(calc).wav'; $p.PlaySync()`
	if got != want {
		t.Fatalf("buildWindowsSoundPlayerScript(%q) = %q, want %q", path, got, want)
	}
}

// TestBuildSoundPlayerCommandPerPlatform pins the two supported
// platforms (macOS → afplay, Windows → pwsh/powershell) and pins that
// every other GOOS returns an empty argv. The empty-argv invariant is
// what makes the "laptop only supports macOS + Windows" product
// decision enforceable — if a future refactor silently re-adds a
// Linux fallback, this test trips.
//
// The path is deliberately nasty ("; rm -rf ~") to assert it is
// carried as a literal argv token, never shell-interpolated.
func TestBuildSoundPlayerCommandPerPlatform(t *testing.T) {
	path := "/tmp/example; rm -rf ~.wav"
	argv := buildSoundPlayerCommand(path)

	switch runtime.GOOS {
	case "darwin":
		if len(argv) == 0 {
			t.Skip("afplay not in PATH on this darwin runner")
		}
		if !strings.HasSuffix(argv[0], "afplay") {
			t.Fatalf("darwin player = %q, want afplay", argv[0])
		}
		if argv[len(argv)-1] != path {
			t.Fatalf("path not passed as literal argv token: %v", argv)
		}
	case "windows":
		if len(argv) == 0 {
			t.Skip("pwsh/powershell not in PATH on this windows runner")
		}
		if !(strings.HasSuffix(argv[0], "pwsh") ||
			strings.HasSuffix(argv[0], "pwsh.exe") ||
			strings.HasSuffix(argv[0], "powershell") ||
			strings.HasSuffix(argv[0], "powershell.exe")) {
			t.Fatalf("windows player = %q, want pwsh(.exe) or powershell(.exe)", argv[0])
		}
		// PowerShell's SoundPlayer gets the path via a PowerShell
		// string literal, not a raw argv token, so we assert the path
		// appears somewhere in the -Command script instead of as the
		// final argv entry.
		if !containsArg(argv, "-NoProfile") {
			t.Fatalf("expected -NoProfile in argv: %v", argv)
		}
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, path) {
			t.Fatalf("path not embedded in powershell command: %v", argv)
		}
	default:
		// Core invariant for non-laptop-supported platforms: no player.
		if len(argv) != 0 {
			t.Fatalf("buildSoundPlayerCommand on %s = %v, want empty argv (laptop unsupported)", runtime.GOOS, argv)
		}
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
