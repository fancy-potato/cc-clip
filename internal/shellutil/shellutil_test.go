package shellutil

import "testing"

func TestShellQuoteSimple(t *testing.T) {
	got := ShellQuote("hello")
	if got != "'hello'" {
		t.Fatalf("expected 'hello', got %q", got)
	}
}

func TestShellQuoteEmpty(t *testing.T) {
	got := ShellQuote("")
	if got != "''" {
		t.Fatalf("expected empty single-quoted string, got %q", got)
	}
}

func TestShellQuoteEscapesEmbeddedSingleQuotes(t *testing.T) {
	got := ShellQuote("it's")
	if got != `'it'\''s'` {
		t.Fatalf("expected escaped single quote, got %q", got)
	}
}

func TestShellQuotePreservesSpaces(t *testing.T) {
	got := ShellQuote("/tmp/cc clip/peer-a")
	if got != "'/tmp/cc clip/peer-a'" {
		t.Fatalf("expected spaces preserved, got %q", got)
	}
}

func TestShellQuotePreservesMetachars(t *testing.T) {
	got := ShellQuote("$HOME/test")
	if got != "'$HOME/test'" {
		t.Fatalf("expected dollar sign not expanded, got %q", got)
	}
}

func TestRemoteShellPathExpandsHome(t *testing.T) {
	got := RemoteShellPath("~/.cache/cc-clip")
	if got != `"$HOME/.cache/cc-clip"` {
		t.Fatalf("expected home expansion, got %q", got)
	}
}

func TestRemoteShellPathQuotesAbsolute(t *testing.T) {
	got := RemoteShellPath("/tmp/cc clip/peer-a")
	if got != "'/tmp/cc clip/peer-a'" {
		t.Fatalf("expected single-quoted absolute path, got %q", got)
	}
}

func TestRemoteShellPathNonHomeTilde(t *testing.T) {
	got := RemoteShellPath("~user/.cache")
	// Not a ~/ path, should be single-quoted
	if got != "'~user/.cache'" {
		t.Fatalf("expected single-quoted non-home tilde, got %q", got)
	}
}

func TestEscapeDoubleQuotedBackslash(t *testing.T) {
	got := EscapeDoubleQuoted(`a\b`)
	if got != `a\\b` {
		t.Fatalf("expected escaped backslash, got %q", got)
	}
}

func TestEscapeDoubleQuotedDollar(t *testing.T) {
	got := EscapeDoubleQuoted("$VAR")
	if got != `\$VAR` {
		t.Fatalf("expected escaped dollar, got %q", got)
	}
}

func TestEscapeDoubleQuotedBacktick(t *testing.T) {
	got := EscapeDoubleQuoted("cmd `date`")
	if got != "cmd \\`date\\`" {
		t.Fatalf("expected escaped backticks, got %q", got)
	}
}

func TestEscapeDoubleQuotedDoubleQuote(t *testing.T) {
	got := EscapeDoubleQuoted(`say "hello"`)
	if got != `say \"hello\"` {
		t.Fatalf("expected escaped double quotes, got %q", got)
	}
}

func TestEscapeDoubleQuotedEmpty(t *testing.T) {
	got := EscapeDoubleQuoted("")
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestRemoteShellPathHomeWithMetachars(t *testing.T) {
	got := RemoteShellPath("~/path with $pecial")
	if got != `"$HOME/path with \$pecial"` {
		t.Fatalf("expected dollar escaped inside double quotes, got %q", got)
	}
}
