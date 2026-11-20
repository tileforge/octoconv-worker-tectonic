package convert

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireTectonicBundle gates the real-engine tests on both the binary and a
// bundle, and skips rather than fails when either is absent -- the convention
// requireGhostscript and requirePdfinfo already follow, including
// OCTOCONV_REQUIRE_BINS=1 turning the skip into a failure.
//
// The bundle is a 122 MiB published artifact, not a repository fixture, so its
// path comes from the environment. TECTONIC_BUNDLE_PATH is the same variable
// cmd/tectonic-worker reads.
func requireTectonicBundle(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tectonic"); err != nil {
		if os.Getenv("OCTOCONV_REQUIRE_BINS") == "1" {
			t.Fatalf("tectonic not on PATH and OCTOCONV_REQUIRE_BINS=1: a skipped test is not a passed test")
		}
		t.Skip("tectonic not found on PATH, skipping real-engine test")
	}
	path := os.Getenv(TectonicBundleEnv)
	if path == "" {
		path = DefaultTectonicBundlePath
	}
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("OCTOCONV_REQUIRE_BINS") == "1" {
			t.Fatalf("no bundle at %q and OCTOCONV_REQUIRE_BINS=1 (set %s)", path, TectonicBundleEnv)
		}
		t.Skipf("no tectonic bundle at %q; set %s to the published artifact", path, TectonicBundleEnv)
	}
	return path
}

// TestTectonicArgsCarriesTheGuarantees asserts on the argument vector directly,
// which is why it is built by its own function.
//
// The assertions are on the SLICE, not on a joined string: a substring check
// would accept "--untrusted-ish", and the whole value of --untrusted is that it
// is exactly itself.
func TestTectonicArgsCarriesTheGuarantees(t *testing.T) {
	args := tectonicArgs("/in/doc.tex", "/out", "/opt/bundle.ttb")

	has := func(want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}

	if !has("--untrusted") {
		t.Errorf("argv has no --untrusted: %v -- that flag is the whole of TEX-04", args)
	}
	if !has("-b") || !has("/opt/bundle.ttb") {
		t.Errorf("argv does not pass the bundle: %v -- without -b tectonic fetches over the "+
			"network and the document decides what is downloaded (TEX-02)", args)
	}
	if !has("-p") {
		t.Errorf("argv has no -p: %v -- the engine's chatter is the only source of a diagnosable "+
			"failure message, and 64-03 parses it for the unresolvable package name", args)
	}
	for _, forbidden := range []string{"-Z", "--keep-intermediates", "-k", "--synctex"} {
		if has(forbidden) {
			t.Errorf("argv carries %q: %v", forbidden, args)
		}
	}
	if has("-r") || has("--reruns") {
		t.Errorf("argv pins a rerun count: %v -- tectonic reruns until convergence, and fixing the "+
			"number would replace the thing 64-05 measures with a guess", args)
	}
}

// TestConvertRefusesWithoutABundle pins that a misconfigured process fails
// loudly instead of falling back to tectonic's default bundle -- which is a
// network fetch, and therefore a silent TEX-02 violation rather than an error.
func TestConvertRefusesWithoutABundle(t *testing.T) {
	err := TectonicConverter{}.Convert(context.Background(), "in.tex", "out.pdf", nil)
	if err == nil {
		t.Fatal("Convert with no bundle configured returned nil -- it would have run against " +
			"tectonic's default bundle, over the network")
	}
	if !strings.Contains(err.Error(), TectonicBundleEnv) {
		t.Errorf("the error does not name %s: %v -- an operator reading it needs to know which "+
			"knob was not set", TectonicBundleEnv, err)
	}
}

// hostileDoc writes the committed hostile fixture into dir with its sentinel
// placeholder replaced by an absolute path, and returns both.
func hostileDoc(t *testing.T, dir string) (docPath, sentinel string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/tectonic/hostile_write18.tex")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sentinel = filepath.Join(dir, "SENTINEL")
	docPath = filepath.Join(dir, "hostile.tex")
	body := strings.ReplaceAll(string(raw), "OCTOCONV_SENTINEL_PATH", sentinel)
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return docPath, sentinel
}

// TestUntrustedDefeatsShellEscape is TEX-04, proven by running and by the one
// inversion that actually isolates it.
//
// The obvious inversion -- drop --untrusted -- proves nothing, because
// shell-escape is OFF BY DEFAULT: the document does not execute either way, and
// a test built on that comparison is green for a reason unrelated to the flag.
// Measured before this test was written, all three configurations, and the
// middle one is the requirement:
//
//	--untrusted                      -> no sentinel
//	--untrusted AND -Z shell-escape  -> no sentinel   <- the guarantee
//	-Z shell-escape alone            -> SENTINEL      <- the fixture works
//
// The third row is what makes the second worth anything. Without it, "no
// sentinel" would be equally consistent with a fixture that never executes --
// and the first version of this fixture was exactly that.
func TestUntrustedDefeatsShellEscape(t *testing.T) {
	bundle := requireTectonicBundle(t)
	dir := t.TempDir()
	doc, sentinel := hostileDoc(t, dir)

	run := func(t *testing.T, args []string) {
		t.Helper()
		_ = os.Remove(sentinel)
		// Ignoring the engine's exit status deliberately: whether the document
		// typesets is not the question. Whether the command ran is.
		_, _ = runCommand(context.Background(), "tectonic", args...)
	}

	t.Run("production argv refuses the shell command", func(t *testing.T) {
		run(t, tectonicArgs(doc, dir, bundle))
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatal("the hostile document ran a shell command under the production argv (TEX-04)")
		}
	})

	t.Run("an explicit -Z shell-escape cannot re-enable it", func(t *testing.T) {
		// The actual guarantee: --untrusted makes the feature un-enableable
		// REGARDLESS of other flags, rather than being a default that a later
		// argument overrides. Appending -Z after --untrusted is the strongest
		// form of that question.
		args := append(tectonicArgs(doc, dir, bundle), "-Z", "shell-escape")
		run(t, args)
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatal("-Z shell-escape re-enabled shell escape despite --untrusted -- the flag is " +
				"then an overridable default, not the structural guarantee TEX-04 relies on")
		}
	})

	t.Run("and the fixture is not inert", func(t *testing.T) {
		// Without --untrusted, -Z shell-escape works and the command runs. If
		// this ever stops producing the sentinel, the two assertions above have
		// stopped meaning anything and are passing on a broken fixture.
		args := []string{"-X", "compile", doc, "-Z", "shell-escape", "-b", bundle, "-o", dir, "-p"}
		run(t, args)
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("the hostile document did NOT run its command even with -Z shell-escape and no "+
				"--untrusted (%v) -- the fixture proves nothing, and the two tests above are green "+
				"for a reason that has nothing to do with the flag", err)
		}
	})
}
