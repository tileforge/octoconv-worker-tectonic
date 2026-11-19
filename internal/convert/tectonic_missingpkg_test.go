package convert

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The parser under test was written AFTER these bytes were captured, against
// them. testdata/tectonic/PROVENANCE.md records the image, the engine version,
// the command and the exit code. This project's retrospective records four
// bypasses of one bound produced by predicting a format instead of testing
// against one, so the order matters more than the assertion.

func readTectonicFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tectonic/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if len(b) == 0 {
		t.Fatalf("fixture %s is empty -- every assertion against it would be vacuous", name)
	}
	return b
}

// TestTectonicMissingPackageFromRealOutput is the positive case, over the real
// capture rather than over a line retyped into this file.
func TestTectonicMissingPackageFromRealOutput(t *testing.T) {
	out := readTectonicFixture(t, "missing-package.txt")

	// The fixture must still contain what it was captured for; a fixture
	// silently replaced by an unrelated log would make the assertion below
	// meaningless in the passing direction too.
	if !strings.Contains(string(out), "not found") {
		t.Fatal("the missing-package fixture no longer contains a 'not found' line")
	}

	if got := tectonicMissingPackage(out); got != "pgfplots" {
		t.Fatalf("tectonicMissingPackage = %q, want %q -- from the engine's own line: "+
			"! LaTeX Error: File 'pgfplots.sty' not found.", got, "pgfplots")
	}
}

// TestTectonicMissingPackageDeclinesToGuess pins the "" contract on inputs
// where naming a package would be a guess. A confidently wrong name is worse
// than none: the client removes the wrong \usepackage and the job fails again.
func TestTectonicMissingPackageDeclinesToGuess(t *testing.T) {
	t.Run("undefined control sequence names no file", func(t *testing.T) {
		out := readTectonicFixture(t, "undefined-control-sequence.txt")
		if got := tectonicMissingPackage(out); got != "" {
			t.Fatalf("tectonicMissingPackage = %q, want \"\" -- this capture is a compilation error "+
				"with no missing file at all", got)
		}
	})

	// Non-.sty misses are faults in the bundle or in a package's own
	// dependency chain, not lines in the client's document.
	for _, tc := range []struct{ name, line string }{
		{"font definition", "! LaTeX Error: File `ts1cmr.fd' not found."},
		{"class", "! LaTeX Error: File `revtex4-2.cls' not found."},
		{"config", "! LaTeX Error: File `color.cfg' not found."},
		{"plain tex input", "! LaTeX Error: File `preamble.tex' not found."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tectonicMissingPackage([]byte(tc.line)); got != "" {
				t.Fatalf("tectonicMissingPackage(%q) = %q, want \"\" -- telling a client to remove "+
					"\\usepackage{%s} would send them deleting a line their document does not have",
					tc.line, got, got)
			}
		})
	}

	// The Fontconfig line appears on EVERY run in this image, including runs
	// that produce a PDF. A parser keying on "error" would report it.
	t.Run("fontconfig noise", func(t *testing.T) {
		noise := []byte("Fontconfig error: Cannot load default config file: No such file: (null)")
		if got := tectonicMissingPackage(noise); got != "" {
			t.Fatalf("tectonicMissingPackage on the ubiquitous Fontconfig line = %q, want \"\"", got)
		}
	})
}

// TestTectonicMissingPackageIsCarriedAsTheSentinel holds the parser and the
// error contract together. The worker classifies on errors.Is and formats its
// message from TeXUnsupportedPackageName, so a parser that worked while the
// wrapping did not would produce a correctly-classified failure that names
// nothing.
func TestTectonicMissingPackageIsCarriedAsTheSentinel(t *testing.T) {
	err := TeXUnsupportedPackageError(tectonicMissingPackage(readTectonicFixture(t, "missing-package.txt")))
	if !errors.Is(err, ErrTeXUnsupportedPackage) {
		t.Fatalf("errors.Is(err, ErrTeXUnsupportedPackage) = false for %v", err)
	}
	if got := TeXUnsupportedPackageName(err); got != "pgfplots" {
		t.Fatalf("TeXUnsupportedPackageName = %q, want pgfplots", got)
	}
}
