package convert

// This file (texpackages.go): the closed, declared set of LaTeX packages the
// tex engine supports (TEX-03).
//
// WHY A LIST AT ALL
//
// The bundle shipped with cmd/tectonic-worker is a REDUCED one, not full
// TeXLive: the full distribution is ~2.69 GiB on a layer, an order of magnitude
// above anything else in this fleet, and letting tectonic fetch at run time
// would mean a client's \usepackage decides what gets downloaded -- the exact
// vector the sanitize engine spends six plans removing from other people's
// files.
//
// Reducing it forces the question "which packages does OctoConv support" to
// have an answer. This is that answer, in the same shape as every other closed
// vocabulary here (AllThreatClasses, the error-code list, pandocPairTable): one
// declaration, an All() that hands out copies, and a mechanical test holding it
// equal to what the bundle actually contains and to what the documentation
// claims.
//
// WHAT THE SET IS FOR, GROUP BY GROUP
//
// Colour and styling are here because the operator asked for them by name. The
// first draft of this list had neither xcolor nor any styling package in it,
// which would have made coloured text, coloured tables and the ordinary
// hyperref link-colour setup fail on day one. That is recorded because a later
// reader trimming this list for bundle size should see they were requested,
// not assumed.
//
// fontspec is deliberately ABSENT: it is the entry point to user-supplied
// fonts, which is TEX-07 and deferred. biber is absent for a different and
// stronger reason -- see 63-CONTEXT.md D-07 and docs/TYPESETTING.md: no biber
// binary ships in the image at all, so biblatex runs on backend=bibtex, which
// tectonic executes inside its own process.

import "sort"

// texPackages is the declared set. Grouped by purpose, because a flat
// alphabetical list makes "why is this here" unanswerable at review time.
var texPackages = []string{
	// Math.
	"amsmath", "amssymb", "amsthm",
	// Graphics and colour.
	"graphicx", "xcolor", "colortbl",
	// Layout and styling.
	"geometry", "titlesec", "fancyhdr", "enumitem", "caption", "booktabs", "tcolorbox",
	// Links.
	"hyperref",
	// Code listings.
	"listings",
	// Language.
	"babel",
	// Bibliography. backend=bibtex only -- see the header.
	"biblatex", "natbib",
}

// TeXPackages returns the declared set, sorted, as a fresh copy on every call
// so a caller cannot mutate the vocabulary through it.
//
// Sorted rather than in declaration order because the consumers that matter --
// the equality test against the bundle manifest, and the documentation table --
// both want a stable order, and the grouping above is for the reader of this
// file, not for them.
func TeXPackages() []string {
	out := make([]string, len(texPackages))
	copy(out, texPackages)
	sort.Strings(out)
	return out
}

// TeXPackageSupported reports whether pkg is in the declared set.
//
// Exact, case-sensitive comparison, deliberately: a LaTeX package name is a
// filename on disk, and \usepackage{XColor} does not load xcolor.sty on a
// case-sensitive filesystem. Normalising here would make this function accept
// what the engine then refuses, which is worse than refusing early.
func TeXPackageSupported(pkg string) bool {
	for _, p := range texPackages {
		if p == pkg {
			return true
		}
	}
	return false
}
