//go:build !windows

package tunnel

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseRunningTunnelProcessesOutput(t *testing.T) {
	longArg := strings.Repeat("x", 70*1024)
	cases := []struct {
		name    string
		input   string
		wantLen int
		check   func(t *testing.T, procs []processInfo)
	}{
		{
			name:    "empty",
			input:   "",
			wantLen: 0,
		},
		{
			name:    "blank lines only",
			input:   "\n\n   \n",
			wantLen: 0,
		},
		{
			name:    "non-numeric pid skipped",
			input:   "abc ssh -N -R 19001:127.0.0.1:18339 example\n",
			wantLen: 0,
		},
		{
			name:    "negative pid skipped",
			input:   "-1 ssh -N\n",
			wantLen: 0,
		},
		{
			name:    "single short line",
			input:   "4321 ssh -N -R 19001:127.0.0.1:18339 example\n",
			wantLen: 1,
			check: func(t *testing.T, procs []processInfo) {
				if procs[0].pid != 4321 {
					t.Fatalf("pid = %d, want 4321", procs[0].pid)
				}
				if !strings.Contains(procs[0].cmdline, "-R 19001:127.0.0.1:18339") {
					t.Fatalf("cmdline = %q, want it to contain -R spec", procs[0].cmdline)
				}
			},
		},
		{
			name:    "line without cmdline skipped",
			input:   "42\n",
			wantLen: 0,
		},
		{
			name: "multiple rows",
			input: strings.Join([]string{
				"100 ssh -N -R 19001:127.0.0.1:18339 a",
				"",
				"200 ssh -N -R 19002:127.0.0.1:18339 b",
				"300 curl -s http://example",
				"",
			}, "\n"),
			wantLen: 3,
		},
		{
			name:    "long argv is preserved whole",
			input:   strconv.Itoa(4321) + " ssh -N -R 19001:127.0.0.1:18339 example " + longArg,
			wantLen: 1,
			check: func(t *testing.T, procs []processInfo) {
				if !strings.Contains(procs[0].cmdline, longArg) {
					t.Fatal("expected long command line to be preserved")
				}
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRunningTunnelProcessesOutput([]byte(tt.input))
			if len(got) != tt.wantLen {
				t.Fatalf("got %d processes, want %d: %+v", len(got), tt.wantLen, got)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}
