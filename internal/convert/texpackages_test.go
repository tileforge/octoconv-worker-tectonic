package convert

import (
	"sort"
	"testing"
)

// TestTeXPackagesReturnsACopy pins that the vocabulary cannot be mutated
// through its accessor -- the same property AllThreatClasses and errcode.All
// hold, and for the same reason: a closed set that a caller can edit is not
// closed.
func TestTeXPackagesReturnsACopy(t *testing.T) {
	first := TeXPackages()
	if len(first) == 0 {
		t.Fatal("TeXPackages() is empty")
	}
	first[0] = "mutated"

	second := TeXPackages()
	if second[0] == "mutated" {
		t.Fatal("TeXPackages() hands out the package's own slice -- mutating the result changed the " +
			"vocabulary for every later caller")
	}
}

// TestTeXPackagesIsSortedAndUnique pins the two properties the mechanical
// equality against the bundle manifest depends on. Without sorting, that
// comparison would have to sort first and would silently tolerate a list whose
// order drifts; without uniqueness, a duplicated entry would make the counts
// disagree while the sets matched, which is a confusing failure to debug.
func TestTeXPackagesIsSortedAndUnique(t *testing.T) {
	got := TeXPackages()

	if !sort.StringsAreSorted(got) {
		t.Errorf("TeXPackages() is not sorted: %v", got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("%q appears more than once in the declared set", p)
		}
		seen[p] = true
	}
}

// TestTeXPackageSupportedIsExact pins the case-sensitivity decision rather than
// leaving it to be discovered.
//
// A LaTeX package name is a filename. \usepackage{XColor} does not load
// xcolor.sty on a case-sensitive filesystem, so accepting it here would admit a
// document the engine then refuses -- a rejection moved later and made less
// legible, which is the opposite of what TEX-03 is for.
func TestTeXPackageSupportedIsExact(t *testing.T) {
	if !TeXPackageSupported("xcolor") {
		t.Error("xcolor is in the declared set but TeXPackageSupported says otherwise")
	}
	for _, wrong := range []string{"XColor", "XCOLOR", "xcolor.sty", " xcolor", "fontspec", "biber"} {
		if TeXPackageSupported(wrong) {
			t.Errorf("TeXPackageSupported(%q) = true -- the comparison must be exact, and fontspec/"+
				"biber are deliberately NOT in the set", wrong)
		}
	}
}

// TestDeferredPackagesAreAbsent pins the two deliberate omissions, so removing
// them from the set's doc comment cannot quietly become adding them to the set.
//
// fontspec is TEX-07 (user-supplied fonts). biber is 63-CONTEXT.md D-07: no
// biber binary ships in the image, so a document asking for biblatex's
// biber-only styles must fail on the declared set rather than on a missing
// executable at run time.
func TestDeferredPackagesAreAbsent(t *testing.T) {
	for _, p := range TeXPackages() {
		if p == "fontspec" {
			t.Error("fontspec is in the declared set -- it is the entry point to user-supplied fonts " +
				"(TEX-07, deferred), and adding it here is a scope decision, not a list edit")
		}
		if p == "biber" {
			t.Error("biber is in the declared set -- no biber binary ships in the image (D-07), so " +
				"declaring it promises something the worker cannot do")
		}
	}
}
