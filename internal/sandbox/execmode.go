package sandbox

import (
	"fmt"
	"os"
	"syscall"
)

// ExecMode is the argv[1] that turns a worker binary into the thing that
// applies the confinement and then BECOMES the engine.
//
// It has to work this way. Landlock restricts the CALLING process and cannot
// be applied to another one, and RLIMIT_CPU has to be set before the exec that
// it should bound -- so something must establish both and then exec the engine
// in the same process. os/exec offers no hook between fork and exec that can
// run Go, which is why this is a MODE of an existing binary rather than a
// callback.
//
// It lives here, and not in one worker's main(), because every engine class
// needs the same thing: the tex engine was simply the first whose input is a
// program. A copy per worker would be nine copies of a security-relevant
// sequence whose ORDER matters (see RunExecMode).
//
// Not a subcommand anyone should call: it is an implementation detail of the
// converters that build the argv.
const ExecMode = "__sandbox-exec"

// SetupExit is the exit status meaning "the ENGINE never ran".
//
// 78 is sysexits.h's EX_CONFIG. The value matters only in that it is not 1 and
// not a status the engines produce: a confinement that could not be
// established is an infrastructure fault the job should be RETRIED on, and
// sharing an exit code with a failed conversion would report a broken node to
// the client as a broken document, and never retry it.
const SetupExit = 78

// RunExecModeIfRequested runs the exec mode when argv asks for it, and reports
// whether it did. It never returns in that case -- it execs or exits.
//
// Shaped as a one-line call for a worker's main() to make first, before flags
// and before any configuration, so that a worker cannot accidentally do half
// its startup as the helper.
func RunExecModeIfRequested(argv []string) bool {
	if len(argv) > 1 && argv[1] == ExecMode {
		RunExecMode(argv[2:])
		return true
	}
	return false
}

// RunExecMode applies the confinement described by argv and execs the rest.
//
//	<worker> __sandbox-exec cpu:271 rw:/tmp/job sock:/tmp ro:/opt/... -- /usr/bin/engine ...
//
// The ORDER here is load-bearing and is the reason this is not inlined per
// worker:
//
//  1. limits before the ruleset, because Apply locks the OS thread and never
//     unlocks it, and setrlimit is per-process rather than per-thread -- doing
//     it after would work today and is the kind of ordering nobody re-derives
//     correctly later;
//  2. the thread id is captured after Apply and checked before exec, because
//     Landlock is a property of the THREAD and execve carries the state of the
//     thread that calls it. A goroutine migrating in between would leave the
//     engine unconfined while producing a correct result and no error.
func RunExecMode(argv []string) {
	p, limits, cmd, err := ParseSpec(argv)
	if err != nil {
		Fatal("%v", err)
	}
	if err := ApplyLimits(limits); err != nil {
		Fatal("%v", err)
	}
	// NO PATH TOKENS MEANS NO FILESYSTEM RULESET WAS ASKED FOR -- and that is
	// a real configuration, not a mistake: a deployment can disable Landlock
	// and still want the CPU bound, which is enforced above and does not
	// depend on it.
	//
	// Applying an EMPTY ruleset would be the opposite of skipping it: Landlock
	// handles everything the ruleset declares and grants only what its rules
	// grant, so a ruleset with no rules denies every path on the system. The
	// engine would fail to open its own binary.
	restricted := ThreadID()
	if len(p.ReadOnly)+len(p.ReadWrite)+len(p.Socket) > 0 {
		if err := Apply(p); err != nil {
			// FAIL CLOSED. Running the engine unconfined because the
			// confinement could not be established is exactly the outcome this
			// exists to prevent, and it would be invisible.
			Fatal("%v", err)
		}
		restricted = ThreadID()
	}

	if now := ThreadID(); now != restricted {
		Fatal("thread changed between applying the sandbox (%d) and exec (%d): "+
			"the engine would run unconfined", restricted, now)
	}
	if err := syscall.Exec(cmd[0], cmd, os.Environ()); err != nil {
		Fatal("exec %s: %v", cmd[0], err)
	}
}

// Fatal reports on stderr and exits with the status that means the engine
// never started.
func Fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, ExecMode+": "+format+"\n", args...)
	os.Exit(SetupExit)
}
