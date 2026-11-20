package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The sandbox spec IS the security claim, so it is asserted directly rather
// than through a running engine -- the same argument tectonicArgs is tested
// under. A kernel is not needed to see that /etc was granted.

// TestSandboxSpecGrantsTheJobDirectoryAndNothingElseWritable is the property
// that matters most: an engine whose input is a PROGRAM must not be able to
// write outside the directory holding its own job.
func TestSandboxSpecGrantsTheJobDirectoryAndNothingElseWritable(t *testing.T) {
	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine)

	var rw []string
	for _, tok := range spec {
		if strings.HasPrefix(tok, "rw:") {
			rw = append(rw, strings.TrimPrefix(tok, "rw:"))
		}
	}
	if len(rw) == 0 {
		t.Fatalf("no writable path at all; the engine could not write its PDF.\nspec = %v", spec)
	}
	// /dev/null is the one other writable entry, because a program that cannot
	// open it fails in ways that have nothing to do with typesetting. Anything
	// BEYOND these is a widening nobody asked for, and adding one has to be an
	// edit to this list.
	//
	// os.TempDir() is deliberately NOT here. It was, and that was the defect:
	// Landlock grants are path-beneath and additive, so granting the shared
	// /tmp granted every other job's directory underneath it and made the
	// jobDir entry decorative. The engine's TMPDIR now points AT the job
	// directory instead (codex, PR #21).
	allowed := map[string]bool{jobDir: true, "/dev/null": true}
	for _, p := range rw {
		if !allowed[p] {
			t.Fatalf("spec grants WRITE to %q, which is neither the job directory (%q) nor TMPDIR (%q).\n"+
				"An engine that runs client-authored programs must not be able to write there.\nspec = %v",
				p, jobDir, os.TempDir(), spec)
		}
	}
}

// TestSandboxSpecDoesNotGrantEtc pins the read that started this work.
//
// PR #19 measured \openin=/etc/hostname typesetting the container's hostname
// into a client's PDF. /etc/ssl/certs is the one exception, and it is named
// explicitly so that widening the exception to /etc has to be a deliberate
// edit to this test.
func TestSandboxSpecDoesNotGrantEtc(t *testing.T) {
	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		path := tok[3:]
		if path == "/etc" || path == "/" || path == "/proc" || path == "/run" {
			t.Fatalf("spec grants %q. Landlock rules are PATH BENEATH: granting %q reaches every "+
				"secret, every mounted credential and /proc/self/environ -- the exact reads this "+
				"sandbox exists to remove.", tok, path)
		}
	}
}

// TestSandboxSpecSkipsPathsThatDoNotExist is the other direction, and it is
// about availability rather than confinement: sandbox.Apply treats a path it
// cannot open as an ERROR, so a spec naming /lib64 on an arm64 image would
// refuse every job on that image.
func TestSandboxSpecSkipsPathsThatDoNotExist(t *testing.T) {
	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		if _, err := os.Stat(tok[3:]); err != nil {
			t.Fatalf("spec names %q, which does not exist here: %v.\n"+
				"sandbox.Apply refuses a rule it cannot open, so this spec would fail EVERY job "+
				"on a host with this layout rather than confining one.", tok, err)
		}
	}
}

// TestSandboxSpecCarriesTheBundleReadOnly: the bundle must be reachable (the
// engine reads every .sty out of it) and must not be writable (one job must
// not be able to edit what the next job typesets with).
func TestSandboxSpecCarriesTheBundleReadOnly(t *testing.T) {
	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine)
	if !slices.Contains(spec, "ro:"+bundle) {
		t.Fatalf("bundle %q is not readable in the spec; the engine would find no packages at all.\nspec = %v", bundle, spec)
	}
	if slices.Contains(spec, "rw:"+bundle) {
		t.Fatalf("bundle %q is WRITABLE in the spec: one client's document could rewrite the "+
			"package set the next client is typeset with.\nspec = %v", bundle, spec)
	}
}

// TestSandboxSetupExitIsNotTheEngineFailureCode holds the contract that keeps
// a broken node from being reported as a broken document.
//
// tectonic exits 1 when a document fails to compile. If the helper shared that
// status, Convert could not tell "the confinement could not be established"
// from "this LaTeX is wrong", and the first would be classified terminal and
// never retried.
func TestSandboxSetupExitIsNotTheEngineFailureCode(t *testing.T) {
	if TectonicSandboxSetupExit == 1 || TectonicSandboxSetupExit == 0 {
		t.Fatalf("TectonicSandboxSetupExit = %d, which collides with the engine's own exit statuses; "+
			"an infrastructure fault would be reported to the client as a document that failed to typeset",
			TectonicSandboxSetupExit)
	}
}

// TestSandboxSpecDoesNotGrantTheSharedTempDir states the regression as its own
// case, because the one above would keep passing if jobDir ever happened to BE
// the shared temporary directory.
func TestSandboxSpecDoesNotGrantTheSharedTempDir(t *testing.T) {
	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	shared := strings.TrimSuffix(os.TempDir(), "/")
	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		if strings.TrimSuffix(tok[3:], "/") == shared {
			t.Fatalf("spec grants %q, the SHARED temporary directory. Landlock grants are recursive: "+
				"this reaches every other job's inputs and outputs, and lets a document write files "+
				"outside the workDir the worker cleans up.", tok)
		}
	}
}

// TestSandboxSetupFailureKeepsTheReason drives Convert through a helper that
// fails the way a real node does, and asserts the operator can still tell WHY.
//
// The defect it pins: runCommandLimitedEnv returns only STDOUT in out and
// embeds stderr in the error, while the helper reports on stderr. Formatting
// out here recorded an empty string, so "the sandbox could not be established"
// arrived with no indication whether ruleset creation, a path rule,
// restrict_self or the exec had failed -- on the one error class whose cause is
// a property of the machine and cannot be reproduced from the job (codex, #21).
func TestSandboxSetupFailureKeepsTheReason(t *testing.T) {
	dir := t.TempDir()

	// A stand-in engine, so exec.LookPath resolves on any host: Convert
	// locates it before invoking the helper, and never runs it here.
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tectonic"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	const reason = "landlock: restrict self: operation not permitted"
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\necho '__sandbox-exec: " + reason + "' >&2\nexit " +
		fmt.Sprint(TectonicSandboxSetupExit) + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(dir, "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("not a real bundle, never opened"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.tex")
	if err := os.WriteFile(in, []byte("\\documentclass{article}\\begin{document}x\\end{document}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := TectonicConverter{BundlePath: bundle, SandboxHelper: []string{helper}}
	err := c.Convert(context.Background(), in, filepath.Join(dir, "out.pdf"), nil)

	if err == nil {
		t.Fatal("a helper exiting with the setup status produced no error")
	}
	if !errors.Is(err, ErrTeXSandboxUnavailable) {
		t.Fatalf("error is not classified as a sandbox fault, so the job would be treated as a bad "+
			"document and never retried: %v", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("the reason the sandbox could not be established is missing from the error.\n"+
			"An operator sees only that it failed, not whether it was the ruleset, a path rule or "+
			"the exec -- and this is the one failure here that cannot be reproduced from the job.\n"+
			"want it to contain: %q\ngot: %v", reason, err)
	}
}

// TestCacheDirValidationCatchesWhatTheSandboxMadeFatal covers a failure mode
// this sandbox INTRODUCED.
//
// Unsandboxed, a tectonic that finds no TECTONIC_CACHE_DIR falls back to
// $HOME/.cache and compiles its own format -- slow, but it works. Under
// Landlock $HOME is not granted, so the same fallback ends as "cannot write
// format file texput.fmt: Permission denied" on every job, measured in the
// shipped image. The message names the sandbox and not the unset variable,
// which is why this is a startup check and not a comment.
func TestCacheDirValidationCatchesWhatTheSandboxMadeFatal(t *testing.T) {
	if err := ValidateTectonicCacheDir(""); err == nil {
		t.Fatal("an unset cache directory was accepted; every job would fail with a permission " +
			"error that points at the sandbox rather than at the missing variable")
	}
	if err := ValidateTectonicCacheDir("/nonexistent/cache/dir"); err == nil {
		t.Fatal("a cache directory that does not exist was accepted")
	}

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTectonicCacheDir(file); err == nil {
		t.Fatal("a regular file was accepted as the format cache")
	}

	// EXISTING IS NOT ENOUGH. An empty directory used to be accepted here, and
	// codex round 2 named the case: an empty volume mounted over the image's
	// pre-warmed cache. It passes every structural check and then fails every
	// job, because the sandbox grants it read-only and the engine cannot
	// compile the format it is missing. Before confinement that same empty
	// cache populated itself on the first run.
	if err := ValidateTectonicCacheDir(t.TempDir()); err == nil {
		t.Fatal("an EMPTY cache directory was accepted. Read-only, the engine cannot populate it, " +
			"so every job fails with a permission error -- and an empty mounted volume is the " +
			"ordinary way to reach this")
	}

	// ...and a directory that actually holds a format is accepted, including
	// one nested the way tectonic lays its cache out.
	warm := t.TempDir()
	if err := os.MkdirAll(filepath.Join(warm, "formats", "x86"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(warm, "formats", "x86", "latex.fmt.gz"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTectonicCacheDir(warm); err != nil {
		t.Fatalf("a cache holding a compiled format was refused: %v", err)
	}

	// Classified as a deployment fault, so the client is not told their
	// document is at fault -- the same sentinel the bundle checks use.
	if !errors.Is(ValidateTectonicCacheDir(""), ErrTeXBundleUnusable) {
		t.Fatal("the cache fault is not classified as a deployment fault, so it would reach the " +
			"client as \"the document could not be typeset\"")
	}
}

// TestSandboxSpecGrantsTheResolvedEngine covers an installation this would
// otherwise break.
//
// tectonicSystemReadOnly names /usr, /lib and /bin, which is where this image
// puts tectonic -- but exec.LookPath answers from PATH, and an image that
// installs it in /opt or a Nix store resolves outside all three. The helper
// would apply the ruleset and then take EACCES from execve, so a deployment
// that worked before confinement would stop working, with an error that says
// nothing about PATH (codex round 2).
func TestOutsideFixtureIsNotUnderTheJobRuleset(t *testing.T) {
	job := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := tectonicSandboxSpecLimited(filepath.Join(job, "in.tex"), job, bundle, engine, 271)
	outside, err := os.MkdirTemp("/var/tmp", "octoconv-tectonic-outside-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	if err := outsideIsUngranted(outside, spec); err != nil {
		t.Fatal(err)
	}
	if err := outsideIsUngranted(job, spec); err == nil {
		t.Fatal("the job directory was treated as outside the grants")
	}
}

func TestSandboxSpecGrantsTheResolvedEngine(t *testing.T) {
	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Deliberately outside every entry in tectonicSystemReadOnly.
	elsewhere := t.TempDir()
	engine := filepath.Join(elsewhere, "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine)
	if !slices.Contains(spec, "ro:"+engine) {
		t.Fatalf("the resolved engine %q is not in the ruleset, so execve would return EACCES "+
			"after the sandbox is applied -- and the failure would name the sandbox, not PATH.\nspec = %v",
			engine, spec)
	}
	if slices.Contains(spec, "rw:"+engine) {
		t.Fatalf("the engine binary is WRITABLE: a document could rewrite the program that typesets "+
			"the next one.\nspec = %v", spec)
	}
}

// TestSandboxSpecNeverGrantsAnAncestorOfTheJobDirectory is the invariant the
// same defect reached twice through two different doors.
//
// Round 1: TMPDIR pointed at the shared /tmp, so the spec granted /tmp.
// Round 3: TECTONIC_CACHE_DIR=/tmp is a legitimate pre-warm layout that
// granted it again, read-only, restoring read access to every leftover job
// directory beside it. Landlock grants are path-beneath: an ancestor hands out
// everything under it.
//
// Stated as a property of the WHOLE spec rather than of the two variables that
// have failed, because there is no reason to believe there is no third door.
func TestSandboxSpecNeverGrantsAnAncestorOfTheJobDirectory(t *testing.T) {
	parent := t.TempDir()
	jobDir := filepath.Join(parent, "job-1")
	sibling := filepath.Join(parent, "job-2")
	for _, d := range []string{jobDir, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The cache pointed at the job directories' PARENT -- codex round 3's case.
	t.Setenv("TECTONIC_CACHE_DIR", parent)

	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		granted := filepath.Clean(tok[3:])
		if granted == filepath.Clean(jobDir) {
			continue
		}
		if strings.HasPrefix(filepath.Clean(sibling), granted+string(filepath.Separator)) {
			t.Fatalf("spec grants %q, an ancestor of another job's directory (%s). Path-beneath "+
				"grants are recursive, so this hands one client read access to every other "+
				"client's inputs and outputs.", tok, sibling)
		}
	}
}

// TestCacheDirIsRefusedWhenItSwallowsTheJobRoot is the startup half: the spec
// drops such a grant structurally, but silently, and a cache that was dropped
// means every job fails with an error about formats. The operator should be
// told at startup what is actually wrong.
func TestCacheDirIsRefusedWhenItSwallowsTheJobRoot(t *testing.T) {
	// The REASON is asserted, not merely that an error came back. Inverting
	// the guard the first time still produced an error here -- the shared
	// temporary directory also happens to hold no *.fmt -- so the test passed
	// while the property it names was gone. A refusal for the wrong reason is
	// not evidence.
	const want = "contains the directory jobs are materialised in"

	// "/" is deliberately NOT in this loop: round 5 added a system-root
	// refusal that runs first and catches it, and TestSystemRootsAreRefused
	// owns that case. Leaving it here would assert which of two correct
	// refusals fires first, which is not a property worth freezing.
	//
	// os.TempDir() stays, because it is the case ONLY containment catches.
	for _, dir := range []string{os.TempDir()} {
		err := ValidateTectonicCacheDir(dir)
		if err == nil {
			t.Fatalf("%q was accepted as the format cache. Jobs are materialised under it, so "+
				"granting it read-only gives every document read access to every other job's files.", dir)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q was refused, but for the wrong reason -- the containment check did not "+
				"fire, and a cache that DOES hold a format would sail through.\nwant the message "+
				"to contain %q\ngot: %v", dir, want, err)
		}
	}

	// The case the wrong-reason pass was hiding: an ancestor of the job root
	// that IS properly pre-warmed. Only the containment check can refuse this.
	warm := os.TempDir()
	if err := os.WriteFile(filepath.Join(warm, "octoconv-test-latex-33.fmt"), []byte("f"), 0o600); err != nil {
		t.Skipf("cannot write a probe format into %s: %v", warm, err)
	}
	defer os.Remove(filepath.Join(warm, "octoconv-test-latex-33.fmt"))

	err := ValidateTectonicCacheDir(warm)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a PRE-WARMED cache that contains the job root was accepted (%v). This is the "+
			"configuration codex round 3 named: it passes every format check and grants read "+
			"access to every job directory beside it.", err)
	}
}

// TestCacheFormatMatchIsASuffix: ".fmt" anywhere in the name accepted
// README.fmt.txt, which is not a format, leaving the deployment in exactly the
// state the check exists to refuse (codex round 3).
func TestCacheFormatMatchIsASuffix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.fmt.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTectonicCacheDir(dir); err == nil {
		t.Fatal("a cache holding only README.fmt.txt was accepted as pre-warmed; the engine would " +
			"still have no format and, read-only, could not build one")
	}

	if err := os.WriteFile(filepath.Join(dir, "abc-latex-33.fmt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTectonicCacheDir(dir); err != nil {
		t.Fatalf("a real format file was not recognised: %v", err)
	}
}

// TestCacheContainmentSeesThroughSymlinks: the containment check was purely
// lexical, and a path that CLEANS somewhere harmless can RESOLVE onto the job
// root. os.Stat and Landlock's O_PATH open both follow the alias, so the grant
// lands on the resolved directory while the check inspected the string
// (codex round 4).
func TestCacheContainmentSeesThroughSymlinks(t *testing.T) {
	root := t.TempDir()

	// A cache that is a legitimate directory, reached through a symlink and a
	// ".." -- the shape of a mounted cache on a host with a linked /tmp.
	jobRootLink := filepath.Join(root, "tmp-link")
	if err := os.Symlink(os.TempDir(), jobRootLink); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	alias := filepath.Join(jobRootLink, "cache", "..")

	// Resolved INDEPENDENTLY of the code under test. The first version of this
	// test asked resolvePath whether the alias resolves onto the job root --
	// so disabling resolvePath made the test SKIP rather than fail, and the
	// inversion passed against code whose guard was gone. A check that
	// validates itself proves nothing.
	realAlias, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Skipf("cannot resolve %q here: %v", alias, err)
	}
	realTemp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Skipf("cannot resolve %q here: %v", os.TempDir(), err)
	}
	if filepath.Clean(realAlias) != filepath.Clean(realTemp) {
		t.Skipf("this platform does not resolve %q onto %q (got %q)", alias, os.TempDir(), realAlias)
	}

	if err := ValidateTectonicCacheDir(alias); err == nil ||
		!strings.Contains(err.Error(), "contains the directory jobs are materialised in") {
		t.Fatalf("a cache path that RESOLVES onto the job root was accepted (%v).\n"+
			"filepath.Clean says %q, which contains nothing; the kernel says %q, which contains "+
			"every job directory -- and the kernel is the one that grants.",
			err, filepath.Clean(alias), realAlias)
	}
}

// TestSandboxSpecDropsAliasedAncestors is the structural backstop with the
// same weakness, checked the same way: the spec must not grant a path that
// resolves onto a writable job directory's parent either.
func TestSandboxSpecDropsAliasedAncestors(t *testing.T) {
	parent := t.TempDir()
	jobDir := filepath.Join(parent, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(parent, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The cache is a SYMLINK to the job directory's parent.
	t.Setenv("TECTONIC_CACHE_DIR", link)

	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Skipf("cannot resolve %q here: %v", parent, err)
	}
	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		// EvalSymlinks directly, never resolvePath: see the note in
		// TestCacheContainmentSeesThroughSymlinks.
		granted, rerr := filepath.EvalSymlinks(tok[3:])
		if rerr != nil {
			continue
		}
		if filepath.Clean(granted) == filepath.Clean(realParent) {
			t.Fatalf("spec grants %q, which resolves onto the job directory's parent (%s). "+
				"Landlock follows the link when it opens the path, so this grants every job "+
				"directory beside this one.", tok, parent)
		}
	}
}

// TestHostileDocumentProbesNothingTheRulesetGrants ties the corpus fixture to
// the code, in the direction that can be checked without a kernel.
//
// testdata/tectonic/hostile_reads.tex is the document this whole change
// exists to defeat, and it was verified live (status done, 1 page, every read
// refused). But a live result ages: someone widens the ruleset for an
// unrelated reason, the fixture keeps passing on a machine nobody runs it on,
// and the demonstration quietly stops demonstrating.
//
// So this asserts the STATIC half: no path the document probes may be
// reachable under the spec. It cannot prove Landlock enforces the spec -- that
// is what the live run and the startup preflight are for -- but it does catch
// the failure that would otherwise be silent, which is the ruleset growing to
// cover something the fixture attacks.
func TestHostileDocumentProbesNothingTheRulesetGrants(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "tectonic", "hostile_reads.tex"))
	if err != nil {
		t.Fatalf("the hostile-reads fixture is missing: %v", err)
	}

	probes := regexp.MustCompile(`\\probe\{([^}]+)\}`).FindAllStringSubmatch(string(src), -1)
	if len(probes) < 5 {
		t.Fatalf("found %d \\probe targets in the fixture; it is supposed to attack a spread of "+
			"them, so either the file was gutted or this matcher no longer matches it", len(probes))
	}

	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var granted []string
	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		granted = append(granted, tok[3:])
	}

	// Compared on BOTH the lexical and the resolved form of each side.
	//
	// Lexical alone is not enough, and the example is in this image: /var/run
	// is a symlink to /run, so a future grant beneath /run/secrets would cover
	// the fixture's /var/run/secrets/... path without ever matching as a
	// string (codex round 5). Resolved alone is not enough either -- on a host
	// where neither path exists, EvalSymlinks fails and there is nothing to
	// compare but the strings.
	for _, p := range probes {
		raw := p[1]
		target := filepath.Clean(raw)
		if !filepath.IsAbs(target) {
			// A relative probe resolves against the job's own directory, which
			// IS granted; the escape it attempts (../../..) is the kernel's to
			// refuse, and the live run is what shows that it does.
			target = filepath.Clean(filepath.Join(jobDir, raw))
		}
		for _, g := range granted {
			for _, pair := range [][2]string{
				{target, filepath.Clean(g)},
				{resolveForTest(t, target), resolveForTest(t, g)},
			} {
				tgt, gc := pair[0], pair[1]
				if gc == "" || tgt == "" {
					continue
				}
				if tgt == gc || strings.HasPrefix(tgt, gc+string(filepath.Separator)) {
					t.Errorf("the ruleset grants %q, which covers %q -- a path the hostile fixture "+
						"attacks and that was measured as REFUSED. Either the grant is wrong, or "+
						"the fixture's demonstration is now stale and says so on a machine nobody "+
						"runs.\n(compared as %q vs %q)", g, raw, tgt, gc)
				}
			}
		}
	}
}

// resolveForTest resolves a path the way the kernel will, WITHOUT going
// through the package's own resolvePath -- a test that asks the code under
// test whether the code under test is right proves nothing, which this file
// learned the hard way in round 4.
//
// The longest existing prefix is resolved, so a path whose tail does not exist
// on this host (/root/.ssh/id_rsa on a Mac) still gets its real ancestors.
func resolveForTest(t *testing.T, path string) string {
	t.Helper()
	rest := ""
	for cur := filepath.Clean(path); ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Clean(filepath.Join(r, rest))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// TestCacheGrantIsNarrowedToFormatDirectories is the fix for the grant source
// the fixture test could not see, because it runs with TECTONIC_CACHE_DIR
// unset (codex round 5).
//
// The configured cache root is operator-supplied. Granting it grants
// everything beneath it, and a root like /etc or a home directory holding a
// *.fmt passes every startup check -- it is a directory, it holds a format,
// and the compile preflight succeeds -- and then hands client TeX every
// credential beside it.
func TestCacheGrantIsNarrowedToTheFormatFilesThemselves(t *testing.T) {
	// A cache root that also holds something that must not be readable, which
	// is what /etc or $HOME looks like from here.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "formats"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "formats", "abc-latex-33.fmt"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TECTONIC_CACHE_DIR", root)

	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine)

	// Compared on the RESOLVED form, independently of the code under test.
	// The grant walk resolves the cache root (a symlinked cache root would
	// otherwise be walked as the symlink and found empty), so on a host where
	// the temp directory is itself a symlink -- /var -> /private/var on macOS
	// -- the emitted path is the resolved one and a lexical comparison misses
	// a grant that is genuinely there.
	wantFormat := resolveForTest(t, filepath.Join(root, "formats", "abc-latex-33.fmt"))

	var formatsGranted bool
	for _, tok := range spec {
		g := filepath.Clean(tok[3:])
		if g == wantFormat || g == filepath.Clean(filepath.Join(root, "formats", "abc-latex-33.fmt")) {
			formatsGranted = true
		}
		// Neither the root NOR the directory holding the format: the grant is
		// the format FILE. A per-directory grant would still hand out
		// everything beside the format, which is the whole finding when the
		// configured cache is a home directory.
		if g == filepath.Clean(root) || g == filepath.Clean(filepath.Join(root, "formats")) ||
			g == resolveForTest(t, root) || g == resolveForTest(t, filepath.Join(root, "formats")) {
			t.Fatalf("spec grants %q, a DIRECTORY in the configured cache, which also holds %q. "+
				"Path-beneath grants are recursive, so a cache configured as /etc or a home "+
				"directory would hand every document the credentials beside the format. The grant "+
				"must be the format FILE.\nspec = %v", tok, secret, spec)
		}
	}
	if !formatsGranted {
		t.Fatalf("the format file itself was not granted; the engine would have no format to "+
			"load.\nspec = %v", spec)
	}
}

// TestSystemRootsAreRefusedAsTheCache is the backstop for what narrowing
// cannot reach: a *.fmt placed directly in a system root makes that root the
// format-holding directory.
func TestSystemRootsAreRefusedAsTheCache(t *testing.T) {
	for _, root := range []string{"/", "/etc", "/usr", "/home", "/root", "/var"} {
		err := ValidateTectonicCacheDir(root)
		if err == nil {
			t.Errorf("%q was accepted as the format cache", root)
			continue
		}
		// The reason matters: several of these would also be refused for
		// holding no format, and that refusal disappears the moment somebody
		// puts one there -- which is the case this test is about.
		if !strings.Contains(err.Error(), "system directory") &&
			!strings.Contains(err.Error(), "contains the directory jobs are materialised in") {
			t.Errorf("%q was refused for the wrong reason, so a *.fmt placed there would make it "+
				"acceptable: %v", root, err)
		}
	}
}

// TestSpecEmitsOnlyAbsolutePaths is the invariant behind codex round 6's
// second finding, stated so it cannot come back through a different setting.
//
// The helper opens every token with O_PATH, and a RELATIVE token is resolved
// against a working directory nothing in the spec states. That is not
// theoretical: this image's runtime stage has no WORKDIR, so the worker starts
// in "/", and TECTONIC_CACHE_DIR=tmp compared as "tmp" (matching nothing)
// while opening as "/tmp" (granting every job directory).
func TestSpecEmitsOnlyAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.MkdirAll("cache/formats", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("cache/formats/abc-latex-33.fmt", []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("bundle.ttb", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("tectonic", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("job", 0o755); err != nil {
		t.Fatal(err)
	}

	// Every configurable path given RELATIVE, which is the shape of the
	// finding.
	t.Setenv("TECTONIC_CACHE_DIR", "cache")

	for _, tok := range tectonicSandboxSpec("job/in.tex", "job", "bundle.ttb", "tectonic") {
		if p := tok[3:]; !filepath.IsAbs(p) {
			t.Errorf("spec emits %q, a RELATIVE path. The helper opens it against a working "+
				"directory the spec never states, so what is granted is not what was compared -- "+
				"which is how a cache named %q grants the whole of the directory it resolves into.",
				tok, "cache")
		}
	}
}

// TestRelativeCachePathIsStillCaughtByContainment is the same finding from the
// validation side: a relative cache that RESOLVES onto the job root must be
// refused, even though as strings the two have nothing in common.
func TestRelativeCachePathIsStillCaughtByContainment(t *testing.T) {
	t.Chdir(os.TempDir())

	err := ValidateTectonicCacheDir(".")
	if err == nil {
		t.Fatal("a relative cache path that resolves onto the job root was accepted")
	}
	if !strings.Contains(err.Error(), "contains the directory jobs are materialised in") {
		t.Fatalf("refused for the wrong reason, so the containment check never fired on the "+
			"relative form: %v", err)
	}
}

// TestSymlinkNamedLikeAFormatIsNotGranted is codex round 7's P1, and it is the
// per-file grant's own shadow: narrowing to files made the NAME the whole
// decision, and a name is something the filesystem can lie about.
//
// addPath opens what it is handed with O_PATH, which follows symlinks. So a
// cache holding "credential.fmt -> id_rsa" beside a genuine format would pass
// validation, pass the compile preflight, and hand client TeX the key.
func TestSymlinkNamedLikeAFormatIsNotGranted(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "formats"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "formats", "abc-latex-33.fmt")
	if err := os.WriteFile(real, []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(root, "formats", "credential.fmt")
	if err := os.Symlink(secret, decoy); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	t.Setenv("TECTONIC_CACHE_DIR", root)

	jobDir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle.ttb")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(t.TempDir(), "tectonic")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// EVERY form the decoy can appear in, because the grant walk resolves the
	// cache ROOT while leaving the entry's own name alone -- so the emitted
	// token is "resolved parent directory + the link's name", which is neither
	// Clean(decoy) nor the fully-resolved target.
	//
	// The first version of this test compared only those last two, and the
	// inversion PASSED: the decoy was granted and the test said nothing. Every
	// form is enumerated now.
	forbidden := map[string]bool{
		filepath.Clean(decoy):     true,
		filepath.Clean(secret):    true,
		resolveForTest(t, decoy):  true,
		resolveForTest(t, secret): true,
		filepath.Join(resolveForTest(t, filepath.Dir(decoy)), filepath.Base(decoy)): true,
	}
	delete(forbidden, "")

	for _, tok := range tectonicSandboxSpec(filepath.Join(jobDir, "in.tex"), jobDir, bundle, engine) {
		if forbidden[filepath.Clean(tok[3:])] {
			t.Fatalf("spec grants %q. That entry is a SYMLINK named *.fmt pointing at %q, and "+
				"O_PATH follows it -- so the grant lands on the credential, not on a format.\n"+
				"The name of a file is not evidence about the file.", tok, secret)
		}
	}

	// And the genuine format is still granted: refusing the decoy must not
	// refuse the cache.
	if !cacheHoldsFormatOrFail(t, root) {
		t.Fatal("the cache is reported to hold no format, though it holds a real one beside the decoy")
	}
}

func cacheHoldsFormatOrFail(t *testing.T, dir string) bool {
	t.Helper()
	ok, err := cacheHoldsFormat(dir)
	if err != nil {
		t.Fatalf("scanning %q: %v", dir, err)
	}
	return ok
}

// TestSymlinkedCacheRootIsWalked is round 7's P2, and it is a false-refusal
// rather than a leak: filepath.WalkDir Lstats its root, so a cache directory
// that is a symlink to the real one -- an ordinary versioned or mounted layout
// -- was visited as the symlink, walked no further, reported empty, and the
// worker refused to start on a deployment that worked before this PR.
func TestSymlinkedCacheRootIsWalked(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "formats"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "formats", "abc-latex-33.fmt"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "cache-current")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if !cacheHoldsFormatOrFail(t, link) {
		t.Fatal("a cache reached through a symlink was reported empty; the worker would refuse to " +
			"start on a layout that is ordinary and that worked before the sandbox existed")
	}
	if err := ValidateTectonicCacheDir(link); err != nil {
		t.Fatalf("a symlinked cache root was refused at startup: %v", err)
	}
	if len(tectonicCacheGrants(link)) == 0 {
		t.Fatal("no format granted through the symlinked root, so the engine would have none to load")
	}
}

// TestTooManyFormatsIsRefusedAtStartup: the per-job grant builder stops at
// maxCacheGrants, so a cache holding more would have the rest silently
// skipped -- and a job needing a skipped format fails with a permission error
// that names the sandbox rather than the cap.
//
// Silent truncation reads as "covered everything" when it did not, which is
// the same failure this PR keeps finding in other shapes. Refused once, at
// startup, with a sentence.
func TestTooManyFormatsIsRefusedAtStartup(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= maxCacheGrants; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%03d-latex-33.fmt", i))
		if err := os.WriteFile(name, []byte("f"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := ValidateTectonicCacheDir(dir)
	if err == nil {
		t.Fatalf("a cache with %d formats was accepted, though only %d can be granted; the rest "+
			"would be silently unreachable", maxCacheGrants+1, maxCacheGrants)
	}
	if !strings.Contains(err.Error(), "more than") {
		t.Fatalf("refused for the wrong reason, so the cap was never checked: %v", err)
	}

	// And a cache at the cap is fine: the check must bound, not forbid.
	ok := t.TempDir()
	for i := 0; i < maxCacheGrants; i++ {
		if err := os.WriteFile(filepath.Join(ok, fmt.Sprintf("f%03d-latex-33.fmt", i)), []byte("f"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateTectonicCacheDir(ok); err != nil {
		t.Fatalf("a cache holding exactly the cap was refused: %v", err)
	}
	if got := len(tectonicCacheGrants(ok)); got != maxCacheGrants {
		t.Fatalf("granted %d of %d formats at the cap", got, maxCacheGrants)
	}
}
