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
		failUsage("usage: cc-clip notify-sound <name|/path/to/sound|off>")
	}
	// Only TrimSpace the raw argv input here; daemon.WriteNotificationSound
	// is the canonical trim/validate point. Stripping twice at the CLI
	// layer used to hide a mismatch where the CLI "normalized" value and
	// the lib's stored value could drift.
	sound := strings.TrimSpace(os.Args[2])
	if sound == "" {
		failUsage("usage: cc-clip notify-sound <name|/path/to/sound|off>")
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
		fmt.Printf("Disabled cc-clip notification sound (%s)\n", path)
		return nil
	}

	kind := daemon.ClassifySoundValue(sound)

	// cc-clip's laptop side only supports macOS and Windows — Linux is
	// the remote target, never the laptop. Name-type values (Apple
	// built-ins) further require terminal-notifier on macOS. Path-type
	// values are played by afplay (macOS) or PowerShell's SoundPlayer
	// (Windows); on any other GOOS we fail fast rather than silently
	// persisting a value that would never play.
	switch kind {
	case daemon.SoundKindName:
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("sound names (Apple built-ins) are only supported on macOS; pass an absolute path to an audio file instead")
		}
		if _, err := deps.lookPath("terminal-notifier"); err != nil {
			return fmt.Errorf("terminal-notifier is not installed.\nInstall it first:\n  brew install terminal-notifier")
		}
	case daemon.SoundKindPath:
		if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
			return fmt.Errorf("notify-sound paths are only supported on macOS and Windows laptops")
		}
	}

	// Validation lives in the lib helper — WriteNotificationSound rejects
	// invalid names or non-existent paths with ErrInvalidNotificationSound,
	// so the CLI does not need to duplicate those checks.
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
