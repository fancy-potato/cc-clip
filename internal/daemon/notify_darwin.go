//go:build darwin

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// DarwinNotifier delivers macOS notifications with image thumbnails
// via terminal-notifier, falling back to osascript (text-only) if unavailable.
type DarwinNotifier struct {
	previewDir         string
	terminalNotifier   string // path to terminal-notifier binary, empty if not found
}

func NewDarwinNotifier() *DarwinNotifier {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "cc-clip", "previews")
	os.MkdirAll(dir, 0700)

	tn, _ := exec.LookPath("terminal-notifier")
	return &DarwinNotifier{previewDir: dir, terminalNotifier: tn}
}

func (n *DarwinNotifier) Notify(_ context.Context, evt NotifyEvent) error {
	// Save preview image to disk
	var previewPath string
	if len(evt.ImageData) > 0 {
		ext := ".png"
		if evt.Format == "jpeg" {
			ext = ".jpeg"
		}
		sid := evt.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		previewPath = filepath.Join(n.previewDir, fmt.Sprintf("preview-%s-%d%s", sid, evt.Seq, ext))
		if err := os.WriteFile(previewPath, evt.ImageData, 0600); err != nil {
			previewPath = ""
		}
	}

	title := fmt.Sprintf("cc-clip #%d", evt.Seq)
	subtitle := fmt.Sprintf("%s · %dx%d · %s", evt.Fingerprint, evt.Width, evt.Height, evt.Format)

	body := "Image transferred"
	if evt.DuplicateOf > 0 {
		body = fmt.Sprintf("Duplicate of #%d", evt.DuplicateOf)
	}

	if n.terminalNotifier != "" {
		return n.sendViaTerminalNotifier(title, subtitle, body, previewPath)
	}
	return n.sendViaOsascript(title, subtitle, body)
}

func (n *DarwinNotifier) sendViaTerminalNotifier(title, subtitle, body, imagePath string) error {
	args := []string{
		"-title", title,
		"-subtitle", subtitle,
		"-message", body,
		"-group", "cc-clip",
	}
	if imagePath != "" {
		args = append(args, "-contentImage", imagePath)
		args = append(args, "-open", "file://"+imagePath)
	}
	return exec.Command(n.terminalNotifier, args...).Run()
}

func (n *DarwinNotifier) sendViaOsascript(title, subtitle, body string) error {
	script := fmt.Sprintf(
		`display notification %q with title %q subtitle %q`,
		body, title, subtitle,
	)
	return exec.Command("osascript", "-e", script).Run()
}
