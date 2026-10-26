// Package procinfo answers "which local process is on the other end of this loopback
// connection, and what launched it". Identity uses it to bind agent tokens to the programs
// that are supposed to hold them.
package procinfo

import (
	"errors"
	"net/netip"
)

// ErrUnsupported is returned on platforms without an implementation.
var ErrUnsupported = errors.New("process lookup not supported on this platform")

// ErrNotFound means no process owns the endpoint (it closed, or it is not local).
var ErrNotFound = errors.New("no local process owns this connection")

// Proc is one process in a parent chain.
type Proc struct {
	PID  int
	Path string // full executable path; empty if the process could not be opened
}

// maxDepth bounds parent-chain walks; real launch chains are a handful deep.
const maxDepth = 16

// ConnOwner returns the PID that owns the client end of a loopback TCP connection, given the
// client's address (the server's RemoteAddr) and the server's address (its LocalAddr).
func ConnOwner(client, server netip.AddrPort) (int, error) {
	return connOwner(client.Addr().Unmap(), client.Port(), server.Addr().Unmap(), server.Port())
}

// Chain returns pid and its ancestors, nearest first. A walk stops at a parent that started
// after its child, since that parent's PID has been reused by an unrelated process.
func Chain(pid int) ([]Proc, error) {
	return chain(pid)
}
