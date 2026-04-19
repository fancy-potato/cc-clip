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
	// PeerNotFound is emitted by `cc-clip peer release|show` when the peer ID
	// is absent from the remote registry. The local SSH caller classifies
	// this specific code into peer.ErrPeerNotFound so idempotent cleanup
	// paths do not have to grep stderr for a prose error.
	PeerNotFound = 22
)

// PeerNotFoundSentinel is a stable token emitted on stderr alongside the
// PeerNotFound exit code. The remote SSH caller requires BOTH the exit
// code AND this sentinel before classifying an error as peer-not-found;
// this defends against ssh transport failures that happen to exit with
// code 22 (OpenSSH does not reserve 22 for its own errors, but a remote
// shell plugin or sandbox could still propagate an arbitrary 22). The
// sentinel is a fixed ASCII marker chosen to be unmistakable in stderr
// grep while remaining harmless to operators reading the log.
const PeerNotFoundSentinel = "cc-clip-peer-not-found"
