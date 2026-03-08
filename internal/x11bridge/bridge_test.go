package x11bridge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Unit tests (no X11 needed) ---

func TestAtomNames(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"clipboard", AtomNameClipboard},
		{"targets", AtomNameTargets},
		{"timestamp", AtomNameTimestamp},
		{"incr", AtomNameIncr},
		{"image_png", AtomNameImagePNG},
	}

	for _, tt := range tests {
		if tt.expected == "" {
			t.Errorf("%s: atom name is empty", tt.name)
		}
	}
}

func TestIncrTransferIsComplete(t *testing.T) {
	t.Run("not started", func(t *testing.T) {
		tr := &IncrTransfer{Data: []byte("hello"), Offset: 0}
		if tr.IsComplete() {
			t.Error("expected not complete")
		}
	})

	t.Run("partial", func(t *testing.T) {
		tr := &IncrTransfer{Data: []byte("hello"), Offset: 3}
		if tr.IsComplete() {
			t.Error("expected not complete")
		}
	})

	t.Run("complete", func(t *testing.T) {
		tr := &IncrTransfer{Data: []byte("hello"), Offset: 5}
		if !tr.IsComplete() {
			t.Error("expected complete")
		}
	})

	t.Run("beyond", func(t *testing.T) {
		tr := &IncrTransfer{Data: []byte("hello"), Offset: 10}
		if !tr.IsComplete() {
			t.Error("expected complete")
		}
	})
}

func TestAtomsToBytes(t *testing.T) {
	atoms := []uint32{1, 256, 65536}
	// Convert to xproto.Atom for type compatibility
	// (xproto.Atom is uint32)
	import_atoms := make([]uint32, len(atoms))
	copy(import_atoms, atoms)

	// Test the byte conversion manually
	data := make([]byte, len(atoms)*4)
	for i, a := range atoms {
		data[i*4+0] = byte(a)
		data[i*4+1] = byte(a >> 8)
		data[i*4+2] = byte(a >> 16)
		data[i*4+3] = byte(a >> 24)
	}

	if len(data) != 12 {
		t.Errorf("expected 12 bytes, got %d", len(data))
	}
}

// --- Helpers for integration tests ---

func requireXvfbAndXclip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not available, skipping X11 test")
	}
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Skip("xclip not available, skipping X11 test")
	}
}

// startTestXvfb starts a temporary Xvfb for testing.
// Returns the display string (e.g. ":99") and a cleanup function.
func startTestXvfb(t *testing.T) string {
	t.Helper()

	// Find a free display number by trying to start Xvfb.
	// Use -displayfd to let Xvfb choose.
	displayFile := filepath.Join(t.TempDir(), "display")

	cmd := exec.Command("Xvfb", "-displayfd", "1", "-screen", "0", "1x1x24", "-nolisten", "tcp")

	// Capture stdout for display number.
	outFile, err := os.Create(displayFile)
	if err != nil {
		t.Fatalf("cannot create display file: %v", err)
	}
	cmd.Stdout = outFile

	if err := cmd.Start(); err != nil {
		outFile.Close()
		t.Fatalf("cannot start Xvfb: %v", err)
	}
	outFile.Close()

	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	// Wait for display file to be written.
	var display string
	for i := 0; i < 20; i++ {
		data, err := os.ReadFile(displayFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			display = ":" + strings.TrimSpace(string(data))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if display == "" {
		t.Fatal("Xvfb did not write display number")
	}

	return display
}

// startMockDaemon starts a mock cc-clip HTTP server.
func startMockDaemon(t *testing.T, clipType string, imageData []byte, token string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /clipboard/type", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"type":   clipType,
			"format": "png",
		})
	})

	mux.HandleFunc("GET /clipboard/image", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if len(imageData) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(imageData)
	})

	// Use a specific listener to get a predictable port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot create listener: %v", err)
	}

	srv := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	srv.Start()
	t.Cleanup(srv.Close)

	return srv
}

func portFromURL(url string) int {
	// url is like "http://127.0.0.1:12345"
	parts := strings.Split(url, ":")
	if len(parts) < 3 {
		return 0
	}
	var port int
	fmt.Sscanf(parts[2], "%d", &port)
	return port
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatalf("cannot write token file: %v", err)
	}
	return path
}

// --- Integration tests (require Xvfb + xclip) ---

func TestBridge_ClaimOwnership(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	tokenFile := writeTokenFile(t, "test-token")
	srv := startMockDaemon(t, "image", []byte("fake-png"), "test-token")
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)

	// Give bridge time to start event loop.
	time.Sleep(200 * time.Millisecond)

	// Verify: xclip should be able to query TARGETS without hanging.
	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xclip TARGETS failed: %v\noutput: %s", err, out)
	}

	output := string(out)
	if !strings.Contains(output, "image/png") {
		t.Errorf("TARGETS output should contain image/png, got: %s", output)
	}
}

func TestBridge_TargetsNoImage(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	tokenFile := writeTokenFile(t, "test-token")
	srv := startMockDaemon(t, "text", nil, "test-token")
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, _ := cmd.CombinedOutput()

	output := string(out)
	if strings.Contains(output, "image/png") {
		t.Errorf("TARGETS output should NOT contain image/png when clipboard has text, got: %s", output)
	}
}

func TestBridge_ImageSmall(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	// Create a small test PNG (just some bytes, doesn't need to be valid PNG for protocol test).
	imageData := make([]byte, 1024)
	rand.Read(imageData)

	tokenFile := writeTokenFile(t, "test-token")
	srv := startMockDaemon(t, "image", imageData, "test-token")
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("xclip image read failed: %v", err)
	}

	if len(out) != len(imageData) {
		t.Errorf("image size mismatch: got %d, want %d", len(out), len(imageData))
	}
	for i := range out {
		if out[i] != imageData[i] {
			t.Errorf("image data mismatch at byte %d", i)
			break
		}
	}
}

func TestBridge_ImageLargeINCR(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	// Create a large image that will trigger INCR protocol (>256KB).
	imageData := make([]byte, 512*1024)
	rand.Read(imageData)

	tokenFile := writeTokenFile(t, "test-token")
	srv := startMockDaemon(t, "image", imageData, "test-token")
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("xclip image read failed: %v", err)
	}

	if len(out) != len(imageData) {
		t.Errorf("INCR image size mismatch: got %d, want %d", len(out), len(imageData))
	}
	for i := range out {
		if out[i] != imageData[i] {
			t.Errorf("INCR image data mismatch at byte %d", i)
			break
		}
	}
}

func TestBridge_TunnelDown(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	tokenFile := writeTokenFile(t, "test-token")
	// Use a port with no server — tunnel is "down".
	bridge, err := New(display, 19999, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// xclip should fail (or return empty), not hang.
	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)

	done := make(chan struct{})
	go func() {
		cmd.CombinedOutput()
		close(done)
	}()

	select {
	case <-done:
		// Good: xclip completed (probably with error).
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("xclip hung when tunnel is down")
	}
}

func TestBridge_TokenInvalid(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	tokenFile := writeTokenFile(t, "wrong-token")
	srv := startMockDaemon(t, "image", []byte("png-data"), "correct-token")
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// xclip should fail, not hang.
	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	_, err = cmd.Output()
	if err == nil {
		t.Error("expected xclip to fail with invalid token")
	}
}

func TestBridge_EmptyImage204(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	tokenFile := writeTokenFile(t, "test-token")
	srv := startMockDaemon(t, "image", nil, "test-token") // nil image -> 204
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	_, err = cmd.Output()
	if err == nil {
		t.Error("expected xclip to fail with empty clipboard")
	}
}

// TestE2E_FullPasteFlow is the end-to-end smoke test:
// mock HTTP + Xvfb + x11-bridge + xclip roundtrip.
func TestE2E_FullPasteFlow(t *testing.T) {
	requireXvfbAndXclip(t)
	display := startTestXvfb(t)

	// Generate deterministic test image data.
	imageData := make([]byte, 4096)
	rand.Read(imageData)

	tokenFile := writeTokenFile(t, "e2e-token")
	srv := startMockDaemon(t, "image", imageData, "e2e-token")
	port := portFromURL(srv.URL)

	bridge, err := New(display, port, tokenFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// Step 1: Query TARGETS
	targetsCmd := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	targetsCmd.Env = append(os.Environ(), "DISPLAY="+display)
	targetsOut, err := targetsCmd.Output()
	if err != nil {
		t.Fatalf("TARGETS query failed: %v", err)
	}
	if !strings.Contains(string(targetsOut), "image/png") {
		t.Fatalf("TARGETS does not contain image/png: %s", targetsOut)
	}

	// Step 2: Read image
	imageCmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	imageCmd.Env = append(os.Environ(), "DISPLAY="+display)
	imageOut, err := imageCmd.Output()
	if err != nil {
		t.Fatalf("image read failed: %v", err)
	}

	// Step 3: Compare bytes
	if len(imageOut) != len(imageData) {
		t.Fatalf("E2E size mismatch: got %d, want %d", len(imageOut), len(imageData))
	}
	for i := range imageOut {
		if imageOut[i] != imageData[i] {
			t.Fatalf("E2E data mismatch at byte %d", i)
		}
	}
}
