//go:build windows

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	modAlt      = 0x0001
	modNoRepeat = 0x4000
	vkV         = 0x56
	wmHotkey    = 0x0312
)

var hotkeyRunning atomic.Bool

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

func cmdHotkey() {
	storedCfg, hasStoredCfg, err := loadHotkeyConfig()
	if err != nil {
		log.Fatalf("hotkey config error: %v", err)
	}
	if !hasStoredCfg {
		storedCfg = hotkeyConfig{
			RemoteDir: defaultRemoteUploadDir,
			DelayMS:   150,
		}
	}

	var host string
	flagArgs := os.Args[2:]
	if len(os.Args) > 2 && !strings.HasPrefix(os.Args[2], "-") {
		host = os.Args[2]
		flagArgs = os.Args[3:]
	}

	fs := flag.NewFlagSet("hotkey", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	remoteDir := fs.String("remote-dir", storedCfg.RemoteDir, "remote upload directory")
	delayMS := fs.Int("delay-ms", storedCfg.DelayMS, "delay before Ctrl+Shift+V after Alt+V")
	stop := fs.Bool("stop", false, "stop the background hotkey process")
	status := fs.Bool("status", false, "show hotkey status")
	enableAutostart := fs.Bool("enable-autostart", false, "start the hotkey automatically at login")
	disableAutostart := fs.Bool("disable-autostart", false, "remove hotkey auto-start at login")
	runLoop := fs.Bool("run-loop", false, "internal background loop")

	if err := fs.Parse(flagArgs); err != nil {
		log.Fatal(err)
	}

	if *delayMS < 0 {
		log.Fatalf("invalid --delay-ms: %d", *delayMS)
	}

	if *stop {
		stopHotkeyProcess()
		return
	}
	if *disableAutostart {
		if err := uninstallHotkeyAutostart(); err != nil {
			log.Fatalf("failed to disable hotkey auto-start: %v", err)
		}
		stopHotkeyProcess()
		fmt.Println("hotkey: auto-start disabled")
		return
	}
	if *status {
		printHotkeyStatus()
		return
	}

	if host == "" {
		host = storedCfg.Host
	}
	if host == "" {
		log.Fatal("usage: cc-clip hotkey [<host>] [--remote-dir DIR] [--delay-ms N] [--enable-autostart] [--disable-autostart] [--stop] [--status]")
	}

	cfg := hotkeyConfig{
		Host:      host,
		RemoteDir: *remoteDir,
		DelayMS:   *delayMS,
	}
	if err := saveHotkeyConfig(cfg); err != nil {
		log.Fatalf("failed to save hotkey config: %v", err)
	}

	if *enableAutostart {
		if err := installHotkeyAutostart(); err != nil {
			log.Fatalf("failed to enable hotkey auto-start: %v", err)
		}
		fmt.Println("hotkey: auto-start enabled")
	}

	if *runLoop {
		runHotkeyLoop(cfg.Host, cfg.RemoteDir, time.Duration(cfg.DelayMS)*time.Millisecond)
		return
	}

	startHotkeyBackground(cfg.Host, cfg.RemoteDir, cfg.DelayMS)
}

func startHotkeyBackground(host, remoteDir string, delayMS int) {
	hotkeyStopIfStale()
	if pid, ok := hotkeyProcessPID(); ok {
		fmt.Printf("hotkey: already running (PID %d)\n", pid)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot determine executable path: %v", err)
	}

	args := []string{
		"hotkey",
		host,
		"--remote-dir", remoteDir,
		"--delay-ms", strconv.Itoa(delayMS),
		"--run-loop",
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start hotkey process: %v", err)
	}

	if err := writeHotkeyPID(cmd.Process.Pid); err != nil {
		log.Fatalf("hotkey started (PID %d) but pid file write failed: %v", cmd.Process.Pid, err)
	}
	fmt.Printf("hotkey: started in background (PID %d), trigger with Alt+V\n", cmd.Process.Pid)
}

func runHotkeyLoop(host, remoteDir string, delay time.Duration) {
	if err := initHotkeyLogger(); err != nil {
		log.Fatalf("hotkey logger setup failed: %v", err)
	}
	if err := writeHotkeyPID(os.Getpid()); err != nil {
		log.Fatalf("hotkey pid file write failed: %v", err)
	}
	defer os.Remove(hotkeyPIDPath())

	log.Printf("hotkey: starting for host=%s remote_dir=%s", host, remoteDir)

	user32 := syscall.NewLazyDLL("user32.dll")
	registerHotKey := user32.NewProc("RegisterHotKey")
	unregisterHotKey := user32.NewProc("UnregisterHotKey")
	getMessage := user32.NewProc("GetMessageW")

	const hotkeyID = 1
	r1, _, err := registerHotKey.Call(0, hotkeyID, modAlt|modNoRepeat, vkV)
	if r1 == 0 {
		log.Fatalf("hotkey: RegisterHotKey failed: %v", err)
	}
	defer unregisterHotKey.Call(0, hotkeyID)
	log.Printf("hotkey: registered Alt+V")

	var m msg
	for {
		ret, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(ret) {
		case -1:
			log.Printf("hotkey: GetMessageW returned error")
			return
		case 0:
			log.Printf("hotkey: message loop exited")
			return
		}

		if m.message != wmHotkey {
			continue
		}
		if hotkeyRunning.Swap(true) {
			log.Printf("hotkey: ignored repeated Alt+V while previous send is still running")
			continue
		}

		go func() {
			defer hotkeyRunning.Store(false)
			if err := handleHotkeyPress(host, remoteDir, delay); err != nil {
				log.Printf("hotkey: send failed: %v", err)
				return
			}
			log.Printf("hotkey: send completed")
		}()
	}
}

func handleHotkeyPress(host, remoteDir string, delay time.Duration) error {
	log.Printf("hotkey: Alt+V pressed")
	result, err := uploadImage(host, remoteDir, "")
	if err != nil {
		return err
	}
	defer func() {
		if result.TempFile {
			os.Remove(result.LocalImagePath)
		}
	}()

	log.Printf("hotkey: uploaded %s", result.RemotePath)
	return pasteRemotePath(result.RemotePath, result.LocalImagePath, delay, true)
}

func printHotkeyStatus() {
	pid, ok := hotkeyProcessPID()
	if !ok {
		fmt.Println("hotkey: not running")
	} else {
		fmt.Printf("hotkey: running (PID %d)\n", pid)
	}

	if hotkeyAutostartEnabled() {
		fmt.Println("hotkey: auto-start enabled")
	} else {
		fmt.Println("hotkey: auto-start disabled")
	}

	cfg, ok, err := loadHotkeyConfig()
	if err != nil {
		fmt.Printf("hotkey: config error: %v\n", err)
		return
	}
	if !ok || cfg.Host == "" {
		fmt.Println("hotkey: no saved default host")
		return
	}

	fmt.Printf("hotkey: default host %s\n", cfg.Host)
	fmt.Printf("hotkey: remote dir %s\n", cfg.RemoteDir)
	fmt.Printf("hotkey: delay %dms\n", cfg.DelayMS)
}

func stopHotkeyProcess() {
	pid, ok := hotkeyProcessPID()
	if !ok {
		fmt.Println("hotkey: not running")
		return
	}

	cmdline, err := localProcessCommand(pid)
	if err == nil && !strings.Contains(strings.ToLower(cmdline), " hotkey ") {
		fmt.Printf("hotkey: pid %d is not a cc-clip hotkey process, refusing to kill\n", pid)
		os.Remove(hotkeyPIDPath())
		return
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Println("hotkey: process not found")
		os.Remove(hotkeyPIDPath())
		return
	}
	_ = proc.Kill()
	os.Remove(hotkeyPIDPath())
	fmt.Printf("hotkey: stopped PID %d\n", pid)
}

func hotkeyStopIfStale() {
	pid, ok := hotkeyProcessPID()
	if !ok {
		return
	}
	cmdline, err := localProcessCommand(pid)
	if err != nil || !strings.Contains(strings.ToLower(cmdline), " hotkey ") {
		os.Remove(hotkeyPIDPath())
	}
}

func hotkeyProcessPID() (int, bool) {
	data, err := os.ReadFile(hotkeyPIDPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(hotkeyPIDPath())
		return 0, false
	}
	cmdline, err := localProcessCommand(pid)
	if err != nil || !strings.Contains(strings.ToLower(cmdline), " hotkey ") {
		_ = os.Remove(hotkeyPIDPath())
		return 0, false
	}
	return pid, true
}

func hotkeyPIDPath() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheDir, "cc-clip", "hotkey.pid")
}

func hotkeyLogPath() string {
	return hotkeyLogPathFunc()
}

var hotkeyLogPathFunc = func() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheDir, "cc-clip", "hotkey.log")
}

func writeHotkeyPID(pid int) error {
	if err := os.MkdirAll(filepath.Dir(hotkeyPIDPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(hotkeyPIDPath(), []byte(strconv.Itoa(pid)), 0644)
}

func initHotkeyLogger() error {
	if err := os.MkdirAll(filepath.Dir(hotkeyLogPath()), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(hotkeyLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags)
	return nil
}
