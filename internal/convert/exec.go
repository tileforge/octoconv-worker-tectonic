package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	engineWaitDelay   = time.Second
	groupKillInterval = time.Millisecond
	groupKillBudget   = time.Second
)

func killGroupAndReap(pgid int, done <-chan error) {
	killGroupWith(syscall.Kill, pgid, done)
}

func killGroupWith(kill func(int, syscall.Signal) error, pgid int, done <-chan error) {
	stop := time.Now().Add(groupKillBudget)
	for {
		started := time.Now()
		// XNU's killpg signals a snapshot of the group's members, so a child forked during the call is missed.
		if err := kill(-pgid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) || started.After(stop) {
			break
		}
		select {
		case <-done:
			done = nil
		case <-time.After(groupKillInterval):
		}
	}
	if done != nil {
		<-done
	}
}

// tailBuffer is an io.Writer that retains at most max bytes, discarding from
// the FRONT as more arrive. max <= 0 retains everything.
type tailBuffer struct {
	max     int
	buf     []byte
	dropped int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.max <= 0 {
		t.buf = append(t.buf, p...)
		return n, nil
	}
	if len(p) >= t.max {
		t.dropped += int64(len(t.buf)) + int64(len(p)-t.max)
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return n, nil
}

// Bytes returns what was retained. When anything was discarded it says so, at
// the FRONT, where no parser reading the tail can mistake it for engine output
// -- silence about a truncation would turn "the log does not contain X" into a
// claim this buffer is not entitled to make.
func (t *tailBuffer) Bytes() []byte {
	if t.dropped == 0 {
		return t.buf
	}
	return append([]byte(fmt.Sprintf("[octoconv: %d earlier bytes of engine output discarded]\n", t.dropped)), t.buf...)
}

func (t *tailBuffer) String() string { return string(t.Bytes()) }

// runCommand executes an external engine with hardened process handling:
//
//   - the child runs in its own process group (Setpgid), so when ctx is
//     cancelled or its deadline fires we kill the whole group — children such as
//     LibreOffice's soffice.bin do not get orphaned/left hanging;
//   - stdout is captured and returned to the caller (some engines, e.g.
//     veraPDF's `--format xml`, write their entire machine-readable result to
//     stdout rather than a file, D-04/phase 23 -- verified live in
//     scripts/verapdf-measure.sh) and stderr is captured and folded into the
//     returned error for diagnostics.
//
// The caller is expected to pass a ctx carrying the engine timeout. stdout is
// returned even when the process exits non-zero (D-09: some engines use a
// non-zero exit code to report a valid-but-negative result rather than a
// process failure -- e.g. veraPDF exits 1 for a non-compliant-but-valid
// report -- so the caller, not runCommand, decides how to interpret
// exit-code-vs-output).
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	// 0 == keep everything, which is what every engine but one needs: veraPDF's
	// entire XML report arrives on stdout and is PARSED, so truncating it would
	// corrupt a result rather than trim a diagnostic.
	return runCommandLimited(ctx, 0, name, args...)
}

// runCommandLimited is runCommand with a bound on how much of the child's
// output is retained. The LAST limit bytes are kept, not the first.
//
// # WHY THIS EXISTS
//
// runCommand buffers the child's stdout and stderr in memory with no bound,
// which is fine for an engine fed a document and not fine for one fed a
// PROGRAM. Found in review (PR #16, codex round 2, P1) and then measured, in
// the shipped image at the chart's own 1 GiB:
//
//	235 bytes of .tex  ->  323 565 198 bytes of stdout in 30 seconds
//
// That is 10.8 MB/s from a \message loop, so the 271-second tectonic timeout
// would have to hold ~2.9 GB in a 1 GiB container. The worker is OOM-killed
// long before the deadline can classify anything, the job is left active, and
// the reconciler hands it back to be allocated again.
//
// The tail is the half worth keeping: every parser downstream reads the END of
// the log -- the missing .sty that aborted the run, the biber launch failure,
// and the undefined-reference warning, which is deliberately scoped to the
// final pass. Keeping the head would drop exactly the bytes that name the
// cause.
func runCommandLimited(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	return runCommandLimitedEnv(ctx, limit, nil, name, args...)
}

// runCommandLimitedEnv is runCommandLimited with an explicit environment for
// the child. A nil env inherits this process's, which is the default and what
// every engine but one uses.
//
// # WHY AN ENGINE WOULD WANT ITS OWN ENVIRONMENT
//
// A conversion engine that can READ FILES can read /proc/self/environ, and this
// process's environment carries the service credentials. Proven on the shipped tectonic image
// rather than reasoned about: a .tex containing
//
//	\newread\f \openin\f=/etc/hostname \read\f to \line
//
// typeset the container's hostname INTO THE RETURNED PDF, and the same read
// against /proc/self/environ returned the process environment. --untrusted
// stops \write18; it does not confine reads to the job's directory.
//
// Scrubbing the child's environment does not confine the reads -- it removes
// what they are worth. See TectonicConverter.Convert for the allowlist and for
// what remains readable.
// engineCPU is what the child actually CONSUMED, as opposed to how long it
// occupied the clock.
//
// Carried out of runCommandLimitedEnv because the two quantities answer
// different questions and only one of them is the document's fault: measured
// in the shipped image at a 2-CPU quota, the same document took 0.96s wall for
// 0.95s of CPU idle, and 3.67s wall for 1.08s of CPU with the node
// oversubscribed 4x. A deadline that fires while the child has barely run says
// something about the NODE.
type engineCPU struct {
	User, Sys time.Duration
	// Elapsed is the wall clock the child occupied, kept beside the CPU it
	// consumed because neither number means anything alone: the question is
	// what SHARE of a CPU the child got, and that is a ratio.
	Elapsed time.Duration
	// Known is false when no child ever ran (a failure to start), so a caller
	// cannot mistake "consumed nothing" for "consumed zero".
	Known bool
}

func (c engineCPU) Total() time.Duration { return c.User + c.Sys }

// Share is how much of one CPU the child actually got, 0..n. Measured in the
// shipped image at a 2-CPU quota: 1.00 idle, 0.28 with the node
// oversubscribed 4x.
func (c engineCPU) Share() float64 {
	if !c.Known || c.Elapsed <= 0 {
		return 0
	}
	return c.Total().Seconds() / c.Elapsed.Seconds()
}

func cpuOf(ps *os.ProcessState, elapsed time.Duration) engineCPU {
	if ps == nil {
		return engineCPU{Elapsed: elapsed}
	}
	return engineCPU{User: ps.UserTime(), Sys: ps.SystemTime(), Elapsed: elapsed, Known: true}
}

func runCommandLimitedEnv(ctx context.Context, limit int, env []string, name string, args ...string) ([]byte, error) {
	out, _, err := runCommandLimitedEnvCPU(ctx, limit, env, name, args...)
	return out, err
}

// runCommandLimitedEnvCPU also reports what the child consumed. Separate entry
// point rather than a changed signature everywhere: only the tex engine has a
// question the answer helps with.
func runCommandLimitedEnvCPU(ctx context.Context, limit int, env []string, name string, args ...string) ([]byte, engineCPU, error) {
	return runCommandCore(ctx, limit, env, name, name, args...)
}

// runCommandAs runs name -- the command actually exec'd -- but names label in
// every error shape it produces.
//
// Exists for the sandbox helper: when an engine runs confined, the process
// this package starts is the WORKER BINARY in its own re-exec mode, not the
// engine. Without this, every error would read "/usr/local/bin/pandoc-worker
// failed: ..." instead of "pandoc failed: ...", and the worker's own
// terminal-error classifiers (isAudioTerminal's "ffprobe failed:" match and
// its siblings) would stop matching the moment confinement is turned on.
// Same process handling as runCommand: Setpgid, SIGKILL of the whole process
// group on ctx.Done, unbounded tail buffers, stdout returned even on a
// non-zero exit.
func runCommandAs(ctx context.Context, label, name string, args ...string) ([]byte, error) {
	out, _, err := runCommandCore(ctx, 0, nil, label, name, args...)
	return out, err
}

// runCommandCore is runCommandLimitedEnvCPU's body, with the process actually
// started (name) separated from the name every error shape reports (label).
// runCommandLimitedEnvCPU calls this with label == name, so every existing
// caller's error text is byte-identical to before this split.
func runCommandCore(ctx context.Context, limit int, env []string, label, name string, args ...string) ([]byte, engineCPU, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = engineWaitDelay
	if env != nil {
		cmd.Env = env
	}

	stdout := &tailBuffer{max: limit}
	stderr := &tailBuffer{max: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, engineCPU{}, fmt.Errorf("start %s: %w", label, err)
	}

	// Kill the entire process group on context cancellation/timeout.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		killGroupAndReap(cmd.Process.Pid, done) // reap -- and it is the reap that populates ProcessState
		// nil, not the partial output: every other engine reaches this path
		// too, and changing what a timeout returns for all of them is not what
		// this change is for.
		return nil, cpuOf(cmd.ProcessState, time.Since(started)), fmt.Errorf("%s killed: %w", label, ctx.Err())
	case err := <-done:
		if err != nil {
			killGroupAndReap(cmd.Process.Pid, nil)
			return stdout.Bytes(), cpuOf(cmd.ProcessState, time.Since(started)), fmt.Errorf("%s failed: %w: %s", label, err, stderr.String())
		}
		return stdout.Bytes(), cpuOf(cmd.ProcessState, time.Since(started)), nil
	}
}
