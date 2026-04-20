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
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/shunmei/cc-clip/internal/userhome"
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

// applyPostLockHookForTest is a test-only injection point that fires
// between acquireConfigLock and the authoritative post-lock readConfig.
// Production callers leave it nil. Tests use it to deterministically
// exercise the pre-lock-vs-post-lock read-drift path — specifically that
// a concurrent rewrite landing in this window is benign because the
// post-lock read wins and Apply operates on the fresh bytes.
//
// The variable is lowercase/unexported so external callers cannot
// accidentally hook into Apply from outside this package.
var applyPostLockHookForTest func(path string)

// ErrOnlyGlobMatch means the only `Host` blocks that would apply to
// the alias use wildcard (`*`, `?`) or negation (`!`) patterns, OR a
// Host block contains BOTH a literal matching the alias AND a wildcard
// pattern (e.g. `Host myalias *`). cc-clip refuses to inject SetEnv
// into such a block because it would leak per-laptop env vars to every
// host the pattern matches, and because negation-bearing blocks have
// semantics that don't map cleanly to a per-alias injection.
var ErrOnlyGlobMatch = errors.New("alias is matched only by a wildcard or negation `Host` pattern, or the matching `Host` block mixes a literal alias with a wildcard token (e.g. `Host myalias *`); add a dedicated literal `Host <alias>` block")

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

// ErrSSHConfigTooLarge is returned when ~/.ssh/config exceeds
// maxSSHConfigSize bytes. The cap is a defense-in-depth measure: a
// runaway or hostile write to the user's config would otherwise force
// readConfig to allocate unbounded memory. Realistic ssh_config files
// are a few KiB; 1 MiB is ~1000x the typical upper bound.
var ErrSSHConfigTooLarge = errors.New("~/.ssh/config exceeds size cap")

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
	home, err := userhome.Dir()
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

// HostBlockStatusFromBytes reports whether the config contains a literal
// `Host <host>` block that cc-clip is allowed to manage. It returns nil when
// such a block exists, or one of ErrOnlyGlobMatch / ErrHostBlockInInclude /
// ErrHostBlockMissing to explain why no manageable block is available.
func HostBlockStatusFromBytes(data []byte, host string) error {
	if err := validateHost(host); err != nil {
		return err
	}
	lines, _ := splitLines(data)
	blocks, status := findHostBlocks(lines, host)
	switch status {
	case hostMatchLiteral:
		if len(blocks) > 0 {
			return nil
		}
	case hostMatchGlob:
		return ErrOnlyGlobMatch
	}
	if hasIncludeDirective(lines) {
		return ErrHostBlockInInclude
	}
	return ErrHostBlockMissing
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
	// Iterate in sorted key order so the user-facing error (and any
	// logs) is deterministic. Go map iteration is randomized, which
	// produced non-reproducible failures in `env with multiple bad
	// keys` cases — different runs picked different keys to complain
	// about first.
	sortedKeys := make([]string, 0, len(env))
	for k := range env {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		if err := validateEnvKey(k); err != nil {
			return err
		}
		if err := validateEnvValue(env[k]); err != nil {
			return err
		}
	}

	// Pre-lock readConfig: this is a fast gate that proves the target is a
	// real, readable, non-symlinked ssh_config BEFORE we materialize the
	// sidecar `.cc-clip.lock` file via acquireConfigLock. Without this, a
	// missing path or a symlinked path would still create the sidecar lock
	// on disk as a side effect of the (then-inevitable) fail-closed return.
	// The second readConfig after the lock acquisition is the
	// authoritative one whose bytes we operate on — this first read is
	// discarded intentionally and a concurrent rewrite between the two
	// reads is fine: the post-lock read wins.
	data, meta, err := readConfig(path)
	if err != nil {
		return err
	}
	// Pass the config's owner uid/gid into the lock acquire so the
	// sidecar `.cc-clip.lock` inherits the same ownership. Without
	// this, `sudo cc-clip setup` materializes the sidecar root-owned
	// (process euid=0) and the user's next non-sudo run can no longer
	// flock it. The pre-lock readConfig is enough for this purpose —
	// the post-lock re-read may pick up a concurrent rewrite, but the
	// owner of the sidecar should match the on-disk owner we just saw.
	release, err := acquireConfigLock(path, meta.uid, meta.gid, meta.hasOwnerID)
	if err != nil {
		return fmt.Errorf("acquire ssh_config advisory lock: %w", err)
	}
	defer release()
	if applyPostLockHookForTest != nil {
		applyPostLockHookForTest(path)
	}
	data, meta, err = readConfig(path)
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

	// Conflict detection MUST run against the original lines, BEFORE the
	// marker-sweep has a chance to remove a user-authored
	// `SetEnv CC_CLIP_PORT=…` line that happens to sit adjacent to a stale
	// orphan marker. The adjacent-SetEnv sweep in removeManagedMarkersFromBlocks
	// is intentional for cc-clip-authored content — but running it ahead of
	// the user-conflict check would silently delete that content before we
	// could raise ErrSetEnvConflict. blockHasUserSetEnv skips lines inside
	// managed marker pairs so it does not false-trigger on a prior Apply.
	for _, block := range blocks {
		if blockHasUserSetEnv(lines, block) {
			return ErrSetEnvConflict
		}
	}
	cleanedLines, _ := removeManagedMarkersFromBlocks(lines, blocks)
	blocks, status = findHostBlocks(cleanedLines, host)
	if status != hostMatchLiteral || len(blocks) == 0 {
		return ErrHostBlockMissing
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

	data, meta, err := readConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// Mirror ApplyToFile's locking, but only after we know the path is an
	// actual readable file. That keeps missing configs as no-ops and
	// symlinked configs as fail-closed without creating sidecar lock files.
	// Inherit the config's owner so a `sudo cc-clip uninstall …` doesn't
	// leave a root-owned sidecar lock behind.
	release, err := acquireConfigLock(path, meta.uid, meta.gid, meta.hasOwnerID)
	if err != nil {
		return fmt.Errorf("acquire ssh_config advisory lock: %w", err)
	}
	defer release()
	data, meta, err = readConfig(path)
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

// maxContinuation caps the per-directive backslash-newline continuation
// walk. ssh_config Host aliases are almost always one line; 64 is well
// past any realistic wrap count. The cap exists so a pathological config
// with thousands of trailing-backslash lines cannot O(n²) us via repeated
// string concat. When the cap fires, collectContinuation / continuationEnd
// stop silently and the rest of the continuation is ignored — a 65-line
// `Host` continuation may return ErrHostBlockMissing because the later
// aliases are not seen. This is an intentional defense-in-depth trade-off:
// realistic configs are one line, pathological configs are unreachable in
// practice, and the silent truncation is safer than unbounded work.
const maxContinuation = 64

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

func continuationEnd(lines []string, i int) int {
	end := i
	for steps := 0; steps < maxContinuation && end+1 < len(lines); steps++ {
		if !strings.HasSuffix(strings.TrimRight(lines[end], " \t"), `\`) {
			break
		}
		end++
	}
	return end
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

	// skipUntil is the index (inclusive) of the last line consumed by a
	// backslash-newline continuation on a `Host` keyword. We advance past it
	// so a continuation line whose textual content happens to begin with
	// `Host ` (after whitespace trim) is NOT mis-classified as a new
	// top-level directive: ssh_config has already joined those tokens into
	// the parent directive via the trailing backslash.
	skipUntil := -1
	for i, line := range lines {
		if i <= skipUntil {
			continue
		}
		if end := continuationEnd(lines, i); end > i {
			skipUntil = end
		}
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
			joinedRest, end := collectContinuation(lines, i, rest)
			tokens := tokenizeHostPatterns(joinedRest)
			current = &pendingBlock{start: i, tokens: tokens}
			skipUntil = end
		case "match":
			// `Match host …` blocks are intentionally not considered:
			// SetEnv inside a Match block has surprising scoping rules
			// and we restrict injection to plain Host blocks.
			_, end := collectContinuation(lines, i, rest)
			skipUntil = end
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

// hasIncludeDirective reports whether the config contains an `Include`
// directive that could plausibly reach the queried alias. The risk we
// care about is the user's `Host <alias>` living in an included file —
// which only matters when the Include itself is reachable for our query.
//
// Discriminator (per ssh_config grammar):
//   - `Include` BEFORE any Host/Match: unconditionally reached.
//   - `Include` after a `Host <pat>` directive: ssh treats it as INSIDE
//     that Host block, conditional on matching <pat>. Since findHostBlocks
//     already proved no literal Host block matches our alias, an Include
//     inside some other Host block cannot reach us — UNLESS we already
//     saw a Match block earlier in the file. Match patterns can match
//     unconditionally (e.g. `Match all`) and we deliberately don't
//     evaluate them, so once a Match has been observed, every subsequent
//     Include is considered reachable regardless of any intervening Host
//     directive that would "close" the Match's apparent scope. This
//     monotonic `reachableViaMatch` bit handles the `Match → Host → Include`
//     ordering: an include after a Match+Host sequence could still be
//     reachable because the Match block's own body can include arbitrary
//     directives that find their way into the user's config surface.
//   - `Include` after a `Match` directive: always reachable for the same
//     unconditional-match reason.
//
// Leading whitespace is intentionally ignored on Host/Match/Include
// directives: OpenSSH accepts indented top-level directives, so
// indentation alone does NOT imply nesting.
//
// Backslash-newline continuations are honored: a physical line ending
// with a trailing `\` (ignoring trailing whitespace) extends the logical
// directive to the next physical line. The continuation lines are
// skipped when scanning for the NEXT directive, so a `Host foo \` that
// continues with aliases on the next physical line cannot be misread as
// a new top-level directive.
func hasIncludeDirective(lines []string) bool {
	insideHostBlock := false
	insideMatchBlock := false
	reachableViaMatch := false
	skipUntil := -1
	for i := 0; i < len(lines); i++ {
		if i <= skipUntil {
			continue
		}
		if end := continuationEnd(lines, i); end > i {
			skipUntil = end
		}
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keyword, _ := splitDirective(trimmed)
		switch strings.ToLower(keyword) {
		case "host":
			insideHostBlock = true
			insideMatchBlock = false
		case "match":
			// Match opens a new block whose body is reachable whenever
			// the Match pattern fires. Any Include within — indented or
			// not — is potentially reachable for our alias. We also set
			// reachableViaMatch so a later `Host` → `Include` sequence
			// stays flagged as reachable: a subsequent Host block does
			// not erase the fact that the file already crossed a Match
			// boundary where an unconditional pattern (e.g. `Match all`)
			// could have fired.
			insideHostBlock = false
			insideMatchBlock = true
			reachableViaMatch = true
		case "include":
			if insideMatchBlock || reachableViaMatch {
				return true
			}
			if !insideHostBlock {
				return true
			}
		}
	}
	return false
}

func blockEnd(lines []string, start int) int {
	skipUntil := continuationEnd(lines, start)
	for i := start + 1; i < len(lines); i++ {
		if i <= skipUntil {
			continue
		}
		if end := continuationEnd(lines, i); end > i {
			skipUntil = end
		}
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
	// OpenSSH matches Host patterns case-insensitively for the hostname
	// component (see OpenSSH misc.c:match_pattern). A byte-exact compare
	// here would let `Host ProdBox` match `ssh prodbox` at runtime while
	// cc-clip returned ErrHostBlockMissing, forcing the user to either
	// duplicate their Host block or rename their alias. Normalize to lower
	// case so literal matching agrees with what OpenSSH actually does.
	aliasLower := strings.ToLower(alias)
	status := hostMatchNone
	// hasNonNegatedWildcard tracks whether this block's token list contains
	// ANY non-negated wildcard pattern, independent of whether it matches
	// the alias. A block like `Host myalias *` would otherwise be
	// classified as a literal match (the `myalias` token matches
	// literally), and Apply would inject CC_CLIP_* into a block OpenSSH
	// applies to every host — leaking per-laptop env vars to every
	// connection. Treat any mix of literal-match + non-negated wildcard as
	// hostMatchGlob so Apply returns ErrOnlyGlobMatch.
	hasNonNegatedWildcard := false
	for _, t := range tokens {
		negated := strings.HasPrefix(t, "!")
		pattern := strings.TrimPrefix(t, "!")
		if pattern == "" {
			continue
		}
		patternLower := strings.ToLower(pattern)
		isGlob := containsGlobMeta(patternLower)
		if isGlob && !negated {
			hasNonNegatedWildcard = true
		}
		matched := false
		if isGlob {
			if m, err := matchSSHPattern(patternLower, aliasLower); err == nil {
				matched = m
			}
		} else {
			matched = patternLower == aliasLower
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
	// Promote a literal-alias match to glob if the same block also carries
	// a non-negated wildcard. This is the `Host myalias *` case: the block
	// legitimately applies to the alias, but OpenSSH also applies its
	// directives to every other host matching `*`, so injecting SetEnv
	// here leaks per-laptop env vars to the entire SSH config surface.
	if status == hostMatchLiteral && hasNonNegatedWildcard {
		return hostMatchGlob
	}
	return status
}

// containsGlobMeta reports whether s contains an OpenSSH wildcard
// metacharacter. Only `*` and `?` are recognised because OpenSSH's
// match_pattern (see misc.c in openssh-portable) intentionally has NO
// character-class (`[...]`) support — adding one here would diverge
// from real ssh behavior and could classify a literal bracketed alias
// as a glob match. Do not add `[`/`]` here.
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

// blockHasUserSetEnv reports whether a block contains a SetEnv directive
// authored by the user (i.e. carrying a key that is NOT one of the
// cc-clip-managed keys). SetEnv lines nested between MarkerBegin/MarkerEnd
// are always treated as managed. A bare `SetEnv CC_CLIP_PORT=…` line
// outside a marker pair (legacy single-key layout, or the orphan-end-
// marker repair case) is also treated as managed so Apply can replace it.
// Any other key — `SetEnv FOO=bar`, or a mixed `SetEnv CC_CLIP_PORT=… FOO=…`
// — is user-authored and must trigger ErrSetEnvConflict.
//
// Call this BEFORE the marker-sweep runs so a user-authored
// `SetEnv FOO=bar` next to a stale orphan marker is detected and rejected
// rather than silently deleted by the adjacent-SetEnv sweep.
func blockHasUserSetEnv(lines []string, block hostBlock) bool {
	// Collect all properly-paired managed marker ranges. An orphan begin
	// marker (with no matching end) is NOT treated as opening a managed
	// region: if we did, any user-authored SetEnv after the orphan would
	// be misclassified as managed and ErrSetEnvConflict would silently
	// not fire. Orphans are swept separately by findOrphanMarker.
	managedRanges := collectManagedMarkerPairs(lines, block.start, block.end)
	inManaged := func(i int) bool {
		for _, r := range managedRanges {
			if i >= r[0] && i <= r[1] {
				return true
			}
		}
		return false
	}
	for i := block.start + 1; i < block.end; i++ {
		if inManaged(i) {
			continue
		}
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keyword, rest := splitDirective(trimmed)
		if !strings.EqualFold(keyword, "setenv") {
			continue
		}
		// A SetEnv directive is "user-authored" only if it carries at
		// least one key that is NOT cc-clip-managed. Legacy single-key
		// `SetEnv CC_CLIP_PORT=…` lines left from older cc-clip releases
		// (and the orphan-repair scenarios exercised by
		// TestApplyRepairsOrphanedEndMarker) need to be sweepable.
		if setEnvHasNonManagedKey(rest) {
			return true
		}
	}
	return false
}

// collectManagedMarkerPairs returns all properly-paired marker ranges
// inside [start+1, end). Orphan begins / ends / mismatched counts are
// intentionally omitted so callers can treat them as non-managed and
// sweep them via findOrphanMarker.
func collectManagedMarkerPairs(lines []string, start, end int) [][2]int {
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
	return pairs
}

// setEnvHasNonManagedKey reports whether the tokens after the `SetEnv`
// keyword include any key beyond the cc-clip-managed set (CC_CLIP_PORT,
// CC_CLIP_STATE_DIR). Malformed tokens (no `=`) are treated as non-managed
// so we err on the side of preservation.
func setEnvHasNonManagedKey(rest string) bool {
	tokens, err := splitSetEnvTokens(rest)
	if err != nil {
		return true
	}
	for _, tok := range tokens {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			return true
		}
		key := tok[:eq]
		if key != "CC_CLIP_PORT" && key != "CC_CLIP_STATE_DIR" {
			return true
		}
	}
	return false
}

// setEnvIsManagedOnly reports whether every token after the `SetEnv`
// keyword is a KEY=VALUE assignment whose KEY is one of the cc-clip
// managed keys (CC_CLIP_PORT, CC_CLIP_STATE_DIR). Returns false for
// empty token lists or malformed input — an empty SetEnv is not "ours",
// and malformed input must be preserved rather than swept. This is
// used by the orphan-adjacent sweep to identify SetEnv lines cc-clip
// itself wrote; a user SetEnv that embeds `CC_CLIP_PORT=…` as part of
// a quoted value does NOT satisfy key-equality and is left alone.
func setEnvIsManagedOnly(rest string) bool {
	tokens, err := splitSetEnvTokens(rest)
	if err != nil || len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			return false
		}
		key := tok[:eq]
		if key != "CC_CLIP_PORT" && key != "CC_CLIP_STATE_DIR" {
			return false
		}
	}
	return true
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
	pairs := collectManagedMarkerPairs(lines, start, end)
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
			// Match only the two specific keys cc-clip itself writes
			// (CC_CLIP_PORT, CC_CLIP_STATE_DIR). A user-authored
			// `SetEnv CC_CLIP_DEBUG=1` next to an orphaned marker is NOT
			// ours to delete — the previous "any `CC_CLIP_` substring"
			// match would have silently removed it, breaking the "never
			// touch unrelated SetEnv lines" invariant in CLAUDE.md.
			//
			// Parse tokens via splitSetEnvTokens so a user SetEnv value
			// that happens to contain the literal substring
			// `CC_CLIP_PORT=` (e.g. `SetEnv MY="prefix-CC_CLIP_PORT=foo"`)
			// is NOT misidentified as managed. Only a bona fide
			// `KEY=VALUE` token whose KEY equals CC_CLIP_PORT or
			// CC_CLIP_STATE_DIR qualifies. Either key alone is enough:
			// older cc-clip releases wrote a single-key SetEnv (just
			// CC_CLIP_PORT) and the orphan-repair path must still clean
			// those up after an upgrade. A SetEnv line carrying a managed
			// key MIXED with a user key (e.g. `CC_CLIP_PORT=… FOO=bar`)
			// would have been rejected by blockHasUserSetEnv before
			// reaching this sweep, so restricting the match to
			// cc-clip-only-keyed lines here is safe.
			if strings.EqualFold(keyword, "setenv") && setEnvIsManagedOnly(rest) {
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
	// Iterate blocks from last to first. Shrinking `out` only affects indices
	// >= the current block's start, so earlier blocks' (smaller) start/end
	// indices remain valid for the next iteration. `block.end -= …` below is
	// a local decrement applied within the current iteration only — the
	// reverse traversal is what keeps blocks[i-1]'s indices correct.
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
			// Only the orphan marker line itself, plus the adjacent
			// managed SetEnv if findAdjacentManagedSetEnv claims one.
			// We DO NOT remove the inclusive range [min..max] between
			// them: a previous version did, which silently deleted any
			// user-authored blank lines or comments that happened to sit
			// between the orphan and the legacy SetEnv. Building a
			// new slice that omits exactly the matched indices preserves
			// everything in between byte-for-byte.
			drop := map[int]bool{orphanIdx: true}
			if setEnvIdx, ok := findAdjacentManagedSetEnv(out, block.start, block.end, orphanIdx); ok {
				drop[setEnvIdx] = true
			}
			newOut := make([]string, 0, len(out)-len(drop))
			for i, line := range out {
				if !drop[i] {
					newOut = append(newOut, line)
				}
			}
			out = newOut
			block.end -= len(drop)
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
		// validator to the printable-ASCII range [0x20, 0x7e] so a direct
		// caller can't smuggle a non-ASCII alias OR a control byte (DEL 0x7f
		// included) into ~/.ssh/config that ssh -G would then refuse to
		// resolve or that could trip a downstream parser on \r / NUL.
		if r > 0x7e || r < 0x20 {
			return fmt.Errorf("%w: non-printable or non-ASCII-printable character %U", ErrInvalidHost, r)
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
		// Reject literal backslash. OpenSSH's ssh_config tokenizer does NOT
		// interpret `\\` or `\"` inside a quoted SetEnv value the same way
		// this package round-trips them: OpenSSH treats the backslash as
		// literal, so an Apply round-trip through `ssh -G` would deliver a
		// different byte sequence to the remote than we wrote. Unix paths
		// (the CC_CLIP_STATE_DIR source) do not legitimately contain
		// backslashes; rejecting them keeps the Apply→remote round-trip
		// byte-identical to the input.
		if r == '\\' {
			return fmt.Errorf("%w: contains backslash (cannot round-trip through OpenSSH's ssh_config parser)", ErrInvalidEnvValue)
		}
	}
	return nil
}

// maxSSHConfigSize caps the bytes readConfig will accept from
// ~/.ssh/config. See ErrSSHConfigTooLarge. The cap is enforced by
// reading maxSSHConfigSize+1 bytes: if the extra byte materialises the
// file is over the cap and we fail closed.
const maxSSHConfigSize = 1 << 20 // 1 MiB

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
	// Cap the read at maxSSHConfigSize+1 bytes. If the extra byte was
	// actually read, the file is over the cap — fail closed with
	// ErrSSHConfigTooLarge. io.LimitReader alone wouldn't distinguish
	// "file is exactly maxSSHConfigSize" from "file was truncated"; the
	// +1 detection byte makes the distinction explicit.
	data, err := io.ReadAll(io.LimitReader(f, maxSSHConfigSize+1))
	if err != nil {
		return nil, meta, err
	}
	if len(data) > maxSSHConfigSize {
		return nil, meta, fmt.Errorf("%w: %s exceeds %d bytes", ErrSSHConfigTooLarge, path, maxSSHConfigSize)
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
	// Decide the dominant EOL style by majority, not "any one line wins".
	// A stray `\r` from a hand-edit in an otherwise-LF file should not
	// flip every subsequent write to CRLF. We pick CRLF only when the
	// majority of populated lines carried `\r`. Ties fall through to LF,
	// matching the common Unix-authored case.
	crlfCount := 0
	lfCount := 0
	for _, l := range lines {
		if strings.HasSuffix(l, "\r") {
			crlfCount++
		} else {
			lfCount++
		}
	}
	if crlfCount > lfCount {
		f.crlf = true
	}
	// Always strip trailing `\r` from every line so a stray CR on a minority
	// line cannot leak into the re-emitted output; the chosen EOL is
	// reapplied in joinLines.
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
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
	mode := meta.mode
	if mode == 0 {
		// When the source info reports a zero permission mode (possible on
		// some Windows / FUSE paths), default to 0600. os.CreateTemp already
		// creates at 0600 on Unix, but making the intent explicit avoids a
		// future refactor silently flipping to 0644 if Go's default changes.
		mode = 0o600
	}
	// Apply chmod and chown via fd BEFORE Close. The previous form
	// chmod'd by name after Close, leaving a window in which an
	// attacker with write access to ~/.ssh/ could swap the temp file
	// for a symlink between Close and Chmod/Chown — the chmod/chown
	// would then land on the swap target. fchmod/fchown operate on the
	// open inode and are immune.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if meta.hasOwnerID {
		// Preserve ownership so a privileged rewrite (e.g. sudo cc-clip
		// setup) does not silently flip ~/.ssh/config from user-owned to
		// root-owned. If the process lacks CAP_CHOWN we abort rather than
		// rename over the user's file with the wrong owner; the user can
		// then re-run without sudo.
		if err := applyOwnershipFd(tmp.Fd(), meta.uid, meta.gid); err != nil {
			tmp.Close()
			cleanup()
			return fmt.Errorf("preserve ~/.ssh/config ownership: %w", err)
		}
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
	// Defense-in-depth against a rename-vs-symlink-swap race: even with
	// the advisory flock held and the pre-write O_NOFOLLOW open, an
	// attacker with write access to ~/.ssh/ could swap the target for a
	// symlink between our locked read and this rename.
	if err := renameAtomic(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// Parent-dir fsync makes the rename durable across a crash. If we
	// couldn't even Open the parent, treat that as best-effort (some
	// platforms — Windows, certain FUSE mounts — refuse O_RDONLY on
	// directories). But if Open succeeded, a Sync failure is a real
	// durability signal: swallowing it turns the "torn rename" class
	// of bugs the atomic write was designed to prevent into silent data
	// loss. Returning the error here would not unwind the rename (the
	// file is in place), so the disk state is consistent either way;
	// we propagate so the caller can decide whether to retry or alert.
	if d, err := os.Open(dir); err == nil {
		syncErr := d.Sync()
		closeErr := d.Close()
		if syncErr != nil {
			return fmt.Errorf("fsync ssh_config dir after rename: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close ssh_config dir handle: %w", closeErr)
		}
	} else if shouldLogFsyncSkip() {
		// This branch fires when os.Open(dir) itself FAILED — we never
		// got a directory handle to sync. On Linux/Darwin a directory
		// fsync is the standard recipe for making a rename durable, so
		// parent-dir open failure means fsync was skipped entirely and
		// durability of the just-completed rename is best-effort on
		// this platform/filesystem (e.g. a restricted shell or FUSE
		// mount that refuses O_RDONLY on directories). Surface a
		// best-effort warning so the operator knows; the rename itself
		// already succeeded, so we do not return the error. Windows has
		// different fsync semantics on directories so we stay silent
		// there via shouldLogFsyncSkip().
		fmt.Fprintf(os.Stderr, "cc-clip: warning: could not open ssh_config parent dir %q to fsync after rename; durability is best-effort: %v\n", dir, err)
	}
	return nil
}

// renameAtomic moves tmp → dst while rejecting symlink-swap attacks.
//
// On Linux, it first attempts renameat2 with RENAME_NOREPLACE. On
// Darwin, it first attempts renamex_np with RENAME_EXCL. Both are
// exclusive renames: if dst already exists the kernel returns EEXIST
// rather than silently replacing, so we then Lstat and confirm it is
// NOT a symlink before falling back to the normal replacing os.Rename.
// This closes the symlink-plant race: an attacker who swapped a symlink
// into dst between our initial O_NOFOLLOW open and here would be caught
// by the post-EEXIST Lstat, because the exclusive-rename syscall could
// not have silently followed the link and succeeded.
//
// On other platforms (non-Darwin BSDs, Windows) renameNoReplace is a
// no-op stub and we fall straight through to the Lstat-then-Rename path,
// which has a residual TOCTOU window: an attacker with directory write
// access can swap the regular file for a symlink between our final
// Lstat and os.Rename. On Windows, creating a symlink requires
// SeCreateSymbolicLinkPrivilege so the residual window is narrow by
// policy; on the BSDs we accept the window rather than ship another
// platform-specific syscall layer.
func renameAtomic(tmp, dst string) error {
	if renameNoReplaceSupported {
		ok, rerr := renameNoReplace(tmp, dst)
		if ok {
			// dst did not exist; rename succeeded atomically.
			return nil
		}
		if rerr != nil && !errors.Is(rerr, os.ErrExist) {
			// Real error from the syscall — propagate.
			return rerr
		}
		// rerr == os.ErrExist: dst already exists (the normal case).
		// rerr == nil: platform fallback signaled (unsupported kernel).
		// Either way, fall through to Lstat + replacing Rename.
	}
	if lstat, lerr := os.Lstat(dst); lerr == nil && lstat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkConfig, dst)
	}
	return os.Rename(tmp, dst)
}

// shouldLogFsyncSkip reports whether a silent parent-dir fsync skip is
// a genuine durability warning (Unix) or an expected platform quirk
// (Windows has different fsync semantics on directories).
func shouldLogFsyncSkip() bool {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return true
	default:
		return false
	}
}
