package tunnel

import "errors"

// ErrTunnelNotFound reports that no saved or live tunnel exists for a host.
var ErrTunnelNotFound = errors.New("tunnel not found")

// ErrManagerShuttingDown reports that a control request arrived while the
// tunnel manager was already shutting down.
var ErrManagerShuttingDown = errors.New("tunnel manager shutting down")

// ErrTunnelLocalPortMismatch reports that a daemon was asked to manage a
// persistent tunnel for a different local daemon port.
var ErrTunnelLocalPortMismatch = errors.New("tunnel local port does not match daemon port")

// ErrTunnelSSHOptionsUnresolved reports that a saved tunnel predates cached
// ssh option persistence and must be refreshed before background reconnects
// can launch it safely.
var ErrTunnelSSHOptionsUnresolved = errors.New("tunnel ssh options are unresolved")
