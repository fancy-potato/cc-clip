package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/token"
)

const (
	maxImageSize    = 20 * 1024 * 1024 // 20MB
	maxNotifyBody   = 64 * 1024        // 64KB
	userAgent       = "cc-clip"
	criticalChCap   = 4
	claudeHookCType = "application/x-claude-hook"
)

const notifyChCap = 8

type Server struct {
	clipboard    ClipboardReader
	tokens       *token.Manager
	sessions     *session.Store
	dedup        *Deduper
	notifyCh     chan NotifyEnvelope
	criticalCh   chan NotifyEnvelope
	notifyNonces map[string]nonceEntry
	noncesMu     sync.RWMutex
	addr         string
	mux          *http.ServeMux
	// served is set once Serve/ListenAndServe begins accepting connections.
	// After that point, any call to Mux() panics because http.ServeMux is
	// not safe for concurrent route registration while ServeHTTP is running.
	served atomic.Bool
}

func NewServer(addr string, clipboard ClipboardReader, tokens *token.Manager, sessions *session.Store) *Server {
	s := &Server{
		clipboard:    clipboard,
		tokens:       tokens,
		sessions:     sessions,
		dedup:        NewDeduper(12 * time.Second),
		notifyCh:     make(chan NotifyEnvelope, notifyChCap),
		criticalCh:   make(chan NotifyEnvelope, criticalChCap),
		notifyNonces: make(map[string]nonceEntry),
		addr:         addr,
		mux:          http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /clipboard/type", s.authMiddleware(s.handleClipboardType))
	s.mux.HandleFunc("GET /clipboard/image", s.authMiddleware(s.handleClipboardImage))
	s.mux.HandleFunc("POST /notify", s.handleNotify)
	s.mux.HandleFunc("POST /register-nonce", s.authMiddleware(s.handleRegisterNonce))
	return s
}

// nonceEntry tracks metadata for a registered notification nonce.
type nonceEntry struct {
	Host      string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Digest    [32]byte
}

// nonceTTL is the default lifetime for notification nonces.
const nonceTTL = 7 * 24 * time.Hour // 7 days

// maxNotificationNonces caps the in-memory nonce registry to bound
// memory growth. A caller holding the clipboard bearer token could
// otherwise register unlimited nonces with distinct or empty host
// values (since the per-host revocation only fires when host matches
// an existing entry), and `CleanupExpiredNonces` only runs on a 30
// minute tick. When the cap is hit, the oldest entries are evicted
// first; that preserves freshly-issued nonces at the expense of
// stale ones that the scheduled cleanup would eventually have removed.
const maxNotificationNonces = 4096

// RegisterNotificationNonce adds a nonce to the dedicated notification
// auth registry. Notification nonces are separate from clipboard bearer
// tokens to enforce distinct auth domains. When a new nonce is registered
// for the same host, any previous nonce for that host is revoked.
// Returns an error if the nonce is empty or collides with a valid clipboard token.
func (s *Server) RegisterNotificationNonce(nonce string) error {
	return s.RegisterNotificationNonceForHost(nonce, "")
}

// RegisterNotificationNonceForHost registers a nonce bound to a specific host.
// Any previous nonce for the same host is automatically revoked.
func (s *Server) RegisterNotificationNonceForHost(nonce, host string) error {
	if nonce == "" {
		return fmt.Errorf("empty nonce is not allowed")
	}
	if s.tokens.Validate(nonce) == nil {
		return fmt.Errorf("refusing to register clipboard token as notification nonce")
	}
	now := time.Now()
	digest := sha256.Sum256([]byte(nonce))
	s.noncesMu.Lock()
	defer s.noncesMu.Unlock()
	// Revoke previous nonce for the same host.
	if host != "" {
		for k, v := range s.notifyNonces {
			if v.Host == host {
				delete(s.notifyNonces, k)
			}
		}
	}
	s.notifyNonces[nonce] = nonceEntry{
		Host:      host,
		IssuedAt:  now,
		ExpiresAt: now.Add(nonceTTL),
		Digest:    digest,
	}
	s.evictOldestNonceIfNeeded()
	return nil
}

// evictOldestNonceIfNeeded enforces the maxNotificationNonces cap by
// removing the entry with the earliest IssuedAt timestamp when the map
// grows beyond the limit. Must be called with s.noncesMu held.
func (s *Server) evictOldestNonceIfNeeded() {
	for len(s.notifyNonces) > maxNotificationNonces {
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, v := range s.notifyNonces {
			if first || v.IssuedAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.IssuedAt
				first = false
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.notifyNonces, oldestKey)
	}
}

// validNotificationNonce checks whether the given nonce is registered and not expired.
//
// The lookup intentionally iterates every registered nonce and compares
// fixed-size SHA-256 digests instead of using the raw nonce as a map lookup.
// The caller-supplied nonce is hashed once up front, then each stored digest
// is compared in constant time so rejection cost stays O(len(header) + n)
// rather than O(len(header) * n). /notify is the daemon's only endpoint
// reachable from across the reverse tunnel, so it is the most exposed auth
// check and deserves the same care as the clipboard bearer path.
//
// The fast-path check on empty nonce is still timing-sensitive against
// "registered but empty" which RegisterNotificationNonceForHost refuses;
// an empty nonce can never match any valid entry.
func (s *Server) validNotificationNonce(nonce string) bool {
	if nonce == "" {
		return false
	}
	nonceDigest := sha256.Sum256([]byte(nonce))
	s.noncesMu.RLock()
	defer s.noncesMu.RUnlock()
	now := time.Now()
	matched := false
	for _, entry := range s.notifyNonces {
		if subtle.ConstantTimeCompare(entry.Digest[:], nonceDigest[:]) == 1 && now.Before(entry.ExpiresAt) {
			matched = true
			// Do NOT break: keep iterating so timing does not leak the
			// match position within the map. Compiler cannot trivially
			// elide this because matched is returned.
		}
	}
	return matched
}

// CleanupExpiredNonces removes nonces that have passed their TTL.
func (s *Server) CleanupExpiredNonces() {
	now := time.Now()
	s.noncesMu.Lock()
	defer s.noncesMu.Unlock()
	for k, v := range s.notifyNonces {
		if now.After(v.ExpiresAt) {
			delete(s.notifyNonces, k)
		}
	}
}

// enqueueEnvelope deduplicates then routes a notification envelope to
// the appropriate channel. Repeated non-critical notifications within
// the dedup window are suppressed. Critical envelopes (permission_prompt)
// bypass dedup and use criticalCh with a 500ms timeout. Non-critical
// envelopes use a select-default send to notifyCh, dropping on full.
func (s *Server) enqueueEnvelope(env NotifyEnvelope) {
	if allowed, _ := s.dedup.AllowAt(env, time.Now()); !allowed {
		return
	}
	if isAlwaysCritical(env) {
		select {
		case s.criticalCh <- env:
		case <-time.After(500 * time.Millisecond):
			log.Printf("WARN: criticalCh full, dropping critical envelope kind=%s", env.Kind)
		}
		return
	}
	select {
	case s.notifyCh <- env:
	default:
		// channel full: drop non-critical notification
	}
}

// RunNotifier consumes transfer events from both criticalCh (priority)
// and notifyCh, then delivers notifications via the Notifier interface.
// It blocks until ctx is cancelled. Panics in Notify/Deliver are recovered
// to prevent notification failures from crashing the daemon.
//
// If the supplied notifier also supports envelope delivery, envelopes are
// delivered directly so generic/tool notifications keep their structured
// payload. Legacy notifiers continue to receive bridged NotifyEvent values.
func (s *Server) RunNotifier(ctx context.Context, n Notifier) {
	for {
		var env NotifyEnvelope

		// Give critical notifications strict priority when both queues are ready.
		select {
		case <-ctx.Done():
			return
		case env = <-s.criticalCh:
		default:
			select {
			case env = <-s.criticalCh:
			case env = <-s.notifyCh:
			case <-ctx.Done():
				return
			}
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("notification panic recovered: %v", r)
				}
			}()

			if deliverer, ok := n.(interface {
				Deliver(context.Context, NotifyEnvelope) error
			}); ok {
				if err := deliverer.Deliver(ctx, env); err != nil {
					log.Printf("notification failed: %v", err)
				}
				return
			}

			evt := envelopeToEvent(env)
			if err := n.Notify(ctx, evt); err != nil {
				log.Printf("notification failed: %v", err)
			}
		}()
	}
}

// envelopeToEvent bridges a NotifyEnvelope back to a NotifyEvent for
// backward compatibility with the Notifier interface.
func envelopeToEvent(env NotifyEnvelope) NotifyEvent {
	if env.ImageTransfer != nil {
		return NotifyEvent{
			SessionID:   env.ImageTransfer.SessionID,
			Seq:         env.ImageTransfer.Seq,
			Fingerprint: env.ImageTransfer.Fingerprint,
			ImageData:   env.ImageTransfer.ImageData,
			Format:      env.ImageTransfer.Format,
			Width:       env.ImageTransfer.Width,
			Height:      env.ImageTransfer.Height,
			DuplicateOf: env.ImageTransfer.DuplicateOf,
		}
	}
	// For non-image envelopes, construct a synthetic event with
	// available metadata so existing Notifier implementations can
	// display something useful.
	return NotifyEvent{
		Format: string(env.Kind),
	}
}

// Handler returns the HTTP handler for this server.
// Useful for testing with httptest.NewServer.
func (s *Server) Handler() http.Handler {
	s.served.Store(true)
	return s.mux
}

// Mux returns the underlying ServeMux so callers can register additional
// routes (e.g., tunnel management endpoints). Callers MUST register all
// routes BEFORE calling Handler / Serve / ListenAndServe; http.ServeMux.HandleFunc
// is not safe to call concurrently with ServeHTTP.
//
// The atomic `served` check below is best-effort detection for the common
// misuse of calling Mux() from a separate goroutine after the server has
// already started. It is NOT a synchronization primitive: there is still
// no happens-before relationship between a caller registering a route
// and a concurrent ServeHTTP reading the mux's internal entry map, so a
// caller that races Mux()/HandleFunc against Serve() can still trigger the
// exact data race this panic is meant to warn about. In practice,
// registerTunnelRoutes is called synchronously on the same goroutine that
// later calls Serve(), so the race is not triggered in-tree; treat this
// gate as a tripwire for future callers, not a guarantee.
//
// WARNING: Routes registered via Mux() bypass the daemon's clipboard
// Bearer-token middleware entirely. The caller is responsible for wiring
// appropriate authentication (for example, the tunnel-control token for
// tunnel management routes). The clipboard token is intentionally not
// re-exported as a reusable wrapper — mixing auth domains across routes on
// this mux would widen attack surface. If your new route needs auth, write
// dedicated middleware in your own package.
func (s *Server) Mux() *http.ServeMux {
	if s.served.Load() {
		panic("daemon.Server.Mux() called after Handler/Serve/ListenAndServe started: http.ServeMux is not safe for concurrent route registration once ServeHTTP may be running. Register all routes before calling Handler.")
	}
	return s.mux
}

// Serve accepts connections on the given listener and serves HTTP.
func (s *Server) Serve(ln net.Listener) error {
	s.served.Store(true)
	return http.Serve(ln, s.mux)
}

func (s *Server) ListenAndServe() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}

	host, _, _ := net.SplitHostPort(s.addr)
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		listener.Close()
		return fmt.Errorf("refusing to listen on non-loopback address: %s", host)
	}

	log.Printf("cc-clip daemon listening on %s", s.addr)
	s.served.Store(true)
	return http.Serve(listener, s.mux)
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check User-Agent
		ua := r.Header.Get("User-Agent")
		if ua != "" && !strings.HasPrefix(ua, userAgent) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		tok := strings.TrimPrefix(auth, "Bearer ")

		if err := s.tokens.Validate(tok); err != nil {
			// Collapse all validation failures into a single opaque
			// response rather than returning err.Error() verbatim. The
			// internal error distinguishes "token expired" from "wrong
			// token entirely", which turns the 401 into an oracle for an
			// unauthenticated caller: a correct-but-expired token gets a
			// different string than a random guess. The tunnel-control
			// middleware already collapses both cases; mirror that here
			// so neither auth path leaks internal state.
			log.Printf("clipboard bearer auth: validation failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleRegisterNonce accepts a notification nonce from an authenticated
// connect session. The request body is a JSON object with a "nonce" field.
// Protected by authMiddleware (requires clipboard bearer token).
//
// The body is capped at 4 KiB and decoded with DisallowUnknownFields so a
// compromised or buggy peer holding the clipboard token cannot force the
// daemon to buffer an unbounded stream or silently accept extra fields as
// a carrier for future payload expansion.
func (s *Server) handleRegisterNonce(w http.ResponseWriter, r *http.Request) {
	const maxBody = 4096
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req struct {
		Nonce string `json:"nonce"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			http.Error(w, fmt.Sprintf("request body exceeds %d bytes", maxBody), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := s.RegisterNotificationNonce(req.Nonce); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClipboardType(w http.ResponseWriter, r *http.Request) {
	info, err := s.clipboard.Type()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (s *Server) handleClipboardImage(w http.ResponseWriter, r *http.Request) {
	info, err := s.clipboard.Type()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.Type != ClipboardImage {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	data, err := s.clipboard.ImageBytes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(data) > maxImageSize {
		http.Error(w, "image exceeds 20MB limit", http.StatusRequestEntityTooLarge)
		return
	}

	contentType := "image/png"
	if info.Format == "jpeg" {
		contentType = "image/jpeg"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	_, writeErr := w.Write(data)

	// Only notify if the image was successfully written to the client.
	// If the client disconnected mid-transfer, skip notification to avoid
	// false-positive "image transferred" confirmations.
	if writeErr != nil {
		return
	}

	sessionID := r.Header.Get("X-CC-Clip-Session")
	if sessionID != "" && s.sessions != nil {
		hash := sha256.Sum256(data)
		fingerprint := hex.EncodeToString(hash[:8])

		width, height := decodeImageDimensions(data)

		evt := s.sessions.AnalyzeAndRecord(sessionID, fingerprint, width, height, info.Format)
		env := newImageTransferEnvelope("clipboard", ImageTransferPayload{
			SessionID:   evt.SessionID,
			Seq:         evt.Seq,
			Fingerprint: evt.Fingerprint,
			ImageData:   data,
			Format:      info.Format,
			Width:       width,
			Height:      height,
			DuplicateOf: evt.DuplicateOf,
		})
		s.enqueueEnvelope(env)
	}
}

// handleNotify accepts notification payloads from remote hook scripts
// or generic senders. Auth is via notification nonce (separate from
// clipboard bearer token).
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	nonce := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if nonce == "" || !s.validNotificationNonce(nonce) {
		// Opaque 401 text. authMiddleware collapses its error to
		// "unauthorized" to avoid an oracle distinguishing "wrong token"
		// from "expired token"; /notify is reachable across the reverse
		// tunnel so it is the most exposed auth path and must not leak
		// whether a nonce was recognised, expired, or simply missing.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	env, err := s.parseNotifyRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.enqueueEnvelope(env)
	w.WriteHeader(http.StatusNoContent)
}

// parseNotifyRequest decodes the request body into a NotifyEnvelope.
// Two content types are supported:
//   - application/x-claude-hook: Claude hook JSON, processed via ClassifyHookPayload
//   - anything else (typically application/json): generic JSON notification
func (s *Server) parseNotifyRequest(r *http.Request) (NotifyEnvelope, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxNotifyBody))
	if err != nil {
		return NotifyEnvelope{}, fmt.Errorf("failed to read body: %w", err)
	}
	if len(body) == 0 {
		return NotifyEnvelope{}, fmt.Errorf("empty request body")
	}

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, claudeHookCType) {
		return s.parseClaudeHookPayload(body)
	}
	return s.parseGenericJSON(body)
}

// parseClaudeHookPayload decodes Claude hook JSON and classifies it.
func (s *Server) parseClaudeHookPayload(body []byte) (NotifyEnvelope, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return NotifyEnvelope{}, fmt.Errorf("invalid JSON: %w", err)
	}

	hookType := "notification"
	if ht, ok := raw["hook_event_name"].(string); ok {
		hookType = strings.ToLower(ht)
	}

	env := ClassifyHookPayload(hookType, raw)
	if env == nil {
		return NotifyEnvelope{}, fmt.Errorf("classifier returned nil")
	}
	return *env, nil
}

// parseGenericJSON decodes a freeform JSON notification payload.
func (s *Server) parseGenericJSON(body []byte) (NotifyEnvelope, error) {
	var payload struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		Urgency int    `json:"urgency"`
		Host    string `json:"host"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return NotifyEnvelope{}, fmt.Errorf("invalid JSON: %w", err)
	}

	return NotifyEnvelope{
		Kind:      KindGenericMessage,
		Source:    "generic",
		Host:      payload.Host,
		Timestamp: time.Now().UTC(),
		GenericMessage: &GenericMessagePayload{
			Title:    payload.Title,
			Body:     payload.Body,
			Urgency:  payload.Urgency,
			Verified: false,
		},
	}, nil
}

// decodeImageDimensions reads width and height from image data.
// Returns 0, 0 if decoding fails.
func decodeImageDimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// NotifyChannel exposes the non-critical notification queue to tests.
func (s *Server) NotifyChannel() <-chan NotifyEnvelope {
	return s.notifyCh
}
