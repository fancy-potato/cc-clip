// Package sshconfig manages cc-clip's per-host `SetEnv` marker block
// inside the user's local ~/.ssh/config. It lets `cc-clip setup`
// inject CC_CLIP_PORT / CC_CLIP_STATE_DIR into a specific
// `Host <alias>` block so that interactive SSH sessions push per-laptop
// env to a shared remote Unix account.
//
// Scope: this package ONLY edits the local laptop's ~/.ssh/config and
// only inside an existing `Host <alias>` literal block. It never
// creates a new Host entry, never touches the daemon's reverse tunnel,
// and never writes to remote paths.
package sshconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
)

// MarkerBegin and MarkerEnd wrap the cc-clip-managed SetEnv block inside
// the user's `Host <alias>` stanza. Exported so external tools (tests,
// doctor-style diagnostics) can match the markers without duplicating
// the literal text, which would drift silently from this package on
// rename.
const (
	MarkerBegin = "# >>> cc-clip SetEnv (do not edit) >>>"
	MarkerEnd   = "# <<< cc-clip SetEnv (do not edit) <<<"

	// Internal aliases retained so the rest of this file stays terse.
	markerBegin = MarkerBegin
	markerEnd   = MarkerEnd
)

// ErrHostBlockMissing means no `Host <alias>` block — literal or
// wildcard — applies to the requested alias. The user should add a
// literal `Host <alias>` block to ~/.ssh/config.
var ErrHostBlockMissing = errors.New("no `Host <alias>` block found in ~/.ssh/config")

// ErrOnlyGlobMatch means the only `Host` blocks that would apply to
// the alias use wildcard (`*`, `?`) or negation (`!`) patterns. cc-clip
// refuses to inject SetEnv into such a block because it would leak
// per-laptop env vars to every host the pattern matches, and because
// negation-bearing blocks have semantics that don't map cleanly to a
// per-alias injection.
var ErrOnlyGlobMatch = errors.New("alias is matched only by a wildcard or negation `Host` pattern; add a literal `Host <alias>` block")

// ErrInvalidHost means the alias contains characters that cannot
// appear in a Host token (whitespace, control chars, `#`, marker
// substrings, or wildcard metacharacters).
var ErrInvalidHost = errors.New("invalid host alias for ssh_config")

// ErrInvalidEnvValue means a SetEnv value contains characters that
// cannot be safely emitted (newline / NUL).
var ErrInvalidEnvValue = errors.New("invalid SetEnv value")

// ErrSetEnvConflict means the matching Host block already contains a
// user-authored SetEnv directive. OpenSSH only honors the first SetEnv
// directive it sees for a host, so cc-clip refuses to inject a second one and
// instead asks the user to merge the variables manually.
var ErrSetEnvConflict = errors.New("host already contains a user-authored SetEnv directive")

// ErrSymlinkConfig means ~/.ssh/config is a symlink. cc-clip refuses
// to rewrite symlinked configs so a setup/uninstall run cannot replace
// the link with a regular file and silently detach the user's dotfiles.
var ErrSymlinkConfig = errors.New("refusing to edit symlinked ~/.ssh/config")

// ErrHostBlockInInclude is returned when the top-level ~/.ssh/config has
// no literal `Host <alias>` block matching the alias, but does contain
// one or more `Include` directives. Because cc-clip does NOT walk
// includes (see docs/troubleshooting.md — walking includes would let a
// path-traversal exploit in an included file rewrite an unrelated one),
// this state is ambiguous: the alias might legitimately live in an
// included file. Fail loud so the user can either inline the Host block
// into the top file or disable the Include.
var ErrHostBlockInInclude = errors.New("no literal `Host <alias>` block in top-level ssh_config; cc-clip does not walk Include directives — add a literal `Host <alias>` block to ~/.ssh/config")

// LocalConfigPath returns ~/.ssh/config (resolved against $HOME). It
// does not check whether the file exists; Apply/Remove will surface
// that as an os error.
func LocalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// Apply writes CC_CLIP_PORT / CC_CLIP_STATE_DIR (and any other env
// pairs) into the cc-clip-managed marker block inside the user's
// existing literal `Host <host>` block. The marker pair is created if
// missing, or its body is replaced if already present.
//
// Apply is idempotent: applying the same env twice produces the same
// file contents.
func Apply(host string, env map[string]string) error {
	path, err := LocalConfigPath()
	if err != nil {
		return err
	}
	return ApplyToFile(path, host, env)
}

// Remove deletes the cc-clip marker pair (and lines between) from the
// user's existing literal `Host <host>` block. No-op if either the
// Host block or the marker pair is absent.
func Remove(host string) error {
	path, err := LocalConfigPath()
	if err != nil {
		return err
	}
	return RemoveFromFile(path, host)
}

// ReadManagedEnvFromBytes parses the cc-clip-managed SetEnv marker block
// inside the `Host <host>` stanza and returns the key=value map it carries.
// Returns nil (and no error) when no managed block for the host exists —
// that state is "no SetEnv has ever been applied" and callers should treat
// it as an advisory, not a failure. An error is returned only when the
// config structurally cannot be parsed (unreadable as ssh_config).
//
// The parser walks the same marker pair that Apply writes, so a round-trip
// Apply(env) → ReadManagedEnvFromBytes returns the input map modulo the
// quoting rules (values are decoded from their ssh_config-quoted form).
// This is what the `cc-clip doctor` SetEnv-alignment check consumes to
// detect stale blocks after a reconnect on a new port.
func ReadManagedEnvFromBytes(data []byte, host string) (map[string]string, error) {
	if err := validateHost(host); err != nil {
		return nil, err
	}
	lines, _ := splitLines(data)
	blocks, status := findHostBlocks(lines, host)
	if status != hostMatchLiteral || len(blocks) == 0 {
		return nil, nil
	}
	foundManagedBlock := false
	for _, block := range blocks {
		begin, end, ok := findMarkerPair(lines, block.start, block.end)
		if !ok {
			continue
		}
		foundManagedBlock = true
		for i := begin + 1; i < end; i++ {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			keyword, rest := splitDirective(trimmed)
			if !strings.EqualFold(keyword, "setenv") {
				continue
			}
			return parseSetEnvAssignments(rest)
		}
	}
	if foundManagedBlock {
		return nil, fmt.Errorf("%w: managed SetEnv block for Host %s contains no SetEnv directive", ErrInvalidEnvValue, host)
	}
	return nil, nil
}

// parseSetEnvAssignments splits the argument of a `SetEnv KEY=V …` line
// into a key=value map, honoring double-quoted values and backslash
// escapes the same way OpenSSH's tokenizer does. Returns an error for
// malformed input (a bare token with no `=`, an unterminated quote).
func parseSetEnvAssignments(rest string) (map[string]string, error) {
	tokens, err := splitSetEnvTokens(rest)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tokens))
	for _, tok := range tokens {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%w: SetEnv token %q has no `=`", ErrInvalidEnvValue, tok)
		}
		out[tok[:eq]] = tok[eq+1:]
	}
	return out, nil
}

func splitSetEnvTokens(rest string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	escape := false
	for _, r := range rest {
		if escape {
			cur.WriteRune(r)
			escape = false
			continue
		}
		switch {
		case r == '\\' && inQuote:
			escape = true
		case r == '"':
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("%w: unterminated quote in SetEnv", ErrInvalidEnvValue)
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

// ApplyToFile is the testable variant of Apply that operates on an
// explicit file path.
func ApplyToFile(path, host string, env map[string]string) error {
	if err := validateHost(host); err != nil {
		return err
	}
	for k, v := range env {
		if err := validateEnvKey(k); err != nil {
			return err
		}
		if err := validateEnvValue(v); err != nil {
			return err
		}
	}

	// Serialize concurrent Apply/Remove on the same config file via an
	// advisory flock on a sidecar. Two racing Applies must not read the
	// same snapshot, both append, and clobber each other's marker block.
	// A lock failure is surfaced as an error rather than silently falling
	// back to an unlocked path: the atomic rename prevents torn writes but
	// does NOT prevent a lost-update race, and previously the silent
	// degrade defeated the cross-process serialization invariant that
	// multi-laptop setups rely on. Windows has no flock and its
	// acquireConfigLock is a documented no-op (returns nil).
	release, err := acquireConfigLock(path)
	if err != nil {
		return fmt.Errorf("acquire ssh_config advisory lock: %w", err)
	}
	defer release()

	data, meta, err := readConfig(path)
	if err != nil {
		return err
	}
	lines, format := splitLines(data)

	blocks, status := findHostBlocks(lines, host)
	switch status {
	case hostMatchLiteral:
		// proceed
	case hostMatchGlob:
		return ErrOnlyGlobMatch
	default:
		if hasIncludeDirective(lines) {
			return ErrHostBlockInInclude
		}
		return ErrHostBlockMissing
	}

	cleanedLines, _ := removeManagedMarkersFromBlocks(lines, blocks)
	blocks, status = findHostBlocks(cleanedLines, host)
	if status != hostMatchLiteral || len(blocks) == 0 {
		return ErrHostBlockMissing
	}
	for _, block := range blocks {
		if blockHasUserSetEnv(cleanedLines, block) {
			return ErrSetEnvConflict
		}
	}
	// When a user has multiple literal `Host <alias>` stanzas in their own
	// ~/.ssh/config, we CONSOLIDATE: managed markers in every matching
	// block were just stripped above, and we now insert a single fresh
	// marker into blocks[0] (the earliest in the file). OpenSSH honors
	// the FIRST SetEnv directive it sees per host, so any later block's
	// SetEnv would be silently ignored anyway — emitting one marker at
	// the top of the match chain matches OpenSSH's effective semantics
	// and keeps Remove idempotent. This is NOT the multi-laptop
	// scenario; that scenario has each laptop writing its own separate
	// ~/.ssh/config and sharing only the remote account. Pinned by
	// TestApplyConsolidatesDuplicateLiteralHostBlocks.
	block := blocks[0]
	indent := detectIndent(cleanedLines, block)
	rendered, err := renderMarkerBlock(env, indent)
	if err != nil {
		return err
	}

	newLines := replaceOrInsertMarker(cleanedLines, block, rendered)
	out := joinLines(newLines, format)
	if bytes.Equal(out, data) {
		return nil
	}
	return writeAtomic(path, out, meta)
}

// RemoveFromFile is the testable variant of Remove that operates on
// an explicit file path.
func RemoveFromFile(path, host string) error {
	if err := validateHost(host); err != nil {
		return err
	}

	// Mirror ApplyToFile's advisory-lock discipline so a concurrent
	// Apply cannot see a half-removed state and vice-versa. Lock failure
	// is fatal for the same reason: a silent no-lock fallback would
	// allow a concurrent Apply to observe a half-removed state.
	release, err := acquireConfigLock(path)
	if err != nil {
		return fmt.Errorf("acquire ssh_config advisory lock: %w", err)
	}
	defer release()

	data, meta, err := readConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	lines, format := splitLines(data)

	blocks, status := findHostBlocks(lines, host)
	if status != hostMatchLiteral || len(blocks) == 0 {
		return nil
	}

	newLines, removed := removeManagedMarkersFromBlocks(lines, blocks)
	if !removed {
		return nil
	}
	out := joinLines(newLines, format)
	if bytes.Equal(out, data) {
		return nil
	}
	return writeAtomic(path, out, meta)
}

type hostMatchStatus int

const (
	hostMatchNone hostMatchStatus = iota
	hostMatchLiteral
	hostMatchGlob
)

type hostBlock struct {
	// start is the index of the `Host …` directive line.
	// end is exclusive: the index of the next top-level `Host`/`Match`
	// directive or len(lines).
	start int
	end   int
}

// collectContinuation returns the logical remainder of a directive that
// uses ssh_config backslash-newline continuation. Given `rest` parsed
// from the keyword line at index i, walk forward through any lines that
// extend the directive and splice their content in, joined by a single
// space. Without this, `Host alpha \<newline>beta` would tokenize as
// `["alpha", "\\"]` and the beta alias would be silently lost.
// Returns the coalesced rest and the last physical line index consumed
// (== i when no continuation was present).
func collectContinuation(lines []string, i int, rest string) (string, int) {
	end := i
	// Cap the continuation walk so a pathological ssh_config with
	// thousands of trailing-backslash lines under one directive cannot
	// O(n²) us via repeated string concat. 64 is well past any realistic
	// Host alias wrap count (ssh_config aliases are usually one line).
	const maxContinuation = 64
	for steps := 0; steps < maxContinuation && strings.HasSuffix(rest, `\`) && end+1 < len(lines); steps++ {
		// Drop the trailing backslash and splice the next line's content.
		// ssh_config joins continuations with an implicit separator, so a
		// single space keeps the tokenizer happy without collapsing
		// existing whitespace around the join point.
		rest = strings.TrimSuffix(rest, `\`) + " " + strings.TrimLeft(lines[end+1], " \t")
		end++
	}
	return rest, end
}

func findHostBlocks(lines []string, alias string) ([]hostBlock, hostMatchStatus) {
	type pendingBlock struct {
		start  int
		tokens []string
	}
	var current *pendingBlock
	var literals []int
	globMatched := false

	flush := func() {
		if current == nil {
			return
		}
		switch classifyHostMatch(current.tokens, alias) {
		case hostMatchLiteral:
			literals = append(literals, current.start)
		case hostMatchGlob:
			globMatched = true
		}
		current = nil
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// `Host` / `Match` start or end blocks even when the user indents the
		// stanza; ssh_config ignores leading whitespace on keywords.
		if !startsTopLevelDirective(line) {
			continue
		}
		// We hit a top-level directive — close any pending block.
		flush()
		keyword, rest := splitDirective(trimmed)
		switch strings.ToLower(keyword) {
		case "host":
			joinedRest, _ := collectContinuation(lines, i, rest)
			tokens := tokenizeHostPatterns(joinedRest)
			current = &pendingBlock{start: i, tokens: tokens}
		case "match":
			// `Match host …` blocks are intentionally not considered:
			// SetEnv inside a Match block has surprising scoping rules
			// and we restrict injection to plain Host blocks.
		}
	}
	flush()

	if len(literals) > 0 {
		blocks := make([]hostBlock, 0, len(literals))
		for _, start := range literals {
			blocks = append(blocks, hostBlock{start: start, end: blockEnd(lines, start)})
		}
		return blocks, hostMatchLiteral
	}
	if globMatched {
		return nil, hostMatchGlob
	}
	return nil, hostMatchNone
}

// hasIncludeDirective reports whether the config contains a top-level
// `Include` directive outside any Host/Match block. OpenSSH allows
// Include inside a block too, but the risk we care about is the
// user's `Host <alias>` living in an included file — which only
// requires a top-level Include to be reachable.
func hasIncludeDirective(lines []string) bool {
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if startsTopLevelDirective(line) {
			keyword, _ := splitDirective(trimmed)
			switch strings.ToLower(keyword) {
			case "host", "match":
				inBlock = true
				continue
			}
		}
		keyword, _ := splitDirective(trimmed)
		if !inBlock && strings.EqualFold(keyword, "include") {
			return true
		}
	}
	return false
}

func blockEnd(lines []string, start int) int {
	for i := start + 1; i < len(lines); i++ {
		if startsTopLevelDirective(lines[i]) {
			keyword, _ := splitDirective(strings.TrimSpace(lines[i]))
			kw := strings.ToLower(keyword)
			if kw == "host" || kw == "match" {
				return i
			}
		}
	}
	return len(lines)
}

// startsTopLevelDirective reports whether the line is a `Host` or `Match`
// directive that starts or ends an ssh_config block. Leading whitespace is
// ignored because OpenSSH accepts indented stanzas.
func startsTopLevelDirective(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	keyword, _ := splitDirective(trimmed)
	switch strings.ToLower(keyword) {
	case "host", "match":
		return true
	default:
		return false
	}
}

func splitDirective(trimmed string) (keyword, rest string) {
	if idx := strings.IndexAny(trimmed, " \t="); idx >= 0 {
		// ssh_config allows `Key=Value` and `Key Value`.
		keyword = trimmed[:idx]
		rest = strings.TrimLeft(trimmed[idx+1:], " \t=")
		return
	}
	return trimmed, ""
}

// tokenizeHostPatterns splits the patterns following `Host` into
// individual tokens, honoring double-quoted strings (which let users
// embed spaces — uncommon but legal). A `#` starts a trailing comment
// only when it appears at a token boundary (line start or immediately
// after whitespace). That matches real-world ssh_config usage like
// `Host alpha beta # staging` without truncating a glued alias such as
// `Host myalias#comment`, which OpenSSH treats as a literal token.
// Returns the raw tokens with quotes stripped.
func tokenizeHostPatterns(rest string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range rest {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '#' && !inQuote && cur.Len() == 0:
			flush()
			return tokens
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func classifyHostMatch(tokens []string, alias string) hostMatchStatus {
	status := hostMatchNone
	for _, t := range tokens {
		negated := strings.HasPrefix(t, "!")
		pattern := strings.TrimPrefix(t, "!")
		if pattern == "" {
			continue
		}
		isGlob := containsGlobMeta(pattern)
		matched := false
		if isGlob {
			if m, err := matchSSHPattern(pattern, alias); err == nil {
				matched = m
			}
		} else {
			matched = pattern == alias
		}
		if !matched {
			continue
		}
		if negated {
			return hostMatchNone
		}
		if !isGlob {
			status = hostMatchLiteral
			continue
		}
		if status != hostMatchLiteral {
			status = hostMatchGlob
		}
	}
	return status
}

func containsGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?")
}

// matchSSHPattern implements ssh_config's `*` and `?` wildcards.
// `*` matches any sequence (including empty); `?` matches exactly one
// character. No character classes (ssh_config doesn't support them
// either).
func matchSSHPattern(pattern, name string) (bool, error) {
	return globMatch(pattern, name), nil
}

func globMatch(pattern, name string) bool {
	pi, ni := 0, 0
	starPi, starNi := -1, 0
	for ni < len(name) {
		if pi < len(pattern) {
			c := pattern[pi]
			if c == '*' {
				starPi = pi
				starNi = ni
				pi++
				continue
			}
			if c == '?' || c == name[ni] {
				pi++
				ni++
				continue
			}
		}
		if starPi != -1 {
			pi = starPi + 1
			starNi++
			ni = starNi
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// detectIndent returns the leading whitespace style used inside the
// host block (tab or N spaces). Defaults to two spaces when the block
// has no indented options yet.
func detectIndent(lines []string, block hostBlock) string {
	for i := block.start + 1; i < block.end; i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Skip our own marker / SetEnv lines so we detect the user's style.
		if strings.HasPrefix(trimmed, "# >>> cc-clip") || strings.HasPrefix(trimmed, "# <<< cc-clip") {
			continue
		}
		indent := line[:len(line)-len(trimmed)]
		if indent != "" {
			return indent
		}
	}
	return "  "
}

func blockHasUserSetEnv(lines []string, block hostBlock) bool {
	for i := block.start + 1; i < block.end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keyword, _ := splitDirective(trimmed)
		if strings.EqualFold(keyword, "setenv") {
			return true
		}
	}
	return false
}

func renderMarkerBlock(env map[string]string, indent string) ([]string, error) {
	line, err := ManagedSetEnvLine(env)
	if err != nil {
		// Callers validate env before reaching renderMarkerBlock, but
		// returning rather than panicking keeps the daemon (which calls
		// into this package via cmd/cc-clip/main.go during `cc-clip
		// connect`) crash-free if a future caller skips validation.
		return nil, err
	}

	out := make([]string, 0, 3)
	out = append(out, indent+markerBegin)
	out = append(out, indent+line)
	out = append(out, indent+markerEnd)
	return out, nil
}

// ManagedSetEnvLine formats env as a single `SetEnv KEY=VALUE ...` directive
// using the same ordering and quoting rules Apply writes into ssh_config.
func ManagedSetEnvLine(env map[string]string) (string, error) {
	keys := make([]string, 0, len(env))
	for k, v := range env {
		if err := validateEnvKey(k); err != nil {
			return "", err
		}
		if err := validateEnvValue(v); err != nil {
			return "", err
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	assignments := make([]string, 0, len(keys))
	for _, k := range keys {
		assignments = append(assignments, formatSetEnvAssignment(k, env[k]))
	}
	// Emit a single SetEnv directive containing every assignment. OpenSSH only
	// remembers the first SetEnv directive it sees, so splitting CC_CLIP_PORT
	// and CC_CLIP_STATE_DIR across multiple lines would silently drop one.
	return "SetEnv " + strings.Join(assignments, " "), nil
}

func formatSetEnvAssignment(key, value string) string {
	if needsQuoting(value) {
		// Escape embedded backslashes and double quotes.
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return fmt.Sprintf(`%s="%s"`, key, escaped)
	}
	return fmt.Sprintf("%s=%s", key, value)
}

func needsQuoting(value string) bool {
	if value == "" {
		return true
	}
	for _, r := range value {
		// Whitespace requires quoting so the value stays one token.
		// A literal `"` or `\` in an unquoted value would make OpenSSH's
		// tokenizer trip on unbalanced quoting; emitting quoted-and-escaped
		// is unambiguous regardless of what the user smuggled in.
		// `#` triggers OpenSSH's trailing-comment tokenizer once emitted
		// unquoted into ssh_config, silently truncating values like
		// /home/u/foo#bar to /home/u/foo. CC_CLIP_STATE_DIR flows from the
		// peer registry, so a `#` in a remote-supplied path must be quoted
		// to survive round-trip through ssh -G.
		if unicode.IsSpace(r) || r == '"' || r == '\\' || r == '#' {
			return true
		}
	}
	return false
}

func findMarkerPair(lines []string, start, end int) (int, int, bool) {
	beginIdx := -1
	for i := start + 1; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == markerBegin {
			beginIdx = i
			break
		}
	}
	if beginIdx == -1 {
		return -1, -1, false
	}
	for i := beginIdx + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) == markerEnd {
			return beginIdx, i, true
		}
	}
	return -1, -1, false
}

// findOrphanMarker returns the first index in [start+1, end) holding a
// bare cc-clip begin or end marker line with no matching partner in the
// same block. Callers use this to sweep markers left behind by a
// hand-edit that deleted one half of the pair — without the sweep, a
// subsequent Apply would stack a new marker block alongside the orphan.
func findOrphanMarker(lines []string, start, end int) (int, bool) {
	// All matched pairs in this block.
	var pairs [][2]int
	cursor := start
	for cursor < end {
		begin, endIdx, found := findMarkerPair(lines, cursor, end)
		if !found {
			break
		}
		pairs = append(pairs, [2]int{begin, endIdx})
		cursor = endIdx
	}
	inPair := func(i int) bool {
		for _, p := range pairs {
			if i >= p[0] && i <= p[1] {
				return true
			}
		}
		return false
	}
	for i := start + 1; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != markerBegin && trimmed != markerEnd {
			continue
		}
		if !inPair(i) {
			return i, true
		}
	}
	return -1, false
}

func findAdjacentManagedSetEnv(lines []string, start, end, orphanIdx int) (int, bool) {
	scan := func(i, step int) (int, bool) {
		for ; i > start && i < end; i += step {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			keyword, rest := splitDirective(trimmed)
			if strings.EqualFold(keyword, "setenv") && strings.Contains(rest, "CC_CLIP_") {
				return i, true
			}
			break
		}
		return 0, false
	}

	switch strings.TrimSpace(lines[orphanIdx]) {
	case markerBegin:
		return scan(orphanIdx+1, 1)
	case markerEnd:
		return scan(orphanIdx-1, -1)
	default:
		return 0, false
	}
}

func removeManagedMarkersFromBlocks(lines []string, blocks []hostBlock) ([]string, bool) {
	out := append([]string{}, lines...)
	removed := false
	for i := len(blocks) - 1; i >= 0; i-- {
		block := blocks[i]
		for {
			beginIdx, endIdx, found := findMarkerPair(out, block.start, block.end)
			if !found {
				break
			}
			removed = true
			out = append(out[:beginIdx], out[endIdx+1:]...)
			block.end -= endIdx - beginIdx + 1
		}
		// Sweep any orphaned begin/end lines left by a hand-edit that
		// deleted exactly one half of a pair. If we skipped this, the next
		// Apply would insert a fresh marker pair next to the orphan and
		// ssh_config would then contain both — confusing the user and
		// making future Remove calls non-idempotent.
		for {
			orphanIdx, found := findOrphanMarker(out, block.start, block.end)
			if !found {
				break
			}
			removed = true
			removeStart, removeEnd := orphanIdx, orphanIdx
			if setEnvIdx, ok := findAdjacentManagedSetEnv(out, block.start, block.end, orphanIdx); ok {
				if setEnvIdx < removeStart {
					removeStart = setEnvIdx
				}
				if setEnvIdx > removeEnd {
					removeEnd = setEnvIdx
				}
			}
			out = append(out[:removeStart], out[removeEnd+1:]...)
			block.end -= removeEnd - removeStart + 1
		}
	}
	return out, removed
}

// replaceOrInsertMarker either swaps an existing marker pair (and
// the lines between it) for `rendered`, or inserts `rendered` just
// before the block's end (i.e. before the next top-level directive,
// or at EOF).
func replaceOrInsertMarker(lines []string, block hostBlock, rendered []string) []string {
	if begin, end, ok := findMarkerPair(lines, block.start, block.end); ok {
		out := make([]string, 0, len(lines)-(end-begin+1)+len(rendered))
		out = append(out, lines[:begin]...)
		out = append(out, rendered...)
		out = append(out, lines[end+1:]...)
		return out
	}

	insertAt := block.end
	// Skip backward over trailing blank lines so the marker hugs the
	// last real option in the block. The skipped blank lines stay
	// after the inserted block.
	for insertAt > block.start+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}

	out := make([]string, 0, len(lines)+len(rendered))
	out = append(out, lines[:insertAt]...)
	out = append(out, rendered...)
	out = append(out, lines[insertAt:]...)
	return out
}

func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: empty", ErrInvalidHost)
	}
	for _, r := range host {
		if r == '#' || r == '\n' || r == '\r' || r == '\t' || r == ' ' || r == 0 {
			return fmt.Errorf("%w: contains forbidden character %q", ErrInvalidHost, r)
		}
		if r == '*' || r == '?' || r == '!' {
			return fmt.Errorf("%w: contains wildcard/negation character %q", ErrInvalidHost, r)
		}
		// OpenSSH matches Host tokens byte-for-byte and tunnel.ValidateSSHHost
		// already constrains live code paths to [A-Za-z0-9._:@-]. Restrict this
		// validator to the same ASCII printable range so a direct caller can't
		// smuggle a non-ASCII alias into ~/.ssh/config that ssh -G would then
		// refuse to resolve.
		if r > 127 || r < 0x20 {
			return fmt.Errorf("%w: non-ASCII or non-printable character %U", ErrInvalidHost, r)
		}
	}
	if strings.Contains(host, ">>>") || strings.Contains(host, "<<<") {
		return fmt.Errorf("%w: contains marker substring", ErrInvalidHost)
	}
	return nil
}

func validateEnvKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidEnvValue)
	}
	for i, r := range key {
		if r == '_' || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && (r >= '0' && r <= '9') {
			continue
		}
		return fmt.Errorf("%w: invalid character %q in env key", ErrInvalidEnvValue, r)
	}
	return nil
}

// maxEnvValueLen caps a single SetEnv value's length. The remote peer
// registry is the ultimate source of CC_CLIP_STATE_DIR, so without a
// cap a compromised or misbehaving remote could push an arbitrarily
// large path into the user's ~/.ssh/config. 4 KiB comfortably exceeds
// Linux PATH_MAX (4096) and any realistic state-dir or label.
const maxEnvValueLen = 4096

func validateEnvValue(value string) error {
	if len(value) > maxEnvValueLen {
		return fmt.Errorf("%w: value exceeds %d bytes", ErrInvalidEnvValue, maxEnvValueLen)
	}
	for _, r := range value {
		if r == '\n' || r == '\r' || r == 0 {
			return fmt.Errorf("%w: contains newline or NUL", ErrInvalidEnvValue)
		}
	}
	return nil
}

// fileMeta carries the pre-existing file's mode, uid, and gid across
// readConfig → writeAtomic so the atomic rename can restore both the
// permission bits and (on Unix) the ownership. Without uid/gid
// preservation, a `sudo cc-clip …` invocation would rename a root-owned
// temp file over the user's ~/.ssh/config and silently break subsequent
// user-mode OpenSSH and cc-clip reads.
type fileMeta struct {
	mode       os.FileMode
	uid, gid   int
	hasOwnerID bool
}

func readConfig(path string) ([]byte, fileMeta, error) {
	var meta fileMeta
	// On Unix, O_NOFOLLOW closes the TOCTOU between an Lstat-based
	// symlink check and a follow-up ReadFile: if `path` is a symlink at
	// open time, the kernel returns ELOOP before any read happens. On
	// Windows openNoFollow is 0, so we do a best-effort Lstat guard up
	// front — creating a symlink there requires SeCreateSymbolicLink
	// privilege, so the narrow remaining window is acceptable.
	//
	// THREAT MODEL (Windows): an attacker with local write access to
	// the user's profile who ALSO holds SeCreateSymbolicLink could race
	// between our Lstat and the O_RDONLY open to swap in a symlink. That
	// attacker already has enough privilege to edit ~/.ssh/config
	// directly, so closing the race buys nothing. This note exists so a
	// future contributor doesn't "fix" the Windows path in a way that
	// breaks on filesystems without symlink support.
	if openNoFollow == 0 {
		if lstat, lerr := os.Lstat(path); lerr == nil && lstat.Mode()&os.ModeSymlink != 0 {
			return nil, meta, fmt.Errorf("%w: %s", ErrSymlinkConfig, path)
		}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, meta, fmt.Errorf("%w: %s", ErrSymlinkConfig, path)
		}
		return nil, meta, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, meta, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, meta, err
	}
	meta.mode = info.Mode().Perm()
	meta.uid, meta.gid, meta.hasOwnerID = captureOwnership(info)
	return data, meta, nil
}

// fileFormat remembers byte-level traits of the input file so the
// writer can round-trip them: a UTF-8 BOM (preserved verbatim),
// CRLF line separators (re-emitted on write), and whether the input
// ended with a newline. Mixed-EOL files are normalized to CRLF if
// any line had \r — real-world configs are not mixed, and a single
// canonical style on write beats preserving per-line EOLs.
type fileFormat struct {
	trailingNewline bool
	crlf            bool
	bom             bool
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func splitLines(data []byte) ([]string, fileFormat) {
	var f fileFormat
	if bytes.HasPrefix(data, utf8BOM) {
		f.bom = true
		data = data[len(utf8BOM):]
	}
	if len(data) == 0 {
		return nil, f
	}
	f.trailingNewline = data[len(data)-1] == '\n'
	body := data
	if f.trailingNewline {
		body = data[:len(data)-1]
	}
	lines := strings.Split(string(body), "\n")
	for _, l := range lines {
		if strings.HasSuffix(l, "\r") {
			f.crlf = true
			break
		}
	}
	if f.crlf {
		for i, l := range lines {
			lines[i] = strings.TrimSuffix(l, "\r")
		}
	}
	return lines, f
}

func joinLines(lines []string, f fileFormat) []byte {
	sep := "\n"
	if f.crlf {
		sep = "\r\n"
	}
	var buf bytes.Buffer
	if f.bom {
		buf.Write(utf8BOM)
	}
	if len(lines) == 0 {
		if f.trailingNewline {
			buf.WriteString(sep)
		}
		if buf.Len() == 0 {
			return nil
		}
		return buf.Bytes()
	}
	buf.WriteString(strings.Join(lines, sep))
	if f.trailingNewline {
		buf.WriteString(sep)
	}
	return buf.Bytes()
}

func writeAtomic(path string, data []byte, meta fileMeta) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ssh-config-cc-clip-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	// Sync the temp file before close so a crash between Rename and the next
	// OS flush can't leave ~/.ssh/config referencing an inode whose data is
	// still in the page cache. Paired with the parent-dir Sync below, this
	// is the standard POSIX atomic-write recipe.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	mode := meta.mode
	if mode == 0 {
		// When the source info reports a zero permission mode (possible on
		// some Windows / FUSE paths), default to 0600. os.CreateTemp already
		// creates at 0600 on Unix, but making the intent explicit avoids a
		// future refactor silently flipping to 0644 if Go's default changes.
		mode = 0o600
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if meta.hasOwnerID {
		// Preserve ownership so a privileged rewrite (e.g. sudo cc-clip
		// setup) does not silently flip ~/.ssh/config from user-owned to
		// root-owned. If the process lacks CAP_CHOWN we abort rather than
		// rename over the user's file with the wrong owner; the user can
		// then re-run without sudo.
		if err := applyOwnership(tmpName, meta.uid, meta.gid); err != nil {
			cleanup()
			return fmt.Errorf("preserve ~/.ssh/config ownership: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// Best-effort fsync on the parent dir so the rename is durable. Some
	// filesystems (Windows, certain FUSE mounts) reject O_RDONLY+Sync on
	// a directory; a failure here doesn't invalidate the write.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
