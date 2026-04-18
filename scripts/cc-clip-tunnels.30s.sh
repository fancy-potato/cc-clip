#!/bin/bash
#
# <xbar.title>cc-clip Tunnels</xbar.title>
# <xbar.version>v1.0</xbar.version>
# <xbar.author>cc-clip</xbar.author>
# <xbar.author.github>fancy-potato</xbar.author.github>
# <xbar.desc>Manage cc-clip persistent SSH tunnels</xbar.desc>
# <xbar.dependencies>cc-clip,jq</xbar.dependencies>
#
# Install: copy or symlink this file into your SwiftBar plugin directory.
#   ln -s "$(pwd)/scripts/cc-clip-tunnels.30s.sh" \
#       ~/Library/Application\ Support/SwiftBar/Plugins/
#
# Requires: cc-clip in PATH, jq

set -euo pipefail

# SwiftBar plugins run under a minimal environment; prepend the common
# package-manager prefixes (Homebrew ARM + Intel, MacPorts, Nix single-user and
# multi-user) and keep /usr/bin + /bin so even a barebones install still
# resolves `cc-clip` and `jq`. Users on non-standard prefixes can prepend their
# own via CC_CLIP_EXTRA_PATH.
#
# Validate CC_CLIP_EXTRA_PATH before splicing it in. Without validation a
# hostile login item (or a typo) that exports `CC_CLIP_EXTRA_PATH=/tmp/evil`
# — or any value containing shell metacharacters or non-absolute path
# components — would redirect `cc-clip` / `jq` lookups to attacker-planted
# binaries. Accept only colon-separated absolute paths containing a
# conservative character set; drop anything else with a warning.
cc_clip_extra_path=""
# When CC_CLIP_EXTRA_PATH is rejected, we still want the user to know —
# SwiftBar discards stderr, so a silent fail-open can hide a broken PATH
# rotation that leaves `cc-clip`/`jq` unreachable.  Capture the bad value
# here and surface it as a visible dropdown line after the status rows.
cc_clip_extra_path_warning=""
if [ -n "${CC_CLIP_EXTRA_PATH:-}" ]; then
    cc_clip_extra_path_valid=1
    IFS=':' read -r -a _cc_clip_extra_path_parts <<<"$CC_CLIP_EXTRA_PATH"
    for _part in "${_cc_clip_extra_path_parts[@]}"; do
        if [ -z "$_part" ]; then
            continue
        fi
        # Require absolute path with a conservative character set. Rejects
        # `;`, backticks, `$`, spaces, newlines — anything shell-expansion-dangerous.
        # `~` is accepted as a literal byte (bash does not expand `~` mid-path
        # in a quoted string, so e.g. `/opt/foo~bar/bin` passes).
        if ! [[ "$_part" =~ ^/[A-Za-z0-9._/~@+-]+$ ]]; then
            cc_clip_extra_path_valid=0
            break
        fi
    done
    unset _cc_clip_extra_path_parts _part
    if [ "$cc_clip_extra_path_valid" -eq 1 ]; then
        cc_clip_extra_path="$CC_CLIP_EXTRA_PATH:"
    else
        echo "cc-clip: ignoring invalid CC_CLIP_EXTRA_PATH=$CC_CLIP_EXTRA_PATH" >&2
        cc_clip_extra_path_warning="ignored invalid CC_CLIP_EXTRA_PATH"
    fi
    unset cc_clip_extra_path_valid
fi
export PATH="${cc_clip_extra_path}/opt/homebrew/bin:/usr/local/bin:/opt/local/bin:/nix/var/nix/profiles/default/bin:$HOME/.nix-profile/bin:$HOME/.local/bin:$HOME/go/bin:/usr/bin:/bin:$PATH"
unset cc_clip_extra_path
CC_CLIP_PORT="${CC_CLIP_PORT:-18339}"

sanitize_menu_text() {
    printf '%s' "$1" | tr '\r\n' '  ' | sed 's@[|]@ / @g; s/[[:space:]][[:space:]]*/ /g; s/^ //; s/ $//'
}

# Reject hosts that would break out of the SwiftBar bash=/paramN= argument
# encoding. SSH host aliases must not contain spaces, =, |, shell metachars.
# The character set mirrors tunnel.ValidateSSHHost on the daemon side so a
# host the daemon would accept does not silently disable its action button.
is_safe_host() {
    [[ "$1" =~ ^[A-Za-z0-9._:@-]+$ ]]
}

# Numeric guard — empty / malformed values must not pass `-gt 0` under set -u.
is_positive_int() {
    [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -gt 0 ]
}

# Reject bogus CC_CLIP_PORT values (e.g. "foo; rm -rf ~") before they reach
# `cc-clip` and get echoed back into error hints. Falling back to the default
# keeps the plugin usable when the environment is misconfigured.
if ! is_positive_int "$CC_CLIP_PORT"; then
    CC_CLIP_PORT=18339
fi

CC_CLIP=$(command -v cc-clip 2>/dev/null || true)
if [ -z "$CC_CLIP" ]; then
    echo "⊘"
    echo "---"
    echo "cc-clip not found | color=#999999"
    exit 0
fi

JQ=$(command -v jq 2>/dev/null || true)
if [ -z "$JQ" ]; then
    echo "⊘"
    echo "---"
    echo "jq not found (brew install jq) | color=#999999"
    exit 0
fi

# Fetch tunnel list as JSON. `set -e` would abort on the non-zero exit, so
# capture both status and output explicitly via `|| true` and inspect $?.
# Capture stderr SEPARATELY: a stderr log line printed alongside valid JSON
# on stdout would otherwise corrupt $tunnels and break every jq filter below.
err_tmp=$(mktemp -t cc-clip-tunnels.XXXXXX 2>/dev/null || mktemp 2>/dev/null || echo "/tmp/cc-clip-tunnels.$$.err")
trap 'rm -f "$err_tmp"' EXIT INT TERM
tunnels_status=0
tunnels=$("$CC_CLIP" tunnel list --port "$CC_CLIP_PORT" --json 2>"$err_tmp") || tunnels_status=$?
tunnels_err=$(cat "$err_tmp" 2>/dev/null || true)
if [ "$tunnels_status" -ne 0 ]; then
    echo "⊘ | sfimage=network color=#FF3B30"
    echo "---"
    echo "Tunnel list failed | color=#FF3B30"
    # Match hints against stderr only. cc-clip's CLI writes human-readable
    # error copy to stderr; stdout may still carry a partial JSON tunnel list
    # whose `last_error` fields legitimately contain strings like "connection
    # refused" for an SSH peer that's down — matching against stdout would
    # misfire the "daemon not running" hint in that case.
    hint_blob="$tunnels_err"
    case "$hint_blob" in
        *401*|*"tunnel token"*|*"tunnel-control"*|*"tunnel control token unavailable"*|*"token unavailable"*|*"unauthorized"*|*"Unauthorized"*)
            echo "Hint: tunnel-control token missing or stale. | color=#FF9500"
            echo "Run \`cc-clip serve\` (rotate if needed) after updating. | color=#999999"
            ;;
        *404*|*"not found"*|*"no such"*)
            echo "Hint: daemon may be too old (no /tunnels routes). | color=#FF9500"
            echo "Upgrade cc-clip and restart the local daemon/service. | color=#999999"
            ;;
        *"connection refused"*|*"dial tcp"*)
            echo "Hint: daemon not running on port ${CC_CLIP_PORT}. | color=#FF9500"
            echo "Run \`cc-clip serve\` or \`cc-clip service install\`. | color=#999999"
            ;;
    esac
    # Prefer stderr (where cc-clip writes human error text) over stdout (which
    # may be empty JSON on failure). Sanitize to a single menu line so
    # embedded newlines or leading `-` characters cannot escape SwiftBar's
    # submenu encoding.
    err_body="${tunnels_err:-$tunnels}"
    safe_err=$(sanitize_menu_text "$err_body")
    echo "-- ${safe_err} | color=#999999 trim=false ansi=false"
    echo "---"
    echo "Refresh | refresh=true"
    exit 0
fi

if [ -z "$tunnels" ] || [ "$tunnels" = "null" ] || [ "$tunnels" = "[]" ]; then
    echo "⊘ | sfimage=network"
    echo "---"
    echo "No tunnels configured | color=#999999"
    echo "---"
    echo "Refresh | refresh=true"
    exit 0
fi

total=$(echo "$tunnels" | "$JQ" 'length' 2>/dev/null || echo 0)
connected=$(echo "$tunnels" | "$JQ" '[.[] | select(.status == "connected")] | length' 2>/dev/null || echo 0)

# If jq produced non-numeric output (malformed JSON mid-restart), coerce to 0
# so subsequent `-eq` comparisons cannot error under `set -u`.
if ! is_positive_int "$total" && [ "$total" != "0" ]; then
    total=0
fi
if ! is_positive_int "$connected" && [ "$connected" != "0" ]; then
    connected=0
fi

# --- Menu bar title ---
if [ "$connected" -eq 0 ]; then
    echo "⊘ 0/${total} | sfimage=network color=#999999"
elif [ "$connected" -eq "$total" ]; then
    echo "● ${connected}/${total} | sfimage=network color=#34C759"
else
    echo "● ${connected}/${total} | sfimage=network color=#FF9500"
fi

echo "---"

# --- Per-tunnel entries ---
# Materialise the jq output into a variable so failures surface as a clear
# error line instead of the silent pipe-failure `set -e` normally hides. Using
# `|| true` on the assignment keeps the script going; we check $? afterwards.
rows_status=0
rows_err_tmp=$(mktemp -t cc-clip-tunnels-rows.XXXXXX 2>/dev/null || mktemp 2>/dev/null || echo "/tmp/cc-clip-tunnels-rows.$$.err")
trap 'rm -f "$err_tmp" "$rows_err_tmp"' EXIT INT TERM
rows=$(echo "$tunnels" | "$JQ" -c '.[]' 2>"$rows_err_tmp") || rows_status=$?
if [ "$rows_status" -ne 0 ]; then
    echo "Failed to parse tunnel JSON | color=#FF3B30"
    rows_err=$(cat "$rows_err_tmp" 2>/dev/null || true)
    safe_rows_err=$(sanitize_menu_text "${rows_err:-$rows}")
    echo "-- ${safe_rows_err} | color=#999999 trim=false ansi=false"
    echo "---"
    echo "Refresh | refresh=true"
    exit 0
fi

while IFS= read -r t; do
    [ -z "$t" ] && continue
    host=$(echo "$t" | "$JQ" -r '.config.host' 2>/dev/null || echo "")
    status=$(echo "$t" | "$JQ" -r '.status' 2>/dev/null || echo "unknown")
    remote_port=$(echo "$t" | "$JQ" -r '.config.remote_port' 2>/dev/null || echo "")
    local_port=$(echo "$t" | "$JQ" -r '.config.local_port' 2>/dev/null || echo "")
    pid=$(echo "$t" | "$JQ" -r '.pid // 0' 2>/dev/null || echo 0)
    reconnects=$(echo "$t" | "$JQ" -r '.reconnect_count // 0' 2>/dev/null || echo 0)
    last_err=$(echo "$t" | "$JQ" -r '.last_error // empty' 2>/dev/null || echo "")
    persist_err=$(echo "$t" | "$JQ" -r '.persistence_error // empty' 2>/dev/null || echo "")
    if [ -n "$persist_err" ]; then
        if [ -n "$last_err" ]; then
            last_err="${last_err}; state: ${persist_err}"
        else
            last_err="state: ${persist_err}"
        fi
    fi

    case "$status" in
        connected)
            icon="🟢"
            action_label="Stop"
            action_cmd="down"
            ;;
        connecting)
            icon="🟡"
            action_label="Stop"
            action_cmd="down"
            ;;
        *)
            icon="🔴"
            action_label="Start"
            action_cmd="up"
            ;;
    esac

    safe_host=$(sanitize_menu_text "$host")
    # Render `?` for ports that the daemon could not produce (state file
    # corruption or a hand-edit). Without this the menu shows an empty
    # `:  → :` which reads like a layout bug.
    display_remote_port="$remote_port"
    if ! is_positive_int "$display_remote_port"; then
        display_remote_port="?"
    fi
    display_local_port="$local_port"
    if ! is_positive_int "$display_local_port"; then
        display_local_port="?"
    fi
    echo "${icon} ${safe_host}  :${display_remote_port} → :${display_local_port} | font=Menlo size=13"

    # Status details (submenu)
    echo "--Status: ${status} | color=#999999 size=12"
    if is_positive_int "$pid"; then
        echo "--PID: ${pid} | color=#999999 size=12"
    fi
    if is_positive_int "$reconnects"; then
        echo "--Reconnects: ${reconnects} | color=#999999 size=12"
    fi
    if [ -n "$last_err" ]; then
        safe_last_err=$(sanitize_menu_text "$last_err")
        echo "--Last error: ${safe_last_err} | color=#FF3B30 size=12"
    fi

    # Action button — only render if the host is safe to embed in SwiftBar's
    # paramN= encoding (no spaces, =, |, or shell metachars).
    if ! is_safe_host "$host"; then
        echo "--(action disabled: unsafe host name) | color=#999999 size=12"
        continue
    fi
    # Require a valid local_port. If state JSON lost it (0 / missing), do NOT
    # silently fall back to CC_CLIP_PORT — that could target a different
    # tunnel's daemon. Show a disabled hint instead.
    if ! is_positive_int "$local_port"; then
        echo "--(action disabled: invalid local_port — delete state file and retry) | color=#999999 size=12"
        continue
    fi
    action_daemon_port="$local_port"
    # $CC_CLIP may contain spaces (e.g. `/Applications/My Dir/cc-clip`).
    # SwiftBar splits the line on unquoted whitespace, so wrap the executable
    # in quotes — SwiftBar strips the outer quotes and passes the full path as
    # a single argv token. Quote $host too so the allowlist isn't the sole
    # barrier against argv injection. The daemon port is passed as an explicit
    # `--port` flag, matching the CLI surface exposed to interactive users.
    echo "--${action_label} | bash=\"${CC_CLIP}\" param1=tunnel param2=${action_cmd} \"param3=${host}\" param4=--port param5=${action_daemon_port} terminal=false refresh=true"
done <<< "$rows"

if [ -n "$cc_clip_extra_path_warning" ]; then
    echo "---"
    echo "⚠ ${cc_clip_extra_path_warning} | color=#FF9500 size=12"
    echo "Fix \$CC_CLIP_EXTRA_PATH (absolute paths, allowed chars A-Za-z0-9._/~@+-) | color=#999999 size=11"
fi

echo "---"
echo "Refresh | refresh=true"
