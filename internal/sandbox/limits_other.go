//go:build !unix

package sandbox

import "errors"

// ApplyLimits fails where there is no RLIMIT_CPU to set.
//
// Loudly, not silently: a caller that asked for a bound and did not get one
// must not proceed believing it has one. Unlike the filesystem ruleset this
// works on every unix (limits_unix.go), so reaching here means a platform with
// no POSIX resource limits at all.
func ApplyLimits(l Limits) error {
	if l.CPUSeconds <= 0 {
		return nil
	}
	return errors.New("rlimit: no resource limits on this platform")
}
