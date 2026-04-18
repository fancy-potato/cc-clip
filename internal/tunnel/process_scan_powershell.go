package tunnel

import (
	"encoding/base64"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf16"
)

// powershellDecodeFailuresLogged counts how many times decodePowershellCommandLine
// has already logged a failure so repeated CIM breakage does not spam the
// daemon's log. A single log line per process lifetime is enough signal for
// an operator to know the scanner is seeing something it cannot parse.
var powershellDecodeFailuresLogged atomic.Int32

// powershellProcessSeparator is an ASCII sentinel that separates pid from the
// base64-encoded cmdline in the output of the PowerShell enumeration the
// Windows scanner runs. CommandLine values are base64-encoded at source so
// a local process whose own CommandLine contains CR/LF or this sentinel
// cannot inject a spoofed row into our parser (previously a hostile
// CommandLine starting with `<digits>|CCCLIP|ssh -N -R …` would parse as a
// valid tunnel row targeting the attacker's PID). Defined here (not in the
// windows-only file) so the parser can be unit-tested on any host.
const powershellProcessSeparator = "|CCCLIP|"

// parsePowershellTunnelProcessOutput converts the raw stdout of the
// PowerShell process enumeration into []processInfo. Each row is
// `<pid>|CCCLIP|<base64(utf-16-le CommandLine)>`. Malformed rows — missing
// separator, non-numeric pid, undecodable base64, blank cmdline — are
// skipped silently; the daemon already tolerates partial output, and one
// malformed row must not poison the entire scan.
func parsePowershellTunnelProcessOutput(out []byte) []processInfo {
	var procs []processInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, powershellProcessSeparator, 2)
		if len(parts) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 0 {
			continue
		}
		encoded := strings.TrimSpace(parts[1])
		if encoded == "" {
			continue
		}
		cmdline := decodePowershellCommandLine(encoded)
		if cmdline == "" {
			continue
		}
		procs = append(procs, processInfo{pid: pid, cmdline: cmdline})
	}
	return procs
}

// decodePowershellCommandLine decodes the base64-wrapped UTF-16-LE
// CommandLine blob. On any decode failure (including ragged byte length or
// an invalid base64 body emitted by a transient CIM quirk) returns "" so
// the caller skips the row rather than feeding a corrupted cmdline into
// the matcher.
func decodePowershellCommandLine(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw)%2 != 0 {
		// Log the first decode failure per process lifetime so an operator
		// on a locked-down Windows host (unusual CIM configuration, etc.)
		// has a signal that process adoption is silently degraded.
		if powershellDecodeFailuresLogged.CompareAndSwap(0, 1) {
			log.Printf("cc-clip tunnel: powershell process scanner: first cmdline decode failure (err=%v, raw_len=%d); subsequent failures suppressed", err, len(raw))
		}
		return ""
	}
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		u16 = append(u16, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return strings.TrimSpace(string(utf16.Decode(u16)))
}
