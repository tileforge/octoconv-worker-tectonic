package convert

import (
	"strings"
	"testing"
)

// TestDenyListCoversItsOwnVariants is the CLASS behind round 2's fontspec
// finding, rather than the instance.
//
// The deny list was added in round 1 naming fontspec, and fontspec is the
// public entry point of a package whose implementation ships as two more .sty
// files in the same bundle -- fontspec-xetex.sty and fontspec-luatex.sty, both
// loadable on their own. \usepackage{fontspec-xetex} was admitted and produced
// a PDF while \usepackage{fontspec} came back 422, from the same deployment,
// for the same capability.
//
// So this does not check that those two names are listed. It checks that NO
// denied package has a bundle variant left off the list -- which is the shape
// that will recur the next time something is denied.
func TestDenyListCoversItsOwnVariants(t *testing.T) {
	if len(texDenied) == 0 {
		t.Skip("nothing is denied; this test has nothing to hold")
	}
	for denied := range texDenied {
		prefix := denied + "-"
		found := 0
		for name := range texResolvable {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			found++
			if _, ok := texDenied[name]; !ok {
				t.Errorf("%s.sty is in the bundle and is a variant of the denied package %q, but is "+
					"not itself denied: \\usepackage{%s} would be admitted and would deliver exactly "+
					"what denying %q refuses", name, denied, name, denied)
			}
		}
		t.Logf("%s: %d bundle variants", denied, found)
	}
}

// TestDenyListEntriesAreActuallyInTheBundle keeps the list from filling up with
// names that were never admissible in the first place.
//
// A denied package that the bundle does not carry is refused by resolvability
// alone, so its entry changes nothing except the sentence the client reads --
// and that sentence would then be wrong: it says "not supported on this
// deployment" where the truth is "this bundle does not have it".
func TestDenyListEntriesAreActuallyInTheBundle(t *testing.T) {
	for denied := range texDenied {
		if _, ok := texResolvable[denied]; !ok {
			t.Errorf("%q is on the deny list but is not in the bundle: it is already refused as "+
				"unresolvable, and the deny entry makes the refusal say something untrue", denied)
		}
	}
}

// TestTeXPackageResolvableRefusesDenied is the one-line invariant the API gate
// leans on: it calls TeXPackageResolvable and nothing else, so a denied package
// that answered true there would be admitted with no further check.
func TestTeXPackageResolvableRefusesDenied(t *testing.T) {
	for denied := range texDenied {
		if TeXPackageResolvable(denied) {
			t.Errorf("TeXPackageResolvable(%q) = true for a denied package: the API's gate would "+
				"admit it", denied)
		}
		if TeXDeniedInSource([]byte("\\documentclass{article}\n\\usepackage{"+denied+"}\n\\begin{document}\n")) != denied {
			t.Errorf("TeXDeniedInSource does not see \\usepackage{%s}: the worker's half of the same "+
				"rule would let it through", denied)
		}
	}
}
