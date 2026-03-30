package session

import (
	"context"
	"sync"
	"time"
)

const ringSize = 5

// TransferEvent is emitted when an image is analyzed and recorded.
type TransferEvent struct {
	SessionID   string
	Seq         int
	Fingerprint string
	Width       int
	Height      int
	Format      string
	DuplicateOf int // 0 = unique, N = matches seq N
}

// ImageRecord stores metadata for a single transferred image.
type ImageRecord struct {
	Seq         int
	Fingerprint string
	Width       int
	Height      int
	Format      string
	At          time.Time
}

// Session tracks transfer state for a single connect session.
type Session struct {
	ID         string
	Recent     [ringSize]ImageRecord
	Cursor     int // next write position in ring buffer
	Count      int // valid entries (0..ringSize)
	SeqNext    int // next sequence number (starts at 1)
	LastAccess time.Time
}

// Store manages sessions with TTL-based cleanup.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	ttl      time.Duration
}

// NewStore creates a session store with the given TTL for idle session expiry.
func NewStore(ttl time.Duration) *Store {
	return &Store{
		sessions: make(map[string]*Session),
		ttl:      ttl,
	}
}

// AnalyzeAndRecord atomically assigns a sequence number, checks for duplicates
// in the ring buffer, records the image, and returns a TransferEvent.
func (s *Store) AnalyzeAndRecord(sessionID, fingerprint string, width, height int, format string) TransferEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		sess = &Session{
			ID:      sessionID,
			SeqNext: 1,
		}
		s.sessions[sessionID] = sess
	}

	now := time.Now()
	sess.LastAccess = now

	// Check for duplicate in ring buffer
	duplicateOf := 0
	limit := sess.Count
	for i := 0; i < limit; i++ {
		if sess.Recent[i].Fingerprint == fingerprint {
			duplicateOf = sess.Recent[i].Seq
			break
		}
	}

	// Assign sequence number
	seq := sess.SeqNext
	sess.SeqNext++

	// Write to ring buffer
	record := ImageRecord{
		Seq:         seq,
		Fingerprint: fingerprint,
		Width:       width,
		Height:      height,
		Format:      format,
		At:          now,
	}
	sess.Recent[sess.Cursor] = record
	sess.Cursor = (sess.Cursor + 1) % ringSize
	if sess.Count < ringSize {
		sess.Count++
	}

	return TransferEvent{
		SessionID:   sessionID,
		Seq:         seq,
		Fingerprint: fingerprint,
		Width:       width,
		Height:      height,
		Format:      format,
		DuplicateOf: duplicateOf,
	}
}

// Cleanup removes sessions that have been idle longer than the TTL.
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for id, sess := range s.sessions {
		if now.Sub(sess.LastAccess) > s.ttl {
			delete(s.sessions, id)
		}
	}
}

// RunCleanup periodically removes expired sessions until ctx is cancelled.
func (s *Store) RunCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Cleanup()
		case <-ctx.Done():
			return
		}
	}
}
