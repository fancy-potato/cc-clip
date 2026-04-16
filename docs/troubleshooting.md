# Troubleshooting Guide

## Quick Diagnostics

```bash
cc-clip doctor --host myserver
```

## Step-by-Step Verification

If image paste isn't working, run these checks **in order** to isolate the problem:

```bash
# 1. Local: Is the daemon running?
curl -s http://127.0.0.1:18339/health
# Expected: {"status":"ok"}

# 2. Remote: Is the tunnel forwarding?
ssh myserver "curl -s http://127.0.0.1:18339/health"
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

**Symptom:** `cc-clip setup alice@example.com`, `cc-clip connect alice@example.com`, or `cc-clip doctor --host alice@example.com` fails with an error about a missing exact host block, or no SSH config changes are applied.

**Cause:** `cc-clip` manages an exact `Host <alias>` block in `~/.ssh/config`. It does not rewrite a raw `user@host` destination.

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
cc-clip doctor --host myserver
ssh myserver
```

---

## Earlier Wildcard Stanzas Override the Managed Host

**Symptom:** `cc-clip setup myserver` completes, but the resulting SSH session still uses unexpected SSH settings, or the tunnel does not behave as expected.

**Cause:** OpenSSH uses the first matching value for most options. If an earlier `Host *` or `Host *.corp` stanza sets `RemoteForward`, `ControlMaster`, or `ControlPath`, that value can win before the exact host entry is reached.

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

---

## SSH ControlMaster Breaks RemoteForward

**Symptom:** `cc-clip connect` reports "tunnel verified", but the tunnel doesn't work in your interactive SSH session. `curl -s http://127.0.0.1:18339/health` hangs on the remote.

**Cause:** If you use SSH `ControlMaster auto` (connection multiplexing), the first SSH connection becomes the "master". All subsequent connections **reuse the master** — even if you later add `RemoteForward` to your config. The old master connection does not have the port forwarding, so the tunnel silently fails.

**Fix:** `cc-clip setup` automatically adds `ControlMaster no` for your host. If you configured SSH manually:

```
# ~/.ssh/config
Host myserver
    HostName 10.x.x.x
    User myuser
    RemoteForward 18339 127.0.0.1:18339
    ControlMaster no
    ControlPath none
```

This ensures every SSH connection creates a fresh tunnel. The trade-off is slightly slower connection setup (no multiplexing), but it guarantees `RemoteForward` works reliably.

---

## Stale sshd Process Blocks RemoteForward

**Symptom:** `ssh myserver` shows `Warning: remote port forwarding failed for listen port 18339`. The tunnel never works regardless of how many times you reconnect.

**Cause:** A previous SSH session left a stale `sshd` child process on the remote server that is still holding port 18339. New SSH connections cannot bind `RemoteForward` to a port that's already in use.

**Diagnosis (on remote):**

```bash
sudo ss -tlnp | grep 18339
# Shows: sshd,pid=XXXXX listening on 18339
```

**Fix:**

```bash
# On remote: kill the stale sshd process
sudo kill <PID>

# Then reconnect from local
ssh myserver
curl -s http://127.0.0.1:18339/health
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
