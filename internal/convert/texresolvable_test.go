package convert_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
)

// styBasenames returns every .sty basename in the committed bundle manifest --
// the same extraction scripts/tectonic-bundle-build.sh performs, expressed
// independently here so the two can disagree.
func styBasenames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	seen := map[string]bool{}
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasSuffix(line, ".sty") {
			continue
		}
		base := line
		if i := strings.LastIndex(line, "/"); i >= 0 {
			base = line[i+1:]
		}
		if !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out
}

// TestResolvableSetMatchesTheManifest holds the embedded admission set equal to
// the bundle that actually ships.
//
// Both directions matter and they fail differently:
//
//   - a .sty in the bundle but missing from the embed means cmd/api refuses a
//     document the worker would have typeset -- a 422 for a valid file;
//   - a name in the embed with no .sty in the bundle means cmd/api admits a
//     document that cannot compile, moving the failure back behind an upload
//     and a queue round trip, which is the whole thing this gate removes.
//
// When it fails the fix is to re-run scripts/tectonic-bundle-build.sh, not to
// hand-edit texresolvable.txt: the file is written by the same run that writes
// the manifest, so that they cannot come from different builds.
func TestResolvableSetMatchesTheManifest(t *testing.T) {
	want := styBasenames(t)
	if len(want) < 100 {
		t.Fatalf("only %d .sty files found in %s -- the manifest or this extraction has drifted, and "+
			"every assertion below would be vacuous", len(want), manifestPath)
	}
	denied := 0
	for _, sty := range want {
		pkg := strings.TrimSuffix(sty, ".sty")
		if _, isDenied := convert.TeXPackageDenied(pkg); isDenied {
			// Deliberately refused despite being present. The next test holds
			// that list honest; here it only has to be subtracted, or this
			// assertion would demand the gate admit what the docs promise it
			// refuses.
			denied++
			continue
		}
		if !convert.TeXPackageResolvable(pkg) {
			t.Errorf("%s is in the bundle but not in the embedded admission set: cmd/api would 422 a "+
				"document the worker typesets", pkg)
		}
	}
	if got := convert.TeXResolvableCount(); got != len(want) {
		t.Errorf("the embedded admission set has %d entries, the manifest has %d .sty files -- re-run "+
			"scripts/tectonic-bundle-build.sh", got, len(want))
	}
	t.Logf("%d of %d bundle packages are explicitly denied", denied, len(want))
}

// TestDeniedPackagesAreRealAndUnsupported keeps the deny list from rotting in
// either direction.
//
// A deny entry for a package the bundle does not have is DEAD: it reads as an
// enforced boundary while enforcing nothing, and the next reader would trust
// it. A deny entry for a package the declared set PROMISES is a contradiction
// the API resolves by refusing what the documentation offers.
func TestDeniedPackagesAreRealAndUnsupported(t *testing.T) {
	stys := map[string]bool{}
	for _, sty := range styBasenames(t) {
		stys[sty] = true
	}

	// The list is small and its members are named here on purpose: adding one
	// is a change to the service's advertised boundary, and it should not be
	// possible to do quietly.
	for _, pkg := range []string{"fontspec"} {
		reason, denied := convert.TeXPackageDenied(pkg)
		if !denied {
			t.Errorf("%s is no longer denied -- docs/TYPESETTING.md still says user-supplied fonts are "+
				"unavailable, so either the deny list or the documentation is now wrong", pkg)
			continue
		}
		if reason == "" {
			t.Errorf("%s is denied with no reason: the 422 would say the package is unsupported and "+
				"nothing about why", pkg)
		}
		if !stys[pkg+".sty"] {
			t.Errorf("%s is denied but is NOT in the bundle -- the entry enforces nothing and reads as "+
				"if it does; the plain resolvable check already refuses it", pkg)
		}
		if convert.TeXPackageSupported(pkg) {
			t.Errorf("%s is BOTH declared supported and denied -- the API refuses what the "+
				"documentation offers", pkg)
		}
		if convert.TeXPackageResolvable(pkg) {
			t.Errorf("%s is denied but still admissible: the deny check does not gate admission", pkg)
		}
	}
}

// TestEveryDeclaredPackageIsAlsoResolvable is the containment that makes the
// two sets coherent: everything OctoConv PROMISES must be something the engine
// can load. The manifest test next door proves the promise against the bundle;
// this proves the two Go-side sets do not contradict each other, which is what
// a caller reading only convert/ can see.
func TestEveryDeclaredPackageIsAlsoResolvable(t *testing.T) {
	for _, pkg := range convert.TeXPackages() {
		if !convert.TeXPackageResolvable(pkg) {
			t.Errorf("%s is declared supported but is not in the admission set -- the API would refuse "+
				"a package this service documents", pkg)
		}
	}
}

// TestResolvableIsStrictlyLargerThanDeclared pins the RELATIONSHIP, not a
// count. If these two ever became the same set, the admission gate would have
// silently turned into the declared-set gate that was measured and rejected --
// and every test above would still pass.
//
// The twelve names below are not invented: each was compiled against the real
// bundle in the built container on 2026-09-04 and produced a PDF
// (evidence/64-03-package-resolution.txt). They are the documents this gate
// must not refuse.
func TestResolvableIsStrictlyLargerThanDeclared(t *testing.T) {
	measured := []string{
		"fontenc", "inputenc", "lmodern", "microtype", "textcomp", "url",
		"array", "longtable", "verbatim", "amsfonts", "subcaption", "tikz",
	}
	for _, pkg := range measured {
		if !convert.TeXPackageResolvable(pkg) {
			t.Errorf("%s compiled against this bundle in the container but is not admissible -- five of "+
				"these twelve are in pandoc's default LaTeX template", pkg)
		}
		if convert.TeXPackageSupported(pkg) {
			t.Errorf("%s is now in the DECLARED set; this test's premise has changed and the measurement "+
				"in evidence/64-03-package-resolution.txt needs re-reading before the row is deleted", pkg)
		}
	}

	// The negative control. Without it, "resolvable is larger" would also be
	// satisfied by a set that admits everything, which is not a gate.
	for _, pkg := range []string{"pgfplots", "siunitx", "minted", "csquotes", "mathtools"} {
		if convert.TeXPackageResolvable(pkg) {
			t.Errorf("%s is admissible, but it is NOT in the bundle -- measured as MISSING in the "+
				"container, so an admitted document would fail in the worker", pkg)
		}
	}
}
