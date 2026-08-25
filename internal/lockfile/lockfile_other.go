//go:build !unix

package lockfile

import "errors"

// supported is false here: without flock there is no way to serialize
// writers across processes, so callers must fall back to an unlocked mode
// (or refuse) rather than leave every call failing closed on a lock this
// platform can never take.
const supported = false

var errUnsupported = errors.New("lockfile: inter-process locking is not supported on this platform")

func acquire(path string) (release func(), err error) {
	return nil, errUnsupported
}

func tryAcquire(path string) (*Lock, error) {
	return nil, errUnsupported
}
