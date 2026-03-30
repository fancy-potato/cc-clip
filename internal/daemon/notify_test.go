package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/token"
)

type countingNotifier struct {
	count atomic.Int64
	last  atomic.Value // stores NotifyEvent
}

func (n *countingNotifier) Notify(_ context.Context, evt NotifyEvent) error {
	n.count.Add(1)
	n.last.Store(evt)
	return nil
}

func newTestServerWithNotifier(clip ClipboardReader) (*Server, string, *countingNotifier) {
	tm := token.NewManager(1 * time.Hour)
	s, _ := tm.Generate()
	store := session.NewStore(12 * time.Hour)
	srv := NewServer("127.0.0.1:0", clip, tm, store)

	notifier := &countingNotifier{}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.RunNotifier(ctx, notifier)
	_ = cancel // caller is responsible; tests are short-lived
	return srv, s.Token, notifier
}

func TestNotificationTriggeredOnImageFetch(t *testing.T) {
	fakeImage := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG header
	clip := &mockClipboard{
		clipType:  ClipboardInfo{Type: ClipboardImage, Format: "png"},
		imageData: fakeImage,
	}
	srv, tok, notifier := newTestServerWithNotifier(clip)

	req := httptest.NewRequest("GET", "/clipboard/image", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "cc-clip/0.1")
	req.Header.Set("X-CC-Clip-Session", "test-session-abc")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for async notification delivery
	deadline := time.Now().Add(2 * time.Second)
	for notifier.count.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if notifier.count.Load() != 1 {
		t.Fatalf("expected 1 notification, got %d", notifier.count.Load())
	}

	evt := notifier.last.Load().(NotifyEvent)
	if evt.SessionID != "test-session-abc" {
		t.Errorf("expected session test-session-abc, got %s", evt.SessionID)
	}
	if evt.Seq != 1 {
		t.Errorf("expected seq 1, got %d", evt.Seq)
	}
	if evt.Format != "png" {
		t.Errorf("expected format png, got %s", evt.Format)
	}
}

func TestNoNotificationWithoutSessionHeader(t *testing.T) {
	fakeImage := []byte{0x89, 0x50, 0x4E, 0x47}
	clip := &mockClipboard{
		clipType:  ClipboardInfo{Type: ClipboardImage, Format: "png"},
		imageData: fakeImage,
	}
	srv, tok, notifier := newTestServerWithNotifier(clip)

	req := httptest.NewRequest("GET", "/clipboard/image", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "cc-clip/0.1")
	// No X-CC-Clip-Session header
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)
	if notifier.count.Load() != 0 {
		t.Fatalf("expected 0 notifications without session header, got %d", notifier.count.Load())
	}
}

func TestNotificationChannelFullDoesNotBlock(t *testing.T) {
	fakeImage := []byte{0x89, 0x50, 0x4E, 0x47}
	clip := &mockClipboard{
		clipType:  ClipboardInfo{Type: ClipboardImage, Format: "png"},
		imageData: fakeImage,
	}

	tm := token.NewManager(1 * time.Hour)
	s, _ := tm.Generate()
	store := session.NewStore(12 * time.Hour)
	srv := NewServer("127.0.0.1:0", clip, tm, store)
	// Do NOT start RunNotifier — channel will fill up

	// Fill the channel
	for i := 0; i < notifyChCap+5; i++ {
		req := httptest.NewRequest("GET", "/clipboard/image", nil)
		req.Header.Set("Authorization", "Bearer "+s.Token)
		req.Header.Set("User-Agent", "cc-clip/0.1")
		req.Header.Set("X-CC-Clip-Session", "fill-test")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	// If we get here, no deadlock — channel full was handled gracefully
}

func TestDuplicateDetectionViaNotification(t *testing.T) {
	fakeImage := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	clip := &mockClipboard{
		clipType:  ClipboardInfo{Type: ClipboardImage, Format: "png"},
		imageData: fakeImage,
	}
	srv, tok, notifier := newTestServerWithNotifier(clip)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/clipboard/image", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", "cc-clip/0.1")
		req.Header.Set("X-CC-Clip-Session", "dup-test")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
	}

	deadline := time.Now().Add(2 * time.Second)
	for notifier.count.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if notifier.count.Load() != 2 {
		t.Fatalf("expected 2 notifications, got %d", notifier.count.Load())
	}

	evt := notifier.last.Load().(NotifyEvent)
	if evt.Seq != 2 {
		t.Errorf("expected seq 2, got %d", evt.Seq)
	}
	if evt.DuplicateOf != 1 {
		t.Errorf("expected duplicate of seq 1, got %d", evt.DuplicateOf)
	}
}
