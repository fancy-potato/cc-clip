package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"bytes"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/token"
)

const (
	maxImageSize = 20 * 1024 * 1024 // 20MB
	userAgent    = "cc-clip"
)

const notifyChCap = 8

type Server struct {
	clipboard ClipboardReader
	tokens    *token.Manager
	sessions  *session.Store
	notifyCh  chan NotifyEvent
	addr      string
	mux       *http.ServeMux
}

func NewServer(addr string, clipboard ClipboardReader, tokens *token.Manager, sessions *session.Store) *Server {
	s := &Server{
		clipboard: clipboard,
		tokens:    tokens,
		sessions:  sessions,
		notifyCh:  make(chan NotifyEvent, notifyChCap),
		addr:      addr,
		mux:       http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /clipboard/type", s.authMiddleware(s.handleClipboardType))
	s.mux.HandleFunc("GET /clipboard/image", s.authMiddleware(s.handleClipboardImage))
	return s
}

// RunNotifier consumes transfer events and delivers notifications.
// It blocks until ctx is cancelled. Panics in Notify are recovered
// to prevent notification failures from crashing the daemon.
func (s *Server) RunNotifier(ctx context.Context, n Notifier) {
	for {
		select {
		case evt := <-s.notifyCh:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("notification panic recovered: %v", r)
					}
				}()
				if err := n.Notify(ctx, evt); err != nil {
					log.Printf("notification failed: %v", err)
				}
			}()
		case <-ctx.Done():
			return
		}
	}
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
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
		notify := NotifyEvent{
			SessionID:   evt.SessionID,
			Seq:         evt.Seq,
			Fingerprint: evt.Fingerprint,
			ImageData:   data,
			Format:      info.Format,
			Width:       width,
			Height:      height,
			DuplicateOf: evt.DuplicateOf,
		}
		select {
		case s.notifyCh <- notify:
		default:
			// channel full: drop notification
		}
	}
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
