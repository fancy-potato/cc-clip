package tunnel

import (
	"encoding/base64"
	"reflect"
	"testing"
	"unicode/utf16"
)

// encodeForPowerShell wraps a cmdline in the same base64(utf-16-le) form
// the PowerShell scanner emits, so tests drive the real parser through
// the real decode path.
func encodeForPowerShell(cmdline string) string {
	u16 := utf16.Encode([]rune(cmdline))
	raw := make([]byte, 0, len(u16)*2)
	for _, c := range u16 {
		raw = append(raw, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestParsePowershellTunnelProcessOutput(t *testing.T) {
	enc := encodeForPowerShell
	tests := []struct {
		name string
		in   string
		want []processInfo
	}{
		{
			name: "single line",
			in:   "1234" + powershellProcessSeparator + enc("ssh -N -R 19001:127.0.0.1:18339 myhost") + "\n",
			want: []processInfo{{pid: 1234, cmdline: "ssh -N -R 19001:127.0.0.1:18339 myhost"}},
		},
		{
			name: "two lines with trailing whitespace",
			in:   "  1234" + powershellProcessSeparator + enc("ssh foo") + "  \n5678" + powershellProcessSeparator + enc("ssh bar") + "\n\n",
			want: []processInfo{
				{pid: 1234, cmdline: "ssh foo"},
				{pid: 5678, cmdline: "ssh bar"},
			},
		},
		{
			name: "blank lines are skipped",
			in:   "\n\n",
			want: nil,
		},
		{
			name: "missing separator is skipped",
			in:   "1234 ssh\n",
			want: nil,
		},
		{
			name: "non-numeric pid is skipped",
			in:   "abc" + powershellProcessSeparator + enc("ssh") + "\n",
			want: nil,
		},
		{
			name: "negative pid is skipped",
			in:   "-1" + powershellProcessSeparator + enc("ssh") + "\n",
			want: nil,
		},
		{
			name: "empty cmdline is skipped",
			in:   "42" + powershellProcessSeparator + "   \n",
			want: nil,
		},
		{
			name: "undecodable base64 row is skipped",
			in:   "42" + powershellProcessSeparator + "!!!not base64!!!" + "\n",
			want: nil,
		},
		{
			name: "cmdline containing separator after base64 decode is preserved verbatim",
			in:   "42" + powershellProcessSeparator + enc("ssh -o LocalForward="+powershellProcessSeparator+"weird") + "\n",
			want: []processInfo{{pid: 42, cmdline: "ssh -o LocalForward=" + powershellProcessSeparator + "weird"}},
		},
		{
			name: "mixed valid and invalid lines yields only the valid ones",
			in: "" +
				"\n" +
				"garbage\n" +
				"100" + powershellProcessSeparator + enc("ssh good") + "\n" +
				"abc" + powershellProcessSeparator + enc("ssh bad") + "\n" +
				"200" + powershellProcessSeparator + enc("ssh also good") + "\n",
			want: []processInfo{
				{pid: 100, cmdline: "ssh good"},
				{pid: 200, cmdline: "ssh also good"},
			},
		},
		{
			name: "crlf line endings",
			in:   "1234" + powershellProcessSeparator + enc("ssh foo") + "\r\n5678" + powershellProcessSeparator + enc("ssh bar") + "\r\n",
			want: []processInfo{
				{pid: 1234, cmdline: "ssh foo"},
				{pid: 5678, cmdline: "ssh bar"},
			},
		},
		{
			name: "embedded crlf inside decoded cmdline is preserved (sourced from a legit base64)",
			in:   "1234" + powershellProcessSeparator + enc("ssh foo\r\nembedded") + "\n",
			want: []processInfo{{pid: 1234, cmdline: "ssh foo\r\nembedded"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePowershellTunnelProcessOutput([]byte(tt.in))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parsePowershellTunnelProcessOutput() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
