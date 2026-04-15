package shellutil

import "strings"

// ShellQuote wraps s in single quotes, escaping embedded single quotes.
// This is safe for values that do not need tilde or variable expansion.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoteShellPath quotes a path for use in remote shell commands.
// Paths starting with ~/ are expanded to "$HOME/..." with proper escaping;
// all other paths are single-quoted.
func RemoteShellPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		return `"$HOME/` + EscapeDoubleQuoted(strings.TrimPrefix(path, "~/")) + `"`
	}
	return ShellQuote(path)
}

// EscapeDoubleQuoted escapes shell metacharacters for use inside double quotes.
func EscapeDoubleQuoted(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
	)
	return replacer.Replace(s)
}
