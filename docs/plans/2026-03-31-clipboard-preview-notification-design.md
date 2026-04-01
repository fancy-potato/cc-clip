# Clipboard Preview Notification Design

**Date:** 2026-03-31
**Status:** Design Complete, Pending Implementation
**Version:** v0.5.0 (planned)

## Problem

When pasting images into Claude Code or Codex CLI through cc-clip, users cannot see what was pasted. During batch pasting, this leads to duplicate or missed images with no way to detect either.

The root cause is that the entire cc-clip pipeline is a transparent byte relay with no observable state or deduplication semantics.

## Solution

Add local macOS notification preview triggered as a side effect of daemon serving image requests. Each notification shows a thumbnail, sequence number, and duplicate indicator. Zero additional user action required.

## Constraints

- Must not add operational burden (no confirmation dialogs)
- Must not interfere with existing image transfer (no stdout pollution)
- Terminal inline preview is not viable: Claude Code and Codex CLI are TUI applications that sit between the terminal emulator and image data, blocking escape sequence rendering
- Must work with both the shim path (Claude Code) and x11-bridge path (Codex CLI)

## Architecture

```
cc-clip connect <host>
  +-- Generate session_id (UUID)
  +-- Write to remote ~/.cache/cc-clip/session.id
  +-- Shim reads session.id, includes in every request

Remote shim: GET /clipboard/image
  Headers: Authorization: Bearer <token>
           X-CC-Clip-Session: <session-id>
           User-Agent: cc-clip/0.x

Local daemon receives request
  +-- Existing: return image bytes (unchanged)
  +-- New (async):
      +-- Store.AnalyzeAndRecord(sessionID, imageData)
      |     +-- Assign seq number
      |     +-- Compute SHA-256 fingerprint
      |     +-- Compare against last 5 images (ring buffer)
      |     +-- Update session state
      +-- Emit TransferEvent to notifyCh
      +-- Notifier goroutine consumes event
            +-- Generate thumbnail temp file
            +-- Trigger macOS UserNotification with attachment
```

### Notification Content

```
Title:    cc-clip #3
Subtitle: a1b2c3d4 . 1920x1080 . PNG
Body:     (only if duplicate) "Duplicate of #1"
Attachment: thumbnail image
```

## Components

### 1. Session Management (`internal/session/`)

```go
type ImageRecord struct {
    Seq         int
    Fingerprint string    // SHA-256 first 16 hex chars (8 bytes)
    Width       int
    Height      int
    Format      string
    At          time.Time
}

type Session struct {
    ID         string
    Recent     [5]ImageRecord  // ring buffer
    Cursor     int             // ring write position
    Count      int             // valid entries (0-5)
    SeqNext    int             // next seq (starts at 1)
    LastAccess time.Time
}

type Store struct {
    mu       sync.Mutex
    sessions map[string]*Session
    ttl      time.Duration  // default 12h
}
```

**Key method:**

```go
// AnalyzeAndRecord atomically: find duplicate + assign seq + write ring buffer + update lastAccess
func (s *Store) AnalyzeAndRecord(sessionID string, fingerprint string, width, height int, format string) TransferEvent
```

- Computes full SHA-256, stores first 16 hex
- Scans only `Count` valid entries in ring buffer (not empty slots)
- Session auto-created on first access
- Background cleanup goroutine every 30 minutes removes sessions with `LastAccess > TTL`

### 2. Notifier Interface (`internal/daemon/notifier.go`)

```go
type TransferEvent struct {
    SessionID   string
    Seq         int
    Fingerprint string
    ImageData   []byte
    Format      string
    Width       int
    Height      int
    DuplicateOf int  // 0 = unique, N = matches seq N
}

type Notifier interface {
    Notify(ctx context.Context, event TransferEvent) error
}
```

### 3. Platform Implementations (v1)

| File | Build Tag | Implementation |
|------|-----------|---------------|
| `notify_darwin.go` | `darwin` | terminal-notifier (with thumbnail + click-to-open), osascript fallback |
| `notify_other.go` | `!darwin` | no-op |

**v1 notification stack:**
- **Primary (terminal-notifier installed):** text notification with small `-contentImage` thumbnail + `-open` click-to-preview in Preview.app. Saves full image to `~/.cache/cc-clip/previews/`.
- **Fallback (no terminal-notifier):** osascript text-only notification with same preview directory.

**Design note:** The original design specified `UNUserNotificationCenter` + `UNNotificationAttachment` via CGo for native thumbnail support. This was abandoned because `UNUserNotificationCenter` requires a Bundle ID (`bundleProxyForCurrentProcess is nil` crash on CLI processes). A Swift .app bundle wrapper was attempted but macOS notification permission flow does not reliably trigger from background/LSUIElement apps. `terminal-notifier` provides the best available thumbnail support for CLI tools on macOS.

**v1 user experience:**
- Zero-effort: seq number, fingerprint, dimensions, duplicate flag in notification text
- One-click: click notification to open full image in Preview.app
- Browse: `open ~/.cache/cc-clip/previews/` to view all transferred images

**v1.1 (future):** Revisit native `UNNotificationAttachment` thumbnails via a properly signed .app helper or Shortcuts integration.

### 4. Async Delivery Pipeline

```go
// In Server struct
notifyCh chan TransferEvent  // buffered, cap=8

// In handleClipboardImage, after writing response:
select {
case s.notifyCh <- event:
default:
    // channel full: drop notification, never block image transfer
}

// Background goroutine:
func (s *Server) runNotifier(ctx context.Context, n Notifier) {
    for {
        select {
        case evt := <-s.notifyCh:
            _ = n.Notify(ctx, evt)
        case <-ctx.Done():
            return
        }
    }
}
```

**Design invariant:** Image transfer latency is never affected by notification delivery.

### 5. Protocol Extension

**Shim change:** Add `X-CC-Clip-Session` header to curl requests.

```bash
# In _cc_clip_fetch_binary():
curl -sf \
  -H "Authorization: Bearer $token" \
  -H "User-Agent: cc-clip/0.1" \
  -H "X-CC-Clip-Session: $session_id" \
  -o "$tmpfile" \
  "http://127.0.0.1:${CC_CLIP_PORT:-18339}/clipboard/image"
```

**Session ID lifecycle:**
- Generated by `cc-clip connect <host>` as UUID
- Written to remote `~/.cache/cc-clip/session.id`
- Shim reads from file on each request (same pattern as token)
- New connect = new session ID = counter resets

### 6. Deduplication

- Compare fingerprint against `session.Recent[0..Count-1]`
- Match found: `DuplicateOf = matched.Seq`
- No match: `DuplicateOf = 0`
- Window: last 5 images per session
- Intentional re-paste after 5+ images will not trigger false positive

## File Change List

| File | Type | Description |
|------|------|-------------|
| `internal/session/session.go` | New | Session, ImageRecord, Store, AnalyzeAndRecord, RunCleanup |
| `internal/session/session_test.go` | New | Ring buffer, dedup, TTL, concurrency tests (9 tests) |
| `internal/daemon/notifier.go` | New | Notifier interface, NotifyEvent, NopNotifier |
| `internal/daemon/notify_darwin.go` | New | terminal-notifier primary + osascript fallback + preview dir |
| `internal/daemon/notify_other.go` | New | no-op for non-darwin |
| `internal/daemon/notify_test.go` | New | Async delivery, channel-full, duplicate detection tests (4 tests) |
| `internal/daemon/server.go` | Modify | Add session store, notifyCh, RunNotifier (with recover), w.Write error check before notification |
| `internal/daemon/server_test.go` | Modify | Adapt to new NewServer signature |
| `internal/shim/template.go` | Modify | Add CC_CLIP_SESSION_FILE, _cc_clip_session_header, X-CC-Clip-Session to curl |
| `internal/shim/ssh.go` | Modify | Add GenerateSessionID + WriteRemoteSessionID |
| `internal/shim/install_test.go` | Modify | Verify shim contains session header |
| `internal/x11bridge/bridge.go` | Modify | Add readSessionID + X-CC-Clip-Session header to both HTTP requests |
| `cmd/cc-clip/main.go` | Modify | Rename session→sess, add session store, DarwinNotifier, RunCleanup, deploy session ID |
| `scripts/cc-clip-notify.swift` | New | Swift notification helper (attempted, not used in v1) |

## Implementation Order

```
Phase 1: Core state layer (pure logic, no dependencies)
  (1) internal/session/session.go + session_test.go

Phase 2: Notification interface + async pipeline
  (2) internal/daemon/notifier.go
  (3) internal/daemon/server.go modifications
  (4) internal/daemon/notify_test.go

Phase 3: Platform notification implementations
  (5) notify_darwin.go (terminal-notifier + osascript fallback)
  (6) notify_other.go (no-op)

Phase 4: Protocol integration
  (7) internal/shim/template.go + install_test.go
  (8) internal/shim/ssh.go (GenerateSessionID + WriteRemoteSessionID)
  (9) internal/x11bridge/bridge.go (session header on both requests)
  (10) cmd/cc-clip/main.go + main_test.go

Phase 5: End-to-end verification
  (11) Local serve -> connect -> remote paste -> receive notification with text + click-to-preview
```

## Test Strategy

| Level | Coverage | Method |
|-------|----------|--------|
| Unit | Ring buffer, seq increment, dedup, TTL expiry | Pure Go tests |
| Unit | AnalyzeAndRecord concurrency safety | `go test -race` |
| Unit | Async delivery: channel full drops without blocking | Mock Notifier + timing assertions |
| Unit | Shim template contains session header | String matching |
| Integration | Daemon + session header -> mock notifier triggered | httptest + mock |
| Manual | macOS native notification with thumbnail + seq + duplicate | Local `cc-clip serve` + curl |
| Manual | Fallback: `CGO_ENABLED=0 go build` -> osascript notification | Build verification |
| E2E | Full connect -> remote paste -> local notification | SSH to test server |

## Future Extensions (Not in Scope)

- **v1.1: Native thumbnail notifications** — Revisit `UNNotificationAttachment` via signed .app helper or macOS Shortcuts integration
- `cc-clip history` — list recent transfers with thumbnails
- `cc-clip watch` — real-time monitoring in separate terminal pane (terminal image protocols work here since no TUI blocking)
- Windows toast notification support
- Web dashboard for transfer visualization
- Configurable dedup window size
