// Package effect provides the filesystem and HTTP effect providers through
// which every agent-reachable side effect must pass.
//
// The point is structural confinement. A tool that reads or writes a host
// file does not call [os.Open]; it calls a method on an [FS] the process
// handed it, and that FS is what decides whether the path is allowed. A tool
// that talks HTTP does not build an [net/http.Client]; it uses a [Doer] whose
// policy decides which hosts it may reach. Confinement therefore cannot be
// lost by omission: a new tool author has nothing to remember, because the
// unconfined primitive is not reachable from the tool at all.
package effect

import "github.com/go-faster/errors"

// ErrDenied is returned by a provider that refuses an effect outright, either
// because the effect is not configured for this process (see [Deny]) or
// because policy forbids it.
var ErrDenied = errors.New("effect denied by policy")

// ErrOutsideRoot is returned by a confined [FS] for a path resolving outside
// its root.
var ErrOutsideRoot = errors.New("path is outside the allowed root")
