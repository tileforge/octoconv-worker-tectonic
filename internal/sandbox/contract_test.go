package sandbox

import (
	"runtime"
	"testing"
)

// The two implementations of this package are held to one contract, stated
// here so it is checked on whatever platform the tests run.
//
// The failure this guards against is silent: a stub that returned nil from
// Apply would make every non-Linux caller believe it was confined, and the
// caller's whole design is "apply, then become the engine". It would run the
// engine unsandboxed and report success.

// TestApplyNeverSucceedsWithoutLandlock: Available() and Apply() must agree.
// Apply is allowed to fail on a host that HAS Landlock (an unopenable path,
// a policy denial), but it must never succeed on one that does not.
func TestApplyNeverSucceedsWithoutLandlock(t *testing.T) {
	if Available() {
		t.Skipf("this host has landlock (%s); the negative half of the contract is not observable here", runtime.GOOS)
	}

	err := Apply(Paths{ReadOnly: []string{"/"}})
	if err == nil {
		t.Fatalf("Apply returned nil on %s, where Available() is false. Every caller treats a nil "+
			"return as 'the engine is now confined' and then execs it -- so this would run a TeX "+
			"interpreter over client-authored programs with no confinement, and report success.",
			runtime.GOOS)
	}
}

// TestAvailableIsCheapAndRepeatable: it is called at startup and its answer
// decides whether the worker runs at all, so it must not depend on call order
// or leave anything behind.
func TestAvailableIsCheapAndRepeatable(t *testing.T) {
	first := Available()
	for i := 0; i < 100; i++ {
		if Available() != first {
			t.Fatalf("Available() changed answer on call %d; the startup decision would depend on timing", i+2)
		}
	}
}

// TestPathsZeroValueGrantsNothing pins the zero value, because "confine to
// nothing" and "confine to everything" are one typo apart and only one of them
// is safe to get by accident.
func TestPathsZeroValueGrantsNothing(t *testing.T) {
	var p Paths
	if len(p.Spec()) != 0 {
		t.Fatalf("the zero Paths renders %v; an empty ruleset must grant nothing, not something", p.Spec())
	}
}
