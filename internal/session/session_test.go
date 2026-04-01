package session

import (
	"sync"
	"testing"
	"time"
)

func TestAnalyzeAndRecord_AssignsSequentialSeq(t *testing.T) {
	s := NewStore(12 * time.Hour)
	sid := "test-session-1"

	e1 := s.AnalyzeAndRecord(sid, "aabbccdd", 1920, 1080, "png")
	e2 := s.AnalyzeAndRecord(sid, "11223344", 800, 600, "jpeg")
	e3 := s.AnalyzeAndRecord(sid, "55667788", 1024, 768, "png")

	if e1.Seq != 1 {
		t.Errorf("expected seq 1, got %d", e1.Seq)
	}
	if e2.Seq != 2 {
		t.Errorf("expected seq 2, got %d", e2.Seq)
	}
	if e3.Seq != 3 {
		t.Errorf("expected seq 3, got %d", e3.Seq)
	}
}

func TestAnalyzeAndRecord_DetectsDuplicate(t *testing.T) {
	s := NewStore(12 * time.Hour)
	sid := "test-session-dup"

	s.AnalyzeAndRecord(sid, "aabbccdd", 1920, 1080, "png")
	s.AnalyzeAndRecord(sid, "11223344", 800, 600, "jpeg")
	e3 := s.AnalyzeAndRecord(sid, "aabbccdd", 1920, 1080, "png")

	if e3.DuplicateOf != 1 {
		t.Errorf("expected duplicate of seq 1, got %d", e3.DuplicateOf)
	}
}

func TestAnalyzeAndRecord_NoDuplicateForUnique(t *testing.T) {
	s := NewStore(12 * time.Hour)
	sid := "test-session-uniq"

	s.AnalyzeAndRecord(sid, "aabbccdd", 1920, 1080, "png")
	e2 := s.AnalyzeAndRecord(sid, "11223344", 800, 600, "jpeg")

	if e2.DuplicateOf != 0 {
		t.Errorf("expected no duplicate (0), got %d", e2.DuplicateOf)
	}
}

func TestAnalyzeAndRecord_RingBufferEvicts(t *testing.T) {
	s := NewStore(12 * time.Hour)
	sid := "test-session-ring"

	// Fill ring buffer with 5 unique images (slots 0-4)
	s.AnalyzeAndRecord(sid, "fp-1", 100, 100, "png") // seq 1, slot 0
	s.AnalyzeAndRecord(sid, "fp-2", 100, 100, "png") // seq 2, slot 1
	s.AnalyzeAndRecord(sid, "fp-3", 100, 100, "png") // seq 3, slot 2
	s.AnalyzeAndRecord(sid, "fp-4", 100, 100, "png") // seq 4, slot 3
	s.AnalyzeAndRecord(sid, "fp-5", 100, 100, "png") // seq 5, slot 4

	// fp-1 is still in buffer at slot 0
	e6 := s.AnalyzeAndRecord(sid, "fp-1", 100, 100, "png")
	if e6.DuplicateOf != 1 {
		t.Errorf("expected duplicate of seq 1 (still in buffer), got %d", e6.DuplicateOf)
	}

	// Now add 5 more unique images to fully cycle the ring buffer
	// This evicts all original entries including fp-1 re-entry at slot 0
	s.AnalyzeAndRecord(sid, "fp-a", 100, 100, "png") // seq 7, overwrites slot 1
	s.AnalyzeAndRecord(sid, "fp-b", 100, 100, "png") // seq 8, overwrites slot 2
	s.AnalyzeAndRecord(sid, "fp-c", 100, 100, "png") // seq 9, overwrites slot 3
	s.AnalyzeAndRecord(sid, "fp-d", 100, 100, "png") // seq 10, overwrites slot 4
	s.AnalyzeAndRecord(sid, "fp-e", 100, 100, "png") // seq 11, overwrites slot 0 (evicts fp-1)

	// fp-1 is now fully evicted from ring buffer
	e12 := s.AnalyzeAndRecord(sid, "fp-1", 100, 100, "png")
	if e12.DuplicateOf != 0 {
		t.Errorf("expected no duplicate after full eviction, got %d", e12.DuplicateOf)
	}
}

func TestAnalyzeAndRecord_SeparateSessions(t *testing.T) {
	s := NewStore(12 * time.Hour)

	e1 := s.AnalyzeAndRecord("session-a", "fp-1", 100, 100, "png")
	e2 := s.AnalyzeAndRecord("session-b", "fp-1", 100, 100, "png")

	if e1.Seq != 1 {
		t.Errorf("session-a: expected seq 1, got %d", e1.Seq)
	}
	if e2.Seq != 1 {
		t.Errorf("session-b: expected seq 1, got %d", e2.Seq)
	}
	if e2.DuplicateOf != 0 {
		t.Errorf("session-b: should not detect duplicate from session-a, got %d", e2.DuplicateOf)
	}
}

func TestAnalyzeAndRecord_EventFields(t *testing.T) {
	s := NewStore(12 * time.Hour)
	e := s.AnalyzeAndRecord("sess-1", "abcd1234", 1920, 1080, "png")

	if e.SessionID != "sess-1" {
		t.Errorf("expected session ID sess-1, got %s", e.SessionID)
	}
	if e.Fingerprint != "abcd1234" {
		t.Errorf("expected fingerprint abcd1234, got %s", e.Fingerprint)
	}
	if e.Width != 1920 || e.Height != 1080 {
		t.Errorf("expected 1920x1080, got %dx%d", e.Width, e.Height)
	}
	if e.Format != "png" {
		t.Errorf("expected format png, got %s", e.Format)
	}
}

func TestCleanup_RemovesExpiredSessions(t *testing.T) {
	s := NewStore(50 * time.Millisecond)

	s.AnalyzeAndRecord("old-session", "fp-1", 100, 100, "png")
	time.Sleep(100 * time.Millisecond)

	s.AnalyzeAndRecord("new-session", "fp-2", 100, 100, "png")

	s.Cleanup()

	// old-session should be gone, new-session should remain
	s.mu.Lock()
	_, oldExists := s.sessions["old-session"]
	_, newExists := s.sessions["new-session"]
	s.mu.Unlock()

	if oldExists {
		t.Error("expected old-session to be cleaned up")
	}
	if !newExists {
		t.Error("expected new-session to still exist")
	}
}

func TestCleanup_KeepsActiveSessions(t *testing.T) {
	s := NewStore(1 * time.Hour)

	s.AnalyzeAndRecord("active-session", "fp-1", 100, 100, "png")
	s.Cleanup()

	s.mu.Lock()
	_, exists := s.sessions["active-session"]
	s.mu.Unlock()

	if !exists {
		t.Error("expected active session to survive cleanup")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStore(12 * time.Hour)
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sid := "concurrent-session"
			s.AnalyzeAndRecord(sid, "fp-"+string(rune('a'+n%26)), 100, 100, "png")
		}(i)
	}

	wg.Wait()

	s.mu.Lock()
	sess := s.sessions["concurrent-session"]
	s.mu.Unlock()

	if sess.SeqNext != 101 {
		t.Errorf("expected SeqNext 101 after 100 concurrent records, got %d", sess.SeqNext)
	}
}
