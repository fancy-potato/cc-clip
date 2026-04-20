package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestInstallScriptInstallsSwiftBarPluginViaSymlink(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS shell-script test")
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	installDir := filepath.Join(home, "bin")
	shareDir := filepath.Join(home, "share")
	swiftBarDir := filepath.Join(home, "Documents", "SwiftBar")
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}
	if err := os.MkdirAll(swiftBarDir, 0700); err != nil {
		t.Fatalf("mkdir swiftbar dir: %v", err)
	}

	version := "v1.2.3"
	platformArch := runtime.GOARCH
	switch platformArch {
	case "amd64", "arm64":
	default:
		t.Fatalf("unsupported GOARCH for installer test: %s", platformArch)
	}
	archiveName := "cc-clip_1.2.3_darwin_" + platformArch + ".tar.gz"
	archivePath := filepath.Join(root, archiveName)
	checksumsPath := filepath.Join(root, "checksums.txt")
	archiveDir := filepath.Join(root, "archive")
	if err := os.MkdirAll(filepath.Join(archiveDir, "scripts"), 0700); err != nil {
		t.Fatalf("mkdir archive scripts dir: %v", err)
	}
	writeExecutable(t, filepath.Join(archiveDir, "cc-clip"), "#!/bin/sh\nexit 0\n")
	pluginBody := "#!/bin/bash\necho cc-clip\n"
	if err := os.WriteFile(filepath.Join(archiveDir, "scripts", "cc-clip-tunnels.30s.sh"), []byte(pluginBody), 0700); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	cmd := exec.Command("tar", "-czf", archivePath, "-C", archiveDir, "cc-clip", "scripts")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create archive: %v\n%s", err, out)
	}
	sumCmd := exec.Command("shasum", "-a", "256", archivePath)
	sumOut, err := sumCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("checksum archive: %v\n%s", err, sumOut)
	}
	fields := strings.Fields(string(sumOut))
	if len(fields) < 1 {
		t.Fatalf("unexpected shasum output: %q", string(sumOut))
	}
	if err := os.WriteFile(checksumsPath, []byte(fields[0]+"  "+archiveName+"\n"), 0600); err != nil {
		t.Fatalf("write checksums.txt: %v", err)
	}

	scriptPath := filepath.Clean(filepath.Join("..", "..", "scripts", "install.sh"))
	installCmd := exec.Command("/bin/sh", scriptPath)
	installCmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH=/usr/bin:/bin",
		"CC_CLIP_INSTALL_DIR="+installDir,
		"CC_CLIP_SHARE_DIR="+shareDir,
		"SWIFTBAR_PLUGIN_DIR="+swiftBarDir,
		"INSTALL_SWIFTBAR=0",
		"INSTALL_JQ=0",
		"CC_CLIP_ALLOW_TEST=1",
		"CC_CLIP_TEST_VERSION="+version,
		"CC_CLIP_TEST_DOWNLOAD="+archivePath,
		"CC_CLIP_TEST_CHECKSUMS="+checksumsPath,
	)
	out, err := installCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\noutput:\n%s", err, out)
	}

	localPlugin := filepath.Join(shareDir, "swiftbar", "cc-clip-tunnels.30s.sh")
	linkPlugin := filepath.Join(swiftBarDir, "cc-clip-tunnels.30s.sh")
	localData, readErr := os.ReadFile(localPlugin)
	if readErr != nil {
		t.Fatalf("read local plugin: %v", readErr)
	}
	if string(localData) != pluginBody {
		t.Fatalf("unexpected local plugin body:\n%s", string(localData))
	}
	info, statErr := os.Lstat(linkPlugin)
	if statErr != nil {
		t.Fatalf("lstat swiftbar link: %v", statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected swiftbar plugin path to be a symlink, mode=%v", info.Mode())
	}
	target, linkErr := os.Readlink(linkPlugin)
	if linkErr != nil {
		t.Fatalf("readlink swiftbar plugin: %v", linkErr)
	}
	if target != localPlugin {
		t.Fatalf("expected symlink target %q, got %q", localPlugin, target)
	}
}

func TestUninstallAllRefusesSoleForeignPeerWithoutLocalIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	peerJSON := `[{"peer_id":"other-peer","label":"laptop","reserved_port":18339,"state_dir":"~/.cache/cc-clip/peers/other-peer","created_at":"","updated_at":"","last_connect_at":""}]`
	out, err := runUninstallAllScript(t, home, peerJSON, 97)
	if err == nil {
		t.Fatal("expected uninstall-all.sh to fail closed when local identity is missing")
	}
	if !strings.Contains(out, "has no local peer identity") {
		t.Fatalf("expected missing-local-identity refusal, got:\n%s", out)
	}
	if strings.Contains(out, "[1/2] Cleaning remote side...") {
		t.Fatalf("shared-peer guard was bypassed:\n%s", out)
	}
}

func TestUninstallAllRefusesWhenLocalPeerIDIsNotInRemoteRegistry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	writeLocalPeerID(t, home, "self-peer")
	peerJSON := `[{"peer_id":"other-peer","label":"laptop","reserved_port":18339,"state_dir":"~/.cache/cc-clip/peers/other-peer","created_at":"","updated_at":"","last_connect_at":""}]`
	out, err := runUninstallAllScript(t, home, peerJSON, 97)
	if err == nil {
		t.Fatal("expected uninstall-all.sh to fail closed when the sole peer is foreign")
	}
	if !strings.Contains(out, `local peer id "self-peer" is not present in the remote registry`) {
		t.Fatalf("expected stale-local-identity refusal, got:\n%s", out)
	}
	if strings.Contains(out, "[1/2] Cleaning remote side...") {
		t.Fatalf("shared-peer guard was bypassed:\n%s", out)
	}
}

func TestUninstallAllAllowsLastPeerWhenRegistryMatchesLocalIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	writeLocalPeerID(t, home, "self-peer")
	peerJSON := `[{"peer_id":"self-peer","label":"laptop","reserved_port":18339,"state_dir":"~/.cache/cc-clip/peers/self-peer","created_at":"","updated_at":"","last_connect_at":""}]`
	out, err := runUninstallAllScript(t, home, peerJSON, 0)
	if err != nil {
		t.Fatalf("expected uninstall-all.sh to proceed for the last peer, got %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Full uninstall complete.") {
		t.Fatalf("expected full uninstall completion, got:\n%s", out)
	}
}

func TestUninstallAllPeerProbeDoesNotForceBatchMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	writeLocalPeerID(t, home, "self-peer")
	peerJSON := `[{"peer_id":"self-peer","label":"laptop","reserved_port":18339,"state_dir":"~/.cache/cc-clip/peers/self-peer","created_at":"","updated_at":"","last_connect_at":""}]`
	out, err := runUninstallAllScriptWithOptions(t, home, uninstallAllScriptOptions{
		peerJSON:        peerJSON,
		nonListExit:     0,
		rejectBatchMode: true,
	})
	if err != nil {
		t.Fatalf("expected uninstall-all.sh to proceed without BatchMode=yes, got %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Full uninstall complete.") {
		t.Fatalf("expected full uninstall completion, got:\n%s", out)
	}
}

func TestUninstallAllPythonFallbackSurfacesParseErrorUnderSetE(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	out, err := runUninstallAllScriptWithOptions(t, home, uninstallAllScriptOptions{
		peerJSON:       `not-json`,
		nonListExit:    97,
		installJQ:      false,
		installPython3: true,
		restrictPATH:   true,
		python3Body: `#!/bin/sh
set -eu
printf 'parse_error: bad json\n' >&2
exit 2
`,
	})
	if err == nil {
		t.Fatal("expected uninstall-all.sh to fail when python3 fallback reports parse error")
	}
	if !strings.Contains(out, "could not parse remote peer registry JSON via python3 fallback: parse_error: bad json") {
		t.Fatalf("expected captured python3 parse-error message, got:\n%s", out)
	}
	if strings.Contains(out, "[1/2] Cleaning remote side...") {
		t.Fatalf("python3 parse error should stop before cleanup, got:\n%s", out)
	}
}

// TestUninstallAllPythonFallbackUsesRealPython3 pins the embedded `python3 -c
// '...'` snippet in scripts/uninstall-all.sh by running it with the real
// system python3 (no stub). A regression that mixed tabs and spaces in the
// snippet — as had landed previously — would fail with IndentationError BEFORE
// the script could even count peers, and this test would catch it. The
// `TestUninstallAllPythonFallbackSurfacesParseErrorUnderSetE` test stubs
// python3 with a trivial shell script and so cannot detect such regressions.
func TestUninstallAllPythonFallbackUsesRealPython3(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("system python3 not available")
	}

	home := t.TempDir()
	writeLocalPeerID(t, home, "self-peer")
	peerJSON := `[{"peer_id":"self-peer","label":"laptop","reserved_port":18339,"state_dir":"~/.cache/cc-clip/peers/self-peer","created_at":"","updated_at":"","last_connect_at":""}]`
	out, err := runUninstallAllScriptWithOptions(t, home, uninstallAllScriptOptions{
		peerJSON:    peerJSON,
		nonListExit: 0,
		// installJQ is intentionally false so the script falls through to
		// the real python3 on PATH.
	})
	if err != nil {
		t.Fatalf("expected uninstall-all.sh to succeed via real python3 fallback, got %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Full uninstall complete.") {
		t.Fatalf("expected full uninstall completion via python3 fallback, got:\n%s", out)
	}
	if strings.Contains(out, "IndentationError") || strings.Contains(out, "TabError") {
		t.Fatalf("python3 fallback raised an indent/tab error:\n%s", out)
	}
}

func TestUninstallAllJQFallbackRejectsNonArrayRegistry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	writeLocalPeerID(t, home, "self-peer")
	out, err := runUninstallAllScriptWithOptions(t, home, uninstallAllScriptOptions{
		peerJSON:    `{"peer_id":"self-peer"}`,
		nonListExit: 0,
		installJQ:   true,
	})
	if err == nil {
		t.Fatal("expected uninstall-all.sh to fail when jq sees a non-array registry payload")
	}
	if !strings.Contains(out, "could not parse remote peer registry JSON via jq fallback") {
		t.Fatalf("expected jq parse failure, got:\n%s", out)
	}
	if strings.Contains(out, "[1/2] Cleaning remote side...") {
		t.Fatalf("jq non-array parse error should stop before cleanup, got:\n%s", out)
	}
}

func TestUninstallLocalUsesRepoBinaryFallbackForCodexCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	installDir := filepath.Join(home, "install")
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}

	out, logPath, err := runUninstallLocalScript(t, home, installDir)
	if err != nil {
		t.Fatalf("uninstall-local.sh failed: %v\noutput:\n%s", err, out)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read fake cc-clip log: %v", readErr)
	}
	logText := string(logData)
	if !strings.Contains(logText, "uninstall --codex") {
		t.Fatalf("expected repo-root cc-clip fallback to run codex cleanup, log:\n%s", logText)
	}
}

func TestUninstallLocalRemovesManagedSSHBlocksAndPreservesUserSetEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	config := `Host example
  HostName example.test
  SetEnv KEEP_ME=1
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18339 CC_CLIP_STATE_DIR=/tmp/peer-a
  # <<< cc-clip SetEnv (do not edit) <<<
  # >>> cc-clip managed host: example >>>
  RemoteForward 18339 127.0.0.1:18339
  # <<< cc-clip managed host: example <<<
`
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatalf("write ssh config: %v", err)
	}

	installDir := filepath.Join(home, "install")
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}

	out, _, err := runUninstallLocalScript(t, home, installDir)
	if err != nil {
		t.Fatalf("uninstall-local.sh failed: %v\noutput:\n%s", err, out)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read ssh config: %v", readErr)
	}
	text := string(got)
	if strings.Contains(text, "cc-clip SetEnv") || strings.Contains(text, "cc-clip managed host:") {
		t.Fatalf("managed blocks should be removed, got:\n%s", text)
	}
	if !strings.Contains(text, "SetEnv KEEP_ME=1") {
		t.Fatalf("user-authored SetEnv should be preserved, got:\n%s", text)
	}
}

func TestUninstallLocalPreservesSymlinkedSSHConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}

	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	targetPath := filepath.Join(home, "real-config")
	target := `Host example
  HostName example.test
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18339
  # <<< cc-clip SetEnv (do not edit) <<<
`
	if err := os.WriteFile(targetPath, []byte(target), 0600); err != nil {
		t.Fatalf("write target config: %v", err)
	}
	linkPath := filepath.Join(sshDir, "config")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("symlink ssh config: %v", err)
	}

	installDir := filepath.Join(home, "install")
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}

	out, _, err := runUninstallLocalScript(t, home, installDir)
	if err != nil {
		t.Fatalf("uninstall-local.sh failed: %v\noutput:\n%s", err, out)
	}
	info, statErr := os.Lstat(linkPath)
	if statErr != nil {
		t.Fatalf("lstat symlink: %v", statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("~/.ssh/config should remain a symlink, mode=%v", info.Mode())
	}
	got, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		t.Fatalf("read target config: %v", readErr)
	}
	if string(got) != target {
		t.Fatalf("symlink target should remain unchanged, got:\n%s", string(got))
	}
}

func runUninstallLocalScript(t *testing.T, home, installDir string) (string, string, error) {
	t.Helper()

	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0700); err != nil {
		t.Fatalf("mkdir scripts dir: %v", err)
	}
	srcPath := filepath.Clean(filepath.Join("..", "..", "scripts", "uninstall-local.sh"))
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read uninstall-local.sh: %v", err)
	}
	scriptPath := filepath.Join(scriptsDir, "uninstall-local.sh")
	if err := os.WriteFile(scriptPath, data, 0700); err != nil {
		t.Fatalf("write copied uninstall-local.sh: %v", err)
	}

	logPath := filepath.Join(root, "cc-clip.log")
	writeExecutable(t, filepath.Join(root, "cc-clip"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CC_CLIP_LOG\"\n")

	cmd := exec.Command("/bin/sh", scriptPath, "--install-dir", installDir)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH=/usr/bin:/bin",
		"FAKE_CC_CLIP_LOG="+logPath,
		// CC_CLIP_PREFER_REPO=1 opts into the repo-root fallback in
		// resolve_existing_cc_clip_bin (see scripts/uninstall-local.sh).
		// Without it, the script logs "skipped (cc-clip not found)" and
		// never invokes the fake binary — the very behaviour this helper
		// is meant to exercise. The opt-in guard was introduced so running
		// the script from a clone doesn't silently prefer a stale
		// ./cc-clip over the installed one; tests deliberately use the
		// repo binary as a stand-in for the installed one.
		"CC_CLIP_PREFER_REPO=1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), logPath, err
}

type uninstallAllScriptOptions struct {
	peerJSON        string
	nonListExit     int
	rejectBatchMode bool
	installJQ       bool
	installPython3  bool
	python3Body     string
	restrictPATH    bool
}

func runUninstallAllScript(t *testing.T, home, peerJSON string, nonListExit int) (string, error) {
	t.Helper()
	return runUninstallAllScriptWithOptions(t, home, uninstallAllScriptOptions{
		peerJSON:    peerJSON,
		nonListExit: nonListExit,
		installJQ:   true,
	})
}

func runUninstallAllScriptWithOptions(t *testing.T, home string, opts uninstallAllScriptOptions) (string, error) {
	t.Helper()

	stubDir := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(stubDir, 0700); err != nil {
		t.Fatalf("mkdir fake-bin: %v", err)
	}
	writeExecutable(t, filepath.Join(stubDir, "ssh"), `#!/bin/sh
set -eu
last=""
for arg in "$@"; do
	case "$arg" in
		BatchMode=yes)
			if [ "${FAKE_SSH_REJECT_BATCHMODE:-0}" = "1" ]; then
				printf 'unexpected BatchMode=yes in ssh args\n' >&2
				exit 91
			fi
			;;
	esac
	last=$arg
done
# The peer-list probe is wrapped in a compound shell expression that
# prefers PATH resolution (via command -v cc-clip) but falls back to
# ~/.local/bin/cc-clip. Match any form containing "cc-clip peer list".
case "$last" in
	*"cc-clip peer list"*)
		printf '%s\n' "$FAKE_SSH_PEER_JSON"
		exit 0
		;;
esac
cat >/dev/null || true
printf 'unexpected ssh invocation: %s\n' "$*" >&2
exit "${FAKE_SSH_NON_LIST_EXIT:-0}"
`)
	if opts.installJQ {
		writeExecutable(t, filepath.Join(stubDir, "jq"), `#!/bin/sh
set -eu
input=$(cat)
id=""
want_length=0
want_any=0
while [ $# -gt 0 ]; do
	case "$1" in
		--arg)
			if [ $# -lt 3 ]; then
				exit 2
			fi
			if [ "$2" = "id" ]; then
				id=$3
			fi
			shift 3
			;;
		-e)
			shift
			;;
		*length*)
			want_length=1
			shift
			;;
		*'.peer_id == $id'*)
			want_any=1
			shift
			;;
		*)
			shift
			;;
	esac
done
if [ "$want_length" -eq 1 ]; then
	case "$input" in
		'['*)
			;;
		*)
			exit 5
			;;
	esac
	printf '%s' "$input" | grep -o '"peer_id"' | wc -l | tr -d ' '
	exit 0
fi
if [ "$want_any" -eq 1 ]; then
	case "$input" in
		'['*)
			;;
		*)
			exit 5
			;;
	esac
	if printf '%s' "$input" | grep -F "\"peer_id\":\"$id\"" >/dev/null 2>&1; then
		exit 0
	fi
	exit 1
fi
exit 2
`)
	}
	if opts.installPython3 {
		body := opts.python3Body
		if body == "" {
			body = "#!/bin/sh\nset -eu\nprintf 'missing python3Body in test\\n' >&2\nexit 99\n"
		}
		writeExecutable(t, filepath.Join(stubDir, "python3"), body)
	}
	pathValue := stubDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	if opts.restrictPATH {
		for _, tool := range []string{"dirname", "mktemp", "cat", "head", "tr", "rm"} {
			linkSystemTool(t, stubDir, tool)
		}
		pathValue = stubDir
	}

	scriptPath := filepath.Clean(filepath.Join("..", "..", "scripts", "uninstall-all.sh"))
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("stat uninstall-all.sh: %v", err)
	}
	cmd := exec.Command("/bin/sh", scriptPath, "--host", "example.test")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+pathValue,
		"FAKE_SSH_PEER_JSON="+opts.peerJSON,
		"FAKE_SSH_NON_LIST_EXIT="+strconv.Itoa(opts.nonListExit),
		"FAKE_SSH_REJECT_BATCHMODE="+strconv.Itoa(boolToInt(opts.rejectBatchMode)),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func linkSystemTool(t *testing.T, dir, name string) {
	t.Helper()

	candidates := []string{
		filepath.Join("/usr/bin", name),
		filepath.Join("/bin", name),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			if err := os.Symlink(candidate, filepath.Join(dir, name)); err != nil {
				t.Fatalf("symlink %s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("could not find system tool %q in /usr/bin or /bin", name)
}

func writeLocalPeerID(t *testing.T, home, peerID string) {
	t.Helper()

	cacheDir := filepath.Join(home, ".cache", "cc-clip")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "local-peer-id"), []byte(peerID+"\n"), 0600); err != nil {
		t.Fatalf("write local-peer-id: %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
