# cc-clip

**Clipboard over SSH for Claude Code** — paste images from your local Mac into remote Claude Code sessions, as if it were local.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev)
[![Release](https://img.shields.io/github/v/release/ShunmeiCho/cc-clip)](https://github.com/ShunmeiCho/cc-clip/releases)

## Why

When running Claude Code on a remote server via SSH, `Ctrl+V` image paste doesn't work. The remote `xclip` reads the server's clipboard, not your local Mac's. No screenshots, no diagrams, no visual context — you're stuck with text-only.

cc-clip fixes this with a transparent bridge:

```
Local Mac clipboard  →  HTTP daemon  →  SSH tunnel  →  xclip shim  →  Claude Code
```

No changes to Claude Code. No terminal-specific hacks. Works with any terminal emulator.

## Quick Start

**1. Install (Mac):**

```bash
curl -fsSL https://raw.githubusercontent.com/ShunmeiCho/cc-clip/main/scripts/install.sh | sh
```

**2. Setup:**

```bash
cc-clip setup myserver
```

This single command:
- Installs `pngpaste` via Homebrew (if missing)
- Configures SSH `RemoteForward` in `~/.ssh/config`
- Starts the local daemon via launchd (auto-restarts on reboot)
- Deploys binary + shim to remote, syncs auth token, verifies tunnel

**3. Done.** `Ctrl+V` in remote Claude Code now pastes images from your Mac.

> **Already set up?** Use `cc-clip connect myserver` for subsequent deploys (incremental, skips unchanged components).

## How It Works

```
                    ┌─────────────┐
                    │  Mac (local) │
                    │              │
                    │  pngpaste ──►│ cc-clip daemon (127.0.0.1:18339)
                    └──────┬───────┘
                           │ SSH RemoteForward
                    ┌──────▼───────┐
                    │ Linux (remote)│
                    │              │
  Claude Code ◄──── xclip shim ◄──┤ 127.0.0.1:18339 (tunneled)
                    └──────────────┘
```

1. **Local daemon** (`cc-clip serve`) — Reads Mac clipboard via `pngpaste`, serves images over HTTP on loopback
2. **SSH tunnel** (`RemoteForward`) — Forwards the daemon port to the remote server
3. **xclip shim** — A bash script at `~/.local/bin/xclip` that intercepts only the clipboard calls Claude Code makes, fetches image data through the tunnel, and passes everything else to the real `xclip`

## Security

| Layer | Protection |
|-------|-----------|
| Network | Daemon listens on loopback only (`127.0.0.1`) — never exposed to the network |
| Auth | Session-scoped Bearer token with sliding expiration (default 30 days) |
| Token delivery | Transmitted via stdin, never in command-line arguments or environment |
| Transparency | All non-image clipboard calls pass through to real `xclip` unchanged |

### Token Lifecycle

The daemon generates a token on first start and persists it to `~/.cache/cc-clip/session.token`. The token has a **30-day TTL with sliding expiration** — every successful request automatically extends the expiry, so active users never encounter token expiry.

**If you do hit token expiry** (e.g., after 30+ days of inactivity):

```bash
# Quick fix: re-sync token only (no binary re-upload)
cc-clip connect myserver --token-only

# Or diagnose first
cc-clip doctor --host myserver
```

Symptoms of expired token: shim debug logs show `"token expired"` or `"401"`, and image paste silently falls back to the remote clipboard.

## Commands

| Command | Description |
|---------|-------------|
| `cc-clip setup <host>` | Full setup: deps, SSH config, daemon, deploy |
| `cc-clip connect <host>` | Deploy to remote (incremental) |
| `cc-clip connect <host> --token-only` | Sync token only (fast) |
| `cc-clip connect <host> --force` | Full redeploy ignoring cache |
| `cc-clip serve` | Start daemon in foreground |
| `cc-clip serve --rotate-token` | Start daemon with forced new token |
| `cc-clip service install` | Install macOS launchd service |
| `cc-clip service uninstall` | Remove launchd service |
| `cc-clip service status` | Show service status |
| `cc-clip doctor` | Local health check |
| `cc-clip doctor --host <host>` | End-to-end health check |
| `cc-clip status` | Show component status |
| `cc-clip uninstall` | Remove xclip shim from remote |

## Configuration

All settings have sensible defaults. Override via environment variables:

| Setting | Default | Env Var |
|---------|---------|---------|
| Port | 18339 | `CC_CLIP_PORT` |
| Token TTL | 30d | `CC_CLIP_TOKEN_TTL` |
| Output dir | `$XDG_RUNTIME_DIR/claude-images` | `CC_CLIP_OUT_DIR` |
| Probe timeout | 500ms | `CC_CLIP_PROBE_TIMEOUT_MS` |
| Fetch timeout | 5000ms | `CC_CLIP_FETCH_TIMEOUT_MS` |
| Debug logs | off | `CC_CLIP_DEBUG=1` |

## Requirements

**Local (Mac):**
- macOS 13+
- `pngpaste` — auto-installed by `cc-clip setup`

**Remote (Linux):**
- `xclip` (the shim wraps it; the real binary must exist)
- `curl`, `bash`
- `~/.local/bin` in `PATH` — auto-configured by `cc-clip connect`
- SSH access with `RemoteForward` capability

## Platform Support

| Local | Remote | Status |
|-------|--------|--------|
| macOS (Apple Silicon) | Linux (amd64) | Stable |
| macOS (Intel) | Linux (arm64) | Stable |

## Troubleshooting

### Quick Diagnostics

```bash
cc-clip doctor --host myserver
```

### Step-by-Step Verification

If image paste isn't working, run these checks **in order**:

```bash
# 1. Local: Is the daemon running?
curl -s http://127.0.0.1:18339/health
# Expected: {"status":"ok"}

# 2. Remote: Is the tunnel forwarding?
ssh myserver "curl -s http://127.0.0.1:18339/health"
# Expected: {"status":"ok"}

# 3. Remote: Is the shim taking priority?
ssh myserver "which xclip"
# Expected: ~/.local/bin/xclip  (NOT /usr/bin/xclip)

# 4. Remote: Does the shim intercept correctly?
ssh myserver 'CC_CLIP_DEBUG=1 xclip -selection clipboard -t TARGETS -o'
# Expected: image/png
```

### Common Issues

<details>
<summary><b>SSH ControlMaster breaks RemoteForward</b></summary>

**Symptom:** Tunnel verified during `connect`, but `curl http://127.0.0.1:18339/health` hangs in your SSH session.

**Cause:** `ControlMaster auto` reuses the first (master) connection which doesn't have `RemoteForward`.

**Fix:** `cc-clip setup` automatically adds `ControlMaster no` for your host. If you configured SSH manually:

```
# ~/.ssh/config
Host myserver
    RemoteForward 18339 127.0.0.1:18339
    ControlMaster no
    ControlPath none
```
</details>

<details>
<summary><b>Stale sshd process blocks RemoteForward</b></summary>

**Symptom:** `Warning: remote port forwarding failed for listen port 18339`

**Cause:** A previous SSH session left a stale `sshd` child process holding the port.

**Fix:**

```bash
# On remote: find and kill the stale process
sudo ss -tlnp | grep 18339
sudo kill <PID>
```

**Prevention:** Update to latest cc-clip — `connect` now uses `ClearAllForwardings=yes`.
</details>

<details>
<summary><b>Token expired or invalid</b></summary>

**Symptom:** Shim debug logs show `"token expired"` or `"401"`. Image paste silently falls back.

**Cause:** Token TTL (30 days) expired due to prolonged inactivity, or daemon restarted and generated a new token.

**Fix:**

```bash
cc-clip connect myserver --token-only
```

The token uses sliding expiration — it auto-renews on every successful request. You'll only hit this after 30+ days of zero usage.
</details>

<details>
<summary><b>Launchd daemon returns "empty" for image clipboard</b></summary>

**Symptom:** Daemon running via launchd, but `/clipboard/type` returns `{"type":"empty"}`.

**Cause:** macOS launchd doesn't source shell profile — `pngpaste` not in PATH.

**Fix:** Reinstall the service (regenerates plist with correct PATH):

```bash
cc-clip service uninstall && cc-clip service install
```
</details>

<details>
<summary><b>Empty image data (API Error 400)</b></summary>

**Symptom:** Claude Code returns `API Error: 400 - image cannot be empty`.

**Cause:** Race condition where clipboard changes between TARGETS check and image fetch.

**Fix:** Update to latest cc-clip, then:

```bash
cc-clip connect myserver   # redeploy shim with fix
```

In Claude Code, run `/clear` to reset the corrupted conversation.
</details>

<details>
<summary><b>~/.local/bin not in PATH</b></summary>

**Symptom:** `which xclip` points to `/usr/bin/xclip` instead of `~/.local/bin/xclip`.

**Fix:** `cc-clip connect` auto-configures PATH. If it didn't work, add manually:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
```
</details>

<details>
<summary><b>No image in clipboard</b></summary>

Copy an image on your Mac first:
- **Screenshot:** `Cmd + Shift + Ctrl + 4` (area) or `Cmd + Shift + Ctrl + 3` (full screen)
- **From app:** Right-click image → Copy Image
</details>

## Contributing

Contributions welcome. Please open an issue first for major changes.

```bash
git clone https://github.com/ShunmeiCho/cc-clip.git
cd cc-clip
make build
make test
```

## Related

- [anthropics/claude-code#5277](https://github.com/anthropics/claude-code/issues/5277) — Image paste in SSH sessions
- [anthropics/claude-code#29204](https://github.com/anthropics/claude-code/issues/29204) — xclip/wl-paste dependency
- [ghostty-org/ghostty#10517](https://github.com/ghostty-org/ghostty/discussions/10517) — SSH image paste discussion

## License

[MIT](LICENSE)
