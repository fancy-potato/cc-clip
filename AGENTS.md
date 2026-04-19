# AGENTS.md

This file provides guidance to Codex CLI (and any other agent harness that reads `AGENTS.md`) when working with code in this repository. It is intentionally kept in sync with `CLAUDE.md` — if you edit one, edit both. The cc-clip product description below is audience-neutral; only the framing line above differs.

## What This Project Does

cc-clip bridges your local Mac/Windows clipboard to a remote Linux server over SSH, so `Ctrl+V` image paste works in remote Claude Code and Codex CLI sessions. It uses an xclip/wl-paste shim that transparently intercepts only Claude Code's clipboard calls, an X11 selection owner bridge for Codex CLI which reads the clipboard via X11 directly, an SSH notification bridge that forwards Claude Code hook events (stop, permission prompt, idle) back to the local machine as native notifications, and a persistent tunnel manager that keeps the SSH reverse forward alive with auto-reconnect (managed via CLI or SwiftBar menu bar plugin). The daemon owns the reverse tunnel lifecycle end to end via its own `ssh -N -R` process; it does NOT write `RemoteForward` or any other tunnel-related directive to `~/.ssh/config`. The only thing `cc-clip setup` / `connect` / `connect --token-only` ever write to `~/.ssh/config` is a marker-wrapped `SetEnv` block (one `SetEnv` directive carrying both `CC_CLIP_PORT` and `CC_CLIP_STATE_DIR`) appended inside the user's existing `Host <alias>` entry — see `internal/sshconfig/`. This block enables the multi-laptop-on-shared-Unix-account scenario (each laptop's SSH session pushes its own per-peer port/state dir so the shared remote shims steer to the right token/nonce). Symlinked `~/.ssh/config` is intentionally unsupported: `internal/sshconfig` refuses to rewrite a symlinked path so setup/connect/uninstall cannot replace the symlink with a regular file.

**Breaking change, no migration support for the legacy tunnel block.** This release stops writing `RemoteForward` / `ControlMaster` directives to `~/.ssh/config`. There is intentionally no migration scaffolding: users upgrading from a pre-daemon-tunnel release must delete the old `# >>> cc-clip managed host: … >>>` block from `~/.ssh/config` by hand before the new binary will behave correctly. The `internal/setup` package ships no SSH-config read/write code; contributors MUST NOT add a cleanup shim back. The only permitted `~/.ssh/config` edits live in `internal/sshconfig/` and are scoped strictly to the `# >>> cc-clip SetEnv (do not edit) >>>` marker pair — never touching `Host`/`Match` structure, tunnel directives, or unrelated `SetEnv` lines.

```
Claude Code path:
  Local Mac clipboard → pngpaste → HTTP daemon (127.0.0.1:18339) → daemon-managed SSH reverse tunnel → xclip shim → Claude Code

Codex CLI path (--codex):
  Local Mac clipboard → pngpaste → HTTP daemon (127.0.0.1:18339) → daemon-managed SSH reverse tunnel → x11-bridge → Xvfb CLIPBOARD → arboard → Codex CLI

Notification path:
  Claude Code hook → cc-clip-hook (stdin JSON) → POST /notify via tunnel → classifier → dedup → DeliveryChain → native notification

Persistent tunnel path:
  daemon TunnelManager → ssh -N -R <remotePort>:127.0.0.1:<localPort> <host> → auto-reconnect on failure
  SwiftBar plugin → cc-clip tunnel list --json → daemon GET /tunnels → display status + start/stop actions
```

## Build & Test Commands

```bash
make build                          # Build binary with version from git tags
make test                           # Run all tests (go test ./... -count=1)
make vet                            # Run go vet
go test ./internal/tunnel/ -v -run TestFetchImageRoundTrip  # Single test
make release-local                  # Build bare binaries for all platforms (dist/), local testing only
```

Version is injected via `-X main.version=$(VERSION)` ldflags. The `version` variable in `cmd/cc-clip/main.go` defaults to `"dev"`.

### Release Process

Production releases use **goreleaser** via GitHub Actions (`.github/workflows/release.yml`). Push a version tag to trigger:

```bash
git tag v0.6.0
git push origin v0.6.0
# CI runs: test → contract check → goreleaser → published release with tar.gz + checksums
```

**NEVER release manually with `make release-local` + `gh release create`.** The install script (`scripts/install.sh`) expects goreleaser's naming convention (`cc-clip_{version}_{os}_{arch}.tar.gz`). Bare binaries from `make release-local` use a different naming scheme (`cc-clip-{os}-{arch}`) and will cause install script 404s. `make release-local` is for local cross-compilation testing only.

goreleaser config: `.goreleaser.yaml`. Release is published automatically (not draft).

## Architecture

### Data Flow

1. **daemon** (`internal/daemon/`) — HTTP server on loopback, reads Mac clipboard via `pngpaste`, serves images at `GET /clipboard/type` and `GET /clipboard/image`. Auth via Bearer token + User-Agent whitelist.
2. **tunnel** (`internal/tunnel/`) — Client-side HTTP calls through the SSH-forwarded port. `Probe()` checks TCP connectivity. `Client.FetchImage()` downloads and saves with timestamp+random filename. Also contains the persistent tunnel manager (see below).
3. **shim** (`internal/shim/template.go`) — Bash script templates for xclip and wl-paste. Intercepts two specific invocation patterns Claude Code uses, fetches via curl through tunnel, falls back to real binary on any failure.
4. **connect** (`cmd/cc-clip/main.go:cmdConnect`) — Orchestrates deployment via SSH master session: detect remote arch → incremental binary upload (hash-based skip) → reserve/read the peer's remote port in the remote peer registry → persist a local tunnel-state record (`~/.cache/cc-clip/tunnels/*.json`) pairing that remote port with the current local daemon port (default `18339`, or overridden by `CC_CLIP_PORT` / `--port`) → install shim → sync token → start the daemon-managed persistent tunnel and poll `/tunnels` until `connected` → verify remote binary. Supports `--force`, `--token-only`, `--no-tunnel` flags. `--no-tunnel` skips the auto-start so operators can drive the tunnel themselves (e.g. `cc-clip tunnel up <host>` later).
5. **ssh** (`internal/shim/ssh.go`) — `SSHSession` wraps a ControlMaster SSH connection. Single passphrase prompt; all subsequent `Exec()` and `Upload()` calls reuse the master.
6. **deploy** (`internal/shim/deploy.go`) — `DeployState` tracks binary hash, version, shim status on the remote. JSON file at `~/.cache/cc-clip/deploy-state.json`. `NeedsUpload()` / `NeedsShimInstall()` enable incremental deploys.
7. **pathfix** (`internal/shim/pathfix.go`) — Auto-detects remote shell (bash/zsh/fish) and injects `~/.local/bin` PATH marker into rc file with `# cc-clip-managed` guards.
8. **service** (`internal/service/launchd.go`) — macOS launchd integration: `Install()`, `Uninstall()`, `Status()`. Generates plist for auto-start daemon.
9. **xvfb** (`internal/xvfb/`) — Manages Xvfb virtual X server on remote. `StartRemote()` auto-detects display via `-displayfd`, reuses healthy instances, writes PID/display to `~/.cache/cc-clip/codex/`. `StopRemote()` verifies PID+command before killing.
10. **x11bridge** (`internal/x11bridge/`) — Go X11 selection owner using `github.com/jezek/xgb` (pure Go, no CGo). Claims CLIPBOARD ownership on Xvfb, responds to SelectionRequest events by fetching image data on-demand from the cc-clip HTTP daemon via SSH tunnel. Supports TARGETS negotiation, direct transfer, and INCR protocol for images >256KB.

### Notification Bridge

11. **session** (`internal/session/`) — Ring-buffer session store tracking last 5 image transfers per session ID. `AnalyzeAndRecord()` atomically assigns sequence numbers and detects duplicates by fingerprint. TTL-based cleanup via `RunCleanup()`.
12. **classifier** (`internal/daemon/classifier.go`) — `ClassifyHookPayload()` translates Claude Code hook JSON (notification, stop, etc.) into a unified `NotifyEnvelope`. Maps hook types to urgency levels: `permission_prompt`=2, `idle_prompt`=1, `stop_at_end_of_turn`=0.
13. **envelope** (`internal/daemon/envelope.go`) — Unified notification model. Three kinds: `KindImageTransfer`, `KindToolAttention`, `KindGenericMessage`. Each carries kind-specific payload structs.
14. **dedup** (`internal/daemon/dedup.go`) — Deduplicates notifications by fingerprint within a session using the session store's ring buffer.
15. **deliver** (`internal/daemon/deliver.go`) — `DeliveryChain` tries adapters in priority order (cmux → platform-native). First success stops the chain. `BuildDeliveryChain()` constructs the default chain. Also implements `Notifier` interface for backward compat.
16. **deliver_cmux** (`internal/daemon/deliver_cmux.go`) — Cross-platform tmux `display-message` adapter. Falls through if not in tmux.
17. **notify_darwin** (`internal/daemon/notify_darwin.go`) — macOS-specific: terminal-notifier or osascript fallback.
18. **clipcc wrapper** (`internal/shim/claude_wrapper.go`) — Bash script installed to `~/.local/bin/clipcc` on remote. Auto-injects `--settings` with Stop and Notification hooks when tunnel is alive, then delegates to the official `claude` launcher. Falls through to real claude binary when the tunnel is down.
19. **cc-clip-hook** (`internal/shim/hook_template.go`) — Bash script installed to `~/.local/bin/cc-clip-hook` on remote. Reads hook JSON from stdin, injects hostname, POSTs to `/notify` endpoint with nonce auth. Logs failures to `~/.cache/cc-clip/notify-health.log`. `CC_CLIP_HOST_ALIAS` is passed to the inline Python payload-rewriter via an environment variable (`os.environ.get("CC_CLIP_HOST_ALIAS")`), not interpolated into the Python source — a hostname containing `'` would otherwise escape the string literal and inject Python. `CC_CLIP_STRICT=1` flips the script from fire-and-forget (always exit 0) to a strict mode where any non-2xx HTTP response is printed to stdout and returns exit 1; `cc-clip connect` uses this mode via `runRemoteNotificationHealthProbe` so the end-to-end notification path is validated through the real tunnel, not just the local `/notify` endpoint.

### Persistent Tunnel Manager

20. **tunnel state** (`internal/tunnel/state.go`) — `TunnelConfig` and `TunnelState` types with JSON file persistence. State files live at `~/.cache/cc-clip/tunnels/<sanitized-host>-<localPort>-<hash>.json`, so multiple daemon ports for the same host can coexist while tracking config, status (`connected`/`connecting`/`disconnected`/`stopped`), PID, error, and reconnect count.
21. **tunnel manager** (`internal/tunnel/manager.go`) — `Manager` manages multiple persistent SSH tunnel processes. `Up(cfg)` starts a tunnel, `Down(host, localPort)` stops it, `List()` merges live and on-disk state. Each tunnel runs `ssh -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R <remotePort>:127.0.0.1:<localPort> <host>` with auto-reconnect on failure (exponential backoff 2s→60s, reset only on confirmed connect). `-v` is load-bearing: `watchTunnelReady` scans ssh's stderr for the "remote forward success" line to know when the reverse forward is actually bound, and promotes the state from `connecting` to `connected` only then. On daemon startup, `LoadAndStartAll()` reads saved configs and restarts enabled tunnels (killing stale PIDs first; adopted tunnels persist `connecting` until the first inspect poll confirms the PID still matches). On daemon shutdown (`SIGINT`/`SIGTERM`), `Shutdown()` sends `SIGTERM` to all tunnel SSH processes, then awaits every reconnect goroutine under a single shared deadline (`shutdownBudget`). The shared deadline is deliberate: per-entry timers would stack to `N * budget` worst-case when N entries are all wedged, so the manager instead fails fast for every entry past the common deadline and logs a `leaking …` line for operators to investigate.
22. **tunnel HTTP endpoints** (`cmd/cc-clip/tunnel_handler.go`) — `GET /tunnels` (list), `POST /tunnels/up` (start), `POST /tunnels/down` (stop, keeps state file), `POST /tunnels/remove` (stop and delete state file). Registered on the daemon's mux via `registerTunnelRoutes()`. All routes require a separate local-only tunnel control token, not the remote-synced clipboard bearer token. Host values are validated against `tunnel.ValidateSSHHost` at the handler so invalid aliases never reach `exec.Command("ssh", ...)`; bodies are capped at 4 KiB via `MaxBytesReader` with a 413 response on overflow.
23. **tunnel CLI** (`cmd/cc-clip/tunnel_cmd.go`) — `cc-clip tunnel list [--json]`, `cc-clip tunnel up <host> [--remote-port N]`, `cc-clip tunnel down <host>`, `cc-clip tunnel remove <host>`. Talks to daemon via HTTP. All three mutating subcommands route to the daemon selected by `--port` / `CC_CLIP_PORT` (default 18339); there is no `--local-port` flag because, by construction, a persistent tunnel's `local_port` equals the owning daemon's HTTP port. `tunnel up` auto-detects the remote port from the saved tunnel state file that `cc-clip connect` wrote for this host when `--remote-port` is omitted; an explicit `--remote-port` bypasses only the saved remote-port lookup, while the CLI still adopts the saved daemon port whenever ownership is unambiguous. If multiple daemon ports are saved for the host, operators must still pass `--port` explicitly. The allocation source of truth remains the remote peer registry; the locally-saved tunnel state is just the cached mapping for CLI convenience. `tunnel remove` stops the tunnel and deletes its state file (unlike `tunnel down`, which keeps state with `enabled=false`); if the daemon is unreachable it falls through to delete the on-disk state file directly.
24. **SwiftBar plugin** (`scripts/cc-clip-tunnels.30s.sh`) — macOS menu bar plugin showing tunnel status for all hosts. Refreshes every 30 seconds. Shows connected/total count in menu bar, per-host status with start/stop actions in dropdown, and sends each action to the tunnel's recorded local daemon port. Requires `cc-clip` and `jq` in PATH.

### Key Design Decisions

- **Shim is a bash script, not a binary** — installed to `~/.local/bin/` with PATH priority over `/usr/bin/xclip`. Uses `which -a` to find the real binary, skipping its own directory.
- **Clipboard session token and tunnel-control token are separate** — `cc-clip serve` persists the remote-synced clipboard session token in `session.token`. Tunnel management also uses a separate local-only `tunnel-control.token` for `GET /tunnels` / `POST /tunnels/*`; that token never leaves the local machine.
- **Binary-safe image transfer** in shim — `_cc_clip_fetch_binary()` uses `mktemp` + `curl -o tmpfile` + `cat tmpfile`, not shell variables (which strip NUL bytes) or `exec curl` (which prevents fallback). After curl succeeds, `[ ! -s "$tmpfile" ]` guards against empty responses (e.g., HTTP 204), returning exit code 10 to trigger fallback instead of outputting empty data.
- **Server-side empty guard** — `handleClipboardImage` checks `len(data) == 0` after `ImageBytes()` and returns 204, preventing 200 with empty body even if the clipboard reader returns empty data without error.
- **Exit codes are segmented** (`internal/exitcode/`) — 0 success, 10-13 business errors (no image, tunnel down, bad token, download failed), 20+ internal. Business codes trigger transparent fallback in the shim.
- **Platform clipboard** — `clipboard_darwin.go` (pngpaste), `clipboard_linux.go` (xclip/wl-paste), `clipboard_windows.go` (PowerShell). Windows uses SCP upload workflow with system tray icon and global hotkey (`Ctrl+Alt+V`).
- **Codex uses X11, not shims** — Codex CLI uses `arboard` (Rust crate) which accesses X11 CLIPBOARD directly in-process. Cannot be shimmed. Solution: Xvfb + Go X11 selection owner that claims CLIPBOARD and serves images on-demand.
- **On-demand fetch in x11-bridge** — No polling or caching. Image data is fetched from the cc-clip daemon only when a SelectionRequest arrives. Always fresh.
- **Token per-request in x11-bridge** — Token is read from file on every HTTP request, enabling `--token-only` rotation without restarting the bridge.
- **DISPLAY injection is file-driven** — The DISPLAY marker block in shell rc reads from `~/.cache/cc-clip/codex/display` at shell startup, not a hardcoded value. This supports `-displayfd` dynamic allocation.
- **Notification uses nonce auth, not session token** — `/notify` endpoint authenticates with a separate nonce (stored at `~/.cache/cc-clip/notify.nonce`), not the clipboard Bearer token. This allows independent rotation.
- **Claude wrapper is conditional** — Only injects `--settings` hooks when the cc-clip tunnel is reachable (health check). When the tunnel is down, passes through transparently so Claude Code still works normally.
- **DeliveryChain fallthrough** — Notification adapters are tried in priority order (cmux → platform-native). First success stops the chain. If all fail, the last error is returned but the hook script always exits 0 (non-blocking).
- **Hook script is fire-and-forget** — `cc-clip-hook` always exits 0 to avoid blocking Claude Code. Failures are logged to a health file, not propagated.
- **Persistent tunnel is daemon-managed** — The tunnel manager lives in the daemon process (already launchd-managed with `KeepAlive`). No extra process supervision needed. On daemon restart, saved tunnel configs are automatically re-established.
- **`cc-clip tunnel up` is the canonical SSH-config refresh** — the `/tunnels/up` HTTP handler always constructs a fresh `TunnelConfig` with `SSHConfigResolved=false` so `Manager.Up` → `resolveSSHTunnelConfig` re-runs `ssh -G <host>` and rewrites the cached options in the state file. The reconnect loop and `LoadAndStartAll` adoption path intentionally do NOT re-resolve — they consume the cached options from state — so that a post-`tunnel up` edit to `~/.ssh/config` (yours or a malicious one) cannot silently change what `ssh` argv the daemon spawns on the next network flap or daemon restart. Users who edit `~/.ssh/config` must re-run `cc-clip tunnel up <host>` to pick up the change. Future contributors MUST NOT "optimize" the handler to reuse `state.Config` (that would defeat both the user-refresh contract and the cache-pin security invariant). Pinned by `TestResolveSSHTunnelConfigReQueriesWhenResolvedFalse` and `TestResolveSSHTunnelConfigSkipsQueryWhenResolvedTrue`.
- **cc-clip does NOT write tunnel directives to `~/.ssh/config`** — the daemon owns the reverse tunnel directly and does not rely on the user's interactive `ssh` session to establish it. `cc-clip connect` / `setup` write a local tunnel-state record under `~/.cache/cc-clip/tunnels/` and never emit `RemoteForward`, `ControlMaster`, or `ControlPath` into any ssh config. The single, narrow exception is the `SetEnv` marker block written by `internal/sshconfig/Apply` for the multi-laptop-on-shared-account use case: `setup`, `connect`, and `connect --token-only` append or refresh `# >>> cc-clip SetEnv (do not edit) >>>` … `# <<< cc-clip SetEnv (do not edit) <<<` inside the user's existing literal `Host <alias>` block, using one `SetEnv` directive that carries both `CC_CLIP_PORT` and `CC_CLIP_STATE_DIR`. The self-release path `uninstall --host <host> --peer` removes that exact marker pair after the remote PATH cleanup succeeds; plain `uninstall --host <host>` preserves it because that command only removes the remote PATH marker. On remote-cleanup failure the local block is preserved. The `internal/setup` package still contains no SSH-config read/write code — only pngpaste detection helpers (`deps.go`); SSH-config editing deliberately lives outside that package to keep the `internal/setup/package_contents_test.go` anti-feature test enforceable. Users upgrading from a pre-daemon-tunnel release with a leftover `# >>> cc-clip managed host: … >>>` block (NB: different marker text from the new SetEnv block) must delete it by hand; there is no migration cleanup path and adding one is out of scope.
- **`internal/sshconfig` refuses wildcard `Host` blocks, symlinked configs, and Include-only resolution** — `Apply(host, env)` matches only literal alias tokens in `Host` directives. If the only candidate that would apply to the alias is a wildcard pattern (`Host *`, `Host *.example.com`, `Host foo?`), `Apply` returns `ErrOnlyGlobMatch` and `cmdConnect` / `cmdSetup` surface a user-facing warning. `Match host …` blocks are always skipped. Separately, `Apply` / `Remove` return `ErrSymlinkConfig` when `~/.ssh/config` is a symlink, so cc-clip never replaces the symlink with a regular file during an atomic rewrite. When no literal `Host <alias>` block exists in the top-level `~/.ssh/config` but the file contains one or more `Include` directives, `Apply` returns `ErrHostBlockInInclude` instead of `ErrHostBlockMissing`: cc-clip intentionally does NOT walk includes (walking them would let a path-traversal exploit in an included file rewrite an unrelated one), so users whose `Host <alias>` lives in an included fragment must either inline the Host block into the top-level file or disable the Include. These three refusals together keep per-peer `CC_CLIP_*` from leaking to every host that happens to match a glob, preserve symlinked dotfile layouts by failing closed, and prevent silent no-ops when includes hide the real Host block. Contributors adding new ssh-config interactions MUST preserve these invariants.
- **Tunnel SSH uses BatchMode** — `BatchMode=yes` prevents interactive prompts (password, host key confirmation). SSH keys must be in ssh-agent or passwordless. This is required since the tunnel SSH process runs without a terminal.
- **Tunnel control uses a separate local-only token** — `GET /tunnels`, `POST /tunnels/up`, `POST /tunnels/down`, and `POST /tunnels/remove` all require `X-CC-Clip-Tunnel-Token`, which is generated locally and never synced to remote peers. The CLI helper `newDaemonTunnelJSONRequest()` reads it from disk. The daemon-side auth middleware also re-reads the token from disk on every request (mirroring the x11-bridge session-token reload pattern), so any out-of-band rotation — a future rotation endpoint, a restart with `--rotate-tunnel-token`, or a manual file replace — is picked up by the very next request without restarting the daemon.
- **Tunnel handler lives in cmd, not daemon package** — To avoid an import cycle (`daemon` → `tunnel` → `daemon` via `fetch.go`), tunnel HTTP handlers are in `cmd/cc-clip/tunnel_handler.go` and registered on the daemon's mux via `Server.Mux()`.
- **Uninstall is multi-peer safe (no cross-laptop collateral)** — When multiple laptops share the same remote Unix account, each gets its own `peer_id` with per-peer state under `~/.cache/cc-clip/peers/<id>/`. One laptop's uninstall MUST NOT disable another laptop's clipboard or notification path. The contract has three parts. (1) `cc-clip uninstall --host H --peer` releases this laptop's peer, scrubs ONLY `~/.cache/cc-clip/peers/<own-id>/`, then queries the remote peer registry via `cc-clip peer list` (`shim.ListPeersViaSession` → `countRemoteActivePeers`); shared assets — `~/.local/bin/clipcc`, `~/.local/bin/cc-clip-hook`, the PATH marker block in `~/.bashrc`/`~/.zshrc`, and the `# >>> cc-clip notify >>>` block in `~/.codex/config.toml` — are deleted ONLY when the registry reports zero remaining peers. (2) `cc-clip uninstall --host H` (without `--peer`) consults the same registry before stripping the PATH marker and preserves it if other peers remain. (3) If the peer-count query fails (ssh flake, corrupt registry), both paths fail safe by PRESERVING the shared assets — operators can rerun once the registry is healthy, but a misread must never destroy another laptop's working setup. Pinned by `TestUninstallPeerRemoteAndConfigPreservesSharedAssetsWhenOtherPeersRemain`, `TestRunShimUninstallWithHostPreservesPATHWhenOtherPeersRemain`, and `…FailsSafeOnCountQueryError`. `removeRemoteNotifyState` also no longer sweeps `~/.cache/cc-clip/peers/*` for stray `notify.nonce` files — it touches only the caller's own `stateDir`. Future contributors MUST NOT add a cross-peer sweep back (pinned in `TestConnectNotifyDisableRemovesManagedAssets`).

### Token Lifecycle

`token.Manager` holds the clipboard session token in memory. `LoadOrGenerate(ttl)` reuses an unexpired token from disk, or generates a new token. The clipboard token file at `~/.cache/cc-clip/session.token` (chmod 600) stores `token\nexpires_at_rfc3339`; `ReadTokenFileWithExpiry()` returns both token and expiry. Tunnel control also persists a separate local-only opaque token at `~/.cache/cc-clip/tunnel-control.token` (chmod 600, same permissions story as the session token — created by the daemon, never written to a remote, rotatable independently via `cc-clip serve --rotate-tunnel-token`) for `/tunnels` management. `token.TokenDirOverride` exists for test isolation — tests set it to `t.TempDir()` to avoid polluting the real cache directory. `--rotate-token` flag forces a new clipboard session token ignoring the existing one.

### Test Patterns

- `internal/daemon/server_test.go` uses a mock `ClipboardReader` — no real clipboard access needed.
- `internal/tunnel/fetch_test.go` uses `newIPv4TestServer(t, handler)` which forces IPv4 binding and calls `t.Skipf` (not panic) if binding fails in restricted environments.
- `internal/shim/install_test.go` uses temp directories to test shim installation without touching real PATH.
- `internal/xvfb/xvfb_test.go` uses `requireXvfb` skip guard — integration tests skip on macOS (no Xvfb available).
- `internal/x11bridge/bridge_test.go` uses `requireXvfbAndXclip` skip guard — E2E smoke test runs mock HTTP + Xvfb + x11-bridge + xclip roundtrip.
- `internal/daemon/classifier_test.go` — Tests hook JSON classification into envelopes for each hook type (notification, stop, unknown).
- `internal/daemon/dedup_test.go` — Tests duplicate detection in the ring buffer across sessions.
- `internal/daemon/deliver_test.go` — Tests DeliveryChain fallthrough behavior with mock adapters.
- `internal/shim/claude_wrapper_test.go` — Validates wrapper script port substitution.
- `internal/shim/hook_template_test.go` — Validates hook script port substitution.
- `internal/session/session_test.go` — Tests ring-buffer wrap-around and TTL cleanup.
- `internal/tunnel/state_test.go` — Tests state serialization, load/save roundtrip, `LoadAllStates` with mixed files, `SanitizeHost` edge cases.
- `internal/tunnel/manager_test.go` — Tests `Up`/`Down`/`List`/`Remove`/`LoadAndStartAll`. SSH to fake hosts exits immediately, verifying reconnect loop and state transitions without real SSH connectivity.

### Shim Interception Patterns

The shim only intercepts these exact Claude Code invocations:
- xclip: `*"-selection clipboard"*"-t TARGETS"*"-o"*` and `*"-selection clipboard"*"-t image/"*"-o"*`
- wl-paste: `*"--list-types"*` and `*"--type"*"image/"*`

Everything else passes through to the real binary via `exec`.

## Cross-Architecture Binary Delivery

When `connect` detects a different remote arch (e.g., Mac arm64 → Linux amd64), it tries in order:
1. Download matching binary from GitHub Releases (needs non-`dev` version)
2. Cross-compile locally (needs Go toolchain + source)
3. Fail with actionable `--local-bin` instruction

## Known Pitfalls

- **Daemon must be running for the clipboard to work**: cc-clip no longer writes a `RemoteForward` to `~/.ssh/config`, so opening an interactive `ssh <host>` session does NOT establish the reverse tunnel on its own. The local daemon (`cc-clip serve`, launchd-managed on macOS with `KeepAlive=true`) owns the tunnel. If the daemon is down, the shim on the remote side cannot fetch images and falls through to the real `xclip`/`wl-paste` (i.e. empty clipboard from Claude Code's perspective).
- **Legacy `~/.ssh/config` block during upgrade (manual fix required)**: users upgrading from a pre-daemon-tunnel release still have a `# >>> cc-clip managed host: … >>>` block with `RemoteForward` / `ControlMaster no` / `ControlPath none`. OpenSSH honors that leftover on new interactive sessions and prints `Warning: remote port forwarding failed for listen port <port>` because the daemon already owns the forward. cc-clip does NOT auto-clean the block; the user must open `~/.ssh/config` and delete the marker range themselves. This is an explicit product decision: the migration surface is a one-time manual step, not code we carry forward.
- **Token rotation on daemon restart**: Mitigated by token persistence — `LoadOrGenerate` reuses unexpired tokens. Use `cc-clip connect <host> --token-only` if only the token changed.
- **Empty image race condition**: The clipboard can change between the TARGETS check (returns "image") and the image fetch (returns 204 No Content). `curl -sf` treats 204 as success → shim outputs empty bytes → Claude Code API rejects empty base64. Guarded by `[ ! -s "$tmpfile" ]` check in `_cc_clip_fetch_binary()`.
- **Remote xclip must exist**: The shim hardcodes the real xclip path at install time. If xclip is not installed on the remote, the shim fallback fails with "No such file or directory".
- **`~/.local/bin` PATH priority**: The shim only works if `~/.local/bin` comes before `/usr/bin` in PATH. Non-interactive SSH commands may not source `.bashrc`, so the `connect` command's `which xclip` check can show the wrong result. Interactive shells (where Claude Code runs) typically source `.bashrc` correctly.
- **Xvfb display collision**: `-displayfd` avoids hardcoded `:99` collisions. If `Xvfb` is not installed on the remote, `connect --codex` fails at step 8 (preflight) with an actionable error.
- **x11-bridge survives SSH session exit**: Launched with `nohup ... < /dev/null &`. PID file at `~/.cache/cc-clip/codex/bridge.pid`. Next `connect --codex` reuses if healthy, restarts if binary was updated.
- **DISPLAY marker vs PATH marker**: Independent lifecycles. `uninstall --codex` removes DISPLAY marker only. `uninstall` (without `--codex`) removes PATH marker only. They use separate `# cc-clip-managed` guard blocks.
- **Persistent tunnel requires passwordless SSH**: The tunnel SSH process uses `BatchMode=yes` (no terminal). If ssh-agent is not running or the key has a passphrase without agent, the tunnel will fail to connect and retry indefinitely. Fix: add the key to ssh-agent (`ssh-add`).
- **Daemon restart kills tunnel SSH processes**: On clean daemon shutdown (`SIGINT`/`SIGTERM`), all tunnel SSH processes receive `SIGTERM`. On daemon restart (launchd `KeepAlive`), `LoadAndStartAll()` kills stale PIDs and spawns fresh SSH processes. Brief tunnel interruption (~2 seconds) is expected during daemon restarts.
- **SwiftBar plugin requires jq**: The SwiftBar plugin (`scripts/cc-clip-tunnels.30s.sh`) parses JSON output from `cc-clip tunnel list --json` using `jq`. Install with `brew install jq`.
- **Shared-account uninstall preserves shared assets while other peers remain**: on a remote host where several laptops share a Unix account, `cc-clip uninstall --host H` / `--host H --peer` checks the remote peer registry (`cc-clip peer list`) and leaves `~/.local/bin/clipcc`, `~/.local/bin/cc-clip-hook`, the PATH marker, and the Codex `# >>> cc-clip notify >>>` block in place when any other peer is still registered. A "Preserving shared remote assets: N other peer(s) still registered" line is printed so operators can see this happened. To force full cleanup, release every peer first (each laptop runs `cc-clip uninstall --host H --peer`) and the last one will delete the shared assets. If the registry query itself fails (ssh down, corrupt registry), the shared assets are also preserved on purpose — fix the registry and rerun rather than assuming the uninstall was a no-op.

## Files That Need Coordinated Changes

- Adding a new API endpoint: `daemon/server.go` (handler) + `tunnel/fetch.go` (client method) + `shim/template.go` (bash interception pattern)
- Changing token format: `token/token.go` + `shim/connect.go:WriteRemoteToken` + shim templates (`_cc_clip_read_token`)
- Adding a new exit code: `exitcode/exitcode.go` + `cmd/cc-clip/main.go:classifyError` + shim templates (return codes)
- Changing Codex deploy flow: `cmd/cc-clip/main.go:runConnectCodex` + `xvfb/xvfb.go` + `x11bridge/bridge.go` + `shim/pathfix.go` (DISPLAY marker)
- Adding a new notification kind: `daemon/envelope.go` (NotifyKind + payload struct) + `daemon/classifier.go` (hook→envelope mapping) + `daemon/deliver.go` (formatNotification display text)
- Changing hook injection: `shim/claude_wrapper.go` (wrapper template) + `shim/hook_template.go` (hook script) + `shim/connect.go` (deploy steps)
- Adding a notification adapter: implement `Deliverer` interface + register in `daemon/deliver.go:BuildDeliveryChain()`
- Changing release asset format: `.goreleaser.yaml` (archive naming/format) + `scripts/install.sh` (download URL + extraction logic) — these MUST stay in sync
- Changing tunnel manager behavior: `tunnel/manager.go` (lifecycle/reconnect) + `tunnel/state.go` (persisted fields) + `cmd/cc-clip/tunnel_handler.go` (HTTP API) + `cmd/cc-clip/tunnel_cmd.go` (CLI) + `scripts/cc-clip-tunnels.30s.sh` (SwiftBar display)
- Changing tunnel HTTP API: `cmd/cc-clip/tunnel_handler.go` (endpoints) + `cmd/cc-clip/tunnel_cmd.go` (client calls) — request/response shapes must stay in sync
- SSH-config interaction: tunnel-related directives are **none** — the `internal/setup` package deliberately contains no `~/.ssh/config` read/write code, and the daemon owns the tunnel lifecycle via `ssh -N -R` argv it builds itself. The single permitted edit is the per-peer `SetEnv` marker block managed by `internal/sshconfig` (called from `cmdConnect` / `cmdSetup` success path and `cmdUninstall` / `cmdUninstallPeer` self-release path). Adding any other ssh_config edit requires extending `internal/sshconfig` (with tests pinning the marker-block contract) — do NOT inline new ssh_config writers in `cmd/cc-clip/`, `internal/setup/`, or anywhere else.
- Adding a safety-critical tunnel ssh option (one that MUST win over the user's `~/.ssh/config`): add the `-o <opt>=<val>` to `tunnel/ssh_args.go` argv assembly AND add the option key to `excludedTunnelSSHOptions` so it is stripped from `ssh -G <host>` output before re-injection. OpenSSH honors the first `-o` for each key; the exclusion is what prevents a user-supplied ssh config from shadowing the safety option.
