package sandbox

import (
	"runtime"
	"testing"
)

// The CPU bound does not depend on Landlock, and it must not depend on Linux.
//
// It lived beside the Landlock code, so the !linux build rejected every
// requested limit -- and on macOS with TECTONIC_SANDBOX_DISABLED=true, where
// the operator gave up filesystem confinement and KEPT the budget, the helper
// exited and the worker refused to start (codex, PR #22). RLIMIT_CPU is POSIX;
// it works here.
func TestCPULimitAppliesOnThisPlatform(t *testing.T) {
	// A budget far larger than this test process will ever spend, applied to
	// the test process itself -- setrlimit is per-process and permanent, so a
	// small value would kill the test binary partway through the package.
	if err := ApplyLimits(Limits{CPUSeconds: 1 << 20}); err != nil {
		t.Fatalf("a CPU limit could not be applied on %s: %v.\n"+
			"With the filesystem sandbox disabled this is the ONLY bound left, and a worker that "+
			"cannot set it refuses to start -- in exactly the deployment the escape hatch exists "+
			"to support.", runtime.GOOS, err)
	}
}

// Zero means unbounded and must not be an error: it is what every unit test
// and any deployment that has not configured a budget passes.
func TestZeroCPULimitIsNotAnError(t *testing.T) {
	if err := ApplyLimits(Limits{}); err != nil {
		t.Fatalf("the zero Limits was rejected: %v", err)
	}
}
