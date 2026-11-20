package convert

// This file (tectonic.go): the ninth engine class -- real typesetting,
// `latex` -> `pdf`, through tectonic.
//
// WHAT MAKES THIS ENGINE DIFFERENT FROM THE OTHER EIGHT
//
// Every other converter here reads a file and writes a file. This one runs a
// TeX interpreter over a document that is a PROGRAM: LaTeX has macros, control
// flow, file input and -- unless stopped -- the ability to run shell commands
// and to fetch what it does not have. Two of this phase's requirements exist
// because of that, and both are properties of the argv below plus the image
// around it, not of anything this Go code computes:
//
//	TEX-02  no network. The bundle is baked into the image and passed with -b.
//	        Without it tectonic reaches for relay.fullyjustified.net and the
//	        document decides what gets downloaded.
//	TEX-04  no shell escape. --untrusted makes \write18 and \input{|cmd}
//	        un-enableable REGARDLESS of any other flag on the same command
//	        line -- a structural guarantee rather than a default that a later
//	        argument can undo.
//
// WHAT IS NOT HERE, AND WHY THE ABSENCE IS THE GUARANTEE
//
// No biber. Tectonic launches biber as an EXTERNAL program when a document
// produces a .bcf, taking its argv from a .run.xml that the document's own
// packages write. Whether --untrusted gates that path was never established
// (64-RESEARCH.md records the reading as UNVERIFIED), and with no binary in the
// image the question does not arise. biblatex runs on backend=bibtex, which
// tectonic executes inside its own process. See 64-CONTEXT.md D-07.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/tileforge/octoconv-worker-tectonic/internal/sandbox"
)

// TectonicBundleEnv names the environment variable cmd/tectonic-worker reads
// to locate the baked-in bundle. Declared here, beside the converter that
// consumes it, rather than in the cmd -- so the default below and the reader
// cannot drift the way PROBE_ENGINE_TIMEOUT once did between two processes.
const TectonicBundleEnv = "TECTONIC_BUNDLE_PATH"

// DefaultTectonicBundlePath is where Dockerfile.tectonic-worker puts the
// bundle. A path rather than a URL, and that is the whole of TEX-02: a bundle
// tectonic can open locally is a bundle it never fetches.
const DefaultTectonicBundlePath = "/opt/tectonic/octoconv-bundle.ttb"

// TectonicConverter is the tex engine's registry entry.
//
// BundlePath is empty in the zero value converters.go registers at init;
// cmd/tectonic-worker replaces it with the configured one at startup, the same
// shape cmd/ocr-worker and cmd/probe-worker use for their own ceilings. A zero
// value therefore cannot typeset, which is deliberate: the failure is "this
// process was not configured", not "the document fetched something".
type TectonicConverter struct {
	BundlePath string

	// MaxOutputBytes bounds the PRODUCED PDF. Zero means
	// DefaultTectonicMaxOutputBytes; see maxOutputBytes.
	MaxOutputBytes int64

	// CPUSeconds bounds what one document may COST, via RLIMIT_CPU on the
	// engine. Zero means unbounded, which is what every unit test and any
	// unconfined deployment gets.
	CPUSeconds int

	// ConfineFilesystem says whether SandboxHelper should also apply the
	// Landlock ruleset, as opposed to only the resource limits.
	//
	// Separate from SandboxHelper being set, because the two controls are
	// independent: a deployment can disable Landlock and still want a CPU
	// budget. Folding them into one flag is what made the CPU bound silently
	// unenforced when the sandbox was off (codex, PR #22), and the fallback
	// that patched it -- resolving os.Executable() as a helper -- quietly
	// assumed the running binary implements the exec mode, which is true of a
	// worker and of nothing else.
	ConfineFilesystem bool

	// SandboxHelper is the argv prefix that establishes the confinement and
	// then BECOMES the engine -- in the deployed image,
	// {"/usr/local/bin/tectonic-worker", "__sandbox-exec"}.
	//
	// Empty means the engine runs unconfined, which is what every unit test
	// on a developer's machine does and what a non-Linux host has to do:
	// Landlock is a Linux LSM. Production does not get that choice --
	// cmd/tectonic-worker refuses to start if it cannot set this (see
	// TECTONIC_SANDBOX_DISABLED for the one documented escape hatch).
	SandboxHelper []string
}

// Engine reports the ninth engine class.
func (TectonicConverter) Engine() string { return EngineTectonic }

// Pairs is the single pair this engine serves.
//
// latex -> pdf only. pandoc already serves latex -> {html, docx, epub, md} and
// registering any of those here would panic at init: Registry.Register refuses
// a triple collision since Phase 57 D-09, which is what makes "two engines, one
// source format" safe to have at all.
func (TectonicConverter) Pairs() []Pair {
	return []Pair{{From: "latex", To: "pdf"}}
}

// TectonicSandboxSetupExit is the exit status the sandbox helper uses to say
// THE ENGINE NEVER RAN -- the confinement could not be established.
//
// Declared here rather than in the helper because the two halves of the
// contract are the helper that exits with it and the classifier below that
// reads it; a constant in one binary that another compares against by literal
// is how the two drift apart.
const TectonicSandboxSetupExit = 78

// cpuExhausted decides whether a signaled child was killed for spending its
// CPU budget, as opposed to any of the other reasons a child can be signaled.
//
// TWO CONDITIONS, and the earlier version had neither properly:
//
//   - THE SIGNAL. RLIMIT_CPU produces SIGXCPU -- measured; see the note in the
//     function body about asserting SIGKILL and being wrong. Accepting ANY
//     signal, which is what the first version did, meant an immediate SIGSEGV
//     or a SIGTERM from a shutdown was reported as a processing-time refusal:
//     terminal, and blaming the document for a crash (codex, PR #22).
//   - THE CONSUMPTION, within a tolerance that CANNOT swallow the budget. The
//     tolerance was a flat one second, so at the smallest budget startup
//     accepts (1s) the threshold became zero and every signaled child
//     qualified. It is now a fraction, floored at a tenth of a second, so it
//     is always strictly inside the budget.
//
// The tolerance exists because RLIMIT_CPU is whole seconds while getrusage
// reports what it accounted for, which can land marginally under.
//
// SIGKILL is also what the cgroup OOM killer sends, so this is not a complete
// discriminator on its own -- the consumption test is what separates them, and
// the OOM branch below catches what falls through.
func cpuExhausted(sig syscall.Signal, used time.Duration, budgetSeconds int) bool {
	// SIGXCPU **or** SIGKILL, and which one arrives is not a detail this code
	// gets to assume.
	//
	// I asserted SIGKILL alone, reasoning that soft == hard means the kernel
	// skips the SIGXCPU-then-grace dance. MEASURED, it does not: the child is
	// reported as "cputime limit exceeded", which is SIGXCPU, and requiring
	// SIGKILL broke the two tests that exercise a real kill. That was the
	// third guess in a row about this branch -- the first two blamed the
	// tolerance, and a measurement showed consumption at 1.0022s against a
	// 1s budget, above it rather than under.
	//
	// Both are accepted because both are real: SIGXCPU is what a soft limit
	// raises, and SIGKILL is what a hard limit produces when the soft one is
	// ignored or already spent. The consumption test below is what separates
	// either from an unrelated kill, including the cgroup OOM killer's
	// SIGKILL.
	if budgetSeconds <= 0 {
		return false
	}
	if sig != syscall.SIGXCPU && sig != syscall.SIGKILL {
		return false
	}
	budget := time.Duration(budgetSeconds) * time.Second

	// THE TOLERANCE IS SMALL, AND SMALLER THAN I TWICE BELIEVED IT NEEDED
	// TO BE.
	//
	// It exists in case getrusage accounts marginally UNDER the limit the
	// kernel enforced, which would make an exact >= comparison miss a genuine
	// kill. Two versions of it were wrong before this one, and both were wrong
	// because the size was reasoned about rather than measured:
	//
	//   - a tenth of the budget: 27 SECONDS at the production 271s, so a
	//     cgroup OOM kill at 244 CPU-seconds was called a processing-time
	//     refusal;
	//   - a flat 500ms: half of the smallest budget startup accepts, so at 1s
	//     an OOM anywhere in the second half was called the same thing.
	//
	// Both told the client to make the document cheaper when the answer was to
	// make it smaller. MEASURED at a 1s limit, ten runs, idle and under eight
	// competing CPU burners: getrusage reported 1.0011s to 1.0065s -- always
	// ABOVE the limit, never under. The under-accounting this tolerance was
	// sized for did not appear at all.
	//
	// So it is 50ms: enough to absorb a rounding artefact if one exists on
	// another kernel, and 5% of even the smallest budget, which leaves the OOM
	// branch the rest of it.
	const tolerance = 50 * time.Millisecond
	if budget <= tolerance {
		// Unreachable while startup enforces a one-second minimum; stated
		// rather than assumed, because a tolerance that reaches the budget
		// makes every signaled child qualify.
		return used >= budget
	}
	return used >= budget-tolerance
}

// classifyDeadline decides whether a deadline that fired says something about
// the DOCUMENT or about the NODE.
//
// A pure function of the two measurements, deliberately: the tests that used to
// cover this ran a busy loop and asserted on what the scheduler gave it, so on
// a loaded runner they failed while the code was behaving exactly as
// configured -- codex reproduced 0.31-0.39 of a CPU with the package's own
// tests running concurrently, and that is the same shape as my one
// unexplained failure earlier in this branch, which I could not reproduce and
// did not diagnose. A test that needs a whole CPU to pass is testing the
// runner.
func classifyDeadline(cpu engineCPU) (starved bool) {
	if !cpu.Known {
		// No measurement, no claim: fall through to the ordinary deadline,
		// which is terminal. Guessing "starved" here would retry a document
		// that may genuinely not terminate.
		return false
	}
	return cpu.Share() < starvedCPUShare
}

// starvedCPUShare is the fraction of one CPU below which a deadline says
// nothing about the document. See the classifier for the measurement.
const starvedCPUShare = 0.5

// ErrTeXCPUExhausted is the document having spent its CPU budget.
//
// TERMINAL, and for a stronger reason than the wall-clock deadline: this is
// what the document COST, measured by the kernel, independent of what else the
// node was doing. A document that burns the budget burns it again.
var ErrTeXCPUExhausted = errors.New("tex document exhausted its CPU budget")

// ErrTeXEngineStarved is the opposite finding from the same pair of numbers:
// the wall-clock deadline fired while the engine had barely run.
//
// RETRYABLE. The deadline says the attempt took too long; it does not say the
// DOCUMENT did, and the difference is measurable. In the shipped image at a
// 2-CPU quota, the same document took 0.96s wall for 0.95s of CPU idle and
// 3.67s wall for 1.08s of CPU with the node oversubscribed 4x -- wall clock
// inflated 4x while consumption did not move. Without this branch, a busy node
// tells the client their document did not finish typesetting, and never
// retries it elsewhere.
var ErrTeXEngineStarved = errors.New("tex engine did not get the CPU to finish")

// ErrTeXSandboxUnavailable is that status, classified.
//
// RETRYABLE, and deliberately unlike every other error in this file: the
// document is fine, this node is not. Left terminal, one bad node would burn
// every tex job routed to it and report each as a client mistake.
var ErrTeXSandboxUnavailable = errors.New("tex sandbox could not be established")

// TectonicPreflight proves the engine can actually be EXECUTED under the
// ruleset this converter would build, before any job depends on it.
//
// It runs the real helper with a real spec and execs the real engine with
// --version, which is the only check that covers a whole class of layout
// problems without pretending to compute a runtime closure: a PATH-resolved
// wrapper script whose shebang interpreter is outside the granted roots, an
// ELF whose PT_INTERP is elsewhere, a build whose shared libraries live in a
// store path. Parsing the binary to derive those would answer some of them and
// silently miss the rest; running it answers all of them at once, and answers
// them the same way the job would (codex round 3).
//
// Called at startup, so a host where the engine cannot run confined refuses to
// consume the queue instead of failing every job on it.
func (c TectonicConverter) TectonicPreflight(ctx context.Context, helper []string) (int, error) {
	if len(helper) == 0 {
		return 0, nil // unconfined by explicit configuration; nothing to prove
	}
	enginePath, err := exec.LookPath("tectonic")
	if err != nil {
		return 0, fmt.Errorf("locate engine: %w", err)
	}

	// A real job directory, because the spec grants one and Apply refuses a
	// rule it cannot open.
	jobDir, err := os.MkdirTemp("", "octoconv-preflight-")
	if err != nil {
		return 0, fmt.Errorf("preflight work directory: %w", err)
	}
	defer os.RemoveAll(jobDir)

	// A REAL DOCUMENT, not --version.
	//
	// --version proves the binary can be executed and nothing else: it never
	// opens the bundle and never loads a format. A persistent cache holding a
	// stale format from another tectonic version passes cacheHoldsFormat and
	// passes --version, and then every real job fails, because building the
	// current format needs a cache the sandbox has made read-only (codex round
	// 4). Compiling the smallest possible document exercises the whole
	// configuration jobs depend on: bundle, format, ruleset, exec.
	inPath := filepath.Join(jobDir, "in.tex")
	const doc = "\\documentclass{article}\n\\begin{document}preflight\\end{document}\n"
	if err := os.WriteFile(inPath, []byte(doc), 0o600); err != nil {
		return 0, fmt.Errorf("preflight document: %w", err)
	}

	// THE SAME SPEC A JOB WOULD GET -- which means honouring
	// ConfineFilesystem, and carrying the CPU budget.
	//
	// Building the ruleset unconditionally made the preflight prove something
	// this deployment does not do: with TECTONIC_SANDBOX_DISABLED=true the
	// helper is still used (it carries the CPU budget), so an unconditional
	// spec applied Landlock on a host that has none, exited 78, and the worker
	// refused to start -- in precisely the environment the escape hatch exists
	// for (codex, PR #22). A preflight that tests a configuration nobody runs
	// is worse than none: it fails the deployments that would have worked.
	//
	// The limit was missing too, so the preflight was not exercising the whole
	// configuration it claims to. It costs nothing here: the document below
	// compiles in under a second against a budget measured in minutes.
	spec := sandbox.Paths{}.SpecWith(sandbox.Limits{CPUSeconds: c.CPUSeconds})
	if c.ConfineFilesystem {
		spec = tectonicSandboxSpecLimited(inPath, jobDir, c.BundlePath, enginePath, c.CPUSeconds)
	}
	args := append(append(append(
		append([]string{}, helper[1:]...),
		spec...),
		"--", enginePath), tectonicArgs(inPath, jobDir, c.BundlePath)...)

	out, err := runCommandLimitedEnv(ctx, tectonicLogLimit, tectonicEnv(jobDir), helper[0], args...)
	if err != nil {
		return 0, fmt.Errorf("the engine cannot typeset under the sandbox: %w: %s",
			err, tectonicTail(out))
	}
	// The engine can exit 0 and produce nothing; the job path checks this too.
	info, serr := os.Stat(filepath.Join(jobDir, "in.pdf"))
	if serr != nil || info.Size() == 0 {
		return 0, fmt.Errorf("the engine produced no PDF for the preflight document: %v: %s",
			serr, tectonicTail(out))
	}
	if c.ConfineFilesystem {
		if err := denyOutsideRead(ctx, helper, enginePath, jobDir, inPath, c.BundlePath, c.CPUSeconds); err != nil {
			return 0, err
		}
	}
	return int(info.Size()), nil
}

// denyOutsideRead runs the engine again under the job directory's ruleset with
// an input that lives in /var/tmp. The spec is built from the inside path, so
// that directory is not a grant. A missing denial is a startup failure.
func denyOutsideRead(ctx context.Context, helper []string, enginePath, jobDir, inPath, bundlePath string, cpuSeconds int) error {
	outsideDir, err := os.MkdirTemp("/var/tmp", "octoconv-tectonic-outside-")
	if err != nil {
		return fmt.Errorf("preflight outside directory: %w", err)
	}
	defer os.RemoveAll(outsideDir)
	spec := tectonicSandboxSpecLimited(inPath, jobDir, bundlePath, enginePath, cpuSeconds)
	if err := outsideIsUngranted(outsideDir, spec); err != nil {
		return err
	}
	const body = "\\documentclass{article}\n\\begin{document}secret\\end{document}\n"
	fixture := filepath.Join(outsideDir, "secret.tex")
	if err := os.WriteFile(fixture, []byte(body), 0o600); err != nil {
		return fmt.Errorf("preflight outside fixture: %w", err)
	}
	args := append(append(append(
		append([]string{}, helper[1:]...),
		spec...),
		"--", enginePath), tectonicArgs(fixture, outsideDir, bundlePath)...)
	_, runErr := runCommandLimitedEnv(ctx, tectonicLogLimit, tectonicEnv(jobDir), helper[0], args...)
	if runErr == nil {
		return errors.New("engine accepted the outside file")
	}
	var setupErr *exec.ExitError
	if errors.As(runErr, &setupErr) && setupErr.ExitCode() == TectonicSandboxSetupExit {
		return fmt.Errorf("sandbox setup failed: %w", runErr)
	}
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		return fmt.Errorf("preflight outside directory: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.tex" {
		return errors.New("engine wrote the outside file")
	}
	got, err := os.ReadFile(fixture)
	if err != nil {
		return fmt.Errorf("preflight outside fixture: %w", err)
	}
	if string(got) != body {
		return errors.New("engine wrote the outside file")
	}
	return nil
}

func outsideIsUngranted(outside string, spec []string) error {
	cleaned, err := filepath.EvalSymlinks(outside)
	if err != nil {
		cleaned = outside
	}
	cleaned = filepath.Clean(cleaned)
	for _, tok := range spec {
		if !strings.HasPrefix(tok, "ro:") && !strings.HasPrefix(tok, "rw:") {
			continue
		}
		granted, gerr := filepath.EvalSymlinks(tok[3:])
		if gerr != nil {
			granted = tok[3:]
		}
		granted = filepath.Clean(granted)
		if cleaned == granted || strings.HasPrefix(cleaned, granted+string(filepath.Separator)) {
			return fmt.Errorf("preflight outside directory %s lies at or beneath granted path %s", cleaned, granted)
		}
	}
	return nil
}

// tectonicSystemReadOnly is what the engine needs merely to be a running
// program: its own binary, its shared libraries, and the loader.
//
// Listed rather than derived because the list IS the security claim, and a
// derived one would grow silently. /etc is deliberately absent, which is the
// whole point -- the read that started this work was \openin=/etc/hostname.
//
// Entries that do not exist on the host are dropped by tectonicSandboxSpec
// rather than being an error: /lib64 exists on x86_64 glibc and not on this
// image's arm64, and /bin and /lib are symlinks into /usr on a merged-usr
// distro. Dropping a MISSING path cannot widen anything; only a present one
// grants, and sandbox.Apply still errors on a configured path that vanished.
var tectonicSystemReadOnly = []string{
	"/usr", "/lib", "/lib64", "/bin", "/etc/ssl/certs",
	// The three character devices a normal program expects to exist. Named
	// individually and never as /dev, which also holds the terminal, the
	// block devices and whatever a runtime chooses to mount there.
	//
	// /dev/null is in the WRITABLE set above for the obvious reason. Its
	// absence was measured rather than reasoned about: with no /dev granted,
	// every `2>/dev/null` inside the sandbox failed with EACCES, which is a
	// surprise that would surface as an unrelated engine error the first time
	// something in the chain redirected.
	"/dev/urandom", "/dev/random", "/dev/zero",
}

// tectonicSandboxSpec renders the ruleset as the argv tokens the helper parses.
//
// It is a pure function of the paths so a test can assert on the whole set
// without a kernel: the argv IS the sandbox, the same way tectonicArgs is the
// hardening.
func tectonicSandboxSpec(inPath, outDir, bundlePath, enginePath string) []string {
	return tectonicSandboxSpecLimited(inPath, outDir, bundlePath, enginePath, 0)
}

// tectonicSandboxSpecLimited is the same ruleset plus what the job may cost.
func tectonicSandboxSpecLimited(inPath, outDir, bundlePath, enginePath string, cpuSeconds int) []string {
	// The job directory, and only the job directory, is writable. inPath's
	// directory and outDir are the same one today; both are named because the
	// worker is free to stop doing that and a sandbox that assumed otherwise
	// would fail as an unexplained engine error.
	// The job's own directories, and NOT os.TempDir(). Landlock grants are
	// path-beneath and additive, so naming the shared /tmp here would grant
	// everything under it and make the two entries that matter meaningless.
	// The engine's TMPDIR is pointed at the job directory instead; see
	// tectonicEnv.
	rw := dedupeExisting([]string{filepath.Dir(inPath), outDir, "/dev/null"})

	// The engine binary ITSELF, not only the directories it usually lives in.
	//
	// tectonicSystemReadOnly covers /usr, /lib and /bin, which is where this
	// image puts it -- but exec.LookPath answers from PATH, and an image that
	// installs tectonic in /opt or a Nix store would resolve outside all three.
	// The helper would then apply the ruleset and take EACCES from execve,
	// breaking an installation that worked before confinement (codex round 2).
	ro := []string{bundlePath, enginePath}
	if cache := os.Getenv("TECTONIC_CACHE_DIR"); cache != "" {
		// THE DIRECTORIES HOLDING FORMATS, NOT THE CONFIGURED ROOT.
		//
		// Granting the root grants everything beneath it, and the root is an
		// operator-supplied path: TECTONIC_CACHE_DIR=/etc or a home directory
		// holding a *.fmt passes every startup check -- it IS a directory, it
		// DOES hold a format, and the compile preflight succeeds -- and then
		// hands client TeX /etc/passwd and any credential beside it (codex
		// round 5).
		//
		// Read-only in either case. The image pre-warms the format at build
		// time, so nothing at run time needs to write here, and a cache the
		// engine could write is a cache one job can poison for the next.
		ro = append(ro, tectonicCacheGrants(cache)...)
	}
	ro = dedupeExisting(append(ro, tectonicSystemReadOnly...))

	// STRUCTURAL BACKSTOP, and the reason it exists is that this exact defect
	// already arrived twice through two different doors.
	//
	// Landlock grants are path-beneath and additive, so ANY grant that is an
	// ancestor of the job directory hands out everything beside it -- every
	// other job's inputs and outputs. The first door was TMPDIR (round 1); the
	// second was TECTONIC_CACHE_DIR=/tmp, a legitimate pre-warm layout that
	// silently restored read access to every leftover job directory (round 3).
	// There is no reason to believe there is no third door, so the invariant is
	// enforced on the whole set rather than on the two names that have failed.
	//
	// Dropping rather than erroring, because this function has no error to
	// return and a missing READ grant fails loudly at job time, while a wrong
	// one fails silently and forever. ValidateTectonicCacheDir refuses the
	// configuration at startup so it should never come to this.
	ro = dropAncestorsOf(ro, rw)

	return sandbox.Paths{ReadWrite: rw, ReadOnly: ro}.SpecWith(sandbox.Limits{CPUSeconds: cpuSeconds})
}

// tectonicCacheGrants is the narrow half of the cache grant: the compiled
// FORMAT FILES themselves, rather than whatever root the operator configured.
//
// Files and not their directories, which is tighter than it first looks. A
// cache pointed at a home directory would still grant that home directory if
// the grant were per-directory, and the system-root backstop does not list
// every home a deployment might have. Per-file, the blast radius is exactly
// the formats: pointing the cache at $HOME grants the .fmt inside it and not
// the SSH keys beside it.
//
// Measured before adopting, because the engine could reasonably have needed to
// enumerate the directory: with only the format file granted, a compile
// returns exit 0 and a PDF, same as with the whole directory granted.
//
// Returning nothing is a legitimate answer and deliberately not backfilled
// with cacheDir: ValidateTectonicCacheDir refused a cache with no format at
// startup, so an empty result here means one disappeared underneath a running
// worker. The engine then fails loudly on the next job, which is the right
// direction -- backfilling the root would restore the very grant this exists
// to remove.
func tectonicCacheGrants(cacheDir string) []string {
	var formats []string
	_ = filepath.WalkDir(resolvePath(cacheDir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(formats) >= maxCacheGrants {
			return fs.SkipAll
		}
		if isFormatFile(d, path) {
			formats = append(formats, path)
		}
		return nil
	})
	return formats
}

// countFormats counts formats beneath dir, stopping at limit. Bounded because
// the answer is only ever compared against a cap.
func countFormats(dir string, limit int) (int, error) {
	n := 0
	err := filepath.WalkDir(resolvePath(dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if isFormatFile(d, path) {
			n++
			if n >= limit {
				return fs.SkipAll
			}
		}
		return nil
	})
	return n, err
}

// maxCacheGrants bounds how many rules one cache can add. A real cache holds a
// handful of formats -- this image holds exactly one -- and the cap exists so a
// misconfigured directory cannot turn every job's setup into thousands of
// add_rule calls.
const maxCacheGrants = 64

// isFormatFile is the one place that decides what counts as a format, so
// cacheHoldsFormat and tectonicCacheGrants cannot disagree about it -- a
// startup check that accepts a file the grant builder ignores would pass boot
// and then fail every job.
//
// A REGULAR FILE, and the name alone is not enough. WalkDir reports a symlink
// as a non-directory entry, and addPath opens what it is handed with O_PATH,
// which FOLLOWS symlinks -- so a cache holding "credential.fmt -> .ssh/id_rsa"
// beside a genuine format would pass validation, pass the preflight, and hand
// client TeX the private key through the alias (codex round 7). d.Type() comes
// from Lstat, so this decides on the entry itself and never on its target.
//
// A HARDLINK is deliberately not excluded, and the limit is stated rather than
// papered over: a hardlink named *.fmt IS a regular file, so one pointing at a
// secret inside the cache would be granted. Excluding it would mean refusing
// any format with st_nlink > 1, which some build systems produce legitimately
// (cp -l into place) -- and refusing valid deployments is the more expensive
// failure here, as this PR has demonstrated four times. The symlink case is
// different in kind: those exist in a home directory for unrelated reasons,
// which is exactly the scenario that raised it.
func isFormatFile(d fs.DirEntry, path string) bool {
	return d.Type().IsRegular() && isCompiledFormat(filepath.Base(path))
}

// isCompiledFormat is the NAME half of that decision.
//
// A SUFFIX, not a substring: ".fmt" anywhere in the name accepted
// README.fmt.txt, which is not a format and leaves the deployment in exactly
// the state these checks exist to refuse (codex round 3). The real artifact in
// the shipped image is formats/<hash>-latex-33.fmt; .fmt.gz is accepted
// because tectonic compresses formats on some builds.
func isCompiledFormat(name string) bool {
	return strings.HasSuffix(name, ".fmt") || strings.HasSuffix(name, ".fmt.gz")
}

// dropAncestorsOf removes any path that contains one of the writable
// directories, which a path-beneath grant would hand out along with it.
//
// Equality counts as containment: granting the job directory read-only
// alongside read-write is harmless, but granting a PARENT of it is the whole
// bug.
func dropAncestorsOf(paths, writable []string) []string {
	out := paths[:0:0]
	for _, p := range paths {
		if !containsAny(p, writable) {
			out = append(out, p)
		}
	}
	return out
}

func containsAny(parent string, children []string) bool {
	pc := resolvePath(parent)
	for _, child := range children {
		cc := resolvePath(child)
		if pc == cc {
			continue // the same directory, not an ancestor of it
		}
		if pc == "/" || strings.HasPrefix(cc, pc+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolvePath is what the KERNEL will see, not what the string says.
//
// Comparing cleaned strings is not enough, and the gap is not theoretical: a
// cache configured as /mnt/tmp-link/cache/.. where tmp-link points at /tmp
// cleans to /mnt/tmp-link, passes every lexical containment check, and then
// resolves to /tmp when os.Stat and Landlock's O_PATH open follow it -- which
// grants the whole job tree, which is the exact thing the check exists to
// refuse (codex round 4).
//
// filepath.Clean is the fallback for a path that cannot be resolved, so a
// non-existent path is still compared rather than silently passing.
func resolvePath(path string) string {
	// ABSOLUTE FIRST. filepath.EvalSymlinks preserves a relative path as
	// relative, so TECTONIC_CACHE_DIR=tmp resolved to "tmp" while the job root
	// resolved to "/tmp" -- the containment check compared two paths that
	// denote the same directory and saw no relation, and the kernel then
	// opened the relative one against the process's working directory and
	// granted the whole of /tmp. This image's runtime stage has no WORKDIR, so
	// that working directory is "/" and the two forms coincide exactly
	// (codex round 6, confirmed: `docker run --entrypoint sh ... -c pwd` = /).
	//
	// filepath.Abs uses this process's working directory, which is the same
	// one the helper inherits and the same one the kernel resolves against --
	// so this is the resolution the grant will actually get, not an
	// approximation of it.
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// dedupeExisting keeps order, drops repeats, and drops what is not there.
func dedupeExisting(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		// ABSOLUTE in the emitted spec, for the same reason resolvePath is:
		// the helper opens these tokens with O_PATH, and a relative one is
		// resolved against a working directory nothing here states. Emitting
		// the absolute form makes the argv say exactly what will be granted.
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if _, serr := os.Stat(abs); serr != nil {
			continue
		}
		out = append(out, abs)
	}
	return out
}

// tectonicArgs builds the argument vector, as its own function for the reason
// ps2writeArgs and ocrPageCountArgs are: the argv IS the security-relevant
// part, and a test can assert on it directly instead of reaching into a running
// process.
//
// Every flag here is either load-bearing or deliberately absent:
//
//	-X compile     the V2 CLI's one-shot compile.
//	--untrusted    TEX-04, and the whole of it.
//	-b <bundle>    TEX-02. Without it tectonic uses its default network bundle.
//	-o <dir>       output beside the input rather than in the process's cwd.
//	-p             print the engine's chatter. This is where a diagnosable
//	               failure message comes from, and 64-03's package-name
//	               extraction reads it -- removing it silently breaks a later
//	               requirement rather than this one.
//
// NOT passed, each for a stated reason:
//
//	-r/--reruns    tectonic reruns until convergence on its own. Pinning a
//	               count would replace "as many passes as this document needs"
//	               with a number, which is exactly what 64-05's measurement
//	               exists to characterise.
//	-k             nothing wants the .aux/.bcf litter.
//	--synctex      an editor feature; a server has no use for it.
//	-Z shell-escape  never, under any circumstances. --untrusted already makes
//	               it inert, and the two together mean a future edit that drops
//	               --untrusted still has to add this one deliberately.
func tectonicArgs(inPath, outDir, bundlePath string) []string {
	return []string{
		"-X", "compile", inPath,
		"--untrusted",
		"-b", bundlePath,
		"-o", outDir,
		"-p",
	}
}

// TeXOptsFromMap is the strict re-parse of a tex job's persisted options.
//
// This engine takes NONE, so any populated map is a corrupt column value, and
// D-10 classifies that terminal for every engine: garbage in jobs.options can
// never fix itself on retry. Mirrors SanitizeOptsFromMap exactly -- probe is
// the other engine that takes no options, and it is the newer precedent.
//
// It exists because of a review finding (PR #16, codex P2): TectonicDescriptor
// carried no OptsFromMap, so Runner.Handle skipped the read-side check entirely
// and Convert below ignored the map. A job whose persisted options said
// something -- an older producer, an operator insert, a corrupted row -- was
// marked DONE with those semantics silently discarded, while every other
// opts-guarded engine fails it as invalid_options.
//
// The API cannot currently produce such a row (a tex job with opts falls into
// handleCreateJob's default branch, which runs ParseDocOpts and
// ValidateApplicability and refuses). That is precisely why the worker-side
// check matters: it guards the paths the API is not on.
func TeXOptsFromMap(m map[string]any) (any, error) {
	if len(m) > 0 {
		return nil, fmt.Errorf("tex takes no options, got %d", len(m))
	}
	return nil, nil
}

// ValidateTectonicBundle reports whether path is something tectonic can
// actually load: a REGULAR file this process can OPEN.
//
// os.Stat is not that check, and the difference is not academic (PR #16, codex
// round 3, confirmed by running it): with TECTONIC_BUNDLE_PATH pointing at the
// directory /opt/tectonic, cmd/tectonic-worker started, logged its bundle, and
// began consuming the queue -- so a configuration mistake became a stream of
// client-visible job failures, each looking like a bad document, which is
// exactly what the startup check exists to prevent.
//
// Opening it is what proves readability UNDER THIS PROCESS's identity: the
// worker runs as nobody, and a mounted file readable by root and nobody else
// stats perfectly.
//
// Used by both the startup check and Convert, because one rule in two places is
// how they drift.
func ValidateTectonicBundle(path string) error {
	if path == "" {
		return fmt.Errorf("%w: no bundle configured (%s unset); refusing to run, because without -b "+
			"tectonic fetches over the network", ErrTeXBundleUnusable, TectonicBundleEnv)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: bundle %q cannot be opened: %v", ErrTeXBundleUnusable, path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: bundle %q cannot be stat'd: %v", ErrTeXBundleUnusable, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: bundle %q is not a regular file (mode %s)", ErrTeXBundleUnusable, path, info.Mode())
	}
	if info.Size() == 0 {
		return fmt.Errorf("%w: bundle %q is empty", ErrTeXBundleUnusable, path)
	}
	return nil
}

// TectonicCacheEnv names the directory holding the pre-compiled TeX format.
const TectonicCacheEnv = "TECTONIC_CACHE_DIR"

// ValidateTectonicCacheDir refuses a deployment whose engine would have nowhere
// to find its compiled format.
//
// It only matters once the engine is SANDBOXED, and it is entirely a
// consequence of that: unsandboxed, tectonic that finds no cache directory
// falls back to $HOME/.cache and compiles the format itself, slowly but
// successfully. Under Landlock $HOME is not granted, so the same fallback ends
// as
//
//	error: cannot write format file texput.fmt
//	caused by: Permission denied (os error 13)
//
// on EVERY job -- measured in the shipped image with the variable unset. That
// message names the sandbox and not the missing variable, so an operator would
// go looking in the wrong place. Checked at startup for the reason the bundle
// is: a worker that cannot typeset anything should say so once, rather than
// consume a queue and fail every task as though each document were at fault.
func ValidateTectonicCacheDir(path string) error {
	if path == "" {
		return fmt.Errorf("%w: no format cache configured (%s unset). The image sets this; "+
			"without it a sandboxed engine has nowhere to write its format and every job fails "+
			"with a permission error that names the sandbox rather than this variable",
			ErrTeXBundleUnusable, TectonicCacheEnv)
	}
	// RESOLVED ONCE. Every check below asks a question about the same path,
	// and resolvePath is two syscalls -- recomputing it per check was sixteen
	// resolutions in the denylist loop alone, and sixteen chances for two
	// checks to disagree about what they are looking at.
	//
	// THE PATH CHECKS RUN BEFORE THE STAT, deliberately. They are decisions
	// about the PATH, not about what is on disk, and ordering them after
	// os.Stat meant a system root that does not exist on the host got refused
	// for "cannot be read" instead -- a refusal that evaporates the moment the
	// directory appears. Found by this file's own test, which asserts the
	// REASON rather than the error.
	resolved := resolvePath(path)
	jobRoot := resolvePath(os.TempDir())

	// Contains where jobs are materialised: granting it recursively hands one
	// client every other client's files. TECTONIC_CACHE_DIR=/tmp is a real
	// pre-warm layout, and so is a relative "tmp" from a working directory of
	// "/", which is what this image has.
	if resolved == jobRoot || strings.HasPrefix(jobRoot, resolved+string(filepath.Separator)) {
		return fmt.Errorf("%w: format cache %q contains the directory jobs are materialised in (%s). "+
			"Landlock grants are recursive, so granting it would give every document read access to "+
			"every other job's files. Point it at a directory of its own",
			ErrTeXBundleUnusable, path, jobRoot)
	}
	// A BACKSTOP, and labelled as one. The guarantee is tectonicCacheGrants,
	// which grants only the directories actually holding formats -- so
	// TECTONIC_CACHE_DIR=/etc grants nothing unless a format sits inside it.
	// This refuses the case that narrowing cannot reach: a *.fmt placed
	// directly in a system root, which would make that root itself the
	// format-holding directory.
	//
	// A denylist is the wrong shape for a security boundary and this one is
	// not carrying that weight; it is here so an obviously wrong value fails
	// at startup with a sentence, rather than quietly granting.
	for _, root := range []string{
		"/", "/etc", "/usr", "/var", "/home", "/root", "/proc", "/sys",
		"/dev", "/run", "/boot", "/lib", "/lib64", "/bin", "/sbin", "/opt",
	} {
		if resolved == resolvePath(root) {
			return fmt.Errorf("%w: format cache %q is a system directory. Granting it read-only "+
				"would give every document read access to everything beneath it. Point it at a "+
				"directory that holds only the TeX format cache", ErrTeXBundleUnusable, path)
		}
	}
	if home := os.Getenv("HOME"); home != "" && home != "/" && resolved == resolvePath(home) {
		return fmt.Errorf("%w: format cache %q is the worker's home directory, which would be "+
			"granted read-only in full. Point it at a directory of its own",
			ErrTeXBundleUnusable, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: format cache %q cannot be read: %v", ErrTeXBundleUnusable, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: format cache %q is not a directory (mode %s)",
			ErrTeXBundleUnusable, path, info.Mode())
	}

	// EXISTING IS NOT ENOUGH -- it has to already HOLD a compiled format.
	//
	// An empty directory passes every check above and then fails every job,
	// and it is not a hypothetical: mounting an empty volume over the image's
	// pre-warmed cache is an ordinary thing to do, and it USED to work,
	// because a writable empty cache populates itself on the first run. The
	// sandbox grants this directory read-only, so it cannot (codex round 2).
	//
	// This is the same check Dockerfile.tectonic-worker runs at build time
	// after the pre-warm, deliberately: the image asserts the format is there
	// when it is created, and this asserts it is still there when the process
	// starts, which is where a mount can have replaced it.

	// EXISTING IS NOT ENOUGH -- it has to already HOLD a compiled format.
	//
	// An empty directory passes every check above and then fails every job,
	// and it is not a hypothetical: mounting an empty volume over the image's
	// pre-warmed cache is an ordinary thing to do, and it USED to work,
	// because a writable empty cache populates itself on the first run. The
	// sandbox grants this directory read-only, so it cannot (codex round 2).
	//
	// This is the same check Dockerfile.tectonic-worker runs at build time
	// after the pre-warm, deliberately: the image asserts the format is there
	// when it is created, and this asserts it is still there when the process
	// starts, which is where a mount can have replaced it.
	// A cache directory that CONTAINS where jobs are materialised grants read
	// access to all of them, because path-beneath rules are recursive.
	// TECTONIC_CACHE_DIR=/tmp is a real pre-warm layout and would do exactly
	// that (codex round 3). Refused here with the reason; the spec builder
	// drops it structurally as well, so a path this check did not anticipate
	// still cannot widen the ruleset.
	// RESOLVED, not cleaned: see resolvePath. A symlinked or ..-laden path that
	// lands on the job root would otherwise pass here and be granted anyway.
	// The per-job grant builder stops at maxCacheGrants. If a cache held more
	// formats than that, it would grant the first 64 and silently skip the
	// rest -- and a job needing a skipped one would fail with a permission
	// error naming the sandbox. Silent truncation reads as "covered
	// everything" when it did not, so the count is refused HERE instead, once,
	// with a sentence (own sweep after round 7).
	n, err := countFormats(path, maxCacheGrants+1)
	if err != nil {
		return fmt.Errorf("%w: format cache %q could not be scanned: %v", ErrTeXBundleUnusable, path, err)
	}
	if n > maxCacheGrants {
		return fmt.Errorf("%w: format cache %q holds more than %d compiled formats. The sandbox "+
			"grants them one rule each and stops at that cap, so some would be unreachable and "+
			"the jobs needing them would fail with a permission error naming the sandbox. Point "+
			"it at a directory holding only the formats this deployment uses",
			ErrTeXBundleUnusable, path, maxCacheGrants)
	}

	found, err := cacheHoldsFormat(path)
	if err != nil {
		return fmt.Errorf("%w: format cache %q could not be scanned: %v", ErrTeXBundleUnusable, path, err)
	}
	if !found {
		return fmt.Errorf("%w: format cache %q holds no compiled format (no regular *.fmt file -- "+
			"a SYMLINK named *.fmt does not count, because granting it would grant whatever it "+
			"points at). The sandbox "+
			"grants it READ-ONLY, so the engine cannot compile one -- every job would fail with a "+
			"permission error. An empty volume mounted over the image's pre-warmed cache is the "+
			"usual cause", ErrTeXBundleUnusable, path)
	}
	return nil
}

// cacheHoldsFormat reports whether a compiled TeX format is present anywhere
// beneath dir. Stops at the first hit; the cache is small and this runs once.
func cacheHoldsFormat(dir string) (bool, error) {
	found := false
	// The RESOLVED root. filepath.WalkDir Lstats its root, so a
	// TECTONIC_CACHE_DIR that is itself a symlink to the real cache -- an
	// ordinary versioned or mounted layout -- would be visited as the symlink
	// and walked no further, reporting an empty cache and refusing at startup
	// a deployment that worked before this PR (codex round 7).
	err := filepath.WalkDir(resolvePath(dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is not a reason to refuse the whole
			// cache: the format may be elsewhere in it.
			return nil
		}
		if isFormatFile(d, path) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}

// ErrTeXBundleUnusable is a DEPLOYMENT fault: the engine has no bundle it can
// load. Carried as a sentinel rather than recognised by a substring, which is
// what the worker used to do -- it looked for the env var's NAME in the error
// text, so only the "unset" wording matched. Every other way a bundle can be
// unusable (a directory, an unreadable mount, an empty file) produced a message
// without that name and was reported to the client as "the document could not
// be typeset" -- a deployment fault blamed on their file, which is the exact
// thing the test named "missing bundle ... does not blame the document" exists
// to prevent, and did not cover.
var ErrTeXBundleUnusable = errors.New("tex: the engine's bundle is not usable")

// texSourceClosure returns the document plus every file it pulls in AS SOURCE,
// transitively, resolved against the job's own directory.
//
// It follows the commands that make TeX read another file as source --
// \input, \include, \InputIfFileExists, \subfile -- and resolves each name
// the way TeX does: the name as written, then the name with .tex appended. It
// deliberately does NOT follow data-reading commands such as
// \lstinputlisting or \addbibresource: those files are rendered or parsed as
// data, and their contents are not directives.
//
// Bounded by the job itself: at most jobs.MaxInputParts files exist in the
// directory, and each is visited once.
func texSourceClosure(inPath string) []string {
	dir := filepath.Dir(inPath)
	seen := map[string]bool{inPath: true}
	out := []string{inPath}

	// BOUNDED BY FILES, NOT BY DIRECTIVES. The first version capped the names
	// it collected per file at 64, and a document can repeat one harmless
	// \input 64 times before the one that matters -- the repeats resolve to a
	// single file, so the cap was spent on nothing and the real preamble fell
	// out of the closure while TeX still read it (PR #20 review, P1).
	//
	// The directory is the bound that already exists: the API admits at most
	// jobs.MaxInputParts files, each is visited once, and a name that resolves
	// to a file already seen costs nothing.
	for i := 0; i < len(out) && len(out) <= maxClosureFiles; i++ {
		src, err := os.ReadFile(out[i])
		if err != nil {
			continue
		}
		for _, inc := range texIncludedNames(src) {
			// TeX reads \input{./preamble} as preamble in this directory.
			// The upload rule forbids a slash in a filename, not in the
			// document's own reference to a file that is already here.
			// A name that leaves the directory is not one of those files.
			local, ok := jobLocalSourceName(inc.name)
			if !ok {
				continue
			}
			inc.name = local
			// THE EXTENSIONS THAT DIRECTIVE WOULD TRY, not every extension.
			//
			// \usepackage{listings} loads listings.sty and never listings.tex,
			// so resolving every extension for every directive pulled an
			// attached DATA file named after a bundle package into the scan --
			// and refused a valid job over the directive that listing was
			// quoting. Measured; it is the same over-refusal this walk has now
			// produced three times, each time while closing a bypass.
			for _, cand := range inc.candidates() {
				p := filepath.Join(dir, filepath.FromSlash(cand))
				if !pathStaysIn(dir, p) {
					continue
				}
				// TeX opens the first file in this order and stops. A later
				// candidate that also exists, such as chapter.tex.tex beside
				// chapter.tex, is not read. An earlier file already in the
				// closure is that selection too, so the fallback stops there.
				if seen[p] {
					break
				}
				st, serr := os.Stat(p)
				if serr != nil || !st.Mode().IsRegular() {
					continue
				}
				seen[p] = true
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// maxClosureFiles bounds the walk itself. The API admits at most
// jobs.MaxInputParts files per job, so this can only bite on a directory this
// service did not create -- and biting means scanning fewer files, which is the
// direction that fails closed for the WORKER's other checks and open for this
// one. It is set above the API's own cap so a legitimate job never reaches it.
const maxClosureFiles = 64

// jobLocalSourceName cleans a TeX file reference to a path that stays in
// the job directory. \input{./preamble} is preamble. A reference that
// climbs out, or a backslash, is not a file this job can hold.
func jobLocalSourceName(name string) (string, bool) {
	if name == "" || strings.Contains(name, `\`) {
		return "", false
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", false
	}
	return cleaned, true
}

// pathStaysIn reports whether p is dir or a file inside it. Join can still
// walk out when a candidate contains "..".
func pathStaysIn(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// texIncludedNames returns the names a TeX source pulls in AS SOURCE.
//
// Parsed, not matched. A regexp over the raw bytes was wrong in both
// directions, and both were measured:
//
//	\input nobrace     TeX reads nobrace.tex -- the braceless form is the
//	                   PRIMITIVE's own syntax, and a brace-only pattern
//	                   missed it, so a preamble could hide there
//	% \input{old}      TeX reads nothing -- a commented-out include would
//	                   have pulled a file into the scan and could refuse a
//	                   job over a line that never executes
//
// So comments are skipped, and the command is matched as a whole CONTROL WORD:
// \inputencoding is not \input, which is the same lesson this package already
// paid for with \fi and \usepackage.
//
// Data-reading commands are deliberately absent: \lstinputlisting renders its
// file verbatim and \addbibresource parses one as bibliography data. Neither
// makes its target TeX source, and treating them as if they did refused a paper
// for quoting the directive it was explaining.
// texInclude is a name and the directive that named it. The directive decides
// which files TeX would actually try, and treating them alike was a defect in
// both directions: too few extensions missed an uploaded custom.sty, too many
// pulled in a data file named after a bundle package.
type texInclude struct {
	name string
	kind string
}

// candidates returns the filenames TeX would try for this directive, in its
// own order. Nothing else in the directory is a candidate.
func (i texInclude) candidates() []string {
	switch i.kind {
	case "usepackage", "RequirePackage":
		return []string{i.name + ".sty"}
	case "documentclass", "LoadClass":
		return []string{i.name + ".cls"}
	case "include":
		// LaTeX \include reads #1.tex and nothing else. An extensionless
		// file of the same name is not a candidate.
		return []string{i.name + ".tex"}
	default:
		// A name that already has a dot is opened as written. Appending
		// another extension made \input{chapter.tex} also open chapter.tex.tex.
		if strings.Contains(i.name, ".") {
			return []string{i.name}
		}
		return []string{i.name, i.name + ".tex", i.name + ".ltx"}
	}
}

func texIncludedNames(src []byte) []texInclude {
	text := string(src)
	var names []texInclude
	// A directive inside a macro body or a conditional may never execute.
	// VisitTeXPreamblePackages already abstains there; following the include
	// anyway pulled the target into the denial scan and refused a document
	// TeX does not read.
	var braceDepth, ifDepth int
	// nil means every \include is read. An empty map means \includeonly{}
	// has excluded all of them. \input is never filtered.
	var includeOnly map[string]bool

	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '%':
			// A comment runs to end of line. Escaped percents never reach
			// here: they are consumed as control symbols below.
			if nl := strings.IndexByte(text[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = len(text)
			}
			continue
		case '{':
			braceDepth++
			continue
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
			continue
		case '\\':
		default:
			continue
		}

		word, wordLen := texControlWord(text[i:])
		if len(word) == 1 && !isASCIILetter(word[0]) {
			// A control SYMBOL (\%, \\) is one token; consuming it here is
			// what keeps the comment branch above honest.
			i += wordLen - 1
			continue
		}
		if word == "verb" {
			i = skipVerbBody(text, i, wordLen)
			continue
		}
		if word == "lstinline" {
			i = skipLstinline(text, i, wordLen)
			continue
		}
		if word == "begin" {
			if next, ok := skipVerbatimEnv(text, i, wordLen); ok {
				i = next
				continue
			}
			// filecontents writes the body. An \input inside it names an
			// example file, which TeX does not read.
			if next, _ := skipFilecontentsBody(text, i, wordLen); next >= 0 {
				i = next
				continue
			}
		}
		switch {
		case word == "endinput":
			// TeX finishes this line and then stops reading the file. An
			// \input on the next line is not a source dependency.
			if braceDepth == 0 && ifDepth == 0 {
				text = text[:endinputLimit(text, i, wordLen)]
			}
			i += wordLen - 1
			continue
		case word == "end":
			// \end{document} runs the job terminator. Tokens after it, on
			// this line or a later one, are not read.
			if braceDepth == 0 && ifDepth == 0 {
				if _, ok := endedDocument(text, i, wordLen); ok {
					return names
				}
			}
			i += wordLen - 1
			continue
		case word == "fi":
			if ifDepth > 0 {
				ifDepth--
			}
			i += wordLen - 1
			continue
		case word == "newif":
			i += wordLen - 1
			for i+1 < len(text) && (text[i+1] == ' ' || text[i+1] == '\t') {
				i++
			}
			if strings.HasPrefix(text[i+1:], `\if`) {
				i++
				for i+1 < len(text) && isASCIILetter(text[i+1]) {
					i++
				}
			}
			continue
		case strings.HasPrefix(word, "if"):
			ifDepth++
			i += wordLen - 1
			continue
		case word == "includeonly":
			// The last top-level \includeonly replaces the set. It applies
			// only to \include commands after it; \input is unaffected.
			if braceDepth == 0 && ifDepth == 0 {
				if list, consumed, ok := parseIncludeOnly(text[i+wordLen:]); ok {
					includeOnly = list
					i += wordLen + consumed
					continue
				}
			}
			i += wordLen - 1
			continue
		}
		switch word {
		case "input", "include", "InputIfFileExists", "subfile":
			// Source inclusion, by name.
		case "usepackage", "RequirePackage", "documentclass", "LoadClass":
			// ALSO source, when the name resolves to an uploaded file:
			// \usepackage{custom} reads custom.sty from the job's own
			// directory before it looks anywhere else. The caller resolves the
			// extension; a name that matches nothing here is a bundle package.
		default:
			i += wordLen - 1
			continue
		}
		if braceDepth > 0 || ifDepth > 0 {
			i += wordLen - 1
			continue
		}

		rest := text[i+wordLen:]
		k := 0
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t') {
			k++
		}
		// \usepackage[draft]{custom} and \documentclass[11pt]{mine}. The
		// brackets are options. Reading them as the filename left the real
		// file out of the closure, and Tectonic still loaded it.
		if word == "usepackage" || word == "RequirePackage" || word == "documentclass" || word == "LoadClass" {
			after := skipTeXOptionGroup(rest[k:])
			k += len(rest[k:]) - len(after)
			for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t') {
				k++
			}
		}
		if k < len(rest) && rest[k] == '{' {
			if end := strings.IndexByte(rest[k:], '}'); end > 0 && end <= 256 {
				inner := rest[k+1 : k+end]
				// \usepackage{a,b} loads two packages. \input{safe,legacy}
				// reads one file: a comma is a legal character in the name.
				parts := []string{inner}
				if word == "usepackage" || word == "RequirePackage" {
					parts = strings.Split(inner, ",")
				}
				for _, one := range parts {
					if one = strings.TrimSpace(one); one != "" && includeKept(word, includeOnly, one) {
						names = append(names, texInclude{name: one, kind: word})
					}
				}
				i += wordLen + k + end
				continue
			}
			i += wordLen - 1
			continue
		}
		// The braceless primitive form: the name runs to whitespace.
		end := k
		for end < len(rest) && !isPDFSpace(rest[end]) && rest[end] != '\\' && rest[end] != '%' &&
			rest[end] != '{' && rest[end] != '}' {
			end++
		}
		if end > k && includeKept(word, includeOnly, rest[k:end]) {
			names = append(names, texInclude{name: rest[k:end], kind: word})
			i += wordLen + end - 1
			continue
		}
		i += wordLen - 1
	}
	return names
}

// parseIncludeOnly reads a braced \includeonly argument. The kernel removes
// spaces and then splits on commas. consumed is the index of the closing
// brace in rest. An empty group is a real list that selects nothing.
func parseIncludeOnly(rest string) (map[string]bool, int, bool) {
	k := 0
	for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t' || rest[k] == '\n') {
		k++
	}
	if k >= len(rest) || rest[k] != '{' {
		return nil, 0, false
	}
	end := strings.IndexByte(rest[k:], '}')
	if end < 1 {
		return nil, 0, false
	}
	list := map[string]bool{}
	for _, part := range strings.Split(rest[k+1:k+end], ",") {
		if part = includeOnlyKey(part); part != "" {
			list[part] = true
		}
	}
	return list, k + end, true
}

// includeKept reports whether an \include name survives the current
// \includeonly set. Every other directive is kept.
func includeKept(word string, list map[string]bool, name string) bool {
	if word != "include" || list == nil {
		return true
	}
	return list[includeOnlyKey(name)]
}

func includeOnlyKey(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		default:
			return r
		}
	}, name)
}

// endedDocument reports whether this \end opens the document environment.
// Spaces before the group are skipped, because \end takes an argument.
func endedDocument(text string, i, wordLen int) (int, bool) {
	rest := text[i+wordLen:]
	k := 0
	for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t' || rest[k] == '\n') {
		k++
	}
	if k >= len(rest) || rest[k] != '{' {
		return 0, false
	}
	end := strings.IndexByte(rest[k:], '}')
	if end <= 1 {
		return 0, false
	}
	if rest[k+1:k+end] != "document" {
		return 0, false
	}
	return i + wordLen + k + end, true
}

// skipVerbBody jumps to the closing delimiter of \verb or \verb*. The body is
// printed, not executed, so an \input inside it is not a source dependency.
func skipVerbBody(text string, i, wordLen int) int {
	j := i + wordLen
	if j < len(text) && text[j] == '*' {
		j++
	}
	if j >= len(text) {
		return len(text) - 1
	}
	delim := text[j]
	j++
	for j < len(text) && text[j] != delim && text[j] != '\n' {
		j++
	}
	if j >= len(text) {
		return len(text) - 1
	}
	return j
}

// skipLstinline jumps past the argument of \lstinline. listings prints it.
// The optional key list may contain brackets inside braces. The code is
// either a brace group or a one-character delimiter, as with \verb.
func skipLstinline(text string, i, wordLen int) int {
	j := skipScanSpace(text, i+wordLen)
	if j < len(text) && text[j] == '[' {
		end, ok := skipLstOptionGroup(text, j)
		if !ok {
			return end
		}
		j = skipScanSpace(text, end+1)
	}
	if j >= len(text) {
		if len(text) == 0 {
			return 0
		}
		return len(text) - 1
	}
	if text[j] == '{' {
		return skipBalancedBrace(text, j)
	}
	delim := text[j]
	j++
	for j < len(text) && text[j] != delim && text[j] != '\n' {
		j++
	}
	if j >= len(text) {
		return len(text) - 1
	}
	return j
}

func skipScanSpace(text string, j int) int {
	for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n') {
		j++
	}
	return j
}

func skipLstOptionGroup(text string, j int) (int, bool) {
	depth, brace := 0, 0
	for ; j < len(text); j++ {
		switch text[j] {
		case '{':
			brace++
		case '}':
			if brace > 0 {
				brace--
			}
		case '[':
			if brace == 0 {
				depth++
			}
		case ']':
			if brace == 0 {
				depth--
				if depth == 0 {
					return j, true
				}
			}
		}
	}
	if len(text) == 0 {
		return 0, false
	}
	return len(text) - 1, false
}

func skipBalancedBrace(text string, j int) int {
	depth := 0
	for ; j < len(text); j++ {
		switch text[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	if len(text) == 0 {
		return 0
	}
	return len(text) - 1
}

// skipVerbatimEnv jumps past a body that LaTeX prints or discards rather
// than executes: verbatim, verbatim*, lstlisting, and the comment
// environment from verbatim.sty. Commands inside are not a load. Brace
// depth does not hide them, because they sit at depth zero.
func skipVerbatimEnv(text string, i, wordLen int) (int, bool) {
	rest := text[i+wordLen:]
	k := 0
	for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t' || rest[k] == '\n') {
		k++
	}
	if k >= len(rest) || rest[k] != '{' {
		return 0, false
	}
	end := strings.IndexByte(rest[k:], '}')
	if end <= 1 {
		return 0, false
	}
	name := rest[k+1 : k+end]
	switch name {
	case "verbatim", "verbatim*", "lstlisting", "comment":
	default:
		return 0, false
	}
	start := i + wordLen + k + end + 1
	group := "{" + name + "}"
	for from := start; from < len(text); {
		rel := strings.Index(text[from:], `\end`)
		if rel < 0 {
			break
		}
		j := from + rel + len(`\end`)
		for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n') {
			j++
		}
		if strings.HasPrefix(text[j:], group) {
			return j + len(group) - 1, true
		}
		from = from + rel + 1
	}
	if len(text) == 0 {
		return 0, true
	}
	return len(text) - 1, true
}

// tectonicEnv is the ONLY environment the engine gets, and the reason is a
// measured leak rather than a precaution.
//
// --untrusted refuses \write18 -- TEX-04 proved that by running a hostile
// document -- and it does NOT confine file reads. Measured on the shipped
// image: a document containing
//
//	\newread\f \openin\f=/etc/hostname \read\f to \line ... \line
//
// typeset the container's hostname into the PDF the client gets back, and
// \openin against /proc/self/environ returned this process's environment. That
// environment holds the service credentials, so a .tex was a credential read.
//
// WHAT THIS FIXES AND WHAT IT DOES NOT. The engine can still read any file in
// its own container -- /etc/passwd, the bundle, its own binary. None of those
// are secret, and after this neither is the environment: the child is given an
// allowlist and nothing else. Confining the reads themselves needs a mount
// namespace per job and is recorded as a todo, not pretended here.
//
// The allowlist is what the engine actually uses. TECTONIC_CACHE_DIR is where
// the image puts the precompiled format; PATH, HOME and TMPDIR are the
// ordinary process minimum. Anything unset stays unset rather than being
// invented.
func tectonicEnv(jobDir string) []string {
	// TMPDIR is not inherited: it is SET, to the job's own directory.
	//
	// Inheriting it pointed the engine at the shared /tmp, and the sandbox had
	// to grant whatever it named -- which is a path-beneath grant on the whole
	// of /tmp, making the per-job grant beside it decorative. Every job could
	// then read and write every other job's directory, and could leave files
	// outside its own workDir that its deferred cleanup never removes. The
	// worker's job directory already lives under /tmp, so pointing TMPDIR at
	// it is strictly narrower and costs the engine nothing (codex, PR #21).
	keep := []string{"PATH", "HOME", "TECTONIC_CACHE_DIR", "SOURCE_DATE_EPOCH", "LANG", "LC_ALL"}
	env := make([]string, 0, len(keep)+1)
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if jobDir != "" {
		env = append(env, "TMPDIR="+jobDir)
	}
	return env
}

// tectonicLogLimit bounds how much of the engine's output is kept in memory.
//
// This engine is the only one here whose input is a PROGRAM, so it is the only
// one where the CLIENT decides how much the engine prints. Measured in the
// shipped image, 1 GiB / 2 CPU, --network none:
//
//	largest ADMISSIBLE document  2 143 444 B .tex  ->  1 416 019 B of log
//	235-byte \message loop       235 B .tex       ->  323 565 198 B in 30 s
//
// The bomb sustains 10.8 MB/s, which over the 271-second timeout is ~2.9 GB in
// a 1 GiB container: the worker dies before the deadline can classify the job.
// 8 MiB is 5.9x the largest legitimate log measured at the input ceiling and
// 0.8% of the container, so nothing real is ever truncated and the abusive case
// costs a fixed 8 MiB.
const tectonicLogLimit = 8 << 20

// Convert typesets inPath into outPath.
//
// opts is accepted and ignored here: the strict refusal lives in
// TeXOptsFromMap, which Runner.Handle applies BEFORE the job is marked active,
// so a corrupt map never reaches this function.
func (c TectonicConverter) Convert(ctx context.Context, inPath, outPath string, _ map[string]any) error {
	// Not a document failure, in any of its forms. A converter with no bundle
	// would fall back to tectonic's default, which is a network fetch -- so
	// this refuses rather than silently violating TEX-02.
	if err := ValidateTectonicBundle(c.BundlePath); err != nil {
		return fmt.Errorf("tectonic: %w", err)
	}

	// The deny list, enforced HERE and not only at the API. Every other refusal
	// in this function is the engine's own verdict read back, which is the
	// stronger form -- the worker cannot drift from the bundle because it IS
	// the bundle. A DENIED package breaks that symmetry: it is in the bundle,
	// tectonic loads it happily, and the engine therefore has no verdict to
	// read. Leaving it to the API alone meant a documented refusal that a
	// client could walk past by pushing the \usepackage beyond the sniff
	// window -- measured, not supposed: 1.2 MiB of comments ahead of
	// \usepackage{fontspec} returned a finished PDF.
	//
	// Reading the whole file is affordable precisely because the input ceiling
	// exists: 2 MiB, enforced before this ever runs.
	// EVERY .tex IN THE JOB'S DIRECTORY, not only the one being compiled.
	//
	// A multi-file project can put its preamble in a second file, so scanning
	// the main document alone would let \usepackage{fontspec} through an
	// \input -- and the API's own scan reads only the main part's sniff
	// window. Two gates that both look at one file out of several is one gate.
	// THE FILES THE DOCUMENT ACTUALLY PULLS IN AS SOURCE, not every text file.
	//
	// Two failures bound this, one on each side. Deciding by EXTENSION was a
	// bypass: \input{preamble} reads a file named exactly "preamble", and an
	// extensionless upload carrying \usepackage{fontspec} was read, loaded and
	// typeset. Deciding by "looks like text" was the opposite failure: a
	// project using \lstinputlisting{sample.txt} to render a source listing
	// VERBATIM would be refused because the listing quotes the directive, and
	// refusing valid work is the more expensive of the two (PR #20 review).
	//
	// So the rule is what TeX itself would do: start at the document and follow
	// its source-inclusion commands. A file nobody \inputs is data, whatever it
	// is called and whatever it contains.
	for _, path := range texSourceClosure(inPath) {
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		if pkg := TeXDeniedInSource(src); pkg != "" {
			return fmt.Errorf("tectonic: %w", TeXUnsupportedPackageError(pkg))
		}
	}

	outDir := filepath.Dir(outPath)
	name, args := "tectonic", tectonicArgs(inPath, outDir, c.BundlePath)
	// THE CPU BOUND DOES NOT DEPEND ON LANDLOCK, and until this it silently
	// did: the cpu: token was only added inside the sandbox branch, so a
	// deployment running with TECTONIC_SANDBOX_DISABLED=true logged an active
	// budget and enforced nothing (codex, PR #22). A bound that is configured
	// and not plumbed is a bound that does not exist -- this repository has
	// shipped that defect before and named it.
	//
	// So the helper runs whenever EITHER control is wanted. With no path
	// tokens it applies the limit and skips the ruleset; see
	// sandbox.RunExecMode.
	if len(c.SandboxHelper) > 0 {
		// syscall.Exec in the helper takes a PATH, not a name, and resolving
		// it HERE means a missing engine is reported as a missing engine
		// rather than as a sandbox that failed to establish.
		enginePath, lerr := exec.LookPath(name)
		if lerr != nil {
			return fmt.Errorf("tectonic: locate engine: %w", lerr)
		}
		// The limits always; the ruleset only when asked for. RunExecMode
		// reads the absence of path tokens as "no filesystem confinement
		// requested" and skips it, because an EMPTY ruleset denies everything
		// rather than nothing.
		spec := sandbox.Paths{}.SpecWith(sandbox.Limits{CPUSeconds: c.CPUSeconds})
		if c.ConfineFilesystem {
			spec = tectonicSandboxSpecLimited(inPath, outDir, c.BundlePath, enginePath, c.CPUSeconds)
		}

		name = c.SandboxHelper[0]
		args = append(append(append(
			append([]string{}, c.SandboxHelper[1:]...),
			spec...),
			"--", enginePath), args...)
	}
	out, cpu, err := runCommandLimitedEnvCPU(ctx, tectonicLogLimit, tectonicEnv(filepath.Dir(inPath)), name, args...)
	if err != nil {
		// The helper's "the engine never ran" status, before any of the
		// engine-shaped classification below: nothing in out is tectonic's
		// output, because tectonic was never execed.
		// THE TWO CPU BRANCHES, before anything that reads the engine's own
		// output -- because in both of them the engine was killed and its
		// output is whatever it had managed to print, which says nothing.
		if c.CPUSeconds > 0 {
			// Killed by the kernel for spending the budget.
			//
			// The kill arrives with the context still alive, which is also
			// the shape of the cgroup OOM kill -- cpuExhausted separates them
			// on CONSUMPTION, not on the signal, because both can be SIGKILL
			// (RLIMIT_CPU raises SIGXCPU first; measured, and the assumption
			// that soft == hard skips it was wrong).
			var xerr *exec.ExitError
			// The signal and the consumption together; see cpuExhausted for
			// what each rules out and why the tolerance is a fixed size rather
			// than a fraction of the budget.
			if ctx.Err() == nil && errors.As(err, &xerr) && cpu.Known {
				if st, ok := xerr.Sys().(syscall.WaitStatus); ok && st.Signaled() &&
					cpuExhausted(st.Signal(), cpu.Total(), c.CPUSeconds) {
					return fmt.Errorf("tectonic: %w after %.0fs of CPU: %s",
						ErrTeXCPUExhausted, cpu.Total().Seconds(), tectonicTail(out))
				}
			}
		}
		// The wall-clock deadline fired while the engine had barely run. That
		// is a statement about the NODE, not the document, and it must not be
		// reported as a document that failed to typeset.
		//
		// THE TEST IS THE SHARE OF A CPU THE CHILD GOT, not how its
		// consumption compares to the configured budget. Comparing against the
		// budget made the verdict depend on the relationship between two
		// INDEPENDENTLY configurable numbers: with CPUSeconds=600 and a 271s
		// deadline, a fully saturated runaway consumes ~271 CPU-seconds, which
		// is under half of 600, and would have been called starvation and
		// retried -- multiplying a runaway's cost across every attempt (codex,
		// PR #22).
		//
		// A ratio has no such dependency. Measured in the shipped image at a
		// 2-CPU quota: 1.00 idle, 0.28 with the node oversubscribed 4x. Half a
		// CPU is far from both, and a document that had a CPU to itself can
		// never be below it however the two settings are configured.
		if errors.Is(err, context.DeadlineExceeded) && classifyDeadline(cpu) {
			return fmt.Errorf("tectonic: %w: the deadline expired after %.1fs of CPU in %.1fs "+
				"of wall clock (%.2f of one CPU)", ErrTeXEngineStarved,
				cpu.Total().Seconds(), cpu.Elapsed.Seconds(), cpu.Share())
		}
		var setupErr *exec.ExitError
		if len(c.SandboxHelper) > 0 && errors.As(err, &setupErr) &&
			setupErr.ExitCode() == TectonicSandboxSetupExit {
			// err, not out. The helper reports on STDERR and
			// runCommandLimitedEnv returns only stdout in out, embedding
			// stderr in the error -- so formatting out here recorded an empty
			// string and lost whether ruleset creation, a path rule,
			// restrict_self or the exec itself failed. That is the entire
			// diagnostic an operator has for a node that cannot confine the
			// engine (codex, PR #21).
			return fmt.Errorf("tectonic: %w: %s", ErrTeXSandboxUnavailable, tectonicTail([]byte(err.Error())))
		}
		// THE ENGINE's deadline, named as such. The worker bounds the WHOLE
		// attempt with one context -- download, convert, upload -- because the
		// unique-lock TTL depends on the attempt never outliving it (T-03-11).
		// So context.DeadlineExceeded reaching the classifier does not mean
		// "this document did not finish": a stalled S3 or Postgres call
		// produces the identical error, and this engine treats a deadline as
		// TERMINAL. An infrastructure stall would have been reported to the
		// client as a document that failed to typeset, and never retried.
		//
		// Found in review (PR #16, codex round 2). Only a deadline that fired
		// while TECTONIC itself was running carries this sentinel; everything
		// else keeps the plain error and stays retryable.
		//
		// context.Canceled is deliberately not wrapped: a canceled job was
		// canceled by a client or a shutdown, and that has its own meaning.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("tectonic: %w: %v", ErrTeXEngineTimeout, err)
		}
		// The engine was KILLED while its context was still alive, which on
		// this deployment means one thing: the cgroup's OOM killer took it.
		//
		// Measured, and it is why capping the log above is only half the fix.
		// The 8 MiB cap bounds what THIS process retains; tectonic keeps its
		// own output files in memory, so a \message loop grows the CHILD.
		// Through the real worker at the chart's 1 GiB:
		//
		//	315 MiB -> 586 -> 818 -> 1006 (limit) -> killed -> 51 MiB -> climb
		//
		// The container survives (OOMKilled fires for the child, PID 1 lives),
		// and without this branch the failure is a plain non-zero exit: asynq
		// retries it, and the second attempt allocates exactly the same way.
		// That is the loop the review described, reached by a route the log cap
		// does not close.
		//
		// Terminal for the same reason the deadline is: concurrency is 1, the
		// cgroup is fixed, and the document is a program. A document that
		// exhausts this container exhausts it again.
		var exitErr *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exitErr) {
			if st, ok := exitErr.Sys().(syscall.WaitStatus); ok && st.Signaled() && st.Signal() == syscall.SIGKILL {
				return fmt.Errorf("tectonic: %w: %s", ErrTeXEngineKilled, tectonicTail(out))
			}
		}
		// The engine's own chatter is the only thing that says WHY. When it
		// names a .sty the bundle does not have, say WHICH -- that is the one
		// failure here the client can act on without guessing (TEX-03).
		if pkg := tectonicMissingPackage(out); pkg != "" {
			return fmt.Errorf("tectonic: %w: %s", TeXUnsupportedPackageError(pkg), tectonicTail(out))
		}
		if tectonicNeedsBiber(out) {
			// The engine tried to launch biber and there is none in the image,
			// by design (D-07). Named, because the client's remedy is one
			// package option and the raw error ("No such file or directory")
			// points at nothing they can see.
			return fmt.Errorf("tectonic: %w: %s", ErrTeXBiberUnavailable, tectonicTail(out))
		}
		return fmt.Errorf("tectonic: %w: %s", err, tectonicTail(out))
	}

	// tectonic writes <input basename>.pdf into -o, which is not necessarily
	// the name the worker asked for. Renaming rather than passing outPath
	// directly: -o takes a DIRECTORY, and pretending otherwise would put the
	// PDF somewhere the caller does not look.
	produced := filepath.Join(outDir, strings.TrimSuffix(filepath.Base(inPath), filepath.Ext(inPath))+".pdf")
	if produced != outPath {
		if err := os.Rename(produced, outPath); err != nil {
			return fmt.Errorf("tectonic: engine reported success but produced no %q: %w", produced, err)
		}
	}

	// Exit 0 is not the same as a correct document. LaTeX resolves references
	// across passes and, when it runs out of passes or a label simply does not
	// exist, it says so and carries on to produce a PDF full of "??".
	//
	// Publishing that silently is the failure this check removes: the client
	// asked for typesetting and got a document whose cross-references are
	// visibly broken, with a green job and no signal anywhere.
	//
	// The signal is LaTeX's OWN warning, not tectonic's rerun-cap message. That
	// distinction was measured, not assumed (PR #16 review, codex P2): 64-04's
	// end-to-end paper printed "TeX rerun seems needed, but stopping at 6
	// passes" and every one of its references resolved. Failing on the pass cap
	// would have failed the very document this phase ships as its proof.
	if tectonicUndefinedReferences(out) {
		return fmt.Errorf("tectonic: %w: %s", ErrTeXUndefinedReferences, tectonicTail(out))
	}

	// Exit 0 with no output is a green job handing the client nothing --
	// sanitizePDF's own intermediate check exists because gs does exactly that
	// on some inputs, and a TeX engine that typesets an empty document is the
	// same trap.
	info, err := os.Stat(outPath)
	if err != nil {
		return fmt.Errorf("tectonic: no output at %q: %w", outPath, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("tectonic: engine exited 0 and produced an empty PDF")
	}
	// ... and the other end of the same question, which had no answer at all
	// until round 2 of PR #16's review asked it. The INPUT ceiling is 2 MiB and
	// bounds what the client is charged for; it does not bound what they get
	// back, because TeX ships pages out and frees the memory as it goes:
	//
	//	198 bytes of .tex  ->  6 040 163 bytes of PDF in 16 seconds
	//
	// measured in the shipped image, and 19 545 890 bytes in 93 seconds through
	// the live API -- both well inside the 271-second timeout. Against that, the
	// largest LEGITIMATE output measured -- the 2 MiB, 745-page document 64-05's
	// timeout is derived from -- is 1 914 057 bytes.
	//
	// Refused here rather than in the worker's upload path so the bytes never
	// reach S3 and the failure names the reason.
	if max := c.maxOutputBytes(); info.Size() > max {
		return fmt.Errorf("tectonic: %w: %d bytes against a %d-byte ceiling",
			ErrTeXOutputTooLarge, info.Size(), max)
	}
	return nil
}

// maxOutputBytes resolves the configured ceiling, defaulting rather than
// failing open: a zero value here is the registry's zero-value converter
// (converters.go registers one at init), and treating that as "no ceiling"
// would put the bound back where it was -- absent.
func (c TectonicConverter) maxOutputBytes() int64 {
	if c.MaxOutputBytes > 0 {
		return c.MaxOutputBytes
	}
	return DefaultTectonicMaxOutputBytes
}

// ErrTeXEngineTimeout is the engine's own deadline: tectonic was running and
// was killed when the attempt's context expired. Distinct from a bare
// context.DeadlineExceeded, which the same attempt context also produces for a
// stalled download or upload -- and those are worth retrying while this is not.
var ErrTeXEngineTimeout = errors.New("tex: the engine did not finish within the time limit")

// ErrTeXEngineKilled is the engine killed by a signal with time still on the
// clock -- in this deployment, the container's OOM killer. The document decides
// how much memory the engine allocates, so this is a property of the document.
var ErrTeXEngineKilled = errors.New("tex: the engine was killed before it finished")

// ErrTeXOutputTooLarge is a document that typeset successfully into a PDF too
// big to accept. Terminal: rerunning the same source produces the same size.
var ErrTeXOutputTooLarge = errors.New("tex: the produced PDF exceeds this deployment's size ceiling")

// DefaultTectonicMaxOutputBytes is the ceiling on the produced PDF.
//
// 32 MiB, and both ends of that are measured rather than picked:
//
//	largest legitimate output  1 914 057 B  (2 MiB source, 745 pages, 7 passes)
//	tiny \newpage loop         6 040 163 B  in 16s from 198 B of source
//	the same shape via the API 19 545 890 B in 93s from 199 B of source
//
// The last line is 10x the largest real document, from an upload the client is
// charged 199 bytes for, in a third of the time available. It is measured
// rather than extrapolated on purpose: page-loop runs are NOT linear under this
// container's memory pressure -- a 240 000-iteration variant produced FEWER
// pages and fewer bytes than a 130 000 one -- so any rate projected out to the
// timeout would be a number nobody measured. 32 MiB is 17x the largest real
// document and caps the abusive case at something a worker's disk and the
// bucket can hold.
//
// A ceiling, not a quota: repeated jobs still add up, which is what the
// per-client quota gate is for.
const DefaultTectonicMaxOutputBytes int64 = 32 << 20

// ErrTeXBiberUnavailable is the document asking for a program this image does
// not ship. biblatex's DEFAULT backend is biber, and no biber binary is in the
// image at all -- a security decision with its own argument (D-07,
// docs/TYPESETTING.md), not an omission. Found in review: the declared set
// promises biblatex, and an ordinary \usepackage{biblatex} therefore failed
// with "Running external tool biber ... No such file or directory", which tells
// the client nothing actionable.
var ErrTeXBiberUnavailable = errors.New("tex: biblatex requires backend=bibtex on this deployment")

// ErrTeXUndefinedReferences is a document that typeset successfully into a PDF
// whose \ref/\cite markers did not resolve. The engine exits 0 for this.
var ErrTeXUndefinedReferences = errors.New("tex: the document has undefined references")

// tectonicUndefinedReferences reports whether the FINAL pass still had
// unresolved \ref/\cite markers.
//
// THE PASS SCOPE IS THE WHOLE POINT. LaTeX resolves references through the
// .aux file, which does not exist on the first pass, so "There were undefined
// references." is emitted by EVERY multi-pass document on its early passes and
// means nothing there. Checking the whole log made the first version of this
// function fail 64-04's end-to-end paper and the measurement corpus's own
// bibliography documents -- both of which resolve everything by the last pass.
// Caught by running those two through the live API immediately after writing
// the check, which is the only reason it is not in this PR.
//
// Passes are delimited by the engine's own per-invocation "Transcript written"
// line -- the same marker scripts/tectonic-timeout-measure.sh counts -- so the
// last segment is the last pass. If the marker is ever absent the function
// falls back to the whole output, which is the conservative direction for a
// check whose false NEGATIVE merely restores today's behaviour.
func tectonicUndefinedReferences(out []byte) bool {
	const passMarker = "Transcript written"
	last := out
	if i := bytes.LastIndex(out, []byte(passMarker)); i >= 0 {
		// Everything after the second-to-last marker is the final pass; the
		// warning is printed BEFORE the transcript line of the pass it belongs
		// to, so the window starts at the previous marker.
		if j := bytes.LastIndex(out[:i], []byte(passMarker)); j >= 0 {
			last = out[j:]
		}
	}
	// LaTeX's OWN wording, at the start of a line, and not merely the phrase
	// anywhere in the output. The input is a program: \typeout{There were
	// undefined references} makes any substring test true, and the cost of
	// believing it is a VALID PDF thrown away. Captured from both runs rather
	// than assumed (testdata/tectonic/{undefined,forged-undefined}-references.txt):
	//
	//	real    LaTeX Warning: There were undefined references.
	//	forged  There were undefined references
	//
	// A document can of course print the prefixed form too -- TeX can print
	// anything, and no reading of stdout fixes that. The asymmetry is what
	// makes this worth doing: forging the full warning fails the FORGER's own
	// job and nobody else's, while an unprefixed substring match fails an
	// innocent document that merely quotes the phrase.
	for _, line := range bytes.Split(last, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("LaTeX Warning: There were undefined ")) {
			return true
		}
	}
	return false
}

// tectonicNeedsBiber reports whether the failure was the engine trying to run
// biber. Matched on the engine's own phrasing, captured from a real run:
//
//	note: Running external tool biber ...
//	error: No such file or directory (os error 2)
func tectonicNeedsBiber(out []byte) bool {
	return bytes.Contains(out, []byte("Running external tool biber"))
}

// tectonicTail returns the last few lines of the engine's output, which is
// where its error lines are. The whole log is unbounded and attacker-influenced;
// an error message is not the place to put it.
func tectonicTail(out []byte) string {
	const (
		keep = 12
		// A BYTE bound as well as a line bound. Twelve lines is not a bound on
		// its own: TeX wraps terminal output at max_print_line, but this string
		// is written into job_events.detail for every failure, and "the engine
		// probably wraps" is not the kind of thing a database row should rest
		// on. Found in this PR's own self-check after round 2, which was about
		// exactly this: what the log buffer holds was bounded, what gets STORED
		// from it was not.
		maxBytes = 4096
	)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	tail := strings.Join(lines, " | ")
	if len(tail) > maxBytes {
		// Keep the END: the cause is at the end of a TeX log, and this is the
		// same reason runCommandLimited keeps a tail rather than a head.
		tail = "..." + tail[len(tail)-maxBytes:]
	}
	return tail
}

// missingStyLine matches LaTeX's own report of a file it could not resolve.
// Written against captured output, never predicted:
// testdata/tectonic/missing-package.txt, from tectonic 0.17.0 in the built
// image on 2026-09-04.
//
//	! ! LaTeX Error: File `pgfplots.sty\' not found..
//
// The backtick-quote pair around the filename is LaTeX's, not a shell's.
var missingStyLine = regexp.MustCompile("File `([A-Za-z0-9_.-]+)\\.sty' not found")

// tectonicMissingPackage returns the package name the engine could not
// resolve, or "" when the output does not permit naming one.
//
// ONLY .sty, AND THAT IS THE POINT. LaTeX reports every unresolvable file the
// same way, and most of them are not packages a client can remove: a missing
// .def, .cfg, .fd or .tex is a fault in the BUNDLE or in a package's own
// dependencies, and telling a client "remove \usepackage{ts1cmr}" for a
// missing font-definition file would send them deleting a line that does not
// exist in their document.
//
// The plan for this task allowed for the possibility that the output would not
// permit naming the package reliably, and said the honest answer would then be
// the generic engine error. That is what "" means here, and it is returned for
// every non-.sty failure and for the undefined-control-sequence capture next to
// it in testdata -- a confidently wrong package name is worse than none,
// because the client removes the wrong line and the job fails again.
//
// FIRST match, not last: tectonic prints the failure once as LaTeX's `!` line
// and again in its own `error:` summary line, and both name the same file. The
// first is where the compilation actually stopped.
func tectonicMissingPackage(out []byte) string {
	m := missingStyLine.FindSubmatch(out)
	if m == nil {
		return ""
	}
	return string(m[1])
}
