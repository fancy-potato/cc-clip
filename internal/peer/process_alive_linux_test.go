//go:build linux

package peer

import "testing"

func TestParseLinuxProcStatState(t *testing.T) {
	stat := "12345 (cc-clip peer) Z 1 2 3 4 5"
	got, err := parseLinuxProcStatState(stat)
	if err != nil {
		t.Fatalf("parseLinuxProcStatState: %v", err)
	}
	if got != 'Z' {
		t.Fatalf("state = %q, want Z", got)
	}
}
