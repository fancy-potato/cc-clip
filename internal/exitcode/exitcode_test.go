package exitcode

import "testing"

// TestExitCodeValuesArePinned pins the numeric values of each exit code.
// The remote cc-clip binary and the local SSH caller classify each code
// into a sentinel error without reading stderr, so silently renumbering
// any value would break the `cc-clip uninstall --peer` idempotency path
// and other cross-version wire contracts without touching this file.
// If a value here changes, update internal/shim/peer_remote.go AND the
// troubleshooting doc at the same time.
func TestExitCodeValuesArePinned(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"Success", Success, 0},
		{"NoImage", NoImage, 10},
		{"TunnelUnreachable", TunnelUnreachable, 11},
		{"TokenInvalid", TokenInvalid, 12},
		{"DownloadFailed", DownloadFailed, 13},
		{"InternalError", InternalError, 20},
		{"UsageError", UsageError, 21},
		{"PeerNotFound", PeerNotFound, 22},
	}
	// Track duplicates by `want` (the pinned contract) rather than `got`:
	// if a refactor flips two constants to the same new value, both of
	// them report the correct `want` mismatch AND a duplicate on the old
	// value. Using `got` as the key would mask the duplicate in that case.
	// Fatalf on the first mismatch so we don't pollute `seen` with a
	// known-wrong value that would then report misleading duplicates.
	seen := map[int]string{}
	for _, c := range cases {
		if c.got != c.want {
			t.Fatalf("%s = %d, want %d", c.name, c.got, c.want)
		}
		if prev, dup := seen[c.want]; dup {
			t.Fatalf("%s collides with %s on exit code %d", c.name, prev, c.want)
		}
		seen[c.want] = c.name
	}
}
