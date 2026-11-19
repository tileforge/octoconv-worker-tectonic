package convert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tileforge/octoconv-worker-tectonic/internal/sandbox"
	"testing"
)

// The worker's classifier keys on the sentinels, and its tests prove it
// handles them. Nothing proved the CONVERTER produces them -- which is the
// same "check and checked thing are different objects" mistake that cost this
// phase four findings. These drive Convert itself.
//
// No tectonic involved: a stand-in engine reproduces the two kernel-level
// outcomes that matter, which is what the classification actually reads.

// helperArgv returns the tokens the stand-in helper was actually handed.
func helperArgv(t *testing.T, in string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(in), "argv"))
	if err != nil {
		t.Fatalf("the helper recorded no argv, so it was never invoked: %v", err)
	}
	return strings.Fields(string(b))
}

func cpuTestConverter(t *testing.T, engineBody string, cpuSeconds int) (TectonicConverter, string) {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// Convert resolves "tectonic" from PATH before invoking the helper.
	if err := os.WriteFile(filepath.Join(bin, "tectonic"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	// The stand-in helper RECORDS ITS ARGV before doing anything, then runs
	// its own body -- so the test needs no kernel support beyond `ulimit -t`,
	// and can still assert that the tokens it was supposed to receive arrived.
	//
	// The recording is not decoration. Without it these tests proved only that
	// the CLASSIFICATION works, and an inversion that stopped emitting the
	// cpu: token passed twice: the helper was killing itself with its own
	// ulimit and the spec was never read.
	helper := filepath.Join(dir, "helper")
	argvFile := filepath.Join(dir, "argv")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" + engineBody
	if err := os.WriteFile(helper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(dir, "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("not a real bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.tex")
	if err := os.WriteFile(in, []byte("\\documentclass{article}\\begin{document}x\\end{document}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return TectonicConverter{
		BundlePath:        bundle,
		SandboxHelper:     []string{helper},
		CPUSeconds:        cpuSeconds,
		ConfineFilesystem: true,
	}, in
}

// TestConvertProducesTheCPUExhaustedSentinel: a child killed by a signal after
// spending its whole budget must be reported as the document costing too much,
// not as a generic engine failure -- the classifier downstream can only act on
// what this produces.
func TestConvertProducesTheCPUExhaustedSentinel(t *testing.T) {
	// Burns CPU under a 1-second RLIMIT_CPU of its own, so the kernel kills it
	// exactly the way ApplyLimits arranges in production.
	c, in := cpuTestConverter(t, "ulimit -t 1\nwhile :; do :; done\n", 1)

	err := c.Convert(context.Background(), in, filepath.Join(filepath.Dir(in), "out.pdf"), nil)
	if err == nil {
		t.Fatal("a child killed for exceeding its CPU limit produced no error")
	}
	if !errors.Is(err, ErrTeXCPUExhausted) {
		t.Fatalf("the CPU refusal is not classified, so the worker sees a generic engine failure "+
			"and the client is told their document could not be typeset rather than that it cost "+
			"too much: %v", err)
	}
	if !strings.Contains(err.Error(), "of CPU") {
		t.Fatalf("the error does not say how much CPU was spent, which is the one number an "+
			"operator needs to tell a bad document from a budget set too low: %v", err)
	}
}

// TestConvertProducesTheStarvedSentinel is the other direction and the one
// that costs a client a working document: the deadline fires while the engine
// has barely run.
func TestConvertProducesTheStarvedSentinel(t *testing.T) {
	// Sleeps rather than working: consumes no CPU at all, so when the deadline
	// fires the budget is untouched.
	c, in := cpuTestConverter(t, "sleep 30\n", 20)

	ctx, cancel := context.WithTimeout(context.Background(), 2*1e9)
	defer cancel()

	err := c.Convert(ctx, in, filepath.Join(filepath.Dir(in), "out.pdf"), nil)
	if err == nil {
		t.Fatal("a deadline that expired produced no error")
	}
	if !errors.Is(err, ErrTeXEngineStarved) {
		t.Fatalf("a deadline that fired while the engine consumed no CPU is not classified as "+
			"starvation, so the worker treats it as a document that failed to typeset -- the "+
			"client is blamed and the job is never retried on a quieter node: %v", err)
	}
	if errors.Is(err, ErrTeXEngineTimeout) {
		t.Fatal("it is ALSO the terminal timeout sentinel; whichever the worker checks first " +
			"decides, and that is not a decision to leave to ordering")
	}
}

// NOTE: TestConvertStillTimesOutWhenTheEngineWasBusy used to live here, and
// it asserted on a busy loop getting a whole CPU -- so it failed on a loaded
// runner while the code did exactly what it is configured to do. Its property
// ("an engine that burned CPU for its whole deadline is NOT starvation") is
// now the "a full CPU for the whole deadline" row of
// TestDeadlineClassificationIsDecidedByTheMeasurements, decided by values.
//
// Deleting it rather than keeping both: a test that fails on a busy machine
// teaches people to re-run the suite until it passes, which is how a real
// failure gets re-run away.

// The two verdicts that come from the same pair of measurements, decided by
// VALUES rather than by what the scheduler happened to grant.
//
// These were busy loops, and on a loaded runner the loop genuinely receives
// less than half a CPU -- which is the production condition that means
// starvation -- so the test failed while the code did exactly what it is
// configured to do. Codex reproduced 0.31-0.39 of a CPU with this package's
// own tests running concurrently, and that is the same shape as the one
// unexplained failure earlier in this branch that I could not reproduce and
// did not diagnose. A test that needs a whole CPU to pass is testing the
// runner.
func TestDeadlineClassificationIsDecidedByTheMeasurements(t *testing.T) {
	cases := []struct {
		name        string
		user, sys   time.Duration
		elapsed     time.Duration
		wantStarved bool
	}{
		{
			// The engine had a CPU to itself and did not finish: the document
			// is at fault, and it will be at fault again.
			name: "a full CPU for the whole deadline", user: 271 * time.Second,
			elapsed: 271 * time.Second, wantStarved: false,
		},
		{
			// Codex's measured range on a contended runner.
			name: "a third of a CPU", user: 90 * time.Second,
			elapsed: 271 * time.Second, wantStarved: true,
		},
		{
			// The shape the original finding named: a budget far larger than
			// the deadline cannot make a saturated runaway look starved,
			// because the test is a RATIO and does not know the budget.
			name: "saturated, with a 600s budget and a 271s deadline", user: 271 * time.Second,
			elapsed: 271 * time.Second, wantStarved: false,
		},
		{
			// Exactly at the threshold is not below it.
			name: "exactly half a CPU", user: 50 * time.Second,
			elapsed: 100 * time.Second, wantStarved: false,
		},
		{
			name: "system time counts as consumption", sys: 200 * time.Second,
			elapsed: 271 * time.Second, wantStarved: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyDeadline(engineCPU{User: c.user, Sys: c.sys, Elapsed: c.elapsed, Known: true})
			if got != c.wantStarved {
				t.Fatalf("starved = %v, want %v (%.2f of one CPU).\nGetting this wrong either "+
					"blames a client for a busy node, or retries a document that does not "+
					"terminate.", got, c.wantStarved,
					(c.user+c.sys).Seconds()/c.elapsed.Seconds())
			}
		})
	}
}

// No measurement means no claim. Guessing "starved" here would retry a
// document that may genuinely never terminate.
func TestUnknownCPUIsNotCalledStarvation(t *testing.T) {
	if classifyDeadline(engineCPU{Elapsed: time.Minute}) {
		t.Fatal("a deadline with no CPU measurement was classified as starvation")
	}
}

// cpuExhausted must not fire on a signal that is not the kernel's CPU kill,
// and its tolerance must never swallow the budget -- at the smallest budget
// startup accepts (1s), a flat one-second tolerance made the threshold zero
// and every signaled child qualified (codex, PR #22).
func TestCPUExhaustedNeedsTheRightSignalAndRealConsumption(t *testing.T) {
	cases := []struct {
		name   string
		sig    syscall.Signal
		used   time.Duration
		budget int
		want   bool
	}{
		// SIGXCPU is what a real kill produces here -- measured, after
		// asserting SIGKILL alone and breaking the two tests that exercise
		// one.
		{"SIGXCPU at its budget", syscall.SIGXCPU, 271 * time.Second, 271, true},
		{"SIGKILL at its budget", syscall.SIGKILL, 271 * time.Second, 271, true},
		{"killed marginally under, within tolerance", syscall.SIGKILL, 270*time.Second + 980*time.Millisecond, 271, true},
		{"the 1s budget, killed at it", syscall.SIGKILL, time.Second, 1, true},

		// The finding: at a 1s budget an immediate crash used to qualify.
		{"the 1s budget, crashed immediately", syscall.SIGXCPU, 5 * time.Millisecond, 1, false},
		{"a segfault at its budget", syscall.SIGSEGV, 271 * time.Second, 271, false},
		{"a shutdown TERM", syscall.SIGTERM, 271 * time.Second, 271, false},
		{"an OOM kill early in the budget", syscall.SIGKILL, 10 * time.Second, 271, false},

		// THE PRODUCTION BUDGET, and the case a proportional tolerance let
		// through: a tenth of 271s is 27 SECONDS, so a cgroup OOM kill at 244
		// CPU-seconds fell inside the window and was reported as a
		// processing-time refusal -- telling the client to make the document
		// cheaper when the answer was to make it smaller (codex, PR #22).
		{"an OOM kill 27s short of the budget", syscall.SIGKILL, 244 * time.Second, 271, false},
		{"an OOM kill 5s short of the budget", syscall.SIGKILL, 266 * time.Second, 271, false},
		{"a real kill at the production budget", syscall.SIGXCPU, 271 * time.Second, 271, true},
		{"a real kill 30ms under, within the tolerance", syscall.SIGXCPU, 270*time.Second + 970*time.Millisecond, 271, true},

		// THE MINIMUM BUDGET, and the case a flat 500ms window let through: a
		// fast OOM in the second half of a 1s budget was called a
		// processing-time refusal (codex, PR #22).
		{"a 1s budget, OOM at 0.6s", syscall.SIGKILL, 600 * time.Millisecond, 1, false},
		{"a 1s budget, OOM at 0.9s", syscall.SIGKILL, 900 * time.Millisecond, 1, false},
		{"a 1s budget, a real kill at 1.002s", syscall.SIGXCPU, 1002 * time.Millisecond, 1, true},
		{"no budget configured", syscall.SIGKILL, time.Hour, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cpuExhausted(c.sig, c.used, c.budget); got != c.want {
				t.Fatalf("cpuExhausted(%v, %s, %ds) = %v, want %v", c.sig, c.used, c.budget, got, c.want)
			}
		})
	}
}

// TestCPUBudgetAppliesWithLandlockOff is codex round 1's first finding, and
// the configuration my other tests never exercised: TECTONIC_SANDBOX_DISABLED
// is a supported deployment, and the CPU budget does not depend on Landlock.
//
// The bug it pins is the quiet kind -- the worker logged an active budget and
// enforced nothing, which is "a bound that is configured and not plumbed is a
// bound that does not exist", a defect this repository has shipped and named
// before.
func TestCPUBudgetAppliesWithLandlockOff(t *testing.T) {
	c, in := cpuTestConverter(t, "ulimit -t 1\nwhile :; do :; done\n", 1)
	c.ConfineFilesystem = false // the sandbox is off; the budget is not

	err := c.Convert(context.Background(), in, filepath.Join(filepath.Dir(in), "out.pdf"), nil)
	if err == nil {
		t.Fatal("no error at all with the filesystem sandbox off")
	}
	if !errors.Is(err, ErrTeXCPUExhausted) {
		t.Fatalf("with the sandbox off the CPU budget is not enforced or not classified, so a "+
			"deployment that disabled Landlock silently loses the bound it configured: %v", err)
	}

	// The classification above can be satisfied by a child that limits itself.
	// This is the half that says the BOUND WAS ASKED FOR: the cpu: token has
	// to reach the helper, and no path token may, because the sandbox is off.
	argv := helperArgv(t, in)
	var sawCPU bool
	for _, tok := range argv {
		if tok == "cpu:1" {
			sawCPU = true
		}
		if strings.HasPrefix(tok, "ro:") || strings.HasPrefix(tok, "rw:") {
			t.Errorf("the helper was handed %q with the filesystem sandbox OFF; a ruleset nobody "+
				"asked for is being applied.\nargv = %v", tok, argv)
		}
	}
	if !sawCPU {
		t.Fatalf("the helper was never handed a cpu: token, so the budget is not being applied at "+
			"all -- the worker would log an active budget and enforce nothing.\nargv = %v", argv)
	}
}

// And the spec proves which half is being asked for: limits with no path
// tokens, because an EMPTY ruleset denies everything rather than nothing.
func TestSpecWithoutConfinementCarriesOnlyTheLimit(t *testing.T) {
	spec := (sandbox.Paths{}).SpecWith(sandbox.Limits{CPUSeconds: 42})
	if len(spec) != 1 || spec[0] != "cpu:42" {
		t.Fatalf("spec = %v, want exactly [cpu:42]. A path token here would be a ruleset nobody "+
			"asked for; no token at all would be a budget that is never applied.", spec)
	}
}

// TestPreflightHonoursTheConfinementFlag is a REGRESSION I introduced fixing
// codex round 1: making the helper carry the CPU budget meant SandboxHelper is
// non-empty even with Landlock disabled, and the preflight then built a
// filesystem ruleset regardless -- so on a host with no Landlock the worker
// exited 78 at startup, in exactly the deployment TECTONIC_SANDBOX_DISABLED
// exists to support.
//
// The test asserts the SPEC, because that is what the helper acts on: a
// preflight that runs is not evidence that it ran the right configuration.
func TestPreflightHonoursTheConfinementFlag(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tectonic"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	helper := filepath.Join(dir, "helper")
	argvFile := filepath.Join(dir, "argv")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\nexit 0\n"
	if err := os.WriteFile(helper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("not a real bundle"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := TectonicConverter{
		BundlePath:        bundle,
		SandboxHelper:     []string{helper},
		CPUSeconds:        271,
		ConfineFilesystem: false, // TECTONIC_SANDBOX_DISABLED=true
	}
	// The stand-in helper exits 0 without producing a PDF, so the preflight
	// reports failure -- what is under test is the ARGV it was handed.
	_, _ = c.TectonicPreflight(context.Background(), c.SandboxHelper)

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the preflight never invoked the helper: %v", err)
	}
	argv := strings.Fields(string(raw))

	var sawCPU bool
	for _, tok := range argv {
		if tok == "cpu:271" {
			sawCPU = true
		}
		if strings.HasPrefix(tok, "ro:") || strings.HasPrefix(tok, "rw:") {
			t.Errorf("the preflight builds a filesystem ruleset with confinement DISABLED: %q.\n"+
				"On a host without Landlock the helper exits 78 and the worker refuses to start, "+
				"which is the one deployment the escape hatch exists for.\nargv = %v", tok, argv)
		}
	}
	if !sawCPU {
		t.Errorf("the preflight carries no cpu: token, so it is not exercising the budget the "+
			"jobs will run under.\nargv = %v", argv)
	}
}
