package sandbox_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/tileforge/octoconv-worker-tectonic/internal/sandbox"
)

// testDisabledEnv is a name no deployment uses, so these tests cannot be
// influenced by whatever a real worker's variable happens to be set to.
const testDisabledEnv = "SANDBOX_DISABLED_UNDER_TEST"

// TestSandboxIsOnUnlessSomebodyWroteItDown is the whole startup contract, and
// it lives HERE rather than beside one worker because two workers now depend
// on it and the rest are coming.
//
// The failure it guards against is not a crash, it is a QUIET one: a worker
// that could not confine its engine, started anyway, and processed client
// files with read access to every file in its container. So the only two
// permitted outcomes are "confined" and "refused to start" -- unless the
// operator set the escape hatch, which is the third and has to be written
// down.
func TestSandboxIsOnUnlessSomebodyWroteItDown(t *testing.T) {
	t.Setenv(testDisabledEnv, "")
	helper, err := sandbox.ResolveHelper(testDisabledEnv)

	switch {
	case err != nil:
		// Refused. Only legitimate where Landlock is genuinely absent, and the
		// message has to say what to do about it.
		if sandbox.Available() {
			t.Fatalf("landlock is available and ResolveHelper still failed: %v", err)
		}
		if !strings.Contains(err.Error(), testDisabledEnv) {
			t.Fatalf("the refusal does not name %s, so an operator on a kernel without landlock "+
				"has no documented way forward: %v", testDisabledEnv, err)
		}
	case len(helper) == 0:
		t.Fatalf("ResolveHelper returned no helper and no error: the engine would run "+
			"unconfined, silently, with %s unset", testDisabledEnv)
	default:
		if !sandbox.Available() {
			t.Fatalf("a helper was returned on a host without landlock (%s): the sandbox cannot "+
				"be established and every job would fail at exec time instead of at startup",
				runtime.GOOS)
		}
		if len(helper) != 2 || helper[1] != sandbox.ExecMode {
			t.Fatalf("helper is %v; the converter prefixes the engine argv with it verbatim, so "+
				"it must be [<this binary> %s]", helper, sandbox.ExecMode)
		}
	}
}

// TestEscapeHatchIsExactlyTrue keeps the documented switch from being opened
// by a value that merely looks affirmative.
func TestEscapeHatchIsExactlyTrue(t *testing.T) {
	for _, v := range []string{"1", "yes", "TRUE", "True", "on", ""} {
		t.Setenv(testDisabledEnv, v)
		helper, err := sandbox.ResolveHelper(testDisabledEnv)
		if err == nil && len(helper) == 0 {
			t.Fatalf("%s=%q disabled the sandbox; only the exact string \"true\" may",
				testDisabledEnv, v)
		}
	}
	t.Setenv(testDisabledEnv, "true")
	helper, err := sandbox.ResolveHelper(testDisabledEnv)
	if err != nil || len(helper) != 0 {
		t.Fatalf("%s=true did not disable the sandbox (helper=%v err=%v); the documented "+
			"escape hatch does not work", testDisabledEnv, helper, err)
	}
}
