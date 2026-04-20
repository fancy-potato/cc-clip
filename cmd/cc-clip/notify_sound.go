package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/shunmei/cc-clip/internal/daemon"
)

type cmdNotifySoundDeps struct {
	lookPath   func(string) (string, error)
	writeSound func(string) error
	soundPath  func() (string, error)
}

func cmdNotifySound() {
	if len(os.Args) != 3 {
		failUsage("usage: cc-clip notify-sound <sound|off>")
	}
	// Only TrimSpace the raw argv input here; daemon.WriteNotificationSound
	// is the canonical trim/validate point. Stripping twice at the CLI
	// layer used to hide a mismatch where the CLI "normalized" value and
	// the lib's stored value could drift.
	sound := strings.TrimSpace(os.Args[2])
	if sound == "" {
		failUsage("usage: cc-clip notify-sound <sound|off>")
	}
	if err := runCmdNotifySound(sound, cmdNotifySoundDeps{
		lookPath:   exec.LookPath,
		writeSound: daemon.WriteNotificationSound,
		soundPath:  daemon.NotificationSoundPath,
	}); err != nil {
		log.Fatal(err)
	}
}

func runCmdNotifySound(sound string, deps cmdNotifySoundDeps) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("notify-sound is only supported on macOS")
	}
	if deps.lookPath == nil {
		deps.lookPath = exec.LookPath
	}
	if deps.writeSound == nil {
		deps.writeSound = daemon.WriteNotificationSound
	}
	if deps.soundPath == nil {
		deps.soundPath = daemon.NotificationSoundPath
	}
	path, err := deps.soundPath()
	if err != nil {
		return fmt.Errorf("locate notification sound preference: %w", err)
	}
	if isDisabledNotificationSound(sound) {
		if err := deps.writeSound(""); err != nil {
			return fmt.Errorf("clear notification sound: %w", err)
		}
		fmt.Printf("Disabled terminal-notifier sound for cc-clip notifications (%s)\n", path)
		return nil
	}
	if _, err := deps.lookPath("terminal-notifier"); err != nil {
		return fmt.Errorf("terminal-notifier is not installed.\nInstall it first:\n  brew install terminal-notifier")
	}
	// Validation lives in the lib helper — WriteNotificationSound rejects
	// invalid names with ErrInvalidNotificationSound, so the CLI does not
	// need to duplicate the regex check.
	if err := deps.writeSound(sound); err != nil {
		return fmt.Errorf("save notification sound: %w", err)
	}
	fmt.Printf("Set cc-clip notification sound to %q (%s)\n", sound, path)
	return nil
}

func isDisabledNotificationSound(sound string) bool {
	switch strings.ToLower(strings.TrimSpace(sound)) {
	case "off", "none", "silent":
		return true
	default:
		return false
	}
}
