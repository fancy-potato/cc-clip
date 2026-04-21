package daemon

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/win32"
)

// soundPlaybackTimeout bounds a single audio-player invocation. Audio
// files in the notification path are intended to be short chirps (<5s);
// a stuck player that never exits must not accumulate goroutines or
// pile up zombie child processes. 30s is generous but finite.
const soundPlaybackTimeout = 30 * time.Second

// buildSoundPlayerCommand returns the argv that plays `path` using the
// platform's audio tool. Returns an empty slice when no player is
// available. Separated from PlayAudioFile so tests can assert the
// command selection without spawning real processes.
//
// cc-clip's laptop side only officially supports macOS (primary) and
// Windows (partial); Linux is the REMOTE target in this product, never
// the laptop. The switch below therefore only targets those two
// platforms — anything else falls through and PlayAudioFile logs a
// "no player" message. The CLI refuses to persist a path on
// unsupported platforms, so reaching this branch in production implies
// a hand-edited config.
//
// The path is passed as an argv token — never through a shell — so
// spaces / special characters in the filename are safe.
func buildSoundPlayerCommand(path string) []string {
	switch runtime.GOOS {
	case "darwin":
		if bin, err := exec.LookPath("afplay"); err == nil {
			return []string{bin, path}
		}
	case "windows":
		// PowerShell's Media.SoundPlayer handles .wav synchronously.
		// We probe pwsh first (cross-platform PowerShell 7+) then fall
		// back to classic powershell.exe.
		for _, candidate := range []string{"pwsh", "powershell"} {
			if bin, err := exec.LookPath(candidate); err == nil {
				// -NoProfile so the player doesn't get slowed by user profile
				// scripts, -NonInteractive / -Command for scriptable use.
				script := buildWindowsSoundPlayerScript(path)
				return []string{bin, "-NoProfile", "-NonInteractive", "-Command", script}
			}
		}
	}
	return nil
}

// buildWindowsSoundPlayerScript returns a PowerShell snippet that treats
// path as a literal string, not an expandable one. PowerShell double-
// quoted strings would expand $var, $(), and backticks inside the
// filename; a single-quoted string with doubled apostrophes preserves
// the raw path bytes instead.
func buildWindowsSoundPlayerScript(path string) string {
	literal := strings.ReplaceAll(path, `'`, `''`)
	return fmt.Sprintf(`$p = New-Object System.Media.SoundPlayer '%s'; $p.PlaySync()`, literal)
}

// PlayAudioFile spawns the platform audio player asynchronously and
// logs any error. Returns immediately — callers must not rely on
// playback completing before they proceed. This is deliberate:
// notification delivery should never block on the audio player.
//
// A missing platform tool is logged once per invocation; we do not
// cache the "no player available" result because operators may install
// one without restarting the daemon.
func PlayAudioFile(path string) {
	argv := buildSoundPlayerCommand(path)
	if len(argv) == 0 {
		log.Printf("sound: no audio player available for %q on this platform (need afplay on macOS or pwsh/powershell on Windows)", path)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), soundPlaybackTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		win32.HideConsoleWindow(cmd)
		if err := cmd.Run(); err != nil {
			log.Printf("sound: play %q via %s failed: %v", path, argv[0], err)
		}
	}()
}
