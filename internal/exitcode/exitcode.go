package exitcode

const (
	Success           = 0
	NoImage           = 10
	TunnelUnreachable = 11
	TokenInvalid      = 12
	DownloadFailed    = 13
	InternalError     = 20
	// UsageError covers bad CLI invocation (unknown subcommand, missing
	// required flag). Distinct from InternalError so wrapper scripts can
	// recognise "you typed it wrong" without capturing stderr.
	UsageError = 21
)
