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

**Fix:** cc-clip does **not** migrate this block for you during the normal CLI flows. This is an intentional manual step; `setup`, `connect`, and `uninstall` leave old managed blocks alone. Open `~/.ssh/config`, locate the block, and delete everything between (and including) the two marker lines:

```
# >>> cc-clip managed host: <alias> >>>
...
# <<< cc-clip managed host: <alias> <<<
```

Save and reconnect. The warning will not return.

The one exception is the destructive local purge script: `scripts/uninstall-local.sh` removes legacy managed blocks too, but only when `~/.ssh/config` is a regular file. Symlinked SSH configs are preserved with a warning.

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
# Note: the `-o ControlMaster=no -o ControlPath=none` flags above come
# from the daemon's argv (it builds the tunnel ssh invocation itself),
# NOT from your ~/.ssh/config. cc-clip does not write those directives
# to your ssh config. See "Daemon-owned Tunnel vs ssh_config" below.
#
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

## `~/.ssh/config` Edits: Scope and Limits

`cc-clip setup` / `connect` / `connect --token-only` writes exactly **one** thing to `~/.ssh/config`: a cc-clip-managed `SetEnv` block wrapped in marker comments, inserted inside your existing `Host <alias>` entry when that host does not already contain a user-authored `SetEnv`. This block is what makes multi-laptop-on-shared-account work (see README: "Multi-laptop on a Shared Remote Account"):

```
Host myalias
  HostName srv.example.com
  User shareduser
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shareduser/.cache/cc-clip/peers/<peerID>
  # <<< cc-clip SetEnv (do not edit) <<<
```

The self-release path `cc-clip uninstall --host <host> --peer` removes this marker block after the remote cleanup succeeds. Plain `cc-clip uninstall --host <host>` preserves the local block because it only removes the remote PATH marker and does not tear down the workstation's peer reservation. If the SSH cleanup step fails, cc-clip warns and preserves the local block so it does not desynchronize a still-installed remote shim. cc-clip does not rewrite unrelated `SetEnv` lines you already manage by hand.

The local daemon still owns the **reverse tunnel** itself: it spawns `ssh -N -R <remote>:127.0.0.1:<local> <host>` with `BatchMode=yes`, `ControlMaster=no`, `ControlPath=none`, independently of your `~/.ssh/config`. cc-clip does **not** write `RemoteForward`, `ControlMaster`, `ControlPath`, or any other tunnel-related directive to your SSH config.

Users upgrading from a pre-daemon-tunnel release must delete the legacy `# >>> cc-clip managed host: … >>>` block manually — see "Upgrade Leftover: Legacy Managed Block in `~/.ssh/config`" above. cc-clip ships no migration scaffolding for this.

### `cc-clip setup` / `connect` / `connect --token-only` warns: "no `Host <alias>` block found"

`cc-clip setup <host>`, `cc-clip connect <host>`, and `cc-clip connect <host> --token-only` require that `~/.ssh/config` already contains a literal `Host <host>` entry. If it doesn't, the rest of the flow still succeeds but prints a warning and skips the SetEnv injection — which means a single-laptop run works, but a second laptop sharing the same remote Unix account will trample the first laptop's shim port. Add the entry and re-run:

```
Host myalias
  HostName srv.example.com
  User shareduser
```

Then `cc-clip setup myalias`, `cc-clip connect myalias`, or `cc-clip connect myalias --token-only` again — the SetEnv block will be appended inside it.

### `cc-clip setup` / `connect` warns: "no literal `Host <alias>` block in top-level ssh_config; cc-clip does not walk Include directives"

This variant of the missing-Host warning fires when the top-level `~/.ssh/config` contains `Include` directives (e.g. `Include ~/.ssh/config.d/*`) but no literal `Host <alias>` block that matches the alias. cc-clip deliberately does **not** follow `Include` chains — walking them would let a path-traversal exploit in an included file rewrite an unrelated one. The alias might legitimately live in an included fragment, but cc-clip cannot safely confirm that, so it fails loud.

The fix is one of:

- **Inline the Host block** into `~/.ssh/config` itself so cc-clip can see it:

  ```
  Host myalias
    HostName srv.example.com
    User shareduser
  ```

- **Move the `Host myalias` stanza into the top-level `~/.ssh/config` and re-run `cc-clip setup` / `connect`.** cc-clip intentionally does not walk `Include` directives, and the warning does not print a paste-ready `SetEnv ...` line. The supported recovery is to add a literal `Host myalias` block to the top-level file, then rerun so cc-clip can write and maintain its managed SetEnv marker block there.

- **Disable the Include** and re-run setup. Not recommended if your included files carry other hosts you need.

### `cc-clip setup` / `connect` warns: "Host is matched only by a wildcard or negation pattern"

cc-clip refuses to inject `SetEnv` into `Host` blocks whose only applicable token is a wildcard (`Host *`, `Host *.example.com`, `Host srv?`) or a negation (`!alias`). Wildcard patterns match multiple hosts; writing per-peer `CC_CLIP_*` there would leak your laptop's token path and port to every host that matches. Negation blocks have semantics that don't map cleanly to a per-alias injection. The fix is to add a **literal** `Host <alias>` entry alongside (or before) the existing block:

```
# NEW — literal entry for cc-clip to inject into
Host myalias
  HostName srv.example.com

# existing — still applies to everything that matches the glob
Host *.example.com
  User shareduser
```

OpenSSH processes all matching `Host` blocks and applies the first value it sees for each option, so layering a literal block over a glob is safe.

`Match host <alias>` blocks are also skipped: `SetEnv` inside a `Match` block has surprising scoping rules, and cc-clip restricts injection to plain `Host` blocks to keep the behaviour predictable.

### `cc-clip setup` / `connect` warns: "Host already has a user-authored SetEnv directive"

OpenSSH only honors the first `SetEnv` directive it sees for a host. If your target `Host <alias>` stanza already has a `SetEnv`, cc-clip refuses to inject its own second `SetEnv` line because that would either shadow your existing vars or be shadowed by them. In this case, merge the two cc-clip vars into your existing first `SetEnv` manually:

```sshconfig
Host myalias
  HostName srv.example.com
  SetEnv FOO=bar CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shareduser/.cache/cc-clip/peers/<peerID>
```

---

## SSH Config Error Sentinels (`ErrOnlyGlobMatch`, `ErrHostBlockInInclude`, `ErrSymlinkConfig`)

The three error names below come straight from `internal/sshconfig` — if `cc-clip setup` / `connect` / `uninstall --peer` prints one of these sentinel strings in a warning or error, the linked section explains the fix.

### `ErrOnlyGlobMatch` — alias only matches a wildcard / negation `Host` block

**Symptom:** `cc-clip setup myalias` or `cc-clip connect myalias` prints a warning mentioning `ErrOnlyGlobMatch`, or the human message *"alias is matched only by a wildcard or negation `Host` pattern"*.

**Cause:** The only `Host` stanza in `~/.ssh/config` that would apply to `myalias` is a wildcard (`Host *`, `Host *.example.com`, `Host srv?`) or negation (`!myalias`). cc-clip refuses to write per-peer `CC_CLIP_*` SetEnv into a wildcard block because it would leak your laptop's token path and port to every host that matches.

**Fix:** Add a dedicated literal `Host <alias>` block. See *"Host is matched only by a wildcard or negation pattern"* above for a copy-paste-ready example. OpenSSH applies the first value for each option across all matching blocks, so layering a literal block over an existing wildcard is safe.

### `ErrHostBlockInInclude` — `Host <alias>` lives in an `Include`d file

**Symptom:** `cc-clip setup myalias` prints a warning mentioning `ErrHostBlockInInclude`, or the human message *"no literal `Host <alias>` block in top-level ssh_config; cc-clip does not walk Include directives"*.

**Cause:** The top-level `~/.ssh/config` has one or more `Include` directives (e.g. `Include ~/.ssh/config.d/*`) but no literal `Host <alias>` block that matches your alias. cc-clip intentionally does **not** walk `Include` chains — walking them would let a path-traversal exploit in an included file rewrite an unrelated one — so the alias might legitimately live in a fragment cc-clip cannot see.

**Fix:** Inline the `Host <alias>` stanza into the top-level `~/.ssh/config` and rerun setup. See *"no literal `Host <alias>` block in top-level ssh_config"* above for the full explanation. Disabling the `Include` also works if you don't rely on the other hosts it carries.

### `ErrSymlinkConfig` — `~/.ssh/config` is a symlink

**Symptom:** `cc-clip setup` / `connect` / `uninstall --peer` prints a warning mentioning `ErrSymlinkConfig`, or the human message *"refusing to edit symlinked ~/.ssh/config"*.

**Cause:** `~/.ssh/config` is a symlink (common with dotfile managers). cc-clip refuses to replace it because an atomic rewrite would replace the symlink itself with a regular file, detaching the path from your dotfiles target. Same refusal applies when removing the managed SetEnv block during `uninstall --peer`.

**Fix:** Decide how you want cc-clip's managed SetEnv block to be stored:

- **Easiest:** resolve the symlink to a regular file (`cp -L ~/.ssh/config ~/.ssh/config.real && rm ~/.ssh/config && mv ~/.ssh/config.real ~/.ssh/config`) and rerun setup. cc-clip will then manage the block in place. You lose the dotfile link.
- **Dotfile-friendly:** edit the symlink's target file through your dotfile manager and add the SetEnv block by hand (copy the template from the *"Multi-laptop on a Shared Remote Account"* section of the README). cc-clip will not touch it on subsequent runs, but it also won't refresh it — rerun your dotfile sync when the per-peer port/state dir changes.

---

## Notifications Silently Drop — Inspect `notify-health.log` and `CC_CLIP_STRICT`

The remote `cc-clip-hook` script is fire-and-forget by design: any failure posting to the local daemon's `/notify` endpoint is logged but never blocks Claude Code. If notifications aren't arriving on your workstation:

```bash
# On the remote: inspect the last few hook failures.
# Each line is: <UTC timestamp> FAIL http=<status>
# http=401 → notify nonce mismatch (re-run `cc-clip connect <host>` to resync)
# http=404 → daemon is running an old build without /notify (upgrade cc-clip)
# http=000 → connection never reached the daemon (tunnel down, daemon down,
#            curl missing on the remote, or another transport failure)
tail -n 20 "${CC_CLIP_STATE_DIR:-$HOME/.cache/cc-clip}/notify-health.log"
```

If you want the hook to fail loudly for debugging (non-2xx → exit 1 + stdout line), set `CC_CLIP_STRICT=1` in the environment Claude Code invokes the hook from. A normal `cc-clip connect <host>` run with notifications enabled and the tunnel active uses the same strict probe once during setup; `--no-notify` and `--no-tunnel` skip that validation.
