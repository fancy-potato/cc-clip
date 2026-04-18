package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shunmei/cc-clip/internal/daemon"
	"github.com/shunmei/cc-clip/internal/doctor"
	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/service"
	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/setup"
	"github.com/shunmei/cc-clip/internal/shellutil"
	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
	"github.com/shunmei/cc-clip/internal/x11bridge"
	"github.com/shunmei/cc-clip/internal/xvfb"
)

var version = "dev"

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe()
	case "paste":
		cmdPaste()
	case "send":
		cmdSend()
	case "hotkey":
		cmdHotkey()
	case "install":
		cmdInstall()
	case "uninstall":
		cmdUninstall()
	case "connect":
		cmdConnect()
	case "status":
		cmdStatus()
	case "doctor":
		cmdDoctor()
	case "setup":
		cmdSetup()
	case "service":
		cmdService()
	case "notify":
		cmdNotify()
	case "x11-bridge":
		cmdX11Bridge()
	case "tunnel":
		cmdTunnel()
	case "peer":
		cmdPeer()
	case "version", "--version", "-v":
		fmt.Printf("cc-clip %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`cc-clip - Clipboard over SSH for Claude Code

Usage:
  cc-clip <command> [flags]

Daemon (local):
  serve                   Start local clipboard daemon
    --port                Listen port (default: 18339, env: CC_CLIP_PORT)
    --rotate-token        Force new clipboard session token (ignore existing)
    --rotate-tunnel-token Force new tunnel-control token (local-only, /tunnels/* auth)
  service            Manage system service (macOS/Windows)
    install          Install and start service
    uninstall        Stop and remove service
    status           Show service status

Remote:
  install            Install xclip/wl-paste shim
    --target         auto|xclip|wl-paste (default: auto)
    --path           Install directory (default: ~/.local/bin)
  uninstall          Remove shim
    --target         auto|xclip|wl-paste (default: auto; auto removes the installed shim when exactly one exists)
    --path           Install directory (default: ~/.local/bin)
    --host           Clean up PATH marker on remote host instead of local shim
    --peer           Remove cc-clip SSH config; with --host also releases the remote peer lease
  paste              Fetch clipboard image and output path
    --out-dir        Output directory (env: CC_CLIP_OUT_DIR)
  send [<host>]      Upload local clipboard image to remote file path
    --file           Upload this image file instead of reading the clipboard
    --remote-dir     Remote directory (default: ~/.cache/cc-clip/uploads)
    --paste          On Windows, paste the remote path into the active window
    --delay-ms       Delay before Ctrl+Shift+V when --paste is used (default: 150)
    --no-restore     Do not restore the original image clipboard after --paste
  hotkey [<host>]    Windows global remote-paste hotkey listener
    --remote-dir     Remote directory (default: ~/.cache/cc-clip/uploads)
    --hotkey         Global hotkey to trigger remote paste (default: alt+shift+v)
    --delay-ms       Delay before Ctrl+Shift+V after the hotkey (default: 150)
    --enable-autostart   Start the hotkey automatically at login
    --disable-autostart  Remove hotkey auto-start at login
    --stop           Stop the background hotkey process
    --status         Show hotkey process status

One-command setup:
  setup <host>       Full setup: deps, update exact SSH Host block, daemon, deploy
    --port           Tunnel port (default: 18339)

Deploy (local -> remote):
  connect <host>     Deploy cc-clip to remote and update exact SSH Host block
    --port           Tunnel port (default: 18339)
    --local-bin      Path to pre-downloaded remote binary
    --force          Ignore remote state, full redeploy
    --token-only     Only sync token, skip binary/shim deploy

Codex support (extends connect/setup/uninstall):
  connect <host> --codex   Deploy with Codex support (Xvfb + x11-bridge)
  setup <host> --codex     Full setup including Codex support
  uninstall --codex        Remove Codex support only (local)
  uninstall --codex --host H  Remove Codex support on remote host

Diagnostics:
  status             Show component status
  doctor             Local health check
  doctor --host H    Full end-to-end check via SSH
  version            Show version

Persistent tunnels:
  tunnel list          List all tunnels and their status
    --port             Daemon port to query (default: 18339, env: CC_CLIP_PORT)
    --json             Output as JSON (for SwiftBar / scripting)
  tunnel up <host>     Start persistent tunnel to host
    --port             Owning daemon port (default: 18339, env: CC_CLIP_PORT)
    --remote-port      Remote listen port (auto-detected from SSH config if omitted)
  tunnel down <host>   Stop persistent tunnel owned by --port daemon (keeps state for restart)
    --port             Owning daemon port (default: 18339, env: CC_CLIP_PORT)
  tunnel remove <host> Stop persistent tunnel AND delete its state file
    --port             Owning daemon port (default: 18339, env: CC_CLIP_PORT)

Notifications:
  notify             Send a notification to the local daemon
    --title          Notification title
    --body           Notification body
    --urgency        Urgency level (default: 1)
    --from-codex     Parse Codex JSON payload (extracts last-assistant-message)
    --port           Daemon port (default: 18339, env: CC_CLIP_PORT)

Internal (used by deploy):
  peer               Internal registry management
  x11-bridge         X11 clipboard bridge daemon (started by connect --codex)
    --display        X11 display (default: $DISPLAY)
    --port           cc-clip daemon port (default: 18339)`)
}

func getPort() int {
	port, _, err := configuredPort()
	if err != nil {
		log.Fatal(err)
	}
	return port
}

func listenerPort(ln net.Listener, fallback int) int {
	if ln == nil || ln.Addr() == nil {
		return fallback
	}
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok && tcpAddr.Port > 0 {
		return tcpAddr.Port
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return fallback
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return fallback
	}
	return port
}

func getFlag(name, fallback string) string {
	for i, arg := range os.Args {
		if arg != "--"+name {
			continue
		}
		if i+1 >= len(os.Args) {
			// The flag was passed but no value follows it. Silently falling
			// back used to mask typos like `cc-clip tunnel up host --port`
			// (missing value) as "flag not set", which then auto-selected
			// defaults — scripts that forgot the value appeared to succeed.
			log.Fatalf("flag --%s requires a value; run `cc-clip help` for usage", name)
		}
		return os.Args[i+1]
	}
	return fallback
}

func configuredPort() (int, bool, error) {
	port := 18339
	explicit := false
	for i, arg := range os.Args {
		if arg != "--port" {
			continue
		}
		if i+1 >= len(os.Args) {
			return 0, true, fmt.Errorf("flag --port requires a value")
		}
		p, err := parsePortSetting("--port", os.Args[i+1])
		if err != nil {
			return 0, true, err
		}
		port = p
		explicit = true
	}
	// Env var is only consulted when --port was not set explicitly. POSIX
	// convention has CLI flags override the environment; letting CC_CLIP_PORT
	// silently clobber an explicit `--port` routed tunnel commands to the
	// wrong daemon without warning.
	if !explicit {
		if env := os.Getenv("CC_CLIP_PORT"); env != "" {
			p, err := parsePortSetting("CC_CLIP_PORT", env)
			if err != nil {
				return 0, true, err
			}
			port = p
			explicit = true
		}
	}
	return port, explicit, nil
}

func parsePortSetting(source, raw string) (int, error) {
	p, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer between 1 and 65535", source)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535", source)
	}
	return p, nil
}

func hasFlag(name string) bool {
	for _, arg := range os.Args {
		if arg == "--"+name {
			return true
		}
	}
	return false
}

func getTokenTTL() time.Duration {
	ttl := 30 * 24 * time.Hour
	if env := os.Getenv("CC_CLIP_TOKEN_TTL"); env != "" {
		if d, err := time.ParseDuration(env); err == nil {
			ttl = d
		}
	}
	return ttl
}

func cmdServe() {
	port := getPort()
	ttl := getTokenTTL()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	rotateToken := hasFlag("rotate-token")
	rotateTunnelToken := hasFlag("rotate-tunnel-token")

	tm := token.NewManager(ttl)

	var sess token.Session
	var reused bool
	var err error

	if rotateToken {
		sess, err = tm.Generate()
		if err != nil {
			log.Fatalf("failed to generate token: %v", err)
		}
		log.Printf("Token rotated (--rotate-token): new token generated")
	} else {
		sess, reused, err = tm.LoadOrGenerate(ttl)
		if err != nil {
			log.Fatalf("failed to load or generate token: %v", err)
		}
		if reused {
			log.Printf("Token reused from existing file (expires %s)", sess.ExpiresAt.Format(time.RFC3339))
		} else {
			log.Printf("Token generated (no valid existing token found)")
		}
	}

	tokenPath, err := token.WriteTokenFile(sess.Token, sess.ExpiresAt)
	if err != nil {
		log.Fatalf("failed to write token file: %v", err)
	}

	// Rotate the tunnel-control token on disk before the daemon begins
	// serving. The tunnel-control auth middleware re-reads the token from
	// disk on every /tunnels/* request, so order relative to registerTunnelRoutes
	// does not affect correctness — this happens pre-listen so CLI clients
	// using the rotated token succeed on their very first call.
	if rotateTunnelToken {
		if _, err := token.RotateTunnelControlToken(); err != nil {
			log.Fatalf("failed to rotate tunnel control token: %v", err)
		}
		log.Printf("Tunnel control token rotated (--rotate-tunnel-token): new token written to ~/.cache/cc-clip/tunnel-control.token")
	}

	clipboard := daemon.NewClipboardReader()
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer(addr, clipboard, tm, store)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("server error: %v", err)
	}
	listenAddr := addr
	if ln.Addr() != nil {
		listenAddr = ln.Addr().String()
	}
	listenPort := listenerPort(ln, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize tunnel manager and shutdown handling before any tunnel processes
	// can be started so SIGINT/SIGTERM cannot orphan a detached ssh child.
	tunnelMgr := tunnel.NewManager(tunnel.DefaultStateDir())
	if err := registerTunnelRoutes(srv.Mux(), tunnelMgr, listenPort); err != nil {
		log.Fatalf("register tunnel routes: %v", err)
	}
	// Bound request/response time and header size so a slow or hostile peer
	// on the loopback surface (or reaching loopback through the reverse-SSH
	// forward) cannot slowloris the daemon or exhaust memory with oversized
	// headers. WriteTimeout is generous enough for the largest expected
	// clipboard image transfer; ReadHeaderTimeout caps header-slow attacks.
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stopSignals()

	var (
		shutdownOnce sync.Once
		shutdownErr  error
	)
	shutdownDone := make(chan struct{})
	loadAndStartDone := make(chan struct{})
	shutdown := func(logMessage bool) {
		shutdownOnce.Do(func() {
			if logMessage {
				log.Println("shutting down...")
			}
			// Cancel the manager before draining HTTP so any handler queued
			// behind LoadAndStartAll's opMu is released to return
			// ErrManagerShuttingDown instead of deadlocking httpSrv.Shutdown
			// behind startup. launchTunnel already rolls a persisted
			// "connecting" placeholder back to a terminal status if the
			// manager is cancelled before the goroutine can take ownership.
			tunnelMgr.Cancel()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				shutdownErr = err
			}
			// HTTP is drained — no new control requests can arrive. Wait
			// for LoadAndStartAll to finish unwinding after the cancel so a
			// SIGTERM that lands mid-startup does not race past partially-
			// registered tunnels and leave orphan ssh processes.
			select {
			case <-loadAndStartDone:
			case <-time.After(5 * time.Second):
				// LoadAndStartAll is honouring m.ctx; if it's still
				// running after cancel+5s, proceed rather than hanging
				// the daemon indefinitely. Any entries not yet in the
				// map will be handled on the next daemon restart.
			}
			cancel()
			tunnelMgr.Shutdown()
			close(shutdownDone)
		})
	}

	go func() {
		<-sigCtx.Done()
		shutdown(true)
	}()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- httpSrv.Serve(ln)
	}()
	go func() {
		defer close(loadAndStartDone)
		tunnelMgr.LoadAndStartAll(listenPort)
	}()

	log.Printf("Token written to: %s", tokenPath)
	log.Printf("Token expires at: %s", sess.ExpiresAt.Format(time.RFC3339))
	log.Printf("Starting daemon on %s", listenAddr)

	// Start notification delivery, session cleanup, and nonce cleanup in background
	go srv.RunNotifier(ctx, daemon.BuildDeliveryChain())
	go store.RunCleanup(ctx, 30*time.Minute)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				srv.CleanupExpiredNonces()
			case <-ctx.Done():
				return
			}
		}
	}()

	serveErr := <-serveErrCh
	shutdownRequested := sigCtx.Err() != nil
	switch {
	case serveErr == nil:
	case errors.Is(serveErr, http.ErrServerClosed):
	case shutdownRequested && errors.Is(serveErr, net.ErrClosed):
	default:
		shutdown(false)
		<-shutdownDone
		if shutdownErr != nil {
			log.Printf("server shutdown error: %v", shutdownErr)
		}
		log.Fatalf("server error: %v", serveErr)
	}

	shutdown(false)
	<-shutdownDone
	if shutdownErr != nil {
		log.Fatalf("server shutdown error: %v", shutdownErr)
	}
}

func cmdPaste() {
	port := getPort()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	tok, err := token.ReadTokenFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-clip: cannot read token: %v\n", err)
		os.Exit(exitcode.TokenInvalid)
	}

	probeTimeout := envDuration("CC_CLIP_PROBE_TIMEOUT_MS", 500*time.Millisecond)
	fetchTimeout := envDuration("CC_CLIP_FETCH_TIMEOUT_MS", 5*time.Second)

	if err := tunnel.Probe(fmt.Sprintf("127.0.0.1:%d", port), probeTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "cc-clip: tunnel unreachable: %v\n", err)
		os.Exit(exitcode.TunnelUnreachable)
	}

	client := tunnel.NewClient(baseURL, tok, fetchTimeout)

	info, err := client.ClipboardType()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-clip: %v\n", err)
		os.Exit(classifyError(err))
	}

	if info.Type != daemon.ClipboardImage {
		fmt.Fprintf(os.Stderr, "cc-clip: no image in clipboard (type: %s)\n", info.Type)
		os.Exit(exitcode.NoImage)
	}

	outDir := tunnel.DefaultOutDir()
	if env := os.Getenv("CC_CLIP_OUT_DIR"); env != "" {
		outDir = env
	}
	if flag := getFlag("out-dir", ""); flag != "" {
		outDir = flag
	}

	path, err := client.FetchImage(outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-clip: %v\n", err)
		os.Exit(classifyError(err))
	}

	fmt.Println(path)
}

func cmdInstall() {
	targetStr := getFlag("target", "auto")
	installPath := getFlag("path", "")
	port := getPort()

	var target shim.Target
	switch targetStr {
	case "auto":
		target = shim.TargetAuto
	case "xclip":
		target = shim.TargetXclip
	case "wl-paste":
		target = shim.TargetWlPaste
	default:
		log.Fatalf("unsupported target: %s", targetStr)
	}

	result, err := shim.Install(target, installPath, port)
	if err != nil {
		log.Fatalf("install failed: %v", err)
	}

	fmt.Printf("Shim installed:\n")
	fmt.Printf("  target:    %s\n", result.Target)
	fmt.Printf("  shim:      %s\n", result.ShimPath)
	fmt.Printf("  real bin:  %s\n", result.RealBinPath)

	ok, msg := shim.CheckPathPriority(result.InstallDir)
	if ok {
		fmt.Printf("  PATH:      %s\n", msg)
	} else {
		fmt.Printf("  WARNING:   %s\n", msg)
		fmt.Printf("  Fix: add to ~/.bashrc or ~/.profile:\n")
		fmt.Printf("    export PATH=\"%s:$PATH\"\n", result.InstallDir)
	}
}

func cmdUninstall() {
	targetStr := getFlag("target", "auto")
	installPath := getFlag("path", "")
	host := getFlag("host", "")
	peerArg := getFlag("peer", "")
	codex := hasFlag("codex")

	if peerArg != "" {
		cmdUninstallPeer(host, peerArg)
		return
	}

	// --codex mode: only clean up Codex assets, don't touch Claude shim.
	if codex {
		if host != "" {
			cmdUninstallCodexRemote(host)
		} else {
			cmdUninstallCodexLocal()
		}
		return
	}

	var target shim.Target
	switch targetStr {
	case "auto":
		target = shim.TargetAuto
	case "xclip":
		target = shim.TargetXclip
	case "wl-paste":
		target = shim.TargetWlPaste
	default:
		log.Fatalf("unsupported target: %s", targetStr)
	}

	if err := runShimUninstall(target, installPath, host, uninstallOps{
		uninstallLocalShim: shim.Uninstall,
		removeRemotePath:   shim.RemoveRemotePath,
	}); err != nil {
		log.Fatalf("uninstall failed: %v", err)
	}
}

type uninstallOps struct {
	uninstallLocalShim func(shim.Target, string) error
	removeRemotePath   func(string) error
}

func runShimUninstall(target shim.Target, installPath, host string, ops uninstallOps) error {
	if host != "" {
		fmt.Printf("Removing PATH marker from remote %s...\n", host)
		if err := ops.removeRemotePath(host); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove PATH marker: %v\n", err)
		} else {
			fmt.Println("PATH marker removed from remote shell rc file.")
		}
		return nil
	}

	if err := ops.uninstallLocalShim(target, installPath); err != nil {
		return err
	}

	fmt.Println("Shim removed successfully.")
	return nil
}

func cmdUninstallPeer(host, peerArg string) {
	ident, err := peer.LoadOrCreateLocalIdentity()
	if err != nil {
		log.Fatalf("uninstall peer failed: %v", err)
	}

	peerID, managedHost, err := resolveUninstallPeerTarget(host, peerArg, ident)
	if err != nil {
		log.Fatalf("uninstall peer failed: %v", err)
	}

	session, err := shim.NewSSHSession(host)
	if err != nil {
		log.Fatalf("uninstall peer failed: %v", err)
	}
	defer session.Close()

	if err := uninstallPeerRemoteAndConfig(managedHost, func() (*peer.Registration, error) {
		return cleanupAndReleasePeer(session, "~/.local/bin/cc-clip", peerID)
	}); err != nil {
		log.Fatalf("uninstall peer failed: %v", err)
	}
}

func resolveUninstallPeerTarget(host, peerArg string, ident peer.Identity) (string, string, error) {
	if host == "" {
		return "", "", fmt.Errorf("--host is required; managed aliases are no longer created — use --peer <id> --host <host> to clean up the remote peer lease and SSH config")
	}
	if peerArg == ident.ID {
		return peerArg, host, nil
	}
	return peerArg, "", nil
}

func uninstallPeerRemoteAndConfig(managedHost string, remoteCleanup func() (*peer.Registration, error)) error {
	return uninstallPeerRemoteAndConfigWithOps(managedHost, remoteCleanup, uninstallPeerCleanupOps{
		removeManagedHostConfig: setup.RemoveManagedHostConfig,
		readManagedTunnelPorts:  setup.ReadManagedTunnelPorts,
		removePersistentTunnel:  removePersistentTunnel,
	})
}

type uninstallPeerCleanupOps struct {
	removeManagedHostConfig func(string) error
	readManagedTunnelPorts  func(string) (setup.ManagedTunnelPorts, error)
	removePersistentTunnel  func(string, int) error
}

func uninstallPeerRemoteAndConfigWithOps(managedHost string, remoteCleanup func() (*peer.Registration, error), ops uninstallPeerCleanupOps) error {
	reg, remoteErr := remoteCleanup()
	var (
		hostErr           error
		tunnelErr         error
		hostConfigRemoved bool
	)
	if managedHost != "" {
		removeTunnelByHost := false
		removeTunnelPort := 0
		if ops.readManagedTunnelPorts != nil {
			managedPorts, err := ops.readManagedTunnelPorts(managedHost)
			switch {
			case err == nil:
				removeTunnelByHost = true
				if managedPorts.LocalPort > 0 {
					removeTunnelPort = managedPorts.LocalPort
				}
			case errors.Is(err, os.ErrNotExist), errors.Is(err, setup.ErrSSHHostBlockNotFound):
				removeTunnelByHost = true
			case errors.Is(err, setup.ErrManagedRemotePortInvalid):
				removeTunnelByHost = true
			default:
				tunnelErr = fmt.Errorf("read managed tunnel ports for Host %s: %w", managedHost, err)
			}
		}
		if tunnelErr == nil && ops.removePersistentTunnel != nil && removeTunnelByHost {
			tunnelErr = ops.removePersistentTunnel(managedHost, removeTunnelPort)
		}
		if tunnelErr == nil && ops.removeManagedHostConfig != nil {
			hostErr = ops.removeManagedHostConfig(managedHost)
			hostConfigRemoved = hostErr == nil
		}
	}

	if remoteErr == nil && reg != nil {
		fmt.Printf("Released peer %s on remote port %d\n", reg.Label, reg.ReservedPort)
	}
	if managedHost != "" && hostConfigRemoved {
		fmt.Printf("Removed cc-clip SSH config from Host %s in ~/.ssh/config\n", managedHost)
	}

	var errs []error
	if remoteErr != nil {
		errs = append(errs, remoteErr)
	}
	if managedHost != "" && hostErr != nil {
		errs = append(errs, fmt.Errorf("failed to remove Host %s cc-clip config: %w", managedHost, hostErr))
	}
	if managedHost != "" && tunnelErr != nil {
		errs = append(errs, fmt.Errorf("failed to remove persistent tunnel for Host %s: %w", managedHost, tunnelErr))
	}
	return errors.Join(errs...)
}

func removePersistentTunnel(host string, localPort int) error {
	if localPort <= 0 {
		states, err := tunnel.LoadStatesForHost(tunnel.DefaultStateDir(), host)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("load persistent tunnel states: %w", err)
		}
		var errs []error
		for _, state := range states {
			if state == nil || state.Config.LocalPort <= 0 {
				continue
			}
			if err := removePersistentTunnelWith(host, state.Config.LocalPort, postTunnelDown, persistTunnelDownOffline, tunnel.RemoveState); err != nil {
				errs = append(errs, fmt.Errorf("local port %d: %w", state.Config.LocalPort, err))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		return nil
	}
	return removePersistentTunnelWith(host, localPort, postTunnelDown, persistTunnelDownOffline, tunnel.RemoveState)
}

func removePersistentTunnelWith(
	host string,
	localPort int,
	postFn func(int, string) error,
	persistFn func(string, int) error,
	removeStateFn func(string, string, int) error,
) error {
	postErr := postFn(localPort, host)
	if postErr != nil {
		if !isRecoverableTunnelDownError(postErr) && !errors.Is(postErr, errDaemonAuth) {
			switch {
			case errors.Is(postErr, tunnel.ErrTunnelNotFound):
				// Nothing registered with the daemon; continue removing any saved state.
			default:
				return fmt.Errorf("stop persistent tunnel: %w", postErr)
			}
		}
	}

	switch {
	case postErr == nil:
		// Tunnel stop confirmed by the daemon; remove persisted state.
	case errors.Is(postErr, tunnel.ErrTunnelNotFound):
		// Nothing registered with the daemon; continue removing any saved state.
	case isRecoverableTunnelDownError(postErr):
		if persistFn != nil {
			if err := persistFn(host, localPort); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("stop persistent tunnel offline: %w", err)
				}
			}
		}
	default:
		return fmt.Errorf("stop persistent tunnel: %w", postErr)
	}

	removeErr := removeStateFn(tunnel.DefaultStateDir(), host, localPort)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove persistent tunnel state: %w", removeErr)
	}
	return nil
}

// cmdUninstallCodexRemote cleans up Codex support on a remote host via SSH.
func cmdUninstallCodexRemote(host string) {
	fmt.Printf("Uninstalling Codex support from %s...\n", host)

	session, err := shim.NewSSHSession(host)
	if err != nil {
		log.Fatalf("SSH connection failed: %v", err)
	}
	defer session.Close()

	var (
		hasError          bool
		remainingCodexEnv bool
	)
	codexStateDirs := remoteCodexStateDirs(session)

	// Step 1: Stop x11-bridge
	fmt.Println("[1/5] Stopping x11-bridge...")
	for _, codexStateDir := range codexStateDirs {
		stopBridgeRemote(session, codexStateDir)
	}
	fmt.Println("      done")

	// Step 2: Stop Xvfb
	fmt.Println("[2/5] Stopping Xvfb...")
	for _, codexStateDir := range codexStateDirs {
		if err := xvfb.StopRemote(session, codexStateDir); err != nil {
			fmt.Printf("      warning: %v\n", err)
			hasError = true
		}
	}
	fmt.Println("      done")

	// Step 3: Remove codex state directory
	fmt.Println("[3/5] Removing codex state files...")
	for _, codexStateDir := range codexStateDirs {
		session.Exec(fmt.Sprintf("rm -rf %s", shimShellQuote(codexStateDir)))
	}
	remainingCodexEnv = remoteHasRemainingCodexState(session)
	fmt.Println("      done")

	// Step 4: Remove DISPLAY marker
	fmt.Println("[4/5] Removing DISPLAY marker...")
	if remainingCodexEnv {
		fmt.Println("      skipped (other peer Codex runtimes still configured)")
	} else {
		if err := shim.RemoveDisplayMarkerSession(session); err != nil {
			fmt.Printf("      warning: %v\n", err)
			hasError = true
		} else {
			fmt.Println("      done")
		}
	}

	// Step 5: Update deploy state
	fmt.Println("[5/5] Updating deploy state...")
	remoteState, err := shim.ReadRemoteState(session)
	if err != nil {
		fmt.Printf("      warning: could not read deploy state: %v\n", err)
	}
	if remoteState != nil {
		if remainingCodexEnv {
			fmt.Println("      preserved codex block (other peer Codex runtimes still configured)")
		} else {
			remoteState.Codex = nil
			if err := shim.WriteRemoteState(session, remoteState); err != nil {
				fmt.Printf("      warning: could not update deploy state: %v\n", err)
				hasError = true
			} else {
				fmt.Println("      codex block removed from deploy.json")
			}
		}
	} else {
		fmt.Println("      no deploy state found (already clean)")
	}

	fmt.Println()
	if hasError {
		fmt.Println("Codex uninstall completed with warnings. Check issues above.")
		os.Exit(1)
	}
	fmt.Println("Codex support removed successfully.")
}

// cmdUninstallCodexLocal cleans up Codex support on the local machine.
func cmdUninstallCodexLocal() {
	fmt.Println("Uninstalling Codex support (local)...")

	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, ".cache", "cc-clip", "codex")

	// Stop bridge
	fmt.Println("[1/3] Stopping x11-bridge...")
	stopLocalProcess(filepath.Join(stateDir, "bridge.pid"), "cc-clip x11-bridge")

	// Stop Xvfb
	fmt.Println("[2/3] Stopping Xvfb...")
	stopLocalProcess(filepath.Join(stateDir, "xvfb.pid"), "Xvfb")

	// Remove state dir
	fmt.Println("[3/3] Removing state files...")
	os.RemoveAll(stateDir)

	fmt.Println("Codex support removed (local).")
}

type connectOpts struct {
	host      string
	port      int
	force     bool
	tokenOnly bool
	codex     bool
	noNotify  bool
}

func cmdConnect() {
	if len(os.Args) < 3 {
		log.Fatal("usage: cc-clip connect <host> [--port PORT] [--force] [--token-only] [--no-notify]")
	}
	runConnect(connectOpts{
		host:      os.Args[2],
		port:      getPort(),
		force:     hasFlag("force"),
		tokenOnly: hasFlag("token-only"),
		codex:     hasFlag("codex"),
		noNotify:  hasFlag("no-notify"),
	})
}

func runConnect(opts connectOpts) {
	host := opts.host
	localPort := opts.port
	force := opts.force
	tokenOnly := opts.tokenOnly
	remoteBin := "~/.local/bin/cc-clip"

	// Step 1: Check local daemon
	fmt.Printf("[1/8] Checking local daemon on :%d...\n", localPort)
	probeTimeout := envDuration("CC_CLIP_PROBE_TIMEOUT_MS", 500*time.Millisecond)
	if err := tunnel.Probe(fmt.Sprintf("127.0.0.1:%d", localPort), probeTimeout); err != nil {
		log.Fatalf("Local daemon not running. Start it first: cc-clip serve")
	}
	fmt.Println("      daemon running")

	// Read the token that `cc-clip serve` already generated and holds in memory.
	// This is the token the daemon validates against — we must send this exact token to the remote.
	daemonToken, err := token.ReadTokenFile()
	if err != nil {
		log.Fatalf("      cannot read daemon token (is 'cc-clip serve' running?): %v", err)
	}

	ident, err := peer.LoadOrCreateLocalIdentity()
	if err != nil {
		log.Fatalf("      cannot load local peer identity: %v", err)
	}
	fmt.Printf("      peer: %s (%s)\n", ident.Label, ident.ID[:12])

	// Step 2: Start SSH master session (passphrase prompted once here)
	fmt.Printf("[2/8] Establishing SSH session to %s...\n", host)
	session, err := shim.NewSSHSession(host)
	if err != nil {
		log.Fatalf("      failed: %v", err)
	}
	defer session.Close()
	fmt.Println("      SSH master connected")

	// Step 3: Read remote deploy state and detect arch
	fmt.Printf("[3/8] Checking remote state...\n")
	remoteState, err := shim.ReadRemoteState(session)
	if err != nil {
		log.Printf("      warning: could not read remote state: %v", err)
	}
	if remoteState != nil && !force {
		fmt.Printf("      remote state: binary=%s shim=%v\n", remoteState.BinaryVersion, remoteState.ShimInstalled)
	} else if force {
		fmt.Println("      --force: ignoring remote state")
		remoteState = nil
	} else {
		fmt.Println("      no previous deploy state")
	}

	needsUpload := false
	localBin := ""
	if tokenOnly {
		fmt.Println("[4/8] Skipping binary prepare/upload (--token-only)")
		if _, err := session.Exec(fmt.Sprintf("test -x %s", remoteBin)); err != nil {
			log.Fatalf("      remote binary missing; re-run without --token-only to deploy it")
		}
		if err := ensureRemotePeerRegistrySupport(session, remoteBin); err != nil {
			log.Fatalf("      %v", err)
		}
	} else {
		remoteOS, remoteArch, err := shim.DetectRemoteArchViaSession(session)
		if err != nil {
			log.Fatalf("      failed to detect remote arch: %v", err)
		}
		fmt.Printf("      %s/%s\n", remoteOS, remoteArch)

		// Step 4: Prepare and upload binary (skip if hash matches)
		localBin, err = prepareBinaryLocal(host, remoteOS, remoteArch)
		if err != nil {
			log.Fatalf("[4/8] Prepare binary failed: %v", err)
		}

		needsUpload = force || shim.NeedsUpload(localBin, remoteState)
		if !needsUpload {
			// Verify the remote binary actually exists — deploy state can be stale.
			if _, err := session.Exec(fmt.Sprintf("test -x %s", remoteBin)); err != nil {
				fmt.Println("[4/8] Remote binary missing despite cached state, re-uploading")
				needsUpload = true
			}
		}
		if needsUpload {
			fmt.Printf("[4/8] Uploading cc-clip binary...\n")
			// Ensure remote directory exists
			session.Exec("mkdir -p ~/.local/bin")
			if err := shim.UploadBinaryViaSession(session, localBin, remoteBin); err != nil {
				log.Fatalf("      failed: %v", err)
			}
			fmt.Printf("      uploaded to %s\n", remoteBin)
		} else {
			fmt.Println("[4/8] Binary up to date, skipping upload")
		}
	}

	existingReg, existingRegErr := lookupPeerReservation(session, remoteBin, ident.ID)
	if tokenOnly {
		createdReservation := false
		rollbackCreatedReservation := func() error {
			if !createdReservation {
				return nil
			}
			return cleanupCreatedTokenOnlyFallback(host, tokenOnlyFallbackCleanupOps{
				removeManagedHostConfig: setup.RemoveManagedHostConfig,
				releasePeer: func() error {
					_, err := cleanupAndReleasePeer(session, remoteBin, ident.ID)
					return err
				},
			})
		}
		failTokenOnly := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if rollbackErr := rollbackCreatedReservation(); rollbackErr != nil {
				log.Fatalf("      %s; rollback failed: %v", msg, rollbackErr)
			}
			log.Fatalf("      %s", msg)
		}
		if existingRegErr == nil && existingReg == nil {
			// Preserve the pre-refactor semantic that `--token-only` was a
			// "fast refresh" shortcut: if no reservation exists yet (freshly
			// re-provisioned remote, first run), fall back to creating one
			// inline so the caller still succeeds instead of erroring out.
			fmt.Printf("[5/8] No existing peer reservation — creating one (--token-only fallback)...\n")
			newReg, err := shim.ReservePeerViaSession(session, remoteBin, ident.ID, ident.Label, peer.DefaultRangeStart, peer.DefaultRangeEnd)
			if err != nil {
				log.Fatalf("      failed to reserve peer port: %v", err)
			}
			existingReg = &newReg
			createdReservation = true
		}
		fmt.Printf("[5/8] Reusing peer reservation and updating SSH config (--token-only)...\n")
		reg, err := resolveTokenOnlyPeerReservation(existingReg, existingRegErr)
		if err != nil {
			failTokenOnly("%v", err)
		}
		changes, err := ensureManagedHostConfigForReservation(host, localPort, &reg)
		if err != nil {
			failTokenOnly("failed to update Host %s in ~/.ssh/config: %v", host, err)
		}
		for _, c := range changes {
			fmt.Printf("      %s: %s\n", c.Action, c.Detail)
		}
		fmt.Printf("      host: %s\n", host)
		fmt.Printf("      remote port: %d\n", reg.ReservedPort)
		fmt.Printf("      state dir: %s\n", reg.StateDir)
		fmt.Println("[6/8] Skipping shim install (--token-only)")
		fmt.Printf("[7/8] Syncing peer token and session...\n")
		sid, _ := shim.GenerateSessionID()
		if err := syncRemoteTokenAndSession(session, daemonToken, reg.StateDir, sid); err != nil {
			failTokenOnly("failed to write token: %v", err)
		}
		fmt.Println("      token synced from local daemon")
		if sid != "" {
			fmt.Printf("      session ID: %s\n", sid[:16])
		}
		if err := connectVerifyTunnel(session, reg.ReservedPort, host); err != nil {
			failTokenOnly("%v", err)
		}
		return
	}

	fmt.Printf("[5/8] Reserving peer port and updating SSH config...\n")
	hadExistingPeerReservation := false
	if existingRegErr != nil {
		log.Printf("      warning: could not check existing peer reservation: %v", existingRegErr)
	} else if existingReg != nil {
		hadExistingPeerReservation = true
	}
	reg, err := shim.ReservePeerViaSession(session, remoteBin, ident.ID, ident.Label, peer.DefaultRangeStart, peer.DefaultRangeEnd)
	if err != nil {
		log.Fatalf("      failed to reserve peer port: %v", err)
	}
	changes, err := ensureManagedHostConfigForReservation(host, localPort, &reg)
	if err != nil {
		bestEffortReleasePeer(session, remoteBin, ident.ID, !hadExistingPeerReservation)
		log.Fatalf("      failed to update Host %s in ~/.ssh/config: %v", host, err)
	}
	for _, c := range changes {
		fmt.Printf("      %s: %s\n", c.Action, c.Detail)
	}
	fmt.Printf("      host: %s\n", host)
	fmt.Printf("      remote port: %d\n", reg.ReservedPort)
	fmt.Printf("      state dir: %s\n", reg.StateDir)

	// Step 6: Install shim (skip if already installed and not forced)
	needsShim := force || shim.NeedsShimInstall(remoteState)
	if !needsShim {
		// Verify the shim file actually exists — cached state can be stale.
		shimTarget := "xclip"
		if remoteState != nil && remoteState.ShimTarget != "" {
			shimTarget = remoteState.ShimTarget
		}
		checkCmd := fmt.Sprintf("test -f ~/.local/bin/%s && head -1 ~/.local/bin/%s | grep -q cc-clip", shimTarget, shimTarget)
		if _, err := session.Exec(checkCmd); err != nil {
			fmt.Println("      shim missing despite cached state, will reinstall")
			needsShim = true
		}
	}
	var installOut string
	if needsShim {
		fmt.Printf("[6/8] Installing shim...\n")
		installCmd := fmt.Sprintf("%s install --port %d", remoteBin, reg.ReservedPort)
		out, err := session.Exec(installCmd)
		if err != nil {
			// Shim might already exist, try uninstall then install
			session.Exec(fmt.Sprintf("%s uninstall", remoteBin))
			out, err = session.Exec(installCmd)
			if err != nil {
				log.Fatalf("      remote install failed: %s: %v", out, err)
			}
		}
		installOut = out
		fmt.Printf("      %s\n", out)
	} else {
		fmt.Println("[6/8] Shim already installed, skipping")
	}

	// Step 5b: Fix PATH if needed — always re-check, don't trust cached state
	var pathFixed bool
	fixed, pathErr := shim.IsPathFixedSession(session)
	if pathErr != nil {
		log.Printf("      warning: could not check PATH: %v", pathErr)
	} else if !fixed {
		fmt.Printf("      fixing remote PATH...\n")
		if err := shim.FixRemotePathSession(session); err != nil {
			log.Printf("      warning: PATH fix failed: %v", err)
		} else {
			pathFixed = true
			fmt.Println("      PATH marker injected")
		}
	} else {
		pathFixed = true
	}
	// Step 7: Sync token and session ID
	fmt.Printf("[7/8] Syncing peer token and session...\n")
	sessionID, _ := shim.GenerateSessionID()
	if err := syncRemoteTokenAndSession(session, daemonToken, reg.StateDir, sessionID); err != nil {
		log.Fatalf("      failed to write token: %v", err)
	}
	fmt.Println("      token synced from local daemon")
	if sessionID != "" {
		fmt.Printf("      session ID: %s\n", sessionID[:16])
	}

	// Update remote deploy state
	localHash, _ := shim.LocalBinaryHash(localBin)
	// Determine actual shim target from install output or prior state.
	shimTarget := "xclip"
	if needsShim {
		// Parse install output: it prints "Installed shim: <target>"
		if strings.Contains(installOut, "wl-paste") {
			shimTarget = "wl-paste"
		}
	} else if remoteState != nil && remoteState.ShimTarget != "" {
		shimTarget = remoteState.ShimTarget
	}
	newState := &shim.DeployState{
		BinaryHash:    localHash,
		BinaryVersion: version,
		ShimInstalled: true,
		ShimTarget:    shimTarget,
		PathFixed:     pathFixed,
	}
	if remoteState != nil {
		newState.Notify = remoteState.Notify
		newState.Codex = remoteState.Codex
	}
	if err := shim.WriteRemoteState(session, newState); err != nil {
		log.Printf("      warning: could not write remote deploy state: %v", err)
	}

	// Step 8: Verify tunnel
	if err := connectVerifyTunnel(session, reg.ReservedPort, host); err != nil {
		log.Fatalf("%v", err)
	}

	// Notification bridge setup (unless --no-notify)
	if opts.noNotify {
		// Notify assets are host-scoped, so --no-notify must remain a pure skip.
		fmt.Println()
		fmt.Println("Notification bridge setup:")
		fmt.Println("  skipped (--no-notify)")
	} else if notifyState := connectNotifySetup(session, localPort, reg.ReservedPort, reg.StateDir); notifyState != nil {
		newState.Notify = notifyState
		if err := shim.WriteRemoteState(session, newState); err != nil {
			log.Printf("      warning: could not update remote deploy state: %v", err)
		}
	}

	// Steps 8-11: Codex support (only if --codex flag is set)
	if opts.codex {
		codexOk := runConnectCodex(session, reg.ReservedPort, reg.StateDir, needsUpload, opts.force, newState)
		if !codexOk {
			fmt.Println()
			fmt.Println("Claude shim is ready, but Codex support failed.")
			fmt.Println("Fix the issues above and re-run: cc-clip connect", host, "--codex")
			os.Exit(1)
		}
		if err := shim.WriteRemoteState(session, newState); err != nil {
			log.Printf("      warning: could not update remote deploy state: %v", err)
		}
	}
}

// connectNotifySetup performs notification bridge setup:
// 1. Generate nonce and register with local daemon
// 2. Write nonce to remote
// 3. Install hook script on remote
// 4. Print Claude Code hook config
// 5. Detect and configure Codex notify (if ~/.codex exists)
// 6. Run health probe
func connectNotifySetup(session *shim.SSHSession, localPort, remotePort int, stateDir string) *shim.NotifyDeployState {
	fmt.Println()
	fmt.Println("Notification bridge setup:")

	// Step N1: Generate nonce
	fmt.Println("  [N1] Generating notification nonce...")
	notifyNonce, err := shim.GenerateNotificationNonce()
	if err != nil {
		log.Printf("      warning: failed to generate notification nonce: %v", err)
		return nil
	}

	// Register nonce with the local daemon via HTTP
	daemonToken, err := token.ReadTokenFile()
	if err != nil {
		log.Printf("      warning: failed to re-read local daemon token: %v", err)
		return nil
	}
	if err := registerNonceWithDaemon(localPort, daemonToken, notifyNonce); err != nil {
		log.Printf("      warning: failed to register nonce with daemon: %v", err)
		return nil
	}
	fmt.Printf("      nonce: %s...\n", notifyNonce[:16])

	// Step N2: Write nonce to remote
	fmt.Println("  [N2] Writing nonce to remote...")
	if err := syncRemoteNotificationNonce(session, notifyNonce, stateDir); err != nil {
		log.Printf("      warning: failed to write remote nonce: %v", err)
		return nil
	}
	fmt.Println("      nonce synced")

	// Step N3: Install hook script
	fmt.Println("  [N3] Installing hook script...")
	if err := shim.InstallRemoteHookScript(session, remotePort); err != nil {
		log.Printf("      warning: failed to install hook script: %v", err)
		return nil
	}
	fmt.Println("      cc-clip-hook installed to ~/.local/bin/cc-clip-hook")

	hookInstalled := true

	// Step N4: Install claude wrapper (auto-injects hooks via --settings)
	fmt.Println("  [N4] Installing claude wrapper...")
	if err := shim.InstallRemoteClaudeWrapper(session, remotePort); err != nil {
		log.Printf("      warning: failed to install claude wrapper: %v", err)
		fmt.Println("      Falling back to manual hook config:")
		fmt.Println()
		for _, line := range strings.Split(claudeHookConfigJSON(), "\n") {
			fmt.Printf("      %s\n", line)
		}
		fmt.Println()
	} else {
		fmt.Println("      claude wrapper installed to ~/.local/bin/claude")
		fmt.Println("      Hooks will be auto-injected when tunnel is alive")
	}

	// Step N5: Detect and configure Codex notify
	codexInjected := false
	if shim.RemoteHasCodex(session) {
		fmt.Println("  [N5] Codex detected, injecting notify config...")
		if err := shim.EnsureRemoteCodexNotifyConfig(session, remotePort); err != nil {
			log.Printf("      warning: codex config injection failed: %v", err)
		} else {
			codexInjected = true
			fmt.Println("      ~/.codex/config.toml updated")
		}
	} else {
		fmt.Println("  [N5] Codex not detected, skipping config injection")
	}

	// Step N6: Health probe
	fmt.Println("  [N6] Running notification health probe...")
	healthVerified := false
	if err := runNotificationHealthProbe(localPort, notifyNonce); err != nil {
		log.Printf("      warning: health probe failed: %v", err)
	} else {
		healthVerified = true
		fmt.Println("      health probe passed")
	}
	return &shim.NotifyDeployState{
		Enabled:        true,
		HookInstalled:  hookInstalled,
		CodexInjected:  codexInjected,
		HealthVerified: healthVerified,
	}
}

func connectNotifyDisable(session remoteExecutor, stateDir string) error {
	fmt.Println()
	fmt.Println("Notification bridge teardown:")

	steps := []struct {
		label   string
		success string
		fn      func() error
	}{
		{
			label:   "Removing notify nonce files",
			success: "notify state removed",
			fn: func() error {
				return removeRemoteNotifyState(session, stateDir)
			},
		},
		{
			label:   "Removing hook script",
			success: "hook script removed",
			fn: func() error {
				return removeRemoteManagedHookScript(session)
			},
		},
		{
			label:   "Restoring Claude wrapper",
			success: "Claude wrapper restored",
			fn: func() error {
				return restoreRemoteClaudeBinary(session)
			},
		},
		{
			label:   "Removing Codex notify config",
			success: "Codex notify config removed",
			fn: func() error {
				return removeRemoteCodexNotifyConfig(session)
			},
		},
	}

	var errs []error
	for i, step := range steps {
		fmt.Printf("  [N%d] %s...\n", i+1, step.label)
		if err := step.fn(); err != nil {
			fmt.Printf("      warning: %v\n", err)
			errs = append(errs, err)
		} else {
			fmt.Printf("      %s\n", step.success)
		}
	}

	return errors.Join(errs...)
}

// registerNonceWithDaemon sends the notification nonce to the local daemon
// via POST /register-nonce, authenticated with the clipboard bearer token.
func registerNonceWithDaemon(port int, bearerToken, nonce string) error {
	payload := fmt.Sprintf(`{"nonce":%q}`, nonce)
	url := fmt.Sprintf("http://127.0.0.1:%d/register-nonce", port)

	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cc-clip/connect")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("daemon returned status %d", resp.StatusCode)
	}
	return nil
}

// runNotificationHealthProbe sends a test notification to the local daemon
// via /notify and checks for 204. This proves the nonce is registered and
// the notification pipeline works end-to-end.
func runNotificationHealthProbe(port int, nonce string) error {
	payload := `{"title":"cc-clip","body":"Notification bridge connected","urgency":0}`
	url := fmt.Sprintf("http://127.0.0.1:%d/notify", port)

	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+nonce)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cc-clip-hook/0.1")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health probe request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("expected 204, got %d", resp.StatusCode)
	}
	return nil
}

func claudeHookConfigJSON() string {
	return `{
  "hooks": {
    "Notification": [
      {
        "type": "command",
        "command": "cc-clip-hook"
      }
    ],
    "Stop": [
      {
        "type": "command",
        "command": "cc-clip-hook"
      }
    ]
  }
}`
}

func cmdSetup() {
	if len(os.Args) < 3 {
		log.Fatal("usage: cc-clip setup <host> [--port PORT]")
	}
	host := os.Args[2]
	localPort := getPort()

	// Step 1: Dependencies
	fmt.Println("[1/4] Checking local dependencies...")
	if runtime.GOOS == "darwin" {
		if p := setup.CheckPngpaste(); p != "" {
			fmt.Printf("      pngpaste: %s\n", p)
		} else {
			fmt.Println("      pngpaste not found, installing via Homebrew...")
			if err := setup.InstallPngpaste(); err != nil {
				log.Fatalf("      %v", err)
			}
			if p := setup.CheckPngpaste(); p != "" {
				fmt.Printf("      pngpaste: installed (%s)\n", p)
			}
		}
	} else {
		fmt.Println("      skipped (not macOS)")
	}

	// Step 2: Daemon
	fmt.Println("[2/4] Starting local daemon...")
	probeTimeout := envDuration("CC_CLIP_PROBE_TIMEOUT_MS", 500*time.Millisecond)
	if err := tunnel.Probe(fmt.Sprintf("127.0.0.1:%d", localPort), probeTimeout); err == nil {
		fmt.Printf("      daemon already running on :%d\n", localPort)
	} else if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		exePath, err := os.Executable()
		if err != nil {
			log.Fatalf("      cannot determine executable path: %v", err)
		}
		exePath, _ = filepath.EvalSymlinks(exePath)
		if err := service.Install(exePath, localPort); err != nil {
			log.Fatalf("      service install failed: %v", err)
		}
		if runtime.GOOS == "darwin" {
			fmt.Println("      launchd service installed and started")
		} else {
			fmt.Println("      scheduled task installed and started")
		}
		// Wait for daemon to be ready
		time.Sleep(500 * time.Millisecond)
	} else {
		log.Fatal("      daemon not running. Start it first: cc-clip serve")
	}

	// Step 3: Deploy to remote and update the existing SSH host entry.
	// Forward the orchestration flags the user passed to `setup` so that
	// e.g. `cc-clip setup host --force --token-only` redeploys instead of
	// reusing the deploy-state cache. Previously these flags were silently
	// dropped, leaving scripts that added --force puzzled when nothing
	// redeployed.
	fmt.Printf("\n[3/4] Deploying to %s and updating SSH host config...\n", host)
	runConnect(connectOpts{
		host:      host,
		port:      localPort,
		codex:     hasFlag("codex"),
		force:     hasFlag("force"),
		tokenOnly: hasFlag("token-only"),
	})
}

// connectVerifyTunnel verifies the SSH tunnel from the remote side.
func connectVerifyTunnel(session *shim.SSHSession, port int, host string) error {
	remoteBin := "~/.local/bin/cc-clip"

	fmt.Printf("[8/8] Verifying tunnel from remote...\n")
	probeCmd := fmt.Sprintf(
		"bash -c 'echo >/dev/tcp/127.0.0.1/%d' 2>/dev/null && echo 'tunnel:ok' || echo 'tunnel:fail'",
		port)
	probeOut, _ := session.Exec(probeCmd)

	if probeOut == "tunnel:ok" {
		fmt.Println("      tunnel verified (via existing SSH session)")
	} else {
		fmt.Println("      tunnel not detected (this is normal if no interactive SSH session is open)")
		fmt.Println("      The tunnel is provided by your SSH connection, not by 'cc-clip connect'.")
		fmt.Printf("      Open your SSH host to activate it: ssh %s\n", host)
	}

	// Verify remote binary is functional
	shimTestCmd := fmt.Sprintf("%s status 2>&1", remoteBin)
	shimOut, shimErr := session.Exec(shimTestCmd)
	if shimErr != nil {
		fmt.Printf("      WARNING: remote cc-clip status failed: %s\n", shimOut)
		fmt.Println("      The remote binary may be missing or broken.")
		fmt.Println("      Re-run with --force to redeploy: cc-clip connect <base-host> --force")
		if strings.TrimSpace(shimOut) == "" {
			return fmt.Errorf("remote binary verification failed")
		}
		return fmt.Errorf("remote binary verification failed: %s", strings.TrimSpace(shimOut))
	}
	fmt.Printf("      %s\n", shimOut)

	fmt.Println()
	fmt.Printf("Setup complete. Open it with: ssh %s\n", host)
	return nil
}

// prepareBinaryLocal resolves the local binary path without performing remote operations.
// Remote operations (mkdir, etc.) are done by the caller using the SSH session.
func prepareBinaryLocal(host, remoteOS, remoteArch string) (localBin string, err error) {
	// User-specified local binary takes highest priority
	if flagBin := getFlag("local-bin", ""); flagBin != "" {
		if _, err := os.Stat(flagBin); err != nil {
			return "", fmt.Errorf("specified --local-bin not found: %s", flagBin)
		}
		return flagBin, nil
	}

	if remoteOS == runtime.GOOS && remoteArch == runtime.GOARCH {
		// Same arch — use current binary
		localBin, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot find current executable: %w", err)
		}
		return localBin, nil
	}

	// Different arch — try downloading matching release binary from GitHub
	fmt.Printf("      downloading cc-clip %s for %s/%s...\n", version, remoteOS, remoteArch)
	downloaded, dlErr := downloadReleaseBinary(remoteOS, remoteArch)
	if dlErr == nil {
		return downloaded, nil
	}
	fmt.Printf("      download failed: %v\n", dlErr)

	// Fallback: cross-compile (requires source + go toolchain)
	fmt.Printf("      trying cross-compile...\n")
	if _, lookErr := exec.LookPath("go"); lookErr != nil {
		return "", fmt.Errorf(
			"cannot obtain cc-clip for %s/%s:\n"+
				"  - GitHub release download failed: %v\n"+
				"  - Cross-compile unavailable: Go toolchain not found\n"+
				"  Fix: download the correct binary from https://github.com/fancy-potato/cc-clip/releases\n"+
				"       and re-run with: cc-clip connect %s --local-bin /path/to/cc-clip",
			remoteOS, remoteArch, dlErr, host)
	}

	srcDir, err := findSourceDir()
	if err != nil {
		return "", fmt.Errorf(
			"cannot obtain cc-clip for %s/%s:\n"+
				"  - GitHub release download failed: %v\n"+
				"  - Cross-compile unavailable: source directory not found\n"+
				"  Fix: download the correct binary from https://github.com/fancy-potato/cc-clip/releases\n"+
				"       and re-run with: cc-clip connect %s --local-bin /path/to/cc-clip",
			remoteOS, remoteArch, dlErr, host)
	}

	tmpBin := filepath.Join(os.TempDir(), fmt.Sprintf("cc-clip-%s-%s", remoteOS, remoteArch))
	buildCmd := exec.Command("go", "build", "-o", tmpBin, "./cmd/cc-clip/")
	buildCmd.Dir = srcDir
	buildCmd.Env = append(os.Environ(), "GOOS="+remoteOS, "GOARCH="+remoteArch)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-compile failed: %s: %w", string(out), err)
	}
	return tmpBin, nil
}

// releaseVersion extracts the base release version from a git describe string.
// "0.3.0-1-g99b1298" → "0.3.0", "0.3.0" → "0.3.0".
// git describe format: <tag>-<N>-g<hash> where N = commits after tag.
func releaseVersion(ver string) string {
	// Split by "-" and check for the git describe pattern: at least 3 parts
	// where the last part starts with "g" (commit hash) and second-to-last is a number.
	parts := strings.Split(ver, "-")
	if len(parts) >= 3 {
		hash := parts[len(parts)-1]
		count := parts[len(parts)-2]
		if strings.HasPrefix(hash, "g") && isNumeric(count) {
			return strings.Join(parts[:len(parts)-2], "-")
		}
	}
	return ver
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func downloadReleaseBinary(targetOS, targetArch string) (string, error) {
	if version == "dev" {
		return "", fmt.Errorf("running dev build, no release version to download")
	}

	// Strip "v" prefix, then extract base release version from git describe output.
	// e.g. "v0.3.0-1-g99b1298" → "0.3.0-1-g99b1298" → "0.3.0"
	ver := releaseVersion(strings.TrimPrefix(version, "v"))
	archiveName := fmt.Sprintf("cc-clip_%s_%s_%s.tar.gz", ver, targetOS, targetArch)
	url := fmt.Sprintf("https://github.com/fancy-potato/cc-clip/releases/download/v%s/%s", ver, archiveName)

	tmpDir, err := os.MkdirTemp("", "cc-clip-download-*")
	if err != nil {
		return "", err
	}

	archivePath := filepath.Join(tmpDir, archiveName)
	dlCmd := exec.Command("curl", "-fsSL", "--max-time", "30", "-o", archivePath, url)
	if out, err := dlCmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("download failed (%s): %s", url, string(out))
	}

	extractCmd := exec.Command("tar", "-xzf", archivePath, "-C", tmpDir)
	if out, err := extractCmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("extract failed: %s", string(out))
	}

	binPath := filepath.Join(tmpDir, "cc-clip")
	if _, err := os.Stat(binPath); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("binary not found in archive")
	}

	return binPath, nil
}

func findSourceDir() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for i := 0; i < 5; i++ {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir, nil
			}
			dir = filepath.Dir(dir)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
		return cwd, nil
	}

	return "", fmt.Errorf("go.mod not found near executable or cwd")
}

func cmdDoctor() {
	port := getPort()
	host := getFlag("host", "")

	if host == "" {
		fmt.Println("cc-clip doctor (local)")
		fmt.Println()
		results := doctor.RunLocal(port)
		allOK := doctor.PrintResults(results)
		fmt.Println()
		if allOK {
			fmt.Println("All local checks passed.")
		} else {
			fmt.Println("Some checks failed. Fix the issues above.")
			os.Exit(1)
		}
	} else {
		fmt.Printf("cc-clip doctor (end-to-end: %s)\n", host)
		fmt.Println()

		fmt.Println("Local checks:")
		localResults := doctor.RunLocal(port)
		localOK := doctor.PrintResults(localResults)
		fmt.Println()

		fmt.Println("Remote checks:")
		remoteResults := doctor.RunRemote(host, port)
		remoteOK := doctor.PrintResults(remoteResults)
		fmt.Println()

		if localOK && remoteOK {
			fmt.Println("All checks passed. cc-clip is ready.")
		} else {
			fmt.Println("Some checks failed. Fix the issues above.")
			os.Exit(1)
		}
	}
}

func cmdStatus() {
	port := getPort()
	probeTimeout := envDuration("CC_CLIP_PROBE_TIMEOUT_MS", 500*time.Millisecond)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := tunnel.Probe(addr, probeTimeout); err != nil {
		fmt.Printf("daemon:  not running on :%d\n", port)
	} else {
		fmt.Printf("daemon:  running on :%d\n", port)
	}

	tok, err := token.ReadTokenFile()
	if err != nil {
		fmt.Println("token:   not found")
	} else {
		fmt.Printf("token:   present (%d chars)\n", len(tok))
	}

	tokenDir, dirErr := token.TokenDir()
	if dirErr == nil {
		tokenPath := filepath.Join(tokenDir, "session.token")
		if info, statErr := os.Stat(tokenPath); statErr == nil {
			age := time.Since(info.ModTime())
			fmt.Printf("token:   modified %s ago\n", formatStatusDuration(age))
		}
	}

	if runtime.GOOS == "darwin" {
		running, err := service.Status()
		if err == nil {
			if running {
				fmt.Println("launchd: running")
			} else {
				fmt.Println("launchd: not running")
			}
		} else {
			fmt.Println("launchd: not installed")
		}
	} else if runtime.GOOS == "windows" {
		running, err := service.Status()
		if err == nil {
			if running {
				fmt.Println("service: running (task scheduler)")
			} else {
				fmt.Println("service: not running")
			}
		} else {
			fmt.Println("service: not installed")
		}
	}

	if ident, err := peer.LoadOrCreateLocalIdentity(); err == nil {
		fmt.Printf("peer:    %s (%s)\n", ident.Label, ident.ID[:12])
	}

	fmt.Printf("out-dir: %s\n", tunnel.DefaultOutDir())
}

func formatStatusDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

func cmdService() {
	if len(os.Args) < 3 {
		log.Fatal("usage: cc-clip service <install|uninstall|status>")
	}

	subcmd := os.Args[2]
	switch subcmd {
	case "install":
		exePath, err := os.Executable()
		if err != nil {
			log.Fatalf("cannot determine executable path: %v", err)
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			log.Fatalf("cannot resolve executable path: %v", err)
		}
		port := getPort()
		if err := service.Install(exePath, port); err != nil {
			log.Fatalf("service install failed: %v", err)
		}
		if runtime.GOOS == "windows" {
			fmt.Printf("Scheduled task created and running.\n")
			fmt.Printf("  task: %s\n", service.PlistPath())
		} else {
			fmt.Printf("Launchd service installed and loaded.\n")
			fmt.Printf("  plist: %s\n", service.PlistPath())
			fmt.Printf("  logs:  ~/Library/Logs/cc-clip.log\n")
		}

	case "uninstall":
		if err := service.Uninstall(); err != nil {
			log.Fatalf("service uninstall failed: %v", err)
		}
		if runtime.GOOS == "windows" {
			fmt.Println("Scheduled task removed.")
		} else {
			fmt.Println("Launchd service unloaded and removed.")
		}

	case "status":
		running, err := service.Status()
		if err != nil {
			log.Fatalf("service status check failed: %v", err)
		}
		if running {
			if runtime.GOOS == "windows" {
				fmt.Println("service: running (task scheduler)")
			} else {
				fmt.Println("service: running (launchd)")
			}
		} else {
			fmt.Println("service: not running")
		}

	default:
		log.Fatalf("unknown service subcommand: %s (use install, uninstall, or status)", subcmd)
	}
}

func classifyError(err error) int {
	if errors.Is(err, tunnel.ErrTokenInvalid) {
		return exitcode.TokenInvalid
	}
	if errors.Is(err, tunnel.ErrNoImage) {
		return exitcode.NoImage
	}
	return exitcode.DownloadFailed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	env := os.Getenv(key)
	if env == "" {
		return fallback
	}
	ms, err := strconv.Atoi(env)
	if err != nil {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func shimShellQuote(s string) string {
	return shellutil.RemoteShellPath(s)
}

// --- Codex support ---

const (
	legacyStateDir      = "~/.cache/cc-clip"
	legacyCodexStateDir = legacyStateDir + "/codex"
)

func remoteCodexStateDirs(session remoteExecutor) []string {
	stateDirs := []string{legacyCodexStateDir}
	out, err := session.Exec(`find "$HOME/.cache/cc-clip/peers" -mindepth 2 -maxdepth 2 -type d -name codex -print 2>/dev/null`)
	if err != nil {
		return stateDirs
	}
	return appendUniqueStrings(stateDirs, parseRemoteCodexStateDirs(out)...)
}

func remoteHasRemainingCodexState(session *shim.SSHSession) bool {
	out, err := session.Exec(`find "$HOME/.cache/cc-clip" -path '*/codex/display' -print -quit 2>/dev/null`)
	return err == nil && strings.TrimSpace(out) != ""
}

func localPeerRegistration(session *shim.SSHSession, remoteBin string) (*peer.Registration, error) {
	ident, err := peer.LoadOrCreateLocalIdentity()
	if err != nil {
		return nil, err
	}
	return lookupPeerReservation(session, remoteBin, ident.ID)
}

func lookupPeerReservation(session *shim.SSHSession, remoteBin, peerID string) (*peer.Registration, error) {
	out, err := session.Exec(fmt.Sprintf("%s peer show --peer-id %s 2>/dev/null || true", remoteBin, shimShellQuote(peerID)))
	if err != nil {
		return nil, err
	}
	return parsePeerRegistration(out)
}

func resolveTokenOnlyPeerReservation(existingReg *peer.Registration, lookupErr error) (peer.Registration, error) {
	if lookupErr != nil {
		return peer.Registration{}, fmt.Errorf("failed to look up existing peer reservation: %w", lookupErr)
	}
	if existingReg == nil {
		return peer.Registration{}, fmt.Errorf("no existing peer reservation found; re-run without --token-only to create one")
	}
	reg := *existingReg
	if reg.ReservedPort <= 0 || reg.ReservedPort > maxTunnelPort {
		return peer.Registration{}, fmt.Errorf("existing peer reservation is incomplete; re-run without --token-only to recreate it")
	}
	if strings.TrimSpace(reg.StateDir) == "" {
		reg.StateDir = legacyPeerStateDir(reg.PeerID)
	}
	if strings.TrimSpace(reg.StateDir) == "" {
		return peer.Registration{}, fmt.Errorf("existing peer reservation is incomplete; re-run without --token-only to recreate it")
	}
	return reg, nil
}

func legacyPeerStateDir(peerID string) string {
	if strings.TrimSpace(peerID) == "" {
		return ""
	}
	return legacyStateDir + "/peers/" + peerID
}

func ensureManagedHostConfigForReservation(host string, localPort int, reg *peer.Registration) ([]setup.SSHConfigChange, error) {
	if reg == nil {
		return nil, fmt.Errorf("peer reservation is required")
	}
	return setup.EnsureManagedHostConfig(setup.ManagedHostSpec{
		Host:       host,
		RemotePort: reg.ReservedPort,
		LocalPort:  localPort,
	})
}

type tokenOnlyFallbackCleanupOps struct {
	removeManagedHostConfig func(string) error
	releasePeer             func() error
}

func cleanupCreatedTokenOnlyFallback(host string, ops tokenOnlyFallbackCleanupOps) error {
	var errs []error
	if ops.removeManagedHostConfig != nil {
		if err := ops.removeManagedHostConfig(host); err != nil {
			errs = append(errs, fmt.Errorf("remove Host %s cc-clip config: %w", host, err))
		}
	}
	if ops.releasePeer != nil {
		if err := ops.releasePeer(); err != nil {
			errs = append(errs, fmt.Errorf("release peer reservation: %w", err))
		}
	}
	return errors.Join(errs...)
}

func targetRemoteCodexStateDir(reg *peer.Registration) string {
	if reg == nil || strings.TrimSpace(reg.StateDir) == "" {
		return legacyCodexStateDir
	}
	return reg.StateDir + "/codex"
}

func codexCleanupStateDirs(reg *peer.Registration) []string {
	return appendUniqueStrings([]string{legacyCodexStateDir}, targetRemoteCodexStateDir(reg))
}

func compatStateDirs(stateDir string) []string {
	return appendUniqueStrings(nil, stateDir, legacyStateDir)
}

func syncRemoteTokenAndSession(session *shim.SSHSession, tok, stateDir, sessionID string) error {
	for _, dir := range compatStateDirs(stateDir) {
		if err := shim.WriteRemoteTokenViaSession(session, tok, dir); err != nil {
			return err
		}
		if sessionID != "" {
			if err := shim.WriteRemoteSessionID(session, sessionID, dir); err != nil {
				return err
			}
		}
	}
	return nil
}

func syncRemoteNotificationNonce(session *shim.SSHSession, nonce, stateDir string) error {
	for _, dir := range compatStateDirs(stateDir) {
		if err := shim.WriteRemoteNotificationNonce(session, nonce, dir); err != nil {
			return err
		}
	}
	return nil
}

func parsePeerRegistration(out string) (*peer.Registration, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return nil, err
	}
	return &reg, nil
}

func ensureRemotePeerRegistrySupport(session remoteExecutor, remoteBin string) error {
	out, err := session.Exec(fmt.Sprintf("%s peer 2>&1 || true", remoteBin))
	if err != nil {
		return fmt.Errorf("failed to probe remote peer support: %w", err)
	}
	if strings.Contains(out, "usage: cc-clip peer") {
		return nil
	}
	return fmt.Errorf("remote binary predates peer registry support; re-run without --token-only to redeploy it")
}

func parseRemoteCodexStateDirs(out string) []string {
	var stateDirs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		stateDirs = append(stateDirs, line)
	}
	return appendUniqueStrings(nil, stateDirs...)
}

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, value := range dst {
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		dst = append(dst, value)
	}
	return dst
}

type remoteExecutor interface {
	Exec(cmd string) (string, error)
}

func removeRemoteNotifyState(session remoteExecutor, stateDir string) error {
	var errs []error

	for _, dir := range compatStateDirs(stateDir) {
		cmd := fmt.Sprintf("rm -f %s %s",
			shimShellQuote(dir+"/notify.nonce"),
			shimShellQuote(dir+"/notify-health.log"),
		)
		if _, err := session.Exec(cmd); err != nil {
			errs = append(errs, fmt.Errorf("remove notify state from %s: %w", dir, err))
		}
	}

	// Notifications are host-scoped because the hook script, Claude wrapper,
	// and Codex config are shared. Remove any peer-scoped leftovers as well.
	if _, err := session.Exec(`find "$HOME/.cache/cc-clip/peers" -mindepth 2 -maxdepth 2 \( -name 'notify.nonce' -o -name 'notify-health.log' \) -delete 2>/dev/null || true`); err != nil {
		errs = append(errs, fmt.Errorf("remove peer notify state: %w", err))
	}

	return errors.Join(errs...)
}

func removeRemoteManagedHookScript(session remoteExecutor) error {
	out, err := session.Exec("head -5 ~/.local/bin/cc-clip-hook 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("inspect hook script: %w", err)
	}
	if !strings.Contains(out, "cc-clip-hook") {
		return nil
	}
	if _, err := session.Exec(fmt.Sprintf("rm -f %s", shimShellQuote("~/.local/bin/cc-clip-hook"))); err != nil {
		return fmt.Errorf("remove hook script: %w", err)
	}
	return nil
}

func restoreRemoteClaudeBinary(session remoteExecutor) error {
	out, err := session.Exec("head -5 ~/.local/bin/claude 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("inspect Claude wrapper: %w", err)
	}
	if !strings.Contains(out, "cc-clip claude wrapper") {
		return nil
	}

	wrapperPath := shimShellQuote("~/.local/bin/claude")
	backupPath := shimShellQuote("~/.local/bin/claude.cc-clip-bak")
	restoreCmd := fmt.Sprintf("if [ -f %s ]; then mv -f %s %s; else rm -f %s; fi",
		backupPath, backupPath, wrapperPath, wrapperPath)
	if _, err := session.Exec(restoreCmd); err != nil {
		return fmt.Errorf("restore Claude wrapper: %w", err)
	}
	return nil
}

func removeRemoteCodexNotifyConfig(session remoteExecutor) error {
	const (
		markerStart = "# >>> cc-clip notify (do not edit) >>>"
		markerEnd   = "# <<< cc-clip notify (do not edit) <<<"
		configPath  = "~/.codex/config.toml"
	)

	sedCmd := fmt.Sprintf(
		`sed -i.cc-clip-bak '/%s/,/%s/d' %s 2>/dev/null || true; rm -f %s.cc-clip-bak`,
		sedPatternEscape(markerStart), sedPatternEscape(markerEnd), configPath, configPath,
	)
	if _, err := session.Exec(sedCmd); err != nil {
		return fmt.Errorf("remove Codex notify config: %w", err)
	}
	return nil
}

func sedPatternEscape(s string) string {
	replacer := strings.NewReplacer(
		"/", `\/`,
		".", `\.`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"*", `\*`,
		"+", `\+`,
		"?", `\?`,
		"{", `\{`,
		"}", `\}`,
		"^", `\^`,
		"$", `\$`,
	)
	return replacer.Replace(s)
}

func cleanupPeerRemoteState(session remoteExecutor, stateDir string) error {
	codexStateDir := stateDir + "/codex"
	stopBridgeRemote(session, codexStateDir)
	if err := xvfb.StopRemote(session, codexStateDir); err != nil {
		return fmt.Errorf("stop peer xvfb: %w", err)
	}
	if _, err := session.Exec(fmt.Sprintf("rm -rf %s", shimShellQuote(stateDir))); err != nil {
		return fmt.Errorf("remove peer state dir: %w", err)
	}
	return nil
}

func cleanupAndReleasePeer(session *shim.SSHSession, remoteBin, peerID string) (*peer.Registration, error) {
	reg, err := shim.LookupPeerViaSession(session, remoteBin, peerID)
	if err != nil {
		return nil, fmt.Errorf("lookup peer lease: %w", err)
	}
	stateDir := strings.TrimSpace(reg.StateDir)
	if stateDir == "" {
		stateDir = legacyPeerStateDir(peerID)
	}
	if err := cleanupPeerRemoteState(session, stateDir); err != nil {
		return nil, fmt.Errorf("cleanup peer state: %w", err)
	}
	released, err := shim.ReleasePeerViaSession(session, remoteBin, peerID)
	if err != nil {
		return nil, fmt.Errorf("release peer lease: %w", err)
	}
	if strings.TrimSpace(released.StateDir) == "" {
		released.StateDir = stateDir
	}
	return &released, nil
}

func bestEffortReleasePeer(session *shim.SSHSession, remoteBin, peerID string, allowRelease bool) {
	if !allowRelease {
		return
	}
	_, _ = cleanupAndReleasePeer(session, remoteBin, peerID)
}

// runConnectCodex executes steps 8-11 of the Codex deploy flow.
// Returns true on success, false on failure (Claude path is preserved).
func runConnectCodex(session *shim.SSHSession, remotePort int, stateDir string, binaryUploaded bool, force bool, state *shim.DeployState) bool {
	codexStateDir := stateDir + "/codex"

	// Step 8: Codex preflight
	fmt.Println("[8/11] Codex preflight...")
	if err := xvfb.CheckAvailable(session); err != nil {
		fmt.Println("      Xvfb not found, attempting auto-install...")
		if installErr := xvfb.TryInstall(session); installErr != nil {
			fmt.Printf("      auto-install failed: %v\n", installErr)
			fmt.Println("      Install Xvfb manually:")
			fmt.Println("        Debian/Ubuntu: sudo apt install xvfb")
			fmt.Println("        RHEL/Fedora:   sudo dnf install xorg-x11-server-Xvfb")
			return false
		}
		fmt.Println("      Xvfb auto-installed")
	} else {
		fmt.Println("      Xvfb available")
	}
	session.Exec(fmt.Sprintf("mkdir -p %s", shimShellQuote(codexStateDir)))

	// --force: tear down both bridge and Xvfb so they restart fresh.
	// This handles port changes, display drift, and stale state.
	if force {
		fmt.Println("      --force: stopping existing Codex runtime")
		stopBridgeRemote(session, codexStateDir)
		xvfb.StopRemote(session, codexStateDir)
	}

	// Step 9: Start or reuse Xvfb
	fmt.Println("[9/11] Starting Xvfb...")
	xvfbState, err := xvfb.StartRemote(session, codexStateDir)
	if err != nil {
		fmt.Printf("      Xvfb start failed: %v\n", err)
		dumpRemoteLog(session, codexStateDir+"/xvfb.log")
		return false
	}
	fmt.Printf("      Xvfb running on DISPLAY=:%s (PID %d)\n", xvfbState.Display, xvfbState.PID)

	// Step 10: Start or reuse x11-bridge
	fmt.Println("[10/11] Starting x11-bridge...")

	// Restart the bridge whenever its effective runtime configuration changes.
	needsBridgeRestart := binaryUploaded || force || !bridgeConfiguredForPort(session, codexStateDir, remotePort)
	if needsBridgeRestart {
		stopBridgeRemote(session, codexStateDir)
	}

	if !needsBridgeRestart && isBridgeHealthy(session, codexStateDir) {
		fmt.Println("      x11-bridge already running, reusing")
	} else {
		// Stop any existing bridge first.
		stopBridgeRemote(session, codexStateDir)

		if err := startBridgeRemote(session, xvfbState.Display, remotePort, stateDir); err != nil {
			fmt.Printf("      x11-bridge start failed: %v\n", err)
			dumpRemoteLog(session, codexStateDir+"/bridge.log")
			return false
		}
		fmt.Println("      x11-bridge started")
	}

	// Step 11: Inject DISPLAY marker + update state
	fmt.Println("[11/11] Injecting DISPLAY marker...")
	if err := shim.FixDisplaySession(session); err != nil {
		fmt.Printf("      DISPLAY marker injection failed: %v\n", err)
		return false
	}
	fmt.Println("      DISPLAY marker injected")

	if state != nil {
		state.Codex = &shim.CodexDeployState{
			Enabled:      true,
			Mode:         "x11-bridge",
			DisplayFixed: true,
		}
	}

	fmt.Println()
	fmt.Println("Codex support ready. Open a new SSH shell and Ctrl+V will work in Codex CLI.")
	return true
}

// startBridgeRemote starts the x11-bridge daemon on the remote.
func startBridgeRemote(session remoteExecutor, display string, port int, stateDir string) error {
	codexStateDir := stateDir + "/codex"
	startScript := fmt.Sprintf(
		`nohup env DISPLAY=":%s" CC_CLIP_STATE_DIR=%s ~/.local/bin/cc-clip x11-bridge --display ":%s" --port %d > %s 2>&1 < /dev/null &
echo $! > %s
printf '%d\n' > %s
sleep 0.3
kill -0 $(cat %s 2>/dev/null) 2>/dev/null && echo 'bridge:ok' || echo 'bridge:fail'`,
		display, shimShellQuote(stateDir), display, port,
		shimShellQuote(codexStateDir+"/bridge.log"),
		shimShellQuote(codexStateDir+"/bridge.pid"),
		port,
		shimShellQuote(codexStateDir+"/bridge.port"),
		shimShellQuote(codexStateDir+"/bridge.pid"),
	)
	out, err := session.Exec(startScript)
	if err != nil {
		return fmt.Errorf("bridge start command failed: %w", err)
	}
	if strings.Contains(out, "bridge:fail") {
		return fmt.Errorf("bridge process died immediately after start")
	}
	return nil
}

// stopBridgeRemote stops the x11-bridge on the remote (safe: verifies command).
func stopBridgeRemote(session remoteExecutor, codexStateDir string) {
	stopScript := fmt.Sprintf(
		`pid=$(cat %s 2>/dev/null) && \
[ -n "$pid" ] && \
ps -p "$pid" -o args= 2>/dev/null | grep -q 'cc-clip x11-bridge' && \
kill "$pid" 2>/dev/null && \
sleep 0.5 && \
kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null; \
 rm -f %s %s; true`,
		shimShellQuote(codexStateDir+"/bridge.pid"),
		shimShellQuote(codexStateDir+"/bridge.pid"),
		shimShellQuote(codexStateDir+"/bridge.port"),
	)
	session.Exec(stopScript)
}

func bridgeConfiguredForPort(session remoteExecutor, codexStateDir string, port int) bool {
	out, err := session.Exec(fmt.Sprintf("cat %s 2>/dev/null", shimShellQuote(codexStateDir+"/bridge.port")))
	if err != nil {
		return false
	}
	got, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return false
	}
	return got == port
}

// isBridgeHealthy checks if x11-bridge is running on the remote.
// Verifies both PID liveness and command name to avoid false positives
// from stale PID files whose PID was reused by an unrelated process.
func isBridgeHealthy(session remoteExecutor, codexStateDir string) bool {
	checkScript := fmt.Sprintf(
		`pid=$(cat %s 2>/dev/null) && \
[ -n "$pid" ] && \
kill -0 "$pid" 2>/dev/null && \
ps -p "$pid" -o args= 2>/dev/null | grep -q 'cc-clip x11-bridge' && \
echo 'ok' || echo 'no'`,
		shimShellQuote(codexStateDir+"/bridge.pid"),
	)
	out, _ := session.Exec(checkScript)
	return strings.TrimSpace(out) == "ok"
}

// dumpRemoteLog prints the last 20 lines of a remote log file.
func dumpRemoteLog(session *shim.SSHSession, logPath string) {
	out, err := session.Exec(fmt.Sprintf("tail -20 %s 2>/dev/null", shimShellQuote(logPath)))
	if err == nil && out != "" {
		fmt.Println("      --- log ---")
		for _, line := range strings.Split(out, "\n") {
			fmt.Printf("      %s\n", line)
		}
		fmt.Println("      --- end ---")
	}
}

// --- Notify subcommand ---

// cmdNotify sends a generic notification to the local cc-clip daemon.
func cmdNotify() {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	title := fs.String("title", "", "notification title")
	body := fs.String("body", "", "notification body")
	urgency := fs.Int("urgency", 1, "notification urgency (0=low, 1=normal, 2=critical)")
	fromCodex := fs.String("from-codex", "", "Codex notify JSON payload")
	fromCodexStdin := fs.Bool("from-codex-stdin", false, "read Codex notify JSON payload from stdin")
	_ = fs.Parse(os.Args[2:])

	msg := daemon.GenericMessagePayload{
		Title:   *title,
		Body:    *body,
		Urgency: *urgency,
	}

	switch {
	case *fromCodex != "" && *fromCodexStdin:
		log.Fatal("notify failed: --from-codex and --from-codex-stdin are mutually exclusive")
	case *fromCodex != "":
		parsed, err := parseCodexNotifyPayload(*fromCodex)
		if err != nil {
			log.Fatalf("invalid codex notify payload: %v", err)
		}
		msg = parsed
	case *fromCodexStdin:
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("failed to read codex payload from stdin: %v", err)
		}
		parsed, err := parseCodexNotifyPayload(string(payload))
		if err != nil {
			log.Fatalf("invalid codex notify payload: %v", err)
		}
		msg = parsed
	}

	port := getPort()
	if err := postGenericNotification(port, msg); err != nil {
		log.Fatalf("notify failed: %v", err)
	}
}

// parseCodexNotifyPayload extracts a GenericMessagePayload from the Codex
// JSON format. Codex passes {"last-assistant-message": "..."} as its notify
// payload. The extracted message becomes the body with title "Codex".
func parseCodexNotifyPayload(payload string) (daemon.GenericMessagePayload, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return daemon.GenericMessagePayload{}, fmt.Errorf("failed to parse JSON: %w", err)
	}

	lastMsg, _ := raw["last-assistant-message"].(string)

	return daemon.GenericMessagePayload{
		Title:   "Codex",
		Body:    lastMsg,
		Urgency: 1,
	}, nil
}

// postGenericNotification sends a generic notification to the local cc-clip daemon.
// It reads the notification nonce from ~/.cache/cc-clip/notify.nonce for auth.
func postGenericNotification(port int, msg daemon.GenericMessagePayload) error {
	tokenDir, err := token.TokenDir()
	if err != nil {
		return fmt.Errorf("cannot determine token dir: %w", err)
	}

	nonceFile := filepath.Join(tokenDir, "notify.nonce")
	nonceBytes, err := os.ReadFile(nonceFile)
	if err != nil {
		return fmt.Errorf("cannot read nonce file %s: %w", nonceFile, err)
	}
	nonce := strings.TrimSpace(string(nonceBytes))

	payload := struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		Urgency int    `json:"urgency"`
	}{
		Title:   msg.Title,
		Body:    msg.Body,
		Urgency: msg.Urgency,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/notify", port)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+nonce)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cc-clip-notify/0.1")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
	}

	return nil
}

func cmdPeer() {
	if len(os.Args) < 3 {
		log.Fatal("usage: cc-clip peer <reserve|release|show> [flags]")
	}

	subcmd := os.Args[2]
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	peerID := fs.String("peer-id", "", "stable local peer id")
	label := fs.String("label", "", "peer label")
	rangeStart := fs.Int("range-start", peer.DefaultRangeStart, "registry port range start")
	rangeEnd := fs.Int("range-end", peer.DefaultRangeEnd, "registry port range end")
	_ = fs.Parse(os.Args[3:])

	if *peerID == "" {
		log.Fatal("peer failed: --peer-id is required")
	}

	baseDir, err := peer.BaseDir()
	if err != nil {
		log.Fatalf("peer failed: %v", err)
	}

	var reg peer.Registration
	switch subcmd {
	case "reserve":
		if strings.TrimSpace(*label) == "" {
			log.Fatal("peer reserve failed: --label is required")
		}
		reg, err = peer.ReservePort(baseDir, *peerID, *label, *rangeStart, *rangeEnd)
	case "release":
		reg, err = peer.ReleasePort(baseDir, *peerID)
	case "show":
		reg, err = peer.Lookup(baseDir, *peerID)
	default:
		log.Fatalf("unknown peer subcommand: %s", subcmd)
	}
	if err != nil {
		log.Fatalf("peer %s failed: %v", subcmd, err)
	}

	data, err := json.Marshal(reg)
	if err != nil {
		log.Fatalf("peer %s failed: %v", subcmd, err)
	}
	fmt.Println(string(data))
}

// cmdX11Bridge runs the X11 clipboard bridge daemon (internal command).
func cmdX11Bridge() {
	display := getFlag("display", os.Getenv("DISPLAY"))
	port := getPort()

	tokenDir, err := token.TokenDir()
	if err != nil {
		log.Fatalf("x11-bridge: cannot determine token dir: %v", err)
	}
	tokenFile := filepath.Join(tokenDir, "session.token")

	if display == "" {
		log.Fatal("x11-bridge: --display or DISPLAY env required")
	}

	bridge, err := x11bridge.New(display, port, tokenFile)
	if err != nil {
		log.Fatalf("x11-bridge: initialization failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals (SIGINT + SIGTERM on Unix, SIGINT on Windows).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals()...)
	go func() {
		<-sigCh
		log.Printf("x11-bridge: received shutdown signal")
		cancel()
	}()

	if err := bridge.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("x11-bridge: %v", err)
	}
}
