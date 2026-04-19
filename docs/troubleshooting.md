# Troubleshooting Guide

## Quick Diagnostics

```bash
cc-clip doctor --host myserver
```

## Doctor Check IDs

`cc-clip doctor --host <host>` prints one line per check in the form `[OK] <check-id>: <message>` or `[FAIL] <check-id>: <message>`. The sections below are indexed by the check ID the doctor emits, so you can grep straight to the relevant fix:

- `tunnel-state` — the locally-saved tunnel state (under `~/.cache/cc-clip/tunnels/`) is missing, ambiguous, or disagrees with the remote peer registry. See *How Port Selection Works* and *Stale Tunnel State Files* below.
- `ssh-config-legacy` — advisory only (doctor still exits 0). See *Upgrade Leftover: Legacy Managed Block* below.
- `tunnel` / `remote-bin` / `path-order` / `remote-token` / `path-fix` / `image-probe` — these names are self-descriptive; the matching failure modes are covered in *Step-by-Step Verification* and the per-symptom sections further down.

## How Port Selection Works

`cc-clip` has two distinct sources in play for tunnel ports:

- Allocation source of truth: the remote peer registry. `cc-clip connect` reserves the remote listen port there.
- Local runtime source: the tunnel state file at `~/.cache/cc-clip/tunnels/<sanitized-host>-<localPort>-<hash>.json`. `cc-clip connect` writes this after reserving the remote port; `cc-clip tunnel up` / `tunnel list` / `doctor` all read it.
- Local daemon port: the local HTTP daemon listens on `18339` by default, or on the current `CC_CLIP_PORT` / `--port` value when overridden.

A persistent tunnel's `local_port` (the target of `ssh -R <remote>:127.0.0.1:<local>`) always equals the owning daemon's HTTP port — the reverse forward only makes sense if it lands where a daemon is listening.

`cc-clip tunnel up` does not SSH back to the remote to re-read the registry. It reads the local tunnel state file that `connect` already wrote. `cc-clip doctor --host` compares the remote peer registry against that saved state and confirms the host alias resolves via `ssh -G`.

## Step-by-Step Verification

If image paste isn't working, run these checks **in order** to isolate the problem:

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

# 3. Remote: Is the shim taking priority over real xclip?
ssh myserver "which xclip"
# Expected: /home/<user>/.local/bin/xclip  (NOT /usr/bin/xclip)

# 4. Remote: Does the shim intercept correctly? (copy an image on Mac first)
ssh myserver 'CC_CLIP_DEBUG=1 xclip -selection clipboard -t TARGETS -o'
# Expected: image/png
```

---

## Fresh SSH Session Still Misses PATH or DISPLAY

**Symptom:** You reconnect with `ssh myserver`, but `which xclip` still shows `/usr/bin/xclip`, or `echo $DISPLAY` is still empty.

**Cause:** `cc-clip` prepends its PATH and DISPLAY markers to `~/.bashrc` or `~/.zshrc`. Some SSH login-shell setups do not source those files automatically.

**Quick fix for the current shell:**

```bash
source ~/.bashrc
# or
source ~/.zshrc
```

Then re-check:

```bash
which xclip
echo $DISPLAY
```

**Persistent fix for bash login shells:**

```bash
printf '\n[ -f ~/.bashrc ] && . ~/.bashrc\n' >> ~/.bash_profile
```

If your environment uses `~/.profile` instead of `~/.bash_profile`, add the same line there instead. After that, open a new SSH session and verify the PATH or DISPLAY again.

---

## Exact Host Alias Is Required

**Symptom:** `cc-clip setup alice@example.com` or `cc-clip connect alice@example.com` fails.

**Cause:** `cc-clip` expects an SSH alias that resolves via `ssh -G <alias>`. Raw `user@host` destinations do not resolve through `~/.ssh/config`, so the daemon-managed tunnel has no way to pick up the user, identity file, or ProxyCommand you would otherwise set there.

**Fix:** Create an alias first:

```sshconfig
Host myserver
    HostName example.com
    User alice
```

Then use the alias consistently:

```bash
cc-clip setup myserver
cc-clip connect myserver
cc-clip tunnel up myserver
ssh myserver
```

`cc-clip doctor --host myserver` is optional here and read-only. It does not touch `~/.ssh/config`; it compares the remote peer registry against the local tunnel state when you suspect port drift.

---

## Upgrade Leftover: Legacy Managed Block in `~/.ssh/config`

Doctor check ID: `ssh-config-legacy` (advisory only — this check is cosmetic; doctor exits 0 even when the block is present).

**Symptom:** After upgrading, opening `ssh myserver` prints `Warning: remote port forwarding failed for listen port <port>`, and `~/.ssh/config` still contains a `# >>> cc-clip managed host: … >>>` block with `RemoteForward` / `ControlMaster no` / `ControlPath none`. `cc-clip doctor --host myserver` will show:

```
[OK] ssh-config-legacy: leftover '# >>> cc-clip managed host: myserver …' block in ~/.ssh/config …
```

**Cause:** Older cc-clip releases wrote that block so interactive SSH sessions would establish the reverse tunnel. The current release owns the tunnel from the local daemon directly; the leftover `RemoteForward` competes with the daemon-held port and OpenSSH warns.

**Fix:** cc-clip does **not** migrate this block for you. This is an intentional manual step; `setup`, `connect`, and `uninstall` leave old managed blocks alone. Open `~/.ssh/config`, locate the block, and delete everything between (and including) the two marker lines:

```
# >>> cc-clip managed host: <alias> >>>
...
# <<< cc-clip managed host: <alias> <<<
```

Save and reconnect. The warning will not return.

---

## Token Expired or Invalid

**Symptom:** "fetch type failed" or "token invalid" / "401" in shim debug logs. Image paste silently falls back to the remote (empty) clipboard.

**Cause:** Token TTL (30 days) expired due to prolonged inactivity, or daemon restarted and generated a new token.

**Fix:**

```bash
# Re-sync token without re-uploading the binary
cc-clip connect myserver --token-only
```

The token uses **sliding expiration** — it auto-renews on every successful request. You'll only hit this after 30+ days of zero usage.

To force a new token: `cc-clip serve --rotate-token`.

---

## Launchd Daemon Returns "empty" for Image Clipboard

**Symptom:** `cc-clip service install` is running, but `/clipboard/type` returns `{"type":"empty"}` even when you have an image in your Mac clipboard. Running `cc-clip serve` in the foreground works correctly.

**Cause:** macOS `launchd` does not source your shell profile, so `PATH` doesn't include Homebrew directories (`/opt/homebrew/bin` on Apple Silicon, `/usr/local/bin` on Intel). The daemon can't find `pngpaste`.

**Fix:** Reinstall the service to regenerate the plist with correct PATH:

```bash
cc-clip service uninstall
cc-clip service install
```

---

## Empty Image Data (API Error 400)

**Symptom:** Claude Code returns `API Error: 400 — image cannot be empty`. The conversation becomes corrupted and all subsequent image pastes fail in the same session.

**Cause:** A race condition where the clipboard content changes between the TARGETS check and the image fetch. The shim outputs empty data, and Claude Code sends an empty base64 image to the API.

**Fix:**

1. In Claude Code, run `/clear` or start a new session (the old conversation is corrupted)
2. Update to the latest cc-clip and re-run `cc-clip connect myserver`

---

## `~/.local/bin` Not in PATH

**Symptom:** `cc-clip connect` shows WARNING: `'which xclip' resolves to /usr/bin/xclip, not ~/.local/bin/xclip`.

**Cause:** The shim is installed to `~/.local/bin/` but it's not first in PATH, so the system uses `/usr/bin/xclip` instead.

**Fix:** `cc-clip connect` auto-detects your remote shell and prepends a PATH marker to the appropriate rc file. If auto-fix didn't work, add manually:

```bash
# Add to the TOP of ~/.bashrc (before the interactive guard)
export PATH="$HOME/.local/bin:$PATH"
```

Verify with `which xclip` — it should point to `~/.local/bin/xclip`.

---

## No Image in Clipboard

**Symptom:** Shim returns `image/png` for TARGETS but Claude Code says "No image found in clipboard".

**Cause:** You may not have an image in your Mac clipboard.

**Fix:** Copy an image on your Mac first:
- **Screenshot to clipboard:** `Cmd + Shift + Ctrl + 4` (select area) or `Cmd + Shift + Ctrl + 3` (full screen)
- **Copy from an app:** Right-click an image → Copy Image

---

## Persistent Tunnel Stuck in `connecting`

**Symptom:** `cc-clip tunnel list` shows a tunnel in `connecting` state indefinitely. Note that a failing-auth tunnel will typically cycle through `disconnected` between attempts rather than staying in `connecting` forever — if `reconnect_count` is climbing, see "Persistent Tunnel Shows `disconnected` With Climbing Reconnect Count" below instead. A tunnel truly stuck in `connecting` usually means the ssh process is alive but never emitted the `remote forward success` line (for example, blocked on a slow network or a ProxyCommand that takes minutes to complete).

**Cause:** The tunnel SSH process uses `BatchMode=yes`, which forbids interactive prompts. If ssh-agent is not running, or the SSH key is passphrase-protected without being loaded into the agent, SSH cannot authenticate and retries forever.

**Fix:**

```bash
# 1. Confirm ssh-agent is running
ssh-add -l
# If "Could not open a connection to your authentication agent", start the agent:
eval "$(ssh-agent -s)"

# 2. Load the key (enter the passphrase once)
ssh-add ~/.ssh/id_rsa   # or whichever key your host uses

# 3. Verify the key is loaded
ssh-add -l

# 4. Restart the tunnel
cc-clip tunnel down myserver
cc-clip tunnel up myserver
```

Alternatively, use a passwordless key dedicated to this host, or configure macOS to load the key into the agent on login (`UseKeychain yes` / `AddKeysToAgent yes` in `~/.ssh/config`).

---

## Persistent Tunnel Shows `disconnected` With Climbing Reconnect Count

**Symptom:** `cc-clip tunnel list` reports `disconnected` for a host, and `reconnect_count` keeps climbing each time you check. The tunnel never becomes `connected`.

**Cause:** The SSH process is crashing on every attempt. Common reasons: the remote `sshd` is down, the reserved remote port is already bound on the remote by a stale `sshd` child, the host key changed, or SSH authentication is failing (see the `connecting` section above).

**Diagnosis:**

```bash
# 1. Inspect the tunnel's own error state. `last_error` is the most recent
#    ssh failure the reconnect loop recorded; `persistence_error` signals a
#    disk problem persisting state. Tunnel events are NOT in notify-health.log
#    (that file is only for notification-hook failures).
cc-clip tunnel list --json | jq '(. // []) | .[] | {host: .config.host, status, last_error, persistence_error, reconnect_count}'

# 2. Look at the daemon's launchd log for reconnect-loop details
#    (path matches your `cc-clip service install` StandardErrorPath — by
#    default ~/Library/Logs/cc-clip.log on macOS).
tail -n 100 ~/Library/Logs/cc-clip.log

# 3. Run an approximation of the ssh command the tunnel manager uses, so
#    errors surface. NOTE: the real tunnel manager *also* reads
#    `ssh -G <host>` and re-applies any options it finds there (excluding
#    safety-critical ones like BatchMode, ControlMaster/Path, ServerAlive*,
#    which cc-clip owns). If your failure mode differs from the below, run
#    `ssh -G myserver` to see what extra options (ProxyCommand, IdentityFile,
#    HostName overrides) are merged in. Pull <remote-port>/<local-port> from
#    `cc-clip tunnel list` for this host. With --json the keys are
#    `remote_port` and `local_port` (e.g.
#    `cc-clip tunnel list --json | jq '.[] | {h: .config.host, r: .config.remote_port, l: .config.local_port}'`);
#    in the human-readable table they appear under the REMOTE and LOCAL columns.
ssh -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=15 -o ServerAliveCountMax=3 \
    -o ControlMaster=no -o ControlPath=none \
    -R <remote-port>:127.0.0.1:<local-port> myserver
# Interpret the output:
# - "Permission denied"         → see "Stuck in connecting" (auth)
# - "remote port forwarding failed for listen port" → stale sshd on remote (kill it with `sudo kill <pid>` using `sudo ss -tlnp | grep <remote-port>`)
# - "Host key verification failed" → known_hosts mismatch
# - silent hang                 → network / firewall
```

Once you have identified the cause, fix it and restart the tunnel with `cc-clip tunnel down myserver && cc-clip tunnel up myserver`.

---

## I Edited `~/.ssh/config` And The Tunnel Still Uses The Old Settings

**Symptom:** You changed `HostName`, `User`, `IdentityFile`, `Port`, `ProxyCommand`, or a similar SSH directive for a host that has an active persistent tunnel, and subsequent reconnects still use the old values — you see the old hostname in `ssh`'s verbose output, auth picks the old key, or the `ProxyCommand` you removed is still firing.

**Cause:** The tunnel manager caches `ssh -G <host>` output (the fully-expanded SSH config for that alias) in the tunnel's state file at the time of `cc-clip tunnel up`. The reconnect loop reuses that cache on every network flap, and the daemon-startup adoption path reuses it across restarts. This is deliberate — it pins the options you approved at `tunnel up` time so that a later edit to `~/.ssh/config` (yours, or anything a malicious process planted) cannot silently change what `ssh` the daemon spawns.

The side effect is that a well-intentioned edit does not take effect on its own.

**Fix:**

```bash
# Refresh the cached SSH options for this host. Always safe to re-run;
# `tunnel up` is idempotent for an already-running tunnel — it re-resolves
# ssh -G, applies the new options, and restarts the underlying ssh process.
cc-clip tunnel up myserver

# Equivalent, slightly more explicit:
cc-clip tunnel down myserver && cc-clip tunnel up myserver
```

**Why not do this automatically on every reconnect?** Re-running `ssh -G` on each reconnect would (a) run a subprocess every 2–60 seconds during a flapping network and (b) silently promote any `~/.ssh/config` edit into the tunnel's argv on the next failure — including edits from tools you did not expect to touch the file. Pinning at `tunnel up` time and requiring explicit refresh keeps the invariant narrow and auditable.

---

## Stale Tunnel State Files

**Symptom:** `cc-clip tunnel list` shows a tunnel with a PID that doesn't exist on your machine, or a stopped tunnel reappears after you kill its SSH process manually.

**Cause:** Tunnel state lives in `~/.cache/cc-clip/tunnels/*.json` and is persisted across daemon restarts. If you kill the SSH process by hand or upgrade `cc-clip` while a tunnel is running, the state file may reference a stale PID. On daemon startup, `LoadAndStartAll()` sweeps stale PIDs automatically, but if you want to clean up without restarting:

**Fix:**

```bash
# Stop and mark stopped (keeps the state file but marks it inactive)
cc-clip tunnel down myserver

# Remove the tunnel entirely (deletes the state file)
cc-clip tunnel remove myserver

# Multi-daemon: target a specific daemon when more than one state file
# exists for the same host (each daemon owns its own tunnel on its own port)
cc-clip tunnel down myserver --port 18444
cc-clip tunnel remove myserver --port 18444
```

As a last resort, you can delete state files directly (they are plain JSON):

```bash
ls ~/.cache/cc-clip/tunnels/
rm ~/.cache/cc-clip/tunnels/myserver-<local-port>-*.json
```

Only do this when the daemon is stopped — otherwise the running manager may rewrite the file.

---


## SwiftBar Plugin Shows Errors

**Symptom:** The SwiftBar menu-bar entry shows `⊘ Tunnel list failed` or `jq not found`, or clicking a Start/Stop entry does nothing.

**Fix checklist:**

```bash
# 1. jq is required for JSON parsing
brew install jq

# 2. cc-clip must be on PATH visible to SwiftBar (not just your shell)
which cc-clip   # confirm a usable path

# 3. The tunnel-control token file must exist (generated by `cc-clip serve`)
ls -la ~/.cache/cc-clip/tunnel-control.token
# Expected: a 0600 file. If missing, restart the daemon: cc-clip serve
# (or `cc-clip service uninstall && cc-clip service install` for launchd).

# 4. Daemon must be running on your local daemon port
#    (default 18339, or your CC_CLIP_PORT / --port value)
curl -s http://127.0.0.1:<local-daemon-port>/health
# Expected: {"status":"ok"}
```

The plugin's dropdown maps common error strings (401, 404, connection refused) to user-facing hints. If you see the hint `daemon may be too old (no /tunnels routes)`, upgrade `cc-clip` and restart the local daemon so the `/tunnels` HTTP routes are registered.

---

## Troubleshooting Assumes cc-clip Does Not Edit `~/.ssh/config`

As of the current release, `cc-clip setup` / `connect` / `uninstall --host` do **not** read or write `~/.ssh/config` at all. The local cc-clip daemon owns the reverse tunnel directly (it spawns `ssh -N -R <remote>:127.0.0.1:<local> <host>` with `BatchMode=yes`, `ControlMaster=no`, `ControlPath=none`), so no SSH-config directives are needed. If a check below suggests running a specific `cc-clip` command, it is safe to do so — none of them will rewrite your SSH config.

Users upgrading from a pre-daemon-tunnel release must delete the legacy `# >>> cc-clip managed host: … >>>` block manually — see "Upgrade Leftover: Legacy Managed Block in `~/.ssh/config`" above. cc-clip ships no migration scaffolding for this.
