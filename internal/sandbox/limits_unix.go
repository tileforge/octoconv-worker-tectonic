//go:build unix

package sandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ApplyLimits sets the resource bounds that must be established before the
// engine replaces this process.
//
// # WHY THIS IS NOT IN sandbox_linux.go
//
// It was, and that was wrong. RLIMIT_CPU is POSIX, not a Linux facility: it
// works on darwin and the BSDs, and it has nothing to do with Landlock. Living
// beside the Landlock code meant the !linux build rejected ANY requested
// limit -- so on macOS with TECTONIC_SANDBOX_DISABLED=true, where the operator
// has deliberately given up filesystem confinement and KEPT the CPU bound, the
// helper exited 78 and the worker refused to start (codex, PR #22).
//
// Verified rather than assumed: unix.Getrlimit(RLIMIT_CPU) returns cleanly on
// the darwin host this was written on.
//
// # WHY soft == hard
//
// A document that has spent its budget has spent it, and a grace period is
// time the same document uses to spend more.
//
// WHAT SIGNAL THIS PRODUCES IS NOT WHAT I ASSUMED. I wrote here that soft ==
// hard means the kernel sends SIGKILL rather than SIGXCPU; measured, the child
// comes back as "cputime limit exceeded", which is SIGXCPU. Acting on the
// assumption broke the classifier until it was measured, so the correction
// stays written down: convert.cpuExhausted accepts SIGXCPU **and** SIGKILL,
// and this comment is the one a future reader would have trusted.
//
// Applied to THIS process and inherited across execve, exactly like the
// filesystem ruleset, and for the same reason: there is no other hook.
func ApplyLimits(l Limits) error {
	if l.CPUSeconds <= 0 {
		return nil
	}
	rl := unix.Rlimit{Cur: uint64(l.CPUSeconds), Max: uint64(l.CPUSeconds)}
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &rl); err != nil {
		return fmt.Errorf("rlimit cpu %ds: %w", l.CPUSeconds, err)
	}
	return nil
}
