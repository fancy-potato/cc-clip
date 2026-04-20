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
	// DaemonShuttingDown is emitted when a `cc-clip tunnel {up,down,remove}`
	// call reaches a daemon that is in the middle of SIGTERM processing
	// (tunnel.ErrManagerShuttingDown). The tunnel manager has already
	// begun tearing down, so the request cannot be honored, but this is a
	// transient condition: shell wrappers should retry after the daemon
	// re-emerges (launchd `KeepAlive` respawns it) rather than surfacing
	// the failure to the user. Distinct from InternalError so a SwiftBar
	// plugin or retry loop can act on it without grep'ing stderr.
	DaemonShuttingDown = 23
	// AmbiguousTunnelState is emitted by `cc-clip tunnel up <host>` when
	// the local on-disk state directory contains multiple saved daemon
	// owners for the same host and the user did not pass `--port`
	// explicitly. The CLI refuses to pick one silently; this code lets
	// automation distinguish "genuinely ambiguous state, needs --port" from
	// "tunnel failed because the daemon is unreachable".
	AmbiguousTunnelState = 24
	// DaemonTunnelControlTokenUnavailable is emitted when the CLI cannot
	// mint an authenticated /tunnels request because the local tunnel-
	// control token file is missing or unreadable. The usual cause is that
	// `cc-clip serve` has never run on this machine (the token is created
	// on first daemon start). Distinct from TokenInvalid (which is the
	// remote-synced clipboard session token) so shell wrappers can guide
	// users to `cc-clip serve` / `cc-clip serve --rotate-tunnel-token`
	// without confusing it with a remote-auth failure.
	DaemonTunnelControlTokenUnavailable = 25
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
