// This file holds the three-way equality 64-CONTEXT.md D-01 requires: the Go
// list, the built bundle's manifest, and docs/TYPESETTING.md must agree.
//
// The manifest half is the one with teeth. Holding the list equal to the
// document proves this project matches what it WROTE DOWN; it cannot notice
// that what it SHIPPED is different -- and a package advertised but not shipped
// is a document accepted and then failed, which is the failure this phase is
// built to avoid.
package convert_test

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
)

const (
	manifestPath = "../../deploy/tectonic-bundle/MANIFEST.txt"
	typesetDoc   = "../../docs/TYPESETTING.md"
)

// readManifest returns every path the built bundle contains, skipping the
// generated header comments.
func readManifest(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v -- the manifest is written by scripts/tectonic-bundle-build.sh "+
			"and committed; without it this test proves nothing about what ships", manifestPath, err)
	}
	defer f.Close()

	var paths []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		paths = append(paths, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", manifestPath, err)
	}
	return paths
}

// TestEveryDeclaredPackageIsInTheBundle is the half that catches a promise the
// bundle does not keep.
//
// THE CHECK IS FOR <pkg>.sty, NOT FOR A DIRECTORY NAMED <pkg>.
//
// The first version of this test looked for tex/latex/<pkg>/ and reported
// amssymb, amsthm, babel and graphicx as missing from a bundle that contains
// all four. A package's name is not its directory: amssymb.sty lives in
// amsfonts, amsthm.sty in amscls, graphicx.sty in graphics, and babel.sty under
// tex/generic rather than tex/latex. What LaTeX actually resolves when a
// document writes \usepackage{x} is the file x.sty, so that is what this
// asserts -- the check matches the engine's rule instead of a directory-layout
// assumption that happens to hold for most packages.
func TestEveryDeclaredPackageIsInTheBundle(t *testing.T) {
	paths := readManifest(t)
	if len(paths) == 0 {
		t.Fatal("the manifest lists no paths -- it would then satisfy every check below vacuously")
	}

	have := make(map[string]string, len(paths))
	for _, p := range paths {
		if i := strings.LastIndex(p, "/"); i >= 0 {
			base := p[i+1:]
			if _, seen := have[base]; !seen {
				have[base] = p
			}
		}
	}

	for _, pkg := range convert.TeXPackages() {
		if _, ok := have[pkg+".sty"]; !ok {
			t.Errorf("%s is declared supported but %s.sty is not in the bundle manifest -- a client "+
				"writing \\usepackage{%s} would be accepted and then fail in the worker", pkg, pkg, pkg)
		}
	}
}

// TestManifestMayContainMore states the asymmetry rather than leaving a future
// reader to "fix" this file into strict equality.
//
// The bundle necessarily carries the transitive closure -- 18 declared packages
// resolve to 67 directories, plus fonts, plus everything the LaTeX format
// itself loads. Requiring the manifest to equal the declared set would fail
// permanently and for no defect.
func TestManifestMayContainMore(t *testing.T) {
	paths := readManifest(t)
	if len(paths) <= len(convert.TeXPackages()) {
		t.Fatalf("the manifest has %d paths for %d declared packages -- it is expected to be very "+
			"much larger, because it carries the dependency closure, the fonts and the LaTeX "+
			"format's own files. A manifest this small means the build shipped almost nothing",
			len(paths), len(convert.TeXPackages()))
	}
}

// docTableRow matches a row of docs/TYPESETTING.md's supported-package table,
// whose first cell is one or more backtick-quoted package names. Anchored to
// the line start and to the first cell so the document's prose -- which quotes
// package names freely -- cannot be mistaken for table entries.
var docTableRow = regexp.MustCompile("^\\|\\s*((?:`[a-zA-Z0-9-]+`(?:,\\s*)?)+)\\s*\\|")

var docTableName = regexp.MustCompile("`([a-zA-Z0-9-]+)`")

// TestDocTableMatchesTheDeclaredSet is the bidirectional proof for the
// documentation half: every declared package appears in the table, and the
// table names nothing the Go list does not declare.
func TestDocTableMatchesTheDeclaredSet(t *testing.T) {
	raw, err := os.ReadFile(typesetDoc)
	if err != nil {
		t.Fatalf("read %s: %v", typesetDoc, err)
	}

	var documented []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		m := docTableRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, n := range docTableName.FindAllStringSubmatch(m[1], -1) {
			documented = append(documented, n[1])
		}
	}
	if len(documented) == 0 {
		t.Fatal("extracted no package names from docs/TYPESETTING.md's table -- the table's shape " +
			"changed in a way this parser no longer understands, and the drift check below is silently " +
			"covering nothing")
	}
	sort.Strings(documented)

	declared := convert.TeXPackages()
	inDoc := make(map[string]bool, len(documented))
	for _, d := range documented {
		inDoc[d] = true
	}
	inGo := make(map[string]bool, len(declared))
	for _, d := range declared {
		inGo[d] = true
	}

	for _, d := range declared {
		if !inDoc[d] {
			t.Errorf("%q is declared in Go and absent from docs/TYPESETTING.md's table -- the document "+
				"is silent about something the service supports", d)
		}
	}
	for _, d := range documented {
		if !inGo[d] {
			t.Errorf("%q is in docs/TYPESETTING.md's table and not declared in Go -- the document "+
				"promises something no gate will admit", d)
		}
	}
}
