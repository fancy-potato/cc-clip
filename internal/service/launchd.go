//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shunmei/cc-clip/internal/userhome"
)

const (
	plistLabel    = "com.cc-clip.daemon"
	plistFileName = "com.cc-clip.daemon.plist"
)

func launchdFallbackBaseDir() string {
	return filepath.Join(os.TempDir(), "cc-clip")
}

// PlistPath returns the full path to the launchd plist file.
func PlistPath() string {
	home, err := userhome.Dir()
	if err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "Library", "LaunchAgents", plistFileName)
	}
	return filepath.Join(launchdFallbackBaseDir(), "LaunchAgents", plistFileName)
}

// logPath returns the path for daemon log output.
func logPath() string {
	home, err := userhome.Dir()
	if err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "Library", "Logs", "cc-clip.log")
	}
	return filepath.Join(launchdFallbackBaseDir(), "Logs", "cc-clip.log")
}

// escapeXMLText escapes a string for safe embedding inside an XML text node
// (i.e. between `<string>` and `</string>`). Hand-rolled to avoid pulling in
// encoding/xml: the `internal/service/package_contents_test.go` anti-feature
// test restricts which packages this subtree may import. The five mappings
// below are the full set required for #PCDATA per XML 1.0, plus `'` which
// some strict XML consumers reject inside attribute values even though our
// usage is text-only. A user installation path such as
// `/Users/alice/My Launch & Setup/cc-clip` would otherwise emit invalid XML
// and be rejected by `launchctl load` at install time.
func escapeXMLText(s string) string {
	// Replace in a single pass to avoid N-pass cascades where `&amp;` gets
	// re-escaped into `&amp;amp;`. Order is irrelevant because the
	// replacement bytes (<, >, ", ', &) are themselves re-escaped by this
	// same map; we just need to ensure we don't loop over our own output.
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// generatePlist creates the launchd plist XML content.
// Includes an explicit PATH so Homebrew tools (pngpaste) are found
// even though launchd doesn't source the user's shell profile.
func generatePlist(binaryPath string, port int) string {
	// Every interpolated path/label/log is passed through escapeXMLText so
	// metacharacters in user-controllable values (binary path, $HOME) can't
	// break the plist XML. The port is an int, not XML-dangerous, but we
	// still %d-format rather than %s-interpolate to keep the contract tight.
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
        <string>--port</string>
        <string>%d</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, escapeXMLText(plistLabel), escapeXMLText(binaryPath), port, escapeXMLText(logPath()), escapeXMLText(logPath()))
}

// launchctlLoad loads a plist via launchctl. Overridable for testing.
var launchctlLoad = func(plistPath string) error {
	cmd := exec.Command("launchctl", "load", "-w", plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// launchctlUnload unloads a plist via launchctl. Overridable for testing.
var launchctlUnload = func(plistPath string) error {
	cmd := exec.Command("launchctl", "unload", "-w", plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl unload failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// launchctlList checks if a job is loaded. Overridable for testing.
var launchctlList = func(label string) (bool, error) {
	cmd := exec.Command("launchctl", "list", label)
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}

// Install writes the plist file and loads the service via launchctl.
func Install(binaryPath string, port int) error {
	plist := PlistPath()

	// Ensure LaunchAgents directory exists
	dir := filepath.Dir(plist)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create LaunchAgents directory: %w", err)
	}

	content := generatePlist(binaryPath, port)
	if err := os.WriteFile(plist, []byte(content), 0644); err != nil {
		return fmt.Errorf("cannot write plist: %w", err)
	}

	if err := launchctlLoad(plist); err != nil {
		// Clean up plist on load failure
		os.Remove(plist)
		return err
	}

	return nil
}

// launchctlRemove force-removes a job by label. Overridable for testing.
var launchctlRemove = func(label string) error {
	cmd := exec.Command("launchctl", "remove", label)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl remove failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Uninstall unloads the service and removes the plist file.
func Uninstall() error {
	plist := PlistPath()

	// Try unload via plist path first (requires file to exist).
	unloadErr := launchctlUnload(plist)

	// Fallback: remove by label (works even if plist is already deleted).
	if unloadErr != nil {
		_ = launchctlRemove(plistLabel)
	}

	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot remove plist: %w", err)
	}

	return nil
}

// Status checks if the launchd job is currently loaded/running.
func Status() (bool, error) {
	return launchctlList(plistLabel)
}
