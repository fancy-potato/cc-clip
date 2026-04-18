package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

const tunnelControlAuthHeader = "X-CC-Clip-Tunnel-Token"

// writeTunnelError writes a structured JSON error response so API
// consumers always see a consistent shape regardless of success vs
// failure. Plain-text http.Error responses would force --json CLI
// readers to probe two formats; this helper keeps /tunnels/* uniform.
func writeTunnelError(w http.ResponseWriter, status int, msg string) {
	writeTunnelErrorWithCode(w, status, "", msg)
}

// writeTunnelErrorWithCode writes the error envelope with a machine-readable
// `code` field so clients can classify the failure without string-matching
// the human message. Empty code omits the field entirely (legacy shape).
func writeTunnelErrorWithCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	out := map[string]string{"error": msg}
	if code != "" {
		out["code"] = code
	}
	_ = json.NewEncoder(w).Encode(out)
}

// Known error codes emitted on /tunnels/* responses. Clients match on these
// rather than on the prefix of the human-readable message.
const (
	tunnelErrCodeNotFound = "tunnel-not-found"
)

func writeTunnelOK(w http.ResponseWriter, host string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok", "host": host}); err != nil {
		log.Printf("tunnel response encode: %v", err)
	}
}

// registerTunnelRoutes adds tunnel management endpoints to the mux.
// All routes require the local-only tunnel control token.
//
// Paths are registered WITHOUT method prefixes so the auth middleware runs
// before the method check. Method-prefixed patterns (e.g. "POST /tunnels/up")
// would make Go's ServeMux return 405 for method mismatches *before* auth
// fires, turning the 405 itself into an unauthenticated probe oracle for
// which routes exist. By enforcing method inside the wrapped handler the
// endpoint existence is only observable after the caller passes auth.
func registerTunnelRoutes(mux *http.ServeMux, mgr *tunnel.Manager, daemonPort int) error {
	if mux == nil {
		return fmt.Errorf("registerTunnelRoutes requires mux")
	}
	if mgr == nil {
		return fmt.Errorf("registerTunnelRoutes requires manager")
	}
	// Create the token file on first run so its path is reliably present on
	// disk and logged elsewhere; the actual token value is re-read from disk
	// on every /tunnels/* request. This mirrors the x11-bridge session-token
	// reload pattern: any out-of-band rotation (--rotate-tunnel-token at
	// next start, a future rotation endpoint, a manual `echo > …`) is
	// observed by the very next request without restarting the daemon.
	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		return fmt.Errorf("load tunnel control token: %w", err)
	}
	mgr.SetLocalPort(daemonPort)

	getControlToken := func() (string, error) {
		return token.ReadTunnelControlToken()
	}

	wrap := func(method string, h http.HandlerFunc) http.HandlerFunc {
		return tunnelControlAuthMiddleware(getControlToken, enforceMethod(method, h))
	}

	// Catch-all for unregistered /tunnels/* paths. Without this, Go's
	// ServeMux returns 404 for e.g. /tunnels/nope while /tunnels/up
	// returns 401 to unauthenticated callers — the differential is a
	// probe oracle for which routes exist. ServeMux routes the longest
	// matching pattern, so specific handlers like /tunnels/up still win
	// regardless of registration order.
	//
	// Run the loopback / Host-Origin / UA gate *before* the 401 stub so a
	// non-loopback peer or a cross-origin browser probe sees 403, matching
	// the posture of every registered /tunnels/* endpoint. Without this
	// gate, "is the daemon reachable on this port?" would still be
	// answerable by anyone (the 401 response itself is the tell).
	mux.HandleFunc("/tunnels/", func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemoteAddr(r.RemoteAddr) || !isLoopbackRequest(r) || !isCCClipUserAgent(r.Header.Get("User-Agent")) {
			writeTunnelError(w, http.StatusForbidden, "forbidden")
			return
		}
		writeTunnelError(w, http.StatusUnauthorized, "missing tunnel control authorization")
	})

	mux.HandleFunc("/tunnels", wrap(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		states, err := mgr.List()
		if err != nil {
			log.Printf("tunnel list: %v", err)
			writeTunnelError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(states); err != nil {
			log.Printf("tunnel list encode: %v", err)
		}
	}))

	mux.HandleFunc("/tunnels/up", wrap(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host       string `json:"host"`
			RemotePort int    `json:"remote_port"`
			LocalPort  int    `json:"local_port,omitempty"`
		}
		if err := decodeTunnelRequest(w, r, &req); err != nil {
			return
		}
		if err := tunnel.ValidateSSHHost(req.Host); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateTunnelPort("remote_port", req.RemotePort, false); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		localPort := req.LocalPort
		if err := validateTunnelPort("local_port", localPort, true); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if localPort == 0 {
			localPort = daemonPort
		}
		// Re-validate after the zero-port substitution. validateManagedTunnelLocalPort
		// treats localPort <= 0 as "no constraint", so without this step a
		// daemonPort of 0 (misconfigured caller) would silently create a
		// tunnel config with an invalid port and fail later in startTunnel.
		if err := validateTunnelPort("local_port", localPort, false); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateManagedTunnelLocalPort(daemonPort, localPort); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Build a fresh cfg for every `tunnel up` request with
		// SSHConfigResolved=false (zero value) so Manager.Up → resolveSSHTunnelConfig
		// always re-runs `ssh -G <host>` and picks up any edits the user made
		// to ~/.ssh/config since the last `tunnel up`. This is the contract:
		// `cc-clip tunnel up <host>` is the canonical "refresh the cached ssh
		// config for this host" command. The reconnect loop and daemon-startup
		// adoption intentionally DO NOT re-resolve — that would silently pick up
		// arbitrary ssh_config changes on every network flap or daemon restart,
		// which is the attack-surface the cache exists to pin. Do NOT load from
		// saved state here; if a future edit starts reusing state.Config the
		// cache invalidation guarantee this handler promises is lost.
		cfg := tunnel.TunnelConfig{
			Host:              req.Host,
			LocalPort:         localPort,
			RemotePort:        req.RemotePort,
			Enabled:           true,
			SSHConfigResolved: false,
		}
		if err := mgr.Up(cfg); err != nil {
			if errors.Is(err, tunnel.ErrManagerShuttingDown) {
				writeTunnelError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if errors.Is(err, tunnel.ErrTunnelLocalPortMismatch) {
				writeTunnelError(w, http.StatusBadRequest, err.Error())
				return
			}
			log.Printf("tunnel up %s:%d->%d: %v", cfg.Host, cfg.LocalPort, cfg.RemotePort, err)
			writeTunnelError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeTunnelOK(w, req.Host)
	}))

	mux.HandleFunc("/tunnels/remove", wrap(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host      string `json:"host"`
			LocalPort int    `json:"local_port,omitempty"`
		}
		if err := decodeTunnelRequest(w, r, &req); err != nil {
			return
		}
		if err := tunnel.ValidateSSHHost(req.Host); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		localPort := req.LocalPort
		// Reject zero/missing local_port on remove. Defaulting to
		// daemonPort here would silently scope "remove host X" to a
		// single port owned by this daemon, even when the caller meant
		// to identify a different saved tunnel for the same host. The
		// CLI substitutes daemonPort explicitly before sending.
		if err := validateTunnelPort("local_port", localPort, false); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateManagedTunnelLocalPort(daemonPort, localPort); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := mgr.Remove(req.Host, localPort); err != nil {
			if errors.Is(err, tunnel.ErrManagerShuttingDown) {
				writeTunnelError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if errors.Is(err, tunnel.ErrTunnelLocalPortMismatch) {
				writeTunnelError(w, http.StatusBadRequest, err.Error())
				return
			}
			if errors.Is(err, tunnel.ErrTunnelNotFound) {
				writeTunnelErrorWithCode(w, http.StatusNotFound, tunnelErrCodeNotFound, err.Error())
				return
			}
			log.Printf("tunnel remove %s:%d: %v", req.Host, localPort, err)
			writeTunnelError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeTunnelOK(w, req.Host)
	}))

	mux.HandleFunc("/tunnels/down", wrap(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host      string `json:"host"`
			LocalPort int    `json:"local_port,omitempty"`
		}
		if err := decodeTunnelRequest(w, r, &req); err != nil {
			return
		}
		if err := tunnel.ValidateSSHHost(req.Host); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		localPort := req.LocalPort
		// Reject zero/missing local_port on down for the same reason
		// /tunnels/remove does — defaulting silently scoped a host-only
		// down request to this daemon's port and nothing else. The CLI
		// substitutes daemonPort explicitly before sending.
		if err := validateTunnelPort("local_port", localPort, false); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateManagedTunnelLocalPort(daemonPort, localPort); err != nil {
			writeTunnelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := mgr.Down(req.Host, localPort); err != nil {
			if errors.Is(err, tunnel.ErrManagerShuttingDown) {
				writeTunnelError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if errors.Is(err, tunnel.ErrTunnelLocalPortMismatch) {
				writeTunnelError(w, http.StatusBadRequest, err.Error())
				return
			}
			if errors.Is(err, tunnel.ErrTunnelNotFound) {
				writeTunnelErrorWithCode(w, http.StatusNotFound, tunnelErrCodeNotFound, err.Error())
				return
			}
			log.Printf("tunnel down %s:%d: %v", req.Host, localPort, err)
			writeTunnelError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeTunnelOK(w, req.Host)
	}))

	return nil
}

// decodeTunnelRequest applies the 4 KiB body cap, rejects unknown fields, and
// writes an appropriate status back to the client on failure. Returns a
// non-nil error ONLY when a response has already been written, so callers
// should simply `return` without writing anything further.
//
// Rejects both a truly empty body and a body that is only whitespace or
// `{}`. Without this check, an empty-object POST would decode into the
// zero-valued struct and fall through to downstream validators, which is
// harder to debug than "missing JSON body" — and if a field validator is
// ever relaxed, it would silently pass through phantom defaults.
func decodeTunnelRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	// Require JSON Content-Type so a browser form POST (which defaults to
	// application/x-www-form-urlencoded and therefore dodges preflight) can
	// never reach the decoder. Missing Content-Type is rejected too: some
	// client libraries (and hand-rolled `fetch({mode: "no-cors"})` calls)
	// omit the header entirely, which must not pass as a "default" JSON body.
	// Combined with the custom X-CC-Clip-Tunnel-Token header this closes any
	// residual CSRF vector even if the UA check is softened in the future.
	// `application/json` is the only accepted MIME; parameters (charset=...)
	// are allowed.
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		writeTunnelError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return fmt.Errorf("missing content type")
	}
	mime, _, _ := strings.Cut(ct, ";")
	if strings.TrimSpace(strings.ToLower(mime)) != "application/json" {
		writeTunnelError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return fmt.Errorf("unsupported content type: %s", ct)
	}
	const maxBody = 4096
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeTunnelError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", maxBody))
			return err
		}
		writeTunnelError(w, http.StatusBadRequest, "failed to read body")
		return err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		writeTunnelError(w, http.StatusBadRequest, "missing JSON body")
		return fmt.Errorf("empty body")
	}
	if bytes.Equal(trimmed, []byte("{}")) {
		writeTunnelError(w, http.StatusBadRequest, "missing required fields")
		return fmt.Errorf("empty JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeTunnelError(w, http.StatusBadRequest, "missing JSON body")
			return err
		}
		writeTunnelError(w, http.StatusBadRequest, "invalid JSON")
		return err
	}
	// Reject trailing tokens after the first object so `{...}{...}` or
	// `{...} garbage` cannot smuggle a second payload past the strict-parse
	// contract implied by DisallowUnknownFields.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeTunnelError(w, http.StatusBadRequest, "unexpected trailing data after JSON object")
		if err == nil {
			return fmt.Errorf("trailing data after JSON object")
		}
		return fmt.Errorf("trailing data after JSON object: %w", err)
	}
	return nil
}

// enforceMethod rejects requests whose method does not match the route. It
// runs AFTER tunnelControlAuthMiddleware so the 405 is only observable to
// authenticated callers — unauthenticated callers see 401 regardless of
// method, which prevents the 405 from leaking which paths are registered.
// Sets the `Allow:` header per RFC 7231 §6.5.5 so programmatic clients can
// discover the permitted verb.
func enforceMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeTunnelError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next(w, r)
	}
}

// tunnelControlAuthMiddleware authenticates /tunnels/* requests by reading
// the control token from disk on every call. The token is NOT captured at
// registration time: an out-of-band rotation (manual file replace, future
// rotation endpoint, or a second `cc-clip serve --rotate-tunnel-token`
// launching on a fresh file) takes effect on the very next request
// without touching the running daemon. This matches the x11-bridge
// token-reload pattern and avoids coupling auth correctness to the
// cmdServe initialization order.
func tunnelControlAuthMiddleware(getToken func() (string, error), next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Reject requests whose peer is not actually on the loopback
		// interface. The reverse SSH forward lets a compromised remote host
		// reach the daemon's loopback listener, and Host/Origin/UA checks
		// alone are satisfied by any attacker with the control token. Gating
		// on the TCP peer address makes the token file itself the last line
		// of defense rather than the only one.
		if !isLoopbackRemoteAddr(r.RemoteAddr) {
			writeTunnelError(w, http.StatusForbidden, "forbidden")
			return
		}
		// Reject requests whose Host / Origin / Referer look non-loopback.
		// This blocks DNS-rebinding and browser-driven CSRF even if a
		// malicious local process learned the control token path.
		if !isLoopbackRequest(r) {
			writeTunnelError(w, http.StatusForbidden, "forbidden")
			return
		}
		// User-Agent prefix check is defense-in-depth only: any caller that
		// already holds the control token can spoof the header trivially.
		// The value here is blocking a browser that stumbled onto the port
		// (browsers cannot override User-Agent in cross-origin fetches) and
		// making misconfigured clients fail loud instead of silently.
		// Require an exact `cc-clip` or a `cc-clip/<suffix>` delimiter to
		// reject look-alike values like `cc-clipper` or `cc-clip-evil`.
		ua := r.Header.Get("User-Agent")
		if !isCCClipUserAgent(ua) {
			writeTunnelError(w, http.StatusForbidden, "forbidden")
			return
		}
		controlToken, err := getToken()
		if err != nil {
			// The token file is missing or unreadable. Auth cannot proceed;
			// this is a daemon-side misconfiguration, not a caller error.
			// Log locally and report 500 rather than 401, so operators see
			// the real cause instead of chasing the wrong remediation
			// ("check your token header" when the daemon cannot read its
			// own file).
			log.Printf("tunnel control auth: cannot read token file: %v", err)
			writeTunnelError(w, http.StatusInternalServerError, "tunnel control token unavailable")
			return
		}
		// ConstantTimeCompare already rejects length-mismatched inputs safely;
		// the prior `len(got) != len(controlToken)` short-circuit leaked the
		// token length and bypassed the constant-time path. Feed both slices
		// into the compare unconditionally.
		got := []byte(strings.TrimSpace(r.Header.Get(tunnelControlAuthHeader)))
		if subtle.ConstantTimeCompare(got, []byte(controlToken)) != 1 {
			writeTunnelError(w, http.StatusUnauthorized, "missing tunnel control authorization")
			return
		}
		next(w, r)
	}
}

// isCCClipUserAgent enforces an exact `cc-clip` or `cc-clip/<suffix>` /
// `cc-clip <suffix>` match, where <suffix> must be non-empty AND consist only
// of printable ASCII without control characters, quotes, or shell metachars.
// Without the delimiter a prefix check would also accept `cc-clipper`,
// `cc-clip-evil`, etc., which widens browser-evasion surface unnecessarily.
// Requiring a non-empty suffix after `/` or ` ` also rejects trailing-
// delimiter values like `cc-clip/` or `cc-clip ` that carry no
// version/component identity and serve no legitimate client. The
// character-class restriction on the suffix keeps CR/LF/NUL/`"`/`\`/etc.
// out — the value is never executed, but tightening the shape reduces
// log-forging surface and makes malformed clients fail loud.
func isCCClipUserAgent(ua string) bool {
	const base = "cc-clip"
	if ua == base {
		return true
	}
	for _, sep := range []string{"/", " "} {
		prefix := base + sep
		if !strings.HasPrefix(ua, prefix) || len(ua) <= len(prefix) {
			continue
		}
		if !isValidCCClipUserAgentSuffix(ua[len(prefix):]) {
			continue
		}
		return true
	}
	return false
}

// isValidCCClipUserAgentSuffix restricts the <suffix> half of
// `cc-clip/<suffix>` / `cc-clip <suffix>` to printable ASCII (excluding
// space, delete, and quotes/backslashes). This mirrors the shape of a
// version string / component identifier and rejects CR, LF, NUL, tab, and
// any other control char that a caller could use to log-forge or break out
// of the UA token.
func isValidCCClipUserAgentSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r < 0x21 || r > 0x7E:
			// Non-printable ASCII, including CR/LF/NUL/tab and anything
			// outside the 7-bit printable range.
			return false
		case r == '"' || r == '\\':
			return false
		}
	}
	return true
}

// isLoopbackRemoteAddr parses r.RemoteAddr and returns true only if the peer
// IP is a loopback address. httptest.NewServer binds to 127.0.0.1 so unit
// tests naturally pass. A TCP connection carried over a reverse SSH forward
// terminates on the loopback interface, so this check holds even under the
// reverse-forward attack model — but only because we also demand a matching
// token; neither alone is sufficient.
func isLoopbackRemoteAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	// Require the Host header carry a port. Go's net/http sets r.Host to
	// whatever the client sent; a bare value like `Host: localhost` (no
	// port) would otherwise pass through the prior fallback and be accepted
	// even though it's not the shape a local HTTP client would ever send.
	// Reverse-proxy and rebinding edge cases rely on the bare-host path, so
	// shut it down here and keep the TCP peer-IP check as the other gate.
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func isLoopbackRequest(r *http.Request) bool {
	if !isLoopbackHost(r.Host) {
		return false
	}
	for _, header := range []string{"Origin", "Referer"} {
		raw := r.Header.Get(header)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return false
		}
		if !isLoopbackHost(u.Host) {
			return false
		}
	}
	return true
}
