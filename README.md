<p align="center">
  <img src="docs/logo.png" alt="cc-clip logo" width="200">
</p>
<h1 align="center">cc-clip</h1>
<p align="center">
  <b>Paste images &amp; receive notifications across SSH — remote Claude Code &amp; Codex CLI feel local.</b>
</p>
<p align="center">
  <a href="https://github.com/fancy-potato/cc-clip/releases"><img src="https://img.shields.io/github/v/release/fancy-potato/cc-clip?color=D97706" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License: MIT"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.25+-00ADD8.svg" alt="Go"></a>
  <a href="https://github.com/fancy-potato/cc-clip/stargazers"><img src="https://img.shields.io/github/stars/fancy-potato/cc-clip?style=social" alt="Stars"></a>
</p>

<p align="center">
  <img src="docs/marketing/demo-quick.gif" alt="cc-clip demo" width="720">
  <br>
  <em>Install → setup → paste. Clipboard works over SSH.</em>
</p>

---

<details>
<summary><b>Table of Contents</b></summary>

- [The Problem](#the-problem)
- [The Solution](#the-solution)
- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Why cc-clip?](#why-cc-clip)
- [How It Works](#how-it-works)
- [SSH Notifications](#ssh-notifications)
- [Security](#security)
- [Daily Usage](#daily-usage)
- [Persistent Tunnels & SwiftBar](#persistent-tunnels--swiftbar)
- [Commands](#commands)
- [Configuration](#configuration)
- [Platform Support](#platform-support)
- [Requirements](#requirements)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [Related](#related)
- [License](#license)

</details>

---

## The Problem

When running Claude Code or Codex CLI on a remote server via SSH, **image paste often doesn't work** and **notifications don't reach you**. The remote clipboard is empty — no screenshots, no diagrams. And when Claude finishes a task or needs approval, you have no idea unless you're staring at the terminal.

## The Solution

```text
Image paste:
  Claude Code (macOS):   Mac clipboard     → cc-clip daemon → SSH tunnel → xclip shim      → Claude Code
  Claude Code (Windows): Windows clipboard → cc-clip hotkey → SSH/SCP    → remote file path → Claude Code
  Codex CLI:             Mac clipboard     → cc-clip daemon → SSH tunnel → x11-bridge/Xvfb → Codex CLI

Notifications:
  Claude Code hook → cc-clip-hook → SSH tunnel → local daemon → macOS/cmux notification
  Codex notify     → cc-clip notify             → SSH tunnel → local daemon → macOS/cmux notification
```

One tool. No changes to Claude Code or Codex. Clipboard and notifications both work over SSH.

## Prerequisites

- **Local machine:** macOS 13+ or Windows 10/11
- **Remote server:** Linux (amd64 or arm64) accessible via SSH
- **SSH config:** An SSH alias that resolves via `ssh -G <alias>` to a real hostname. A `Host <alias>` entry in `~/.ssh/config` is the safest shape. A wildcard stanza like `Host *` can work, but only if `ssh -G <alias>` expands `HostName` to an actual DNS name or IP — if `HostName` is just the alias string passed through verbatim, `cc-clip tunnel up <alias>` will cache that useless value and every reconnect will fail.

If you don't have an SSH config entry yet, add one:

```
# ~/.ssh/config
Host myserver
    HostName 10.0.0.1       # your server's IP or domain
    User your-username
    IdentityFile ~/.ssh/id_rsa  # optional, if using key auth
```

Use a stable alias such as `myserver` for the cc-clip commands (`cc-clip setup`, `cc-clip connect`, `cc-clip tunnel up`). cc-clip does **not** modify `~/.ssh/config`; it only needs the alias to resolve via `ssh -G` and to know how to reach the remote. The reverse tunnel used for clipboard and notifications is owned by the local cc-clip daemon (`cc-clip serve`, launchd-managed on macOS), not by your interactive SSH session.

If you are on Windows and want the SSH/Claude Code workflow, use the dedicated guide:

- [Windows Quick Start](docs/windows-quickstart.md)

## Quick Start

### Step 1: Install cc-clip

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/fancy-potato/cc-clip/main/scripts/install.sh | sh
```

Windows:

Follow the dedicated guide:

- [Windows Quick Start](docs/windows-quickstart.md)

On macOS / Linux, add `~/.local/bin` to your PATH if prompted:

```bash
# Add to your shell profile (~/.zshrc or ~/.bashrc)
export PATH="$HOME/.local/bin:$PATH"

# Reload your shell
source ~/.zshrc  # or: source ~/.bashrc
```

Verify the installation:

```bash
cc-clip --version
```

> **macOS "killed" error?** If you see `zsh: killed cc-clip`, macOS Gatekeeper is blocking the binary. Fix: `xattr -d com.apple.quarantine ~/.local/bin/cc-clip`

### Step 2: Setup (one command)

```bash
cc-clip setup myserver
```

This single command handles everything:
1. Installs local dependencies (`pngpaste`)
2. Starts the local daemon (via macOS launchd)
3. Deploys the binary and shim to the remote server
4. Reserves the peer's remote port, records a local tunnel state file under `~/.cache/cc-clip/tunnels/`, and auto-starts the daemon-managed reverse tunnel

`cc-clip` does **not** write to `~/.ssh/config`. Your interactive `ssh <alias>` sessions are unaffected; the reverse tunnel used for clipboard and notifications is owned entirely by the local cc-clip daemon.

Pass `--no-tunnel` to `cc-clip setup` / `cc-clip connect` if you prefer to drive the tunnel yourself (e.g. from systemd, SwiftBar, or a CI runner). The state file is still recorded so `cc-clip tunnel up <host>` later has everything it needs.

> **Upgrading from an older cc-clip?** Older releases wrote a `# >>> cc-clip managed host: … >>>` block into `~/.ssh/config`. Current cc-clip intentionally does not migrate or clean that block during `setup`, `connect`, or `uninstall`. Open `~/.ssh/config`, delete everything from the `# >>> cc-clip managed host: …` line through the matching `# <<< cc-clip managed host: …` line, and save. Until you remove it, new interactive `ssh <alias>` sessions will print a harmless `Warning: remote port forwarding failed for listen port <port>` because the daemon already owns the forward.

Before you run setup, make sure:
- You pass the SSH alias from `~/.ssh/config`, not `user@host`.
- The alias resolves via `ssh -G <alias>` — cc-clip only reads this; it does not rewrite the file.
- Your SSH key is in `ssh-agent` (or password-less). The daemon-managed tunnel uses `BatchMode=yes` and cannot prompt for a passphrase.

<details>
<summary>See it in action (macOS)</summary>
<p align="center">
  <img src="docs/marketing/demo-macos.gif" alt="cc-clip macOS demo" width="720">
</p>
</details>

<details>
<summary>Also use Codex CLI? Add <code>--codex</code></summary>

```bash
cc-clip setup myserver --codex
```

This additionally installs Xvfb and the x11-bridge on the remote. If `Xvfb` is not found and auto-install fails, you'll see the exact command to run:

```bash
ssh myserver
sudo apt install xvfb          # Debian/Ubuntu
sudo dnf install xorg-x11-server-Xvfb   # RHEL/Fedora
```

Then re-run `cc-clip setup myserver --codex`.

</details>

<details>
<summary>Windows? Use the dedicated guide</summary>

- [Windows Quick Start](docs/windows-quickstart.md)

<p align="center">
  <img src="docs/marketing/demo-windows.gif" alt="cc-clip Windows demo" width="720">
</p>

</details>

### Step 3: Connect and use

After `cc-clip setup myserver` completes, the daemon already has a reverse tunnel open for you. Just open a shell and use Claude Code or Codex CLI as normal — `Ctrl+V` pastes images from your Mac clipboard.

```bash
ssh myserver    # shell session only; the clipboard tunnel is already alive
```

Tunnel lifecycle at a glance:

```bash
cc-clip tunnel list               # show connected/stopped state per host
cc-clip tunnel down myserver      # pause the tunnel without forgetting it
cc-clip tunnel up   myserver      # restart it (re-resolves ~/.ssh/config)
cc-clip tunnel remove myserver    # delete the saved state entirely
```

> **Edited `~/.ssh/config`?** The daemon caches `ssh -G <host>` output the first time you run `cc-clip tunnel up <host>` and reuses that cache on every reconnect. This is a deliberate security pin — it prevents a later edit (yours or anything else's) from silently changing the `ssh` argv the daemon spawns. To pick up your edit, re-run `cc-clip tunnel up <host>`. See [docs/troubleshooting.md → "I Edited `~/.ssh/config` And The Tunnel Still Uses The Old Settings"](docs/troubleshooting.md) for the full rationale.

> **Important:** image paste works through the cc-clip daemon's reverse tunnel. The daemon runs in the background (launchd `KeepAlive=true` on macOS) and re-establishes tunnels automatically when it restarts. If the daemon is down, the shim on the remote side falls through to the real `xclip`/`wl-paste` and Claude Code sees an empty clipboard.
>
> If `which xclip` still points to `/usr/bin/xclip` or `DISPLAY` is still empty after reconnecting, your login shell may not source `~/.bashrc` or `~/.zshrc` automatically. Run `source ~/.bashrc` or `source ~/.zshrc`, then see [Troubleshooting Guide](docs/troubleshooting.md) for a persistent fix.
>
> If your usual habit is `ssh alice@example.com`, create an alias such as `Host myserver` in `~/.ssh/config` and use `ssh myserver` for both setup and daily use with `cc-clip`.

### Verify it works

```bash
# Copy an image to your Mac clipboard first (Cmd+Shift+Ctrl+4), then:
cc-clip doctor --host myserver
```

On Windows, the equivalent quick check is:

- [Windows Quick Start](docs/windows-quickstart.md)

## Why cc-clip?

| Approach | Works over SSH? | Any terminal? | Image support? | Setup complexity |
|----------|:-:|:-:|:-:|:--:|
| Native Ctrl+V | Local only | Some | Yes | None |
| X11 Forwarding | Yes (slow) | N/A | Yes | Complex |
| OSC 52 clipboard | Partial | Some | Text only | None |
| **cc-clip** | **Yes** | **Yes** | **Yes** | **One command** |

## How It Works

```mermaid
graph LR
    subgraph local ["Local Mac"]
        A["Clipboard"] --> B["pngpaste"]
        B --> C["cc-clip daemon<br/>127.0.0.1:18339"]
    end

    subgraph win ["Local Windows"]
        J["Clipboard"] --> K["cc-clip hotkey / send"]
    end

    subgraph remote ["Remote Linux"]
        F["Claude Code"] -- "Ctrl+V" --> E["xclip shim"]
        E -- "curl" --> D["127.0.0.1:18339"]
        K -- "ssh/scp upload" --> L["~/.cache/cc-clip/uploads"]
        L -- "paste path" --> F
        G["Codex CLI"] -- "Ctrl+V / arboard" --> H["Xvfb CLIPBOARD"]
        H -- "SelectionRequest" --> I["x11-bridge"]
        I -- "HTTP" --> D
    end

    C == "daemon-managed SSH reverse tunnel" ==> D

    style local fill:#1a1a2e,stroke:#e94560,color:#eee
    style remote fill:#1a1a2e,stroke:#0f3460,color:#eee
    style A fill:#e94560,stroke:#e94560,color:#fff
    style F fill:#0f3460,stroke:#0f3460,color:#fff
    style G fill:#0f3460,stroke:#0f3460,color:#fff
```

1. **macOS Claude path:** the local daemon reads your Mac clipboard via `pngpaste`, serves images over HTTP on loopback, and the remote `xclip` shim fetches images through the SSH tunnel
2. **Windows Claude path:** the local hotkey reads your Windows clipboard, uploads the image over SSH/SCP, and pastes the remote file path into the active terminal
3. **Codex CLI path:** x11-bridge claims CLIPBOARD ownership on a headless Xvfb, serves images on-demand when Codex reads the clipboard via X11
4. **Notification path:** remote Claude Code hooks and Codex notify pipe events through `cc-clip-hook` → SSH tunnel → local daemon → macOS Notification Center or cmux

## SSH Notifications

When Claude Code or Codex CLI runs on a remote server, **notifications don't work over SSH** — `TERM_PROGRAM` isn't forwarded, hooks execute on the remote where `terminal-notifier` doesn't exist, and tmux swallows OSC sequences.

cc-clip solves this by acting as a **notification transport bridge**: remote hook events travel through the same SSH tunnel used for clipboard, and the local daemon delivers them to your macOS notification system (or cmux if installed).

### What you'll see

| Event | Notification | Example |
|-------|-------------|---------|
| Claude finishes responding | "Claude stopped" + last message preview | `Claude stopped: I've implemented the notification bridge...` |
| Claude needs tool approval | "Tool approval needed" + tool name | `Tool approval needed: Claude wants to Edit cmd/main.go` |
| Codex task completes | "Codex" + completion message | `Codex: Added error handling to fetch module` |
| Image pasted via Ctrl+V | "cc-clip #N" + fingerprint + dimensions | `cc-clip #3: a1b2c3d4 . 1920x1080 . PNG` |
| Duplicate image detected | Same as above + duplicate marker | `cc-clip #4: Duplicate of #2` |

Image paste notifications help you track what was pasted without leaving your workflow:
- **Sequence number** (#1, #2, #3...) lets you detect gaps (e.g., #1 → #3 means #2 was lost)
- **Duplicate detection** alerts when the same image is pasted twice within 5 images
- **Click notification** to open the full image in Preview.app (macOS, requires `terminal-notifier`)

### Setup notifications

**Step 1:** Make sure `cc-clip serve` is running locally (or use `cc-clip service install` for auto-start).

**Step 2:** Configure your remote Claude Code hooks. The easiest way is to **ask Claude Code itself to do it**. SSH into your server, start Claude Code, and paste this prompt:

```
Please add cc-clip-hook to my Claude Code hooks configuration. Add it to both Stop and Notification hooks in ~/.claude/settings.json. The command is just "cc-clip-hook" (it's already in PATH at ~/.local/bin/). Keep any existing hooks (like ralph-wiggum) — just append cc-clip-hook alongside them. Show me the diff before and after.
```

Claude Code will read your current `settings.json`, add the hooks correctly, and show you the changes.

Alternatively, you can configure manually:

<details>
<summary>Manual hook configuration</summary>

Edit `~/.claude/settings.json` on the **remote server** and add `cc-clip-hook` to the `Stop` and `Notification` hook arrays:

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "cc-clip-hook" }
        ]
      }
    ],
    "Notification": [
      {
        "hooks": [
          { "type": "command", "command": "cc-clip-hook" }
        ]
      }
    ]
  }
}
```

If you already have hooks (e.g., `ralph-wiggum-stop.sh`), add a new entry to the array — don't replace existing ones.

**Restart Claude Code** after editing (hooks are read at startup).

</details>

**Step 3 (Codex only):** Codex notification is auto-configured by `cc-clip connect` if `~/.codex/` exists on the remote. No manual steps needed.

**Step 4:** Generate and register a notification nonce (if you haven't used `cc-clip connect`):

```bash
# On local Mac — generate nonce and write to remote
NONCE=$(openssl rand -hex 32)
curl -s -X POST -H "Authorization: Bearer $(head -1 ~/.cache/cc-clip/session.token)" \
  -H "User-Agent: cc-clip/0.1" -H "Content-Type: application/json" \
  -d "{\"nonce\":\"$NONCE\"}" http://127.0.0.1:18339/register-nonce
ssh myserver "mkdir -p ~/.cache/cc-clip && echo '$NONCE' > ~/.cache/cc-clip/notify.nonce && chmod 600 ~/.cache/cc-clip/notify.nonce"
```

> **Note:** `cc-clip connect` handles steps 2-4 automatically. Manual setup is only needed if you use plain `ssh` instead of `cc-clip connect`.

### Troubleshooting notifications

<details>
<summary><b>Notifications don't appear</b></summary>

**Step-by-step verification (on the remote server):**

```bash
# 1. Is the tunnel working?
curl -sf --connect-timeout 2 http://127.0.0.1:18339/health
# Expected: {"status":"ok"}

# 2. Is the hook script the correct version?
grep "curl" ~/.local/bin/cc-clip-hook
# Expected: a curl command with --connect-timeout

# 3. Is the nonce file present?
cat ~/.cache/cc-clip/notify.nonce
# Expected: a 64-character hex string

# 4. Manual test:
echo '{"hook_event_name":"Stop","stop_hook_reason":"stop_at_end_of_turn","last_assistant_message":"test"}' | cc-clip-hook
# Expected: local Mac shows notification popup

# 5. Check health log for failures:
cat ~/.cache/cc-clip/notify-health.log
# If exists: shows timestamps and HTTP error codes
```

**Common issues:**

| Problem | Fix |
|---------|-----|
| Tunnel down (step 1 fails) | Find the remote port with `cc-clip tunnel list --json \| jq '.[] \| select(.config.host=="myserver") \| .config.remote_port'`, then kill any stale listener on that port on the remote (`sudo ss -tlnp \| grep <port>`) and re-run `cc-clip tunnel up myserver` |
| Old hook script (step 2 empty) | Reinstall: `cc-clip connect myserver` or manually copy the script |
| Missing nonce (step 3 fails) | Register nonce (see Step 4 above) |
| Daemon running old binary | Rebuild (`make build`) and restart (`cc-clip serve`) |

</details>

## Security

| Layer | Protection |
|-------|-----------|
| Network | Loopback only (`127.0.0.1`) — never exposed |
| Clipboard auth | Bearer token with 30-day sliding expiration (auto-renews on use) |
| Notification auth | Dedicated nonce per-connect session (separate from clipboard token) |
| Token delivery | Via stdin, never in command-line args |
| Notification trust | Hook notifications marked `verified`; generic JSON shows `[unverified]` prefix |
| Transparency | Non-image calls pass through to real `xclip` unchanged |

## Daily Usage

After initial setup, your daily workflow is:

```bash
# 1. SSH to your server. The daemon-managed tunnel is already alive in the
#    background (it has been since `cc-clip setup` / `cc-clip tunnel up`);
#    interactive ssh does NOT establish or tear it down.
ssh myserver

# 2. Use Claude Code or Codex CLI normally
claude          # Claude Code
codex           # Codex CLI

# 3. Ctrl+V pastes images from your Mac clipboard
```

On macOS, the local daemon runs as a launchd service and starts automatically on login — no need to re-run setup. On Linux, there is no bundled auto-start integration; either run `cc-clip serve` manually in a terminal multiplexer, or wire it up with systemd/supervisord as you prefer.

### Windows workflow

On Windows, some `Windows Terminal -> SSH -> tmux -> Claude Code` combinations do not trigger the remote `xclip` path when you press `Alt+V` or `Ctrl+V`. `cc-clip` therefore provides a Windows-native workflow that does not depend on remote clipboard interception.

For first-time setup and day-to-day usage, use:

- [Windows Quick Start](docs/windows-quickstart.md)

The Windows workflow uses a dedicated remote-paste hotkey (default: `Alt+Shift+V`) so it does not collide with local Claude Code's native `Alt+V`.

## Persistent Tunnels & SwiftBar

The cc-clip daemon owns the reverse tunnel end-to-end. After `cc-clip setup` (or `cc-clip connect`) the daemon spawns its own `ssh -N -R` process and keeps the forward alive with auto-reconnect, independent of any interactive `ssh` session. Interactive `ssh myserver` sessions no longer establish or tear down the clipboard tunnel — they are purely for your shell work. The commands below are the management surface for tunnels the daemon already owns.

### Managing persistent tunnels (CLI)

```bash
# Start a persistent tunnel for a host
cc-clip tunnel up myserver

# List all tunnels and their status
cc-clip tunnel list

# Stop the tunnel on the current daemon (default port 18339).
# For multi-daemon setups, pass --port (or CC_CLIP_PORT) to select another daemon.
cc-clip tunnel down myserver

# Forget a tunnel entirely (stops it, then deletes its saved state file)
cc-clip tunnel remove myserver
```

`tunnel up` auto-detects the remote listen port from the local tunnel-state file that `cc-clip connect` wrote under `~/.cache/cc-clip/tunnels/`. Tunnels survive daemon restarts — the daemon re-establishes saved tunnels on startup.

**Picking up `~/.ssh/config` changes.** When you first run `cc-clip tunnel up <host>`, cc-clip runs `ssh -G <host>` to expand your SSH config (HostName, User, IdentityFile, ProxyCommand, …) and caches the result in the tunnel's state file. The reconnect loop and daemon-startup adoption re-use that cached snapshot; they do **not** re-read `~/.ssh/config` on every reconnect. This is deliberate: it keeps the options you approved at `tunnel up` time from being silently replaced on the next network flap. If you later edit `~/.ssh/config` (e.g. you updated `HostName`, rotated an `IdentityFile`, or added a `ProxyCommand`), re-run `cc-clip tunnel up <host>` to refresh the cache — that is the canonical "tell cc-clip about my new SSH config" command. Editing the config alone is not enough; `tunnel down && tunnel up` also works but `tunnel up` is sufficient.

#### Port source model

- `cc-clip connect` reserves the remote listen port in the remote peer registry, then writes that reservation into the local tunnel-state file (`~/.cache/cc-clip/tunnels/*.json`).
- The allocation source of truth is the remote peer registry.
- The runtime source used by `cc-clip tunnel up` is the local tunnel-state file. `tunnel up` does not SSH back to the remote just to look up the port.
- `cc-clip doctor --host` compares the remote peer registry with the local tunnel state and confirms the host alias resolves via `ssh -G`.

#### Daemon/port model

- The local daemon port defaults to `18339`. Override per-command with `--port`, or globally with `CC_CLIP_PORT`.
- A persistent tunnel's `local_port` (the endpoint of `ssh -R <remote>:127.0.0.1:<local_port>`) is always equal to the owning daemon's HTTP port — the reverse forward is only useful if it lands on a port where a daemon is actually listening. A daemon only manages tunnels whose `local_port` matches its own listening port.
- `tunnel up` / `down` / `remove` accept a single `--port` flag that selects the daemon to talk to. `tunnel up` will also adopt the daemon port recorded in the tunnel-state file when `--port` is not given, so `cc-clip tunnel up <host>` routes to the right daemon automatically in single-daemon setups.
- To act on a specific daemon's tunnel in a multi-daemon setup, point `--port` (or `CC_CLIP_PORT`) at that daemon.

> **Requirement:** Persistent tunnels use `BatchMode=yes` (no interactive prompts). Your SSH key must be in `ssh-agent` or passwordless. Run `ssh-add` if needed.
>
> If you upgraded from an older `cc-clip` while the local daemon was already running, restart the local daemon/service once before using `cc-clip tunnel ...` so the `/tunnels` routes are registered.
>
> **Token rotation:** The tunnel-control token at `~/.cache/cc-clip/tunnel-control.token` is local-only (never synced to the remote). Rotate it with `cc-clip serve --rotate-tunnel-token`; the SwiftBar plugin and `cc-clip tunnel ...` CLI read the fresh token on next invocation.

### SwiftBar menu bar plugin (macOS)

[SwiftBar](https://github.com/swiftbar/SwiftBar) is a macOS menu bar app that runs shell scripts and displays their output. The included plugin shows tunnel status at a glance and lets you start/stop already-saved tunnels from the menu bar.

<p align="center">
  <code>● 2/3</code> — 2 of 3 tunnels connected
</p>

**Prerequisites:**

- [SwiftBar](https://github.com/swiftbar/SwiftBar) installed (`brew install --cask swiftbar`)
- `jq` installed (`brew install jq`)
- `cc-clip` in PATH

**Install the plugin:**

Symlink the script into SwiftBar's plugin directory. Use an absolute path so
the link keeps working when you move your shell elsewhere:

```bash
ln -s /absolute/path/to/cc-clip/scripts/cc-clip-tunnels.30s.sh \
    ~/Library/Application\ Support/SwiftBar/Plugins/
```

If you are currently `cd`'d into the repo root, `$(pwd)/scripts/...` expands
to the same absolute path.

The `30s` in the filename means SwiftBar refreshes every 30 seconds.

Create the first tunnel from the CLI with `cc-clip tunnel up myserver`. After that, SwiftBar can manage the saved tunnel.

If you keep multiple daemon instances on different ports for the same host, select which one to act against with `--port` (or `CC_CLIP_PORT`):

```bash
cc-clip tunnel down myserver --port 18444
# or
CC_CLIP_PORT=18444 cc-clip tunnel down myserver
```

See the [Daemon/port model](#daemonport-model) above for why `--port` is the only selector needed.

**What you'll see:**

| Menu bar | Meaning |
|----------|---------|
| `● 2/2` (green) | All tunnels connected |
| `● 1/2` (orange) | Some tunnels disconnected |
| `⊘ 0/2` (gray) | All tunnels down |
| `⊘` | No tunnels configured |

Click the menu bar icon to see per-host details:

- Status (connected / connecting / disconnected)
- Port mapping (remote:port -> local:port)
- PID, reconnect count, last error
- **Start** / **Stop** button for each tunnel, sent to that tunnel's recorded local daemon port

## Commands

| Command | Description |
|---------|-------------|
| `cc-clip setup <host>` | **Full setup**: deps, daemon, deploy, tunnel state |
| `cc-clip setup <host> --codex` | Full setup with Codex CLI support |
| `cc-clip connect <host>` | Deploy to remote (incremental) |
| `cc-clip connect <host> --token-only` | Sync token only (fast) |
| `cc-clip connect <host> --force` | Full redeploy ignoring cache |
| `cc-clip tunnel list` | List persistent tunnels and their status |
| `cc-clip tunnel up <host>` | Start a persistent tunnel to a host |
| `cc-clip tunnel down <host>` | Stop a persistent tunnel owned by this daemon (select with `--port` / `CC_CLIP_PORT` if you run several) |
| `cc-clip tunnel remove <host>` | Stop and delete a persistent tunnel's saved state (select daemon with `--port`) |
| `cc-clip notify --title T --body B` | Send a notification through the tunnel |
| `cc-clip notify --from-codex-stdin` | Read Codex JSON from stdin and notify |
| `cc-clip doctor --host <host>` | End-to-end health check |
| `cc-clip status` | Show local component status |
| `cc-clip service install` | Install macOS launchd service |
| `cc-clip service uninstall` | Remove launchd service |
| `cc-clip send [<host>] --paste` | Windows: upload clipboard image and paste remote path |
| `cc-clip hotkey [<host>]` | Windows: register the remote upload/paste hotkey |
| `cc-clip install --target <target>` | Install a local `xclip` or `wl-paste` shim |
| `cc-clip uninstall --target <target>` | Remove a local shim; `auto` works when exactly one cc-clip shim is installed |

<details>
<summary>All commands</summary>

| Command | Description |
|---------|-------------|
| `cc-clip setup <host>` | Full setup: deps, daemon, deploy, tunnel state |
| `cc-clip setup <host> --codex` | Full setup including Codex CLI support |
| `cc-clip connect <host>` | Deploy to remote (incremental) |
| `cc-clip connect <host> --codex` | Deploy with Codex support (Xvfb + x11-bridge) |
| `cc-clip connect <host> --token-only` | Sync token only (fast) |
| `cc-clip connect <host> --force` | Full redeploy ignoring cache |
| `cc-clip tunnel list [--json]` | List persistent tunnels |
| `cc-clip tunnel up <host> [--remote-port N]` | Start a persistent tunnel (remote port auto-detected from saved tunnel state if omitted) |
| `cc-clip tunnel down <host>` | Stop the tunnel owned by the current daemon (select with `--port` / `CC_CLIP_PORT`) |
| `cc-clip tunnel remove <host>` | Stop and delete the tunnel's saved state (select daemon with `--port`) |
| `cc-clip serve` | Start daemon in foreground |
| `cc-clip serve --rotate-token` | Start daemon with forced new clipboard session token |
| `cc-clip serve --rotate-tunnel-token` | Start daemon with forced new tunnel-control token |
| `cc-clip service install` | Install macOS launchd service |
| `cc-clip service uninstall` | Remove launchd service |
| `cc-clip service status` | Show service status |
| `cc-clip send [<host>]` | Upload clipboard image to a remote file |
| `cc-clip send [<host>] --paste` | Windows: paste the uploaded remote path into the active window |
| `cc-clip hotkey [<host>]` | Windows: run a background remote-paste hotkey listener |
| `cc-clip hotkey --enable-autostart` | Windows: start the hotkey listener automatically at login |
| `cc-clip hotkey --disable-autostart` | Windows: remove hotkey auto-start at login |
| `cc-clip hotkey --status` | Windows: show hotkey status |
| `cc-clip hotkey --stop` | Windows: stop the hotkey listener |
| `cc-clip notify --title T --body B` | Send a generic notification through the tunnel |
| `cc-clip notify --from-codex "$1"` | Parse Codex JSON arg and notify |
| `cc-clip notify --from-codex-stdin` | Read Codex JSON from stdin and notify |
| `cc-clip doctor` | Local health check |
| `cc-clip doctor --host <host>` | End-to-end health check |
| `cc-clip status` | Show component status |
| `cc-clip install --target <target>` | Install a local `xclip` or `wl-paste` shim |
| `cc-clip uninstall` | Remove a local shim; `auto` removes the installed shim when exactly one exists |
| `cc-clip uninstall --target <target>` | Remove the specified local shim explicitly |
| `cc-clip uninstall --host <host>` | Remove the remote PATH marker (add `--peer` to also release the peer lease and delete local tunnel state). Upgraders: also delete any legacy `~/.ssh/config` managed block by hand — see [Complete uninstall](#complete-uninstall). |
| `cc-clip uninstall --host <host> --peer` | Remote PATH marker + release the peer lease + delete local tunnel state for this host. Upgraders: also delete any legacy `~/.ssh/config` managed block by hand — see [Complete uninstall](#complete-uninstall). |
| `cc-clip uninstall --codex` | Remove Codex support (local) |
| `cc-clip uninstall --codex --host <host>` | Remove Codex support from remote |

</details>

### Complete uninstall

cc-clip no longer ships `uninstall-all.sh` / `uninstall-local.sh`. For a full removal, run the subcommands in order:

```bash
# Remote: release the peer lease, remove the remote PATH marker, delete local tunnel state for this host.
# Bare `--peer` auto-discovers your local peer id (see `cc-clip status` for the value);
# pass --peer-id <id> only when cleaning up a different workstation's lease.
cc-clip uninstall --host myserver --peer

# Remote Codex assets (only if you set up --codex)
cc-clip uninstall --codex --host myserver

# Local Codex DISPLAY marker (only if you used --codex)
cc-clip uninstall --codex

# Local xclip / wl-paste shim
cc-clip uninstall

# Local daemon
cc-clip service uninstall
```

If you upgraded from a pre-daemon-tunnel release, also delete any leftover `# >>> cc-clip managed host: … >>>` block from `~/.ssh/config` by hand (cc-clip does not auto-clean it).

## Configuration

All settings have sensible defaults. The local daemon port defaults to `18339`. Override it with `CC_CLIP_PORT`, or with command-specific `--port` flags where supported:

| Setting | Default | Env Var |
|---------|---------|---------|
| Local daemon port | 18339 | `CC_CLIP_PORT` |
| Token TTL | 30d | `CC_CLIP_TOKEN_TTL` |
| Debug logs | off | `CC_CLIP_DEBUG=1` |

<details>
<summary>All settings</summary>

| Setting | Default | Env Var |
|---------|---------|---------|
| Local daemon port | 18339 | `CC_CLIP_PORT` |
| Token TTL | 30d | `CC_CLIP_TOKEN_TTL` |
| Output dir | `$XDG_RUNTIME_DIR/claude-images` | `CC_CLIP_OUT_DIR` |
| Probe timeout | 500ms | `CC_CLIP_PROBE_TIMEOUT_MS` |
| Fetch timeout | 5000ms | `CC_CLIP_FETCH_TIMEOUT_MS` |
| Debug logs | off | `CC_CLIP_DEBUG=1` |

</details>

## Platform Support

| Local | Remote | Status |
|-------|--------|--------|
| macOS (Apple Silicon) | Linux (amd64) | Stable |
| macOS (Intel) | Linux (arm64) | Stable |
| Windows 10/11 | Linux (amd64/arm64) | Experimental (`send` / `hotkey`) |

## Requirements

**Local (macOS):** macOS 13+ (`pngpaste`, auto-installed by `cc-clip setup`)

**Local (Windows):** Windows 10/11 with PowerShell, `ssh`, and `scp` available in `PATH`

**Remote:** Linux with `xclip`, `curl`, `bash`, and SSH access. The macOS tunnel/shim path is auto-configured by `cc-clip connect`; the Windows upload/hotkey path uses SSH/SCP directly.

**Remote (Codex `--codex`):** Additionally requires `Xvfb`. Auto-installed if passwordless sudo is available, otherwise: `sudo apt install xvfb` (Debian/Ubuntu) or `sudo dnf install xorg-x11-server-Xvfb` (RHEL/Fedora).

## Troubleshooting

```bash
# One command to check everything
cc-clip doctor --host myserver
```

<details>
<summary><b>"zsh: killed" after installation</b></summary>

**Symptom:** Running any `cc-clip` command immediately shows `zsh: killed cc-clip ...`

**Cause:** macOS Gatekeeper blocks unsigned binaries downloaded from the internet.

**Fix:**

```bash
xattr -d com.apple.quarantine ~/.local/bin/cc-clip
```

Or reinstall (the latest install script handles this automatically):

```bash
curl -fsSL https://raw.githubusercontent.com/fancy-potato/cc-clip/main/scripts/install.sh | sh
```

</details>

<details>
<summary><b>"cc-clip: command not found"</b></summary>

**Cause:** `~/.local/bin` is not in your PATH.

**Fix:**

```bash
# Add to your shell profile
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

Replace `~/.zshrc` with `~/.bashrc` if you use bash.

</details>

<details>
<summary><b>Ctrl+V doesn't paste images (Claude Code)</b></summary>

**Step-by-step verification:**

```bash
# Replace <local-daemon-port> with your current local daemon port
# (default 18339, or your CC_CLIP_PORT / --port value).

# 1. Local: Is the daemon running?
curl -s http://127.0.0.1:<local-daemon-port>/health
# Expected: {"status":"ok"}

# 2. Remote: Is the tunnel forwarding?
# Replace <remote-port> with the port reported by `cc-clip tunnel list` for this host.
ssh myserver "curl -s http://127.0.0.1:<remote-port>/health"
# Expected: {"status":"ok"}

# 3. Remote: Is the shim taking priority?
ssh myserver "which xclip"
# Expected: ~/.local/bin/xclip  (NOT /usr/bin/xclip)

# 4. Remote: Does the shim intercept correctly?
# (copy an image to Mac clipboard first)
ssh myserver 'CC_CLIP_DEBUG=1 xclip -selection clipboard -t TARGETS -o'
# Expected: image/png
```

If step 2 fails, check the daemon tunnel status with `cc-clip tunnel list`, then restart it with `cc-clip tunnel up myserver` if needed. (Note: since the daemon owns the tunnel, opening a new interactive `ssh` session does not re-establish the forward.)

If step 3 fails, the PATH fix didn't take effect. Open a fresh SSH session, or run `source ~/.bashrc` / `source ~/.zshrc`. See [Troubleshooting Guide](docs/troubleshooting.md) if your login shell does not load that file automatically.

</details>

<details>
<summary><b>Interactive SSH tab warns "remote port forwarding failed for listen port ..."</b></summary>

**Symptom:** You run `ssh myserver` in a fresh tab and see `Warning: remote port forwarding failed for listen port <remote-port>`.

**Cause:** You are upgrading from an older `cc-clip` release whose managed block in `~/.ssh/config` still contains a `RemoteForward` directive. The cc-clip daemon already owns that port, so OpenSSH's attempt to bind the forward on your interactive session fails. The warning is harmless — clipboard and notifications continue to work through the daemon-owned tunnel.

**Fix:** delete the stale block by hand. This migration is intentionally manual; cc-clip does not rewrite `~/.ssh/config` for you during `setup`, `connect`, or `uninstall`:

```
# >>> cc-clip managed host: myserver >>>
...
# <<< cc-clip managed host: myserver <<<
```

Open `~/.ssh/config`, delete everything from the opening `# >>> cc-clip managed host: …` line through the matching `# <<< cc-clip managed host: …` line inclusive, save, and the warning will not return.

</details>

<details>
<summary><b>Ctrl+V doesn't paste images (Codex CLI)</b></summary>

> **Most common cause:** DISPLAY environment variable is empty. You must open a **new** SSH session after setup — existing sessions don't pick up the updated shell rc file.

**Step-by-step verification (run these on the remote server):**

```bash
# 1. Is DISPLAY set?
echo $DISPLAY
# Expected: 127.0.0.1:0 (or 127.0.0.1:1, etc.)
# If empty → open a NEW SSH session, or run: source ~/.bashrc / source ~/.zshrc

# 2. Is the SSH tunnel working?
# Replace <remote-port> with the port reported by `cc-clip tunnel list` for this host.
curl -s http://127.0.0.1:<remote-port>/health
# Expected: {"status":"ok"}
# If fails → if you use persistent tunnels, run `cc-clip tunnel list` locally and restart it with
# `cc-clip tunnel up myserver`; otherwise open a NEW SSH connection

# 3. Is Xvfb running?
ps aux | grep Xvfb | grep -v grep
# Expected: a Xvfb process
# If missing → re-run: cc-clip connect myserver --codex --force

# 4. Is x11-bridge running?
ps aux | grep 'cc-clip x11-bridge' | grep -v grep
# Expected: a cc-clip x11-bridge process
# If missing → re-run: cc-clip connect myserver --codex --force

# 5. Does the X11 socket exist?
ls -la /tmp/.X11-unix/
# Expected: X0 file (matching your display number)

# 6. Can xclip read clipboard via X11? (copy an image on Mac first)
xclip -selection clipboard -t TARGETS -o
# Expected: image/png
```

**Common fixes:**

| Step fails | Fix |
|-----------|-----|
| Step 1 (DISPLAY empty) | Open a **new** SSH session. If still empty: `source ~/.bashrc` or `source ~/.zshrc` |
| Step 2 (tunnel down) | If you use persistent tunnels, run `cc-clip tunnel list` locally and restart it with `cc-clip tunnel up myserver`; otherwise open a **new** SSH connection |
| Steps 3-4 (processes missing) | `cc-clip connect myserver --codex --force` from local |
| Step 6 (no image/png) | Copy an image on Mac first: `Cmd+Shift+Ctrl+4` |

> **Note:** DISPLAY uses TCP loopback format (`127.0.0.1:N`) instead of Unix socket format (`:N`) because Codex CLI's sandbox blocks access to `/tmp/.X11-unix/`. If you previously set up cc-clip with an older version, re-run `cc-clip connect myserver --codex --force` to update.

</details>

<details>
<summary><b>Fresh SSH session still misses PATH or DISPLAY</b></summary>

**Symptom:** You reconnect with `ssh myserver`, but `which xclip` still resolves to `/usr/bin/xclip`, or `echo $DISPLAY` is still empty.

**Cause:** `cc-clip` writes its PATH and DISPLAY markers into `~/.bashrc` or `~/.zshrc`. Some login-shell setups do not source those files automatically on SSH login.

**Quick fix for the current shell:**

```bash
source ~/.bashrc
# or
source ~/.zshrc
```

**Persistent fix for bash login shells:**

```bash
printf '\n[ -f ~/.bashrc ] && . ~/.bashrc\n' >> ~/.bash_profile
```

If your system uses `~/.profile` instead of `~/.bash_profile`, add the same line there. Then open a new SSH session and re-check `which xclip` or `echo $DISPLAY`.

</details>

<details>
<summary><b>Setup fails because you passed <code>user@host</code> instead of a Host alias</b></summary>

**Symptom:** `cc-clip setup alice@example.com` or `cc-clip connect alice@example.com` fails.

**Cause:** `cc-clip` expects an SSH alias that resolves via `ssh -G <alias>`. It does not manage raw `user@host` destinations directly.

**Fix:** Define an alias first, then use that alias everywhere:

```sshconfig
Host myserver
    HostName example.com
    User alice
```

Then run:

```bash
cc-clip setup myserver
ssh myserver
```

</details>

<details>
<summary><b>Stale sshd process blocks the remote port</b></summary>

**Symptom:** The daemon-managed tunnel for `myserver` reports `disconnected` in `cc-clip tunnel list`, and its last error mentions "address already in use".

**Fix:** Kill the stale process on the remote host, then restart the tunnel:

```bash
sudo ss -tlnp | grep <remote-port>     # find the PID (remote)
sudo kill <PID>                        # kill it

cc-clip tunnel up myserver             # back on the local machine
```

</details>

<details>
<summary><b>Token expired after 30+ days of inactivity</b></summary>

**Fix:** `cc-clip connect myserver --token-only`

Token uses sliding expiration — auto-renews on every use. Only expires after 30 days of zero activity.

</details>

<details>
<summary><b>Launchd daemon can't find pngpaste</b></summary>

**Fix:** `cc-clip service uninstall && cc-clip service install` (regenerates plist with correct PATH).

</details>

<details>
<summary><b>Setup fails: "killed" during re-deployment</b></summary>

**Symptom:** `cc-clip setup` was working before, but now shows `zsh: killed` when re-running.

**Cause:** The launchd service is running the old binary. Replacing the binary while the daemon holds it open can cause conflicts.

**Fix:**

```bash
cc-clip service uninstall
curl -fsSL https://raw.githubusercontent.com/fancy-potato/cc-clip/main/scripts/install.sh | sh
cc-clip setup myserver
```

</details>


<details>
<summary><b>More issues</b></summary>

See [Troubleshooting Guide](docs/troubleshooting.md) for detailed diagnostics, or run `cc-clip doctor --host myserver`.

</details>

## Contributing

Contributions welcome! For bug reports and feature requests, [open an issue](https://github.com/fancy-potato/cc-clip/issues).

For code contributions:

```bash
git clone https://github.com/fancy-potato/cc-clip.git
cd cc-clip
make build && make test
```

- **Bug fixes:** Open a PR directly with a clear description of the fix
- **New features:** Open an issue first to discuss the approach
- **Commit style:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, etc.)

## Related

**Claude Code — Clipboard:**
- [anthropics/claude-code#5277](https://github.com/anthropics/claude-code/issues/5277) — Image paste in SSH sessions
- [anthropics/claude-code#29204](https://github.com/anthropics/claude-code/issues/29204) — xclip/wl-paste dependency

**Claude Code — Notifications:**
- [anthropics/claude-code#19976](https://github.com/anthropics/claude-code/issues/19976) — Terminal notifications fail in tmux/SSH
- [anthropics/claude-code#29928](https://github.com/anthropics/claude-code/issues/29928) — Built-in completion notifications
- [anthropics/claude-code#36885](https://github.com/anthropics/claude-code/issues/36885) — Notification when waiting for input (headless/SSH)
- [anthropics/claude-code#29827](https://github.com/anthropics/claude-code/issues/29827) — Webhook/push notification for permission requests
- [anthropics/claude-code#36850](https://github.com/anthropics/claude-code/issues/36850) — Terminal bell on tool approval prompt
- [anthropics/claude-code#32610](https://github.com/anthropics/claude-code/issues/32610) — Terminal bell on completion
- [anthropics/claude-code#40165](https://github.com/anthropics/claude-code/issues/40165) — OSC-99 notification support assumed, not queried

**Codex CLI — Clipboard:**
- [openai/codex#6974](https://github.com/openai/codex/issues/6974) — Linux: cannot paste image
- [openai/codex#6080](https://github.com/openai/codex/issues/6080) — Image pasting issue
- [openai/codex#13716](https://github.com/openai/codex/issues/13716) — Clipboard image paste failure on Linux
- [openai/codex#7599](https://github.com/openai/codex/issues/7599) — Image clipboard does not work in WSL

**Codex CLI — Notifications:**
- [openai/codex#3962](https://github.com/openai/codex/issues/3962) — Play a sound when Codex finishes (34 comments)
- [openai/codex#8929](https://github.com/openai/codex/issues/8929) — Notify hook not getting triggered
- [openai/codex#8189](https://github.com/openai/codex/issues/8189) — WSL2: notifications fail for approval prompts

**Terminal / Multiplexer:**
- [manaflow-ai/cmux#833](https://github.com/manaflow-ai/cmux/issues/833) — Notifications over SSH+tmux sessions
- [manaflow-ai/cmux#559](https://github.com/manaflow-ai/cmux/issues/559) — Better SSH integration
- [ghostty-org/ghostty#10517](https://github.com/ghostty-org/ghostty/discussions/10517) — SSH image paste discussion

## License

[MIT](LICENSE)
