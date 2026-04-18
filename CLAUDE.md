# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Does

cc-clip bridges your local Mac/Windows clipboard to a remote Linux server over SSH, so `Ctrl+V` image paste works in remote Claude Code and Codex CLI sessions. It uses an xclip/wl-paste shim that transparently intercepts only Claude Code's clipboard calls, an X11 selection owner bridge for Codex CLI which reads the clipboard via X11 directly, an SSH notification bridge that forwards Claude Code hook events (stop, permission prompt, idle) back to the local machine as native notifications, and a persistent tunnel manager that keeps SSH port forwarding alive with auto-reconnect (managed via CLI or SwiftBar menu bar plugin).

```
Claude Code path:
  Local Mac clipboard → pngpaste → HTTP daemon (127.0.0.1:18339) → SSH RemoteForward → xclip shim → Claude Code

Codex CLI path (--codex):
  Local Mac clipboard → pngpaste → HTTP daemon (127.0.0.1:18339) → SSH RemoteForward → x11-bridge → Xvfb CLIPBOARD → arboard → Codex CLI

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
4. **connect** (`cmd/cc-clip/main.go:cmdConnect`) — Orchestrates deployment via SSH master session: detect remote arch → incremental binary upload (hash-based skip) → reserve/read the peer's remote port in the remote peer registry → write that port into the local managed `RemoteForward` block, targeting the current local daemon port (default `18339`, or overridden by `CC_CLIP_PORT` / `--port`) → install shim → sync token → verify tunnel. Supports `--force`, `--token-only` flags. Persistent tunnels are managed separately via `cc-clip tunnel up`.
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
19. **cc-clip-hook** (`internal/shim/hook_template.go`) — Bash script installed to `~/.local/bin/cc-clip-hook` on remote. Reads hook JSON from stdin, injects hostname, POSTs to `/notify` endpoint with nonce auth. Logs failures to `~/.cache/cc-clip/notify-health.log`.

### Persistent Tunnel Manager

20. **tunnel state** (`internal/tunnel/state.go`) — `TunnelConfig` and `TunnelState` types with JSON file persistence. State files live at `~/.cache/cc-clip/tunnels/<sanitized-host>-<localPort>-<hash>.json`, so multiple daemon ports for the same host can coexist while tracking config, status (`connected`/`connecting`/`disconnected`/`stopped`), PID, error, and reconnect count.
21. **tunnel manager** (`internal/tunnel/manager.go`) — `Manager` manages multiple persistent SSH tunnel processes. `Up(cfg)` starts a tunnel, `Down(host, localPort)` stops it, `List()` merges live and on-disk state. Each tunnel runs `ssh -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R <remotePort>:127.0.0.1:<localPort> <host>` with auto-reconnect on failure (exponential backoff 2s→60s, reset only on confirmed connect). `-v` is load-bearing: `watchTunnelReady` scans ssh's stderr for the "remote forward success" line to know when the reverse forward is actually bound, and promotes the state from `connecting` to `connected` only then. On daemon startup, `LoadAndStartAll()` reads saved configs and restarts enabled tunnels (killing stale PIDs first; adopted tunnels persist `connecting` until the first inspect poll confirms the PID still matches). On daemon shutdown (`SIGINT`/`SIGTERM`), `Shutdown()` sends `SIGTERM` to all tunnel SSH processes, then awaits every reconnect goroutine under a single shared deadline (`shutdownBudget`). The shared deadline is deliberate: per-entry timers would stack to `N * budget` worst-case when N entries are all wedged, so the manager instead fails fast for every entry past the common deadline and logs a `leaking …` line for operators to investigate.
22. **tunnel HTTP endpoints** (`cmd/cc-clip/tunnel_handler.go`) — `GET /tunnels` (list), `POST /tunnels/up` (start), `POST /tunnels/down` (stop, keeps state file), `POST /tunnels/remove` (stop and delete state file). Registered on the daemon's mux via `registerTunnelRoutes()`. All routes require a separate local-only tunnel control token, not the remote-synced clipboard bearer token. Host values are validated against `tunnel.ValidateSSHHost` at the handler so invalid aliases never reach `exec.Command("ssh", ...)`; bodies are capped at 4 KiB via `MaxBytesReader` with a 413 response on overflow.
23. **tunnel CLI** (`cmd/cc-clip/tunnel_cmd.go`) — `cc-clip tunnel list [--json]`, `cc-clip tunnel up <host> [--remote-port N]`, `cc-clip tunnel down <host>`, `cc-clip tunnel remove <host>`. Talks to daemon via HTTP. All three mutating subcommands route to the daemon selected by `--port` / `CC_CLIP_PORT` (default 18339); there is no `--local-port` flag because, by construction, a persistent tunnel's `local_port` equals the owning daemon's HTTP port. `tunnel up` auto-detects the remote port from the local `~/.ssh/config` managed block via `setup.ReadManagedTunnelPorts()`, or falls back to existing tunnel state. If `--port` was not set and the managed block (or the only saved state for the host) points at a different daemon port, the CLI adopts that port so the tunnel is sent to the daemon that actually owns it. The allocation source of truth remains the remote peer registry; runtime tunnel startup uses the locally synced managed block instead of SSHing back to the remote. `tunnel remove` stops the tunnel and deletes its state file (unlike `tunnel down`, which keeps state with `enabled=false`); if the daemon is unreachable it falls through to delete the on-disk state file directly.
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
- **Persistent tunnel coexists with SSH config RemoteForward** — The managed SSH config block retains its `RemoteForward` as a fallback. When the persistent tunnel is up, the user's interactive SSH session's RemoteForward fails to bind (port already in use) with a warning — this is harmless. When the persistent tunnel is down, the interactive session's RemoteForward takes over.
- **Tunnel SSH uses BatchMode** — `BatchMode=yes` prevents interactive prompts (password, host key confirmation). SSH keys must be in ssh-agent or passwordless. This is required since the tunnel SSH process runs without a terminal.
- **Tunnel control uses a separate local-only token** — `GET /tunnels`, `POST /tunnels/up`, `POST /tunnels/down`, and `POST /tunnels/remove` all require `X-CC-Clip-Tunnel-Token`, which is generated locally and never synced to remote peers. The CLI helper `newDaemonTunnelJSONRequest()` reads it from disk. The daemon-side auth middleware also re-reads the token from disk on every request (mirroring the x11-bridge session-token reload pattern), so any out-of-band rotation — a future rotation endpoint, a restart with `--rotate-tunnel-token`, or a manual file replace — is picked up by the very next request without restarting the daemon.
- **Tunnel handler lives in cmd, not daemon package** — To avoid an import cycle (`daemon` → `tunnel` → `daemon` via `fetch.go`), tunnel HTTP handlers are in `cmd/cc-clip/tunnel_handler.go` and registered on the daemon's mux via `Server.Mux()`.
- **Shared SSH host stanzas are unsupported by design** — `cc-clip` manages only a dedicated exact `Host <alias>` block. Shared multi-pattern stanzas such as `Host prod staging` must be split into separate exact aliases; treat this as a documented product boundary, not an implementation bug to “fix”.
- **Wildcard SSH stanzas are a documented manual-boundary** — `cc-clip` updates only the exact `Host <alias>` block. It does not rewrite or reorder earlier `Host *` / `Host *.corp` stanzas. If an earlier wildcard stanza sets `RemoteForward`, `ControlMaster`, or `ControlPath`, the right guidance is to move the exact alias block above that wildcard stanza or remove the conflicting wildcard directive.
- **`~/.ssh/config` rewrites still assume a single-user path you control** — `cc-clip` reads `~/.ssh/config`, rewrites the managed block, and `os.Rename`s a tmp file over the resolved target. Dotfiles-managed symlinks are supported and preserved, but there is still no advisory file lock on the read/parse/write cycle. Document this for users: do not edit `~/.ssh/config` from another editor while `cc-clip setup`/`connect`/`uninstall --host` is running, and do not use cc-clip on a shared/multi-user config path. `cc-clip tunnel up` only reads the managed block to discover ports, and `cc-clip doctor --host` is read-only. The product boundary is "single user, sole owner of the resolved ssh-config target, no concurrent editors" — not "arbitrary hardening".
- **Line endings are normalized by majority vote** — `splitSSHConfigLines` picks CRLF if the CRLF count strictly exceeds LF, otherwise LF. A hand-edited file with balanced LF/CRLF counts will be rewritten as LF. Document this as "cc-clip picks a single dominant ending"; it is not a guarantee of byte-identical round-trip on mixed-ending inputs. Treat any bug report about "cc-clip reformatted my line endings" as an expected product behavior to document, not a regression to fix.
- **Quoted `Host` tokens are preserved only structurally** — `splitSSHHostTokens` strips surrounding single/double quotes when parsing, and the managed block is rewritten with unquoted `Host <alias>`. Users who wrote `Host "myserver"` will see the quotes removed after the first cc-clip rewrite. This is the intended product behavior (ssh does not require quoted tokens for the patterns we manage) and should be noted in docs, not patched to preserve quoting.
- **`# >>> cc-clip managed host: … >>>` markers are load-bearing comment strings** — parsing treats an exact `TrimSpace == startMarker` line as the start of a managed range, even if that line sits outside any real managed block (e.g., a copy-pasted doc snippet). Do not paste the literal marker into `~/.ssh/config` as free-form commentary; treat the marker strings as reserved.
- **`Include` directives are tolerated, but only the main file is managed** — `cc-clip` reads and rewrites `~/.ssh/config` itself. If an exact `Host` block lives only behind an `Include`, `cc-clip` will not edit that included file for you. Keep the managed alias block in the main config when you want automatic updates; treat included files as manual territory.
- **Windows reparse-point edge case is documented, not handled** — `writeSSHConfigFile`'s `os.Lstat` check refuses true symlinks on all platforms, but Windows-specific reparse points that `Lstat` does not report with `ModeSymlink` (e.g. some junctions, mount points) are not categorically rejected. Real-world impact is limited, but Windows users who rely on junctions/reparse points for their `~/.ssh/config` should keep a backup before running cc-clip and should not place the file under a reparse point that redirects across volumes.

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

- **SSH ControlMaster + RemoteForward**: If the user has `ControlMaster auto` globally, a pre-existing master connection without `RemoteForward` will be reused. The tunnel silently fails. Fix: set `ControlMaster no` and `ControlPath none` on hosts that need `RemoteForward`.
- **Shared SSH host stanza (`Host prod staging`) is unsupported**: `setup`, `connect`, and tunnel port auto-detection require a dedicated exact alias block. If the user's SSH config groups aliases in one stanza, the correct guidance is to split them into separate `Host` entries, not to extend cc-clip to manage the shared stanza. `doctor --host` stays read-only and may still report through `ssh -G`.
- **Earlier wildcard SSH stanzas can override the managed host**: `setup` only edits the exact alias block. If an earlier `Host *` or `Host *.corp` stanza sets `RemoteForward`, `ControlMaster`, or `ControlPath`, OpenSSH can still apply the wildcard value first. The correct fix is documentation/user guidance: keep the exact alias block above the conflicting wildcard stanza, or remove the conflicting directive from the wildcard stanza.
- **Token rotation on daemon restart**: Mitigated by token persistence — `LoadOrGenerate` reuses unexpired tokens. Use `cc-clip connect <host> --token-only` if only the token changed.
- **Empty image race condition**: The clipboard can change between the TARGETS check (returns "image") and the image fetch (returns 204 No Content). `curl -sf` treats 204 as success → shim outputs empty bytes → Claude Code API rejects empty base64. Guarded by `[ ! -s "$tmpfile" ]` check in `_cc_clip_fetch_binary()`.
- **Remote xclip must exist**: The shim hardcodes the real xclip path at install time. If xclip is not installed on the remote, the shim fallback fails with "No such file or directory".
- **`~/.local/bin` PATH priority**: The shim only works if `~/.local/bin` comes before `/usr/bin` in PATH. Non-interactive SSH commands may not source `.bashrc`, so the `connect` command's `which xclip` check can show the wrong result. Interactive shells (where Claude Code runs) typically source `.bashrc` correctly.
- **Xvfb display collision**: `-displayfd` avoids hardcoded `:99` collisions. If `Xvfb` is not installed on the remote, `connect --codex` fails at step 8 (preflight) with an actionable error.
- **x11-bridge survives SSH session exit**: Launched with `nohup ... < /dev/null &`. PID file at `~/.cache/cc-clip/codex/bridge.pid`. Next `connect --codex` reuses if healthy, restarts if binary was updated.
- **DISPLAY marker vs PATH marker**: Independent lifecycles. `uninstall --codex` removes DISPLAY marker only. `uninstall` (without `--codex`) removes PATH marker only. They use separate `# cc-clip-managed` guard blocks.
- **Persistent tunnel + interactive SSH RemoteForward conflict**: When the persistent tunnel is up, user SSH sessions that also have `RemoteForward` (from managed SSH config) will see `Warning: remote port forwarding failed for listen port XXXX`. This is harmless — the persistent tunnel already provides the forward. The warning disappears if the persistent tunnel is down (interactive SSH takes over).
- **Persistent tunnel requires passwordless SSH**: The tunnel SSH process uses `BatchMode=yes` (no terminal). If ssh-agent is not running or the key has a passphrase without agent, the tunnel will fail to connect and retry indefinitely. Fix: add the key to ssh-agent (`ssh-add`).
- **Daemon restart kills tunnel SSH processes**: On clean daemon shutdown (`SIGINT`/`SIGTERM`), all tunnel SSH processes receive `SIGTERM`. On daemon restart (launchd `KeepAlive`), `LoadAndStartAll()` kills stale PIDs and spawns fresh SSH processes. Brief tunnel interruption (~2 seconds) is expected during daemon restarts.
- **SwiftBar plugin requires jq**: The SwiftBar plugin (`scripts/cc-clip-tunnels.30s.sh`) parses JSON output from `cc-clip tunnel list --json` using `jq`. Install with `brew install jq`.
- **`~/.ssh/config` edits during config-writing cc-clip commands can be lost**: `setup`, `connect`, and `uninstall --host` do a read → parse → tmp-write → `os.Rename` cycle without an advisory lock. A `vim ~/.ssh/config` save that lands between our read and rename is clobbered silently. Guidance (docs, not code): users must not edit `~/.ssh/config` while a config-writing cc-clip command is running. `tunnel up` only reads the managed block, and `doctor` only checks/reports. If the user reports "my edit disappeared", verify timing against the `cc-clip` invocation and recommend the `.cc-clip-backup` sidecar — do not retrofit file locking unless product scope changes.
- **Mixed / tied line endings are normalized, not preserved byte-for-byte**: The file rewrite picks CRLF if CRLF count strictly exceeds LF count, otherwise LF. A manually edited file with balanced endings will come out LF. Same applies to user-added `Host "alias"` quoting — the rewrite drops quotes. Treat these as documented boundaries; bug reports should be redirected to docs.
- **SSH config writes follow the resolved target path**: `writeSSHConfigFile` rewrites the file that `~/.ssh/config` resolves to, preserving the symlink itself when dotfiles-managed setups point at a real config elsewhere. The atomic rename is still single-user scoped; a racing attacker who can swap path components during the rewrite can still interfere, so this remains outside cc-clip's threat model.
- **`Include` directives are tolerated, but only the main file is managed**: cc-clip reads and rewrites `~/.ssh/config` itself. If an exact `Host` block lives behind an `Include`, cc-clip will not edit that included file for you; keep the managed alias block in the main config when you want automatic updates.

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
- Changing SSH config managed block format: `setup/sshconfig.go` (write + `ReadManagedTunnelPorts` read) + `cmd/cc-clip/tunnel_cmd.go` (auto-detect ports via `ReadManagedTunnelPorts`)
- Adding a safety-critical tunnel ssh option (one that MUST win over the user's `~/.ssh/config`): add the `-o <opt>=<val>` to `tunnel/ssh_args.go` argv assembly AND add the option key to `excludedTunnelSSHOptions` so it is stripped from `ssh -G <host>` output before re-injection. OpenSSH honors the first `-o` for each key; the exclusion is what prevents a user-supplied ssh config from shadowing the safety option.
