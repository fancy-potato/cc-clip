# Troubleshooting Guide

## Quick Diagnostics

```bash
cc-clip doctor --host myserver
```

## How Port Selection Works

`cc-clip` has two distinct sources in play for tunnel ports, and they are used for different purposes:

- Allocation source of truth: the remote peer registry. `cc-clip connect` reserves the remote listen port there.
- Runtime source for `cc-clip tunnel up`: the local managed `RemoteForward` block in `~/.ssh/config`.
- Local daemon port: the local HTTP daemon listens on `18339` by default, or on the current `CC_CLIP_PORT` / `--port` value when overridden.

`connect` bridges those two worlds by reserving the remote port first, then writing the resulting `RemoteForward <remote-port> 127.0.0.1:<local-port>` into the local managed block.

In the current design, that managed block's `<local-port>` is intended to be the owning local daemon's listening port. Persistent tunnels do not forward to an arbitrary separate local target; the tunnel's `local_port` and the owning daemon port are the same value.

`cc-clip tunnel up` does not SSH back to the remote to re-read the registry. It reads the local managed block that `connect` already synced.

`cc-clip doctor --host` is the command that checks consistency: it compares the remote peer registry against the local managed block, then verifies the effective SSH config with `ssh -G`.

## Step-by-Step Verification

If image paste isn't working, run these checks **in order** to isolate the problem:

```bash
# Replace <local-daemon-port> with your current local daemon port
# (default 18339, or your CC_CLIP_PORT / --port value).

# 1. Local: Is the daemon running?
curl -s http://127.0.0.1:<local-daemon-port>/health
# Expected: {"status":"ok"}

# 2. Remote: Is the tunnel forwarding?
# Replace <remote-port> with the managed RemoteForward listen port.
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

**Symptom:** `cc-clip setup alice@example.com`, `cc-clip connect alice@example.com`, or `cc-clip tunnel up alice@example.com` fails with an error about a missing exact host block, or `tunnel up` cannot auto-detect ports.

**Cause:** `cc-clip` manages and auto-detects an exact `Host <alias>` block in `~/.ssh/config`. It does not rewrite a raw `user@host` destination, and it does not manage shared multi-pattern stanzas such as `Host prod staging`.

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

`cc-clip doctor --host myserver` is optional here and read-only. It does not rewrite `~/.ssh/config`, and it is the command that compares the remote peer registry to the local managed block when you suspect port drift.

---

## Shared `Host prod staging` Stanzas Are Unsupported

**Symptom:** `cc-clip setup staging`, `cc-clip connect staging`, or `cc-clip tunnel up staging` fails even though `staging` is listed in a shared SSH config stanza.

**Cause:** `cc-clip` requires a dedicated exact `Host <alias>` stanza for the alias it manages. Shared multi-pattern stanzas such as `Host prod staging` are unsupported by design.

**Fix:** Split the shared stanza into separate exact aliases:

```sshconfig
Host prod
    HostName prod.example.com
    User alice

Host staging
    HostName staging.example.com
    User alice
```

Then run `cc-clip` against the exact alias you want to manage, for example `cc-clip setup staging`.

---

## Earlier Wildcard Stanzas Override the Managed Host

**Symptom:** `cc-clip setup myserver` completes, but the resulting SSH session still uses unexpected SSH settings, or the tunnel does not behave as expected.

**Cause:** OpenSSH uses the first matching value for most options. If an earlier `Host *` or `Host *.corp` stanza sets `RemoteForward`, `ControlMaster`, or `ControlPath`, that value can win before the exact host entry is reached. `cc-clip` only edits the exact `Host myserver` block; it does not rewrite or reorder wildcard blocks for you.

**Current supported layout:**

- `Host myserver` exists as its own exact stanza
- Conflicting wildcard stanzas come later in the file, or they do not set `RemoteForward`, `ControlMaster`, or `ControlPath`

**Problematic layout:**

- There is no exact `Host myserver` block, only wildcard stanzas
- A `Host *` / `Host *.corp` stanza appears earlier and already sets `RemoteForward`, `ControlMaster`, or `ControlPath`

**Diagnosis:**

```bash
ssh -G myserver | grep -E '^(hostname|user|remoteforward|controlmaster|controlpath) '
```

Check whether the output matches the alias you intend `cc-clip` to manage.

**Fix:** Keep your exact host block in `~/.ssh/config` and place conflicting wildcard directives after it, or remove those directives from the earlier wildcard stanza for this host path.

Recommended structure:

```sshconfig
Host myserver
    HostName example.com
    User alice

Host *
    ServerAliveInterval 30
```

Problematic structure:

```sshconfig
Host *
    ControlMaster auto
    ControlPath ~/.ssh/cm-%r@%h:%p
    RemoteForward <remote-port> 127.0.0.1:<local-port>

Host myserver
    HostName example.com
    User alice
```

In that case, manually reorder the file so `Host myserver` comes first, then re-run:

```bash
cc-clip setup myserver
ssh -G myserver | grep -E '^(hostname|user|remoteforward|controlmaster|controlpath) '
```

If the effective values still look wrong, remove the conflicting directive from the wildcard stanza for this host path and reconnect with a fresh `ssh myserver` session.

---

## SSH ControlMaster Breaks RemoteForward

**Symptom:** `cc-clip connect` reports "tunnel verified", but the tunnel doesn't work in your interactive SSH session. `curl -s http://127.0.0.1:<remote-port>/health` hangs on the remote.

**Cause:** If you use SSH `ControlMaster auto` (connection multiplexing), the first SSH connection becomes the "master". All subsequent connections **reuse the master** — even if you later add `RemoteForward` to your config. The old master connection does not have the port forwarding, so the tunnel silently fails.

**Fix:** `cc-clip setup` automatically adds `ControlMaster no` for your host. If you configured SSH manually:

```
# ~/.ssh/config
Host myserver
    HostName 10.x.x.x
    User myuser
    RemoteForward <remote-port> 127.0.0.1:<local-port>
    ControlMaster no
    ControlPath none
```

This ensures every SSH connection creates a fresh tunnel. The trade-off is slightly slower connection setup (no multiplexing), but it guarantees `RemoteForward` works reliably.

---

## Stale sshd Process Blocks RemoteForward

**Symptom:** `ssh myserver` shows `Warning: remote port forwarding failed for listen port <remote-port>`. The tunnel never works regardless of how many times you reconnect.

**Cause:** A previous SSH session left a stale `sshd` child process on the remote server that is still holding the managed remote port. New SSH connections cannot bind `RemoteForward` to a port that's already in use.

**Diagnosis (on remote):**

```bash
# First discover the managed RemoteForward mapping:
ssh -G myserver | awk '$1 == "remoteforward" {print $2, $3; exit}'

# Then check the remote listen port:
sudo ss -tlnp | grep <remote-port>
# Shows: sshd,pid=XXXXX listening on <remote-port>
```

**Fix:**

```bash
# On remote: kill the stale sshd process
sudo kill <PID>

# Then reconnect from local
ssh myserver
curl -s http://127.0.0.1:<remote-port>/health
# Expected: {"status":"ok"}
```

**Prevention:** Update to the latest cc-clip. The `connect` command uses `ClearAllForwardings=yes` for its internal SSH session, so it never competes for the RemoteForward port.

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

**Cause:** The SSH process is crashing on every attempt. Common reasons: the remote `sshd` is down, the managed `RemoteForward` port is already bound on the remote by a stale `sshd` child (see the stale sshd section above), the host key changed, or SSH authentication is failing (see the `connecting` section above).

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

# 3. Inspect the managed RemoteForward mapping for this alias
ssh -G myserver | awk '$1 == "remoteforward" {print $2, $3; exit}'
# Example output: 19001 127.0.0.1:18444

# 4. Run an approximation of the ssh command the tunnel manager uses, so
#    errors surface. NOTE: the real tunnel manager *also* reads
#    `ssh -G <host>` and re-applies any options it finds there (excluding
#    safety-critical ones like BatchMode, ControlMaster/Path, ServerAlive*,
#    which cc-clip owns). If your failure mode differs from the below, run
#    `ssh -G myserver` to see what extra options (ProxyCommand, IdentityFile,
#    HostName overrides) are merged in.
ssh -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=15 -o ServerAliveCountMax=3 \
    -o ControlMaster=no -o ControlPath=none \
    -R <remote-port>:127.0.0.1:<local-port> myserver
# Interpret the output:
# - "Permission denied"         → see "Stuck in connecting" (auth)
# - "remote port forwarding failed for listen port" → stale sshd on remote
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

## Persistent Tunnel: `Warning: remote port forwarding failed for listen port`

**Symptom:** When you open an interactive `ssh myserver`, SSH prints `Warning: remote port forwarding failed for listen port <remote-port>`.

**Cause:** When `cc-clip tunnel up myserver` is running, the daemon's SSH process already holds the alias's managed `RemoteForward` port. The managed SSH config block still carries its own `RemoteForward` as a fallback, so your interactive session tries (and fails) to bind the same port.

**This is harmless.** The persistent tunnel is already providing the forward. Clipboard and notifications continue to work. The warning disappears when the persistent tunnel is down (the interactive session's RemoteForward takes over instead).

If the noise bothers you, stop the persistent tunnel (`cc-clip tunnel down myserver`) before opening the session, or suppress the warning per-invocation with `ssh -o LogLevel=error myserver`.

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

## `~/.ssh/config` Safety Constraints

`cc-clip setup`, `cc-clip connect`, and `cc-clip uninstall --host` rewrite the exact `Host <alias>` block in `~/.ssh/config`. The rewrite is a read → tmp-file write → atomic rename. It is designed for the common case (single user, one regular file at `~/.ssh/config`, no concurrent editors). `cc-clip tunnel up` reads the managed block to discover ports, and `cc-clip doctor` only checks and reports. The sections below cover boundaries you should know about.

### Do Not Edit `~/.ssh/config` While a Config-Writing cc-clip Command Is Running

**Symptom:** You edited `~/.ssh/config` in an editor while a config-writing cc-clip command was in flight and your edit was silently lost.

**Cause:** cc-clip reads the file, computes the new contents, and renames a tmp file over the original. It does not take an advisory lock around that read-modify-write cycle. If your editor's save lands between the read and the rename, the new tmp file overwrites it.

**Fix:** Restore from `~/.ssh/config.cc-clip-backup` (cc-clip writes this sidecar on its first rewrite and preserves it afterwards) or from your own backup, then re-apply the edit after cc-clip finishes. Do not run config-writing cc-clip commands while any editor has unsaved changes to `~/.ssh/config`.

**Planned behavior, not a bug:** cc-clip treats "sole editor of `~/.ssh/config`" as a product boundary. Retrofitting file locking would complicate the rewrite path without changing the expected usage pattern.

---

### Line Endings Are Normalized by Majority Vote

**Symptom:** Your `~/.ssh/config` used mixed CRLF/LF endings before cc-clip ran, and now every line shares the same ending.

**Cause:** cc-clip counts CRLF and LF line terminators and picks CRLF only when its count strictly exceeds LF. Balanced or mostly-LF files come out all-LF. A file that was evenly mixed will collapse to LF.

**Fix:** If you need CRLF preserved, make sure `~/.ssh/config` is already majority-CRLF before cc-clip runs. Most Windows-native editors default to CRLF, which keeps the file in CRLF after cc-clip's rewrite. There is no flag to force a specific line ending.

---

### Quoted `Host` Tokens Lose Their Quotes

**Symptom:** Your config had `Host "myserver"` with quotes and cc-clip rewrote it as `Host myserver`.

**Cause:** cc-clip strips surrounding single or double quotes when parsing `Host` tokens and writes the managed block using unquoted `Host <alias>`. ssh accepts both forms for simple tokens, so connectivity is unaffected.

**Fix:** Use the unquoted form, or keep the quoted entry in a `Host` block cc-clip does not manage (for example, a different alias with no cc-clip managed marker inside).

---

### `Include` Directives Are Tolerated, But Only the Main File Is Managed

**Symptom:** You use `Include ~/.ssh/config.d/*` (or similar) to split SSH configuration across files and are unsure how `cc-clip setup`, `cc-clip connect`, or `cc-clip tunnel up` will behave.

**Cause:** cc-clip reads and rewrites `~/.ssh/config` itself. `Include` directives are preserved and do not cause a refusal, but cc-clip will not edit included files for you. If the `Host <alias>` block cc-clip is managing lives only behind an `Include`, changes cc-clip makes to `~/.ssh/config` will add a second managed block in the main file rather than modifying the included stanza.

**Fix:** Keep the managed alias block in `~/.ssh/config` itself when you want `cc-clip` to update it. Treat included files as manual territory. Other unrelated `Host` blocks can continue to live behind `Include` without changes.

---

### The `# >>> cc-clip managed host: … >>>` Comment Is Reserved

**Symptom:** You copy-pasted the cc-clip managed marker into `~/.ssh/config` as documentation and now cc-clip's parser is confused — either it reports the alias as unmanaged, or it drops lines after the comment.

**Cause:** cc-clip detects its managed block by exact line match on the start/end marker strings. A line whose trimmed content equals the start marker is treated as the beginning of a managed block regardless of where it appears in the file.

**Fix:** Remove the stray comment line, or replace the literal marker with a different wording (for example, `# cc-clip block goes below`). Re-run `cc-clip setup myserver` to re-apply the managed block cleanly.

---

### `~/.ssh/config` Is a Symlink

**Symptom:** You keep `~/.ssh/config` as a symlink into a dotfiles repository and want to know what `cc-clip setup` / `connect` / `uninstall --host` will rewrite.

**Behavior:** cc-clip follows the symlink and rewrites the resolved target file in place. The `~/.ssh/config.cc-clip-backup` sidecar still refuses to be a symlink; `O_EXCL|O_NOFOLLOW` is used on its creation.

**Supported configuration:**

- `~/.ssh/config` is owned by your user, whether it is a real file or a symlink into a dotfiles repo you control.
- You do not share `~/.ssh/config` with another user or with a service account.

**Fix:** no extra fix is required. If you want a dry run, back up the resolved target file before rerunning the cc-clip command.

---

### Windows: Junctions and Reparse Points on `~/.ssh/config`

**Symptom:** On Windows, cc-clip behaves unexpectedly when `~/.ssh/config` (typically `%USERPROFILE%\.ssh\config`) sits under a directory junction or reparse point.

**Behavior:** On Windows, cc-clip rewrites the resolved target file. Some reparse-point types are not flagged as symlinks by `Lstat`, so the write proceeds directly against whatever the reparse point resolves to.

**Supported configuration:** Keep `~/.ssh/config` as a regular file under `%USERPROFILE%\.ssh`. Do not place it under a junction or reparse point that redirects across volumes, ACL boundaries, or user profiles.

**If you must use a junction:** keep a manual backup of `~/.ssh/config` before the first `cc-clip setup`, and verify the final path resolves to a file you own. cc-clip's `~/.ssh/config.cc-clip-backup` sidecar refuses to follow symlinks and is still a secondary safety net, not the primary one.

---

### Restoring From `~/.ssh/config.cc-clip-backup`

On the first rewrite, `cc-clip` writes `~/.ssh/config.cc-clip-backup` as a one-shot copy of your pristine pre-cc-clip config and preserves it across subsequent runs. If a later rewrite produces an unwanted change (reformatted line endings, dropped `Host` quotes, or a clobbered concurrent edit), you can restore from that sidecar:

```bash
# Inspect the backup first (UNIX)
diff -u ~/.ssh/config ~/.ssh/config.cc-clip-backup

# Restore the pristine version, then re-apply cc-clip's managed block
cp ~/.ssh/config.cc-clip-backup ~/.ssh/config
cc-clip setup myserver
```

On Windows PowerShell:

```powershell
Copy-Item -LiteralPath "$env:USERPROFILE\.ssh\config.cc-clip-backup" -Destination "$env:USERPROFILE\.ssh\config" -Force
cc-clip setup myserver
```

If the backup does not exist, `cc-clip` was not the first tool to touch the file after installation. Check your own version control (dotfiles repo) or recreate the file manually.
