//go:build !linux

package sandbox

import "errors"

// Landlock is a Linux LSM. On every other platform this package reports that
// honestly rather than pretending to confine anything: the workers run in Linux
// containers, and a developer machine running the tests is not the deployment
// this guarantee is about.

// Available reports false: there is no Landlock here.
func Available() bool { return false }

// Apply always fails, so a caller that requires the sandbox fails loudly on a
// platform that cannot provide it instead of running unconfined.
func Apply(Paths) error {
	return errors.New("landlock: not available on this platform")
}

// ThreadID has no meaning where there is no sandbox to pin to a thread.
func ThreadID() int { return 0 }
