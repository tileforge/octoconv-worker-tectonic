package convert

// This file (texresolvable.go): what the shipped bundle can actually RESOLVE,
// as opposed to what OctoConv declares support for (texpackages.go).
//
// TWO SETS, AND WHY THEY ARE NOT THE SAME SET
//
// convert.TeXPackages is a PROMISE: eighteen packages this service documents,
// tests against the bundle, and lists in docs/TYPESETTING.md. It answers "what
// will OctoConv help you with".
//
// This set answers a different question -- "what can the engine load at all" --
// and it is strictly larger, because a bundle necessarily carries the
// dependency closure of the declared set plus everything the LaTeX format
// itself loads. 608 .sty files for 18 declared packages.
//
// The admission gate in cmd/api uses THIS one, and the difference is not
// academic. It was measured against the real bundle in the built container
// (evidence/64-03-package-resolution.txt): of twenty-three common packages,
// twelve resolve and compile perfectly while being outside the declared
// eighteen -- fontenc, inputenc, lmodern, microtype, textcomp, url, array,
// longtable, verbatim, amsfonts, subcaption, tikz. Five of those are in
// pandoc's DEFAULT LaTeX template, so gating on the declared set would have
// refused, with "package not supported", documents this service typesets
// correctly today -- including the very paper 64-04 ran end to end.
//
// That is the same defect 63-02 removed a whole admission gate for: seven ways
// a valid document was rejected. A gate is only worth having if the thing it
// refuses genuinely cannot succeed, and \usepackage{pgfplots} against a bundle
// with no pgfplots.sty is exactly that -- it fails in the worker, every time,
// after an upload and a queue round trip.
//
// TEX-03's wording ("outside the declared set") is therefore reworded the way
// PROBE-01's was, and for the same reason: the requirement named a mechanism,
// the mechanism was measured, and the measurement said the mechanism refuses
// valid work. The GOAL -- a clear refusal naming the package instead of an
// unexplained engine failure -- is unchanged and is what the gate delivers.

import (
	_ "embed"
	"strings"
)

// texResolvableRaw is the sorted, de-duplicated list of every .sty basename in
// the built bundle, written by scripts/tectonic-bundle-build.sh in the same run
// that writes MANIFEST.txt.
//
// Embedded rather than generated into Go source: it is data produced by a
// build, it changes only when the bundle changes, and a diff of 608 quoted
// string literals would hide the one line that moved.
//
//go:embed texresolvable.txt
var texResolvableRaw string

// texResolvable is the parsed form, keyed by PACKAGE name (the .sty suffix
// removed), which is what a document writes in \usepackage{...}.
var texResolvable = func() map[string]struct{} {
	out := make(map[string]struct{}, 700)
	for _, line := range strings.Split(texResolvableRaw, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		out[strings.TrimSuffix(name, ".sty")] = struct{}{}
	}
	return out
}()

// texDenied names packages the bundle CAN resolve but this service refuses on
// purpose. A resolvable-set gate has exactly one hole, and this is it: the
// bundle carries the dependency closure of the declared set, and that closure
// can include a package whose absence is a documented promise.
//
// fontspec is the case, found in review (PR #16, codex P2) and confirmed by
// running: fontspec.sty is in the bundle (pulled in by the closure, not by
// choice), \usepackage{fontspec} compiles and produces a PDF -- while
// docs/TYPESETTING.md says in as many words that fontspec and user-supplied
// fonts are deliberately unavailable and deferred to TEX-07. The documented
// boundary and the enforced one had drifted apart, in the direction where the
// document wins.
//
// The deny list is checked BEFORE resolvability, so a package here is refused
// with the same clear, named 422 as one the bundle lacks -- the client is told
// it is unsupported either way, which is true either way.
//
// THE THREE NAMES ARE ONE PACKAGE. \usepackage{fontspec} loads fontspec.sty,
// which loads fontspec-xetex.sty -- and the bundle carries all three, so
// denying only the public name left a one-line bypass. Proven live, through
// the running API, before this list grew:
//
//	\usepackage{fontspec}         ->  422 unsupported_package
//	\usepackage{fontspec-xetex}   ->  202, then engine_error
//	\usepackage{fontspec-luatex}  ->  202, then a finished PDF
//
// The message names whatever the client actually wrote, so the refusal matches
// their file rather than the name we happen to think of as canonical.
var texDenied = map[string]string{
	"fontspec":        texDeniedFonts,
	"fontspec-xetex":  texDeniedFonts,
	"fontspec-luatex": texDeniedFonts,
}

// texDeniedFonts is one string because one decision (TEX-07) is behind all
// three entries; three copies would be three things to update.
const texDeniedFonts = "user-supplied fonts are not available on this deployment (TEX-07)"

// TeXPackageDenied reports whether pkg is deliberately refused despite being
// resolvable, and why. The reason is returned rather than looked up separately
// so the refusal can say something more useful than "no".
func TeXPackageDenied(pkg string) (reason string, denied bool) {
	reason, denied = texDenied[pkg]
	return reason, denied
}

// TeXPackageResolvable reports whether the shipped bundle contains a .sty the
// engine could load for pkg. It is the ADMISSION rule -- "can this possibly
// typeset" -- and never the support rule; TeXPackageSupported answers that.
//
// Exact and case-sensitive, for the reason TeXPackageSupported states: a
// package name is a filename, and \usepackage{XColor} does not load xcolor.sty
// on a case-sensitive filesystem. Normalising here would admit what the engine
// then refuses, which puts the failure back where this gate exists to take it
// from.
func TeXPackageResolvable(pkg string) bool {
	if _, denied := texDenied[pkg]; denied {
		return false
	}
	_, ok := texResolvable[pkg]
	return ok
}

// TeXResolvableCount reports the size of the set. Exported for the tests that
// refuse to run against an empty or truncated embed -- a resolvable set that
// parsed to nothing would admit nothing at the API and would look, from the
// outside, exactly like a service that refuses every LaTeX document.
func TeXResolvableCount() int { return len(texResolvable) }

// TeXUnsupportedPackageMessage is the sentence a client gets for pkg, built in
// ONE place because two gates say it.
//
// cmd/api refuses before the upload when its bounded scan sees the package;
// cmd/tectonic-worker refuses after the download when it does not. D-03 says
// they share the error CODE, and the code was already shared -- but each side
// formatted its own sentence, so the wording could drift while the code stayed
// equal, and a client would still be able to tell which gate saw them.
func TeXUnsupportedPackageMessage(pkg string) string {
	if reason, denied := TeXPackageDenied(pkg); denied {
		// A package the bundle CAN load but this service refuses on purpose
		// gets its own sentence. "The bundle does not provide it" would be
		// false, and a client who checked would find the .sty listed.
		return "the package " + pkg + " is not supported: " + reason
	}
	return "this deployment's TeX bundle does not provide the package " + pkg
}

// TeXDeniedInSource reports the first deliberately-refused package visible in
// src, which is the WHOLE input rather than the API's bounded window.
//
// This is the worker's half of the deny list, and it exists because the engine
// cannot supply this verdict: a denied package is IN the bundle, so tectonic
// loads it and succeeds. Every other refusal here is the engine's own answer
// read back; this one is a policy the engine knows nothing about, so leaving it
// to the engine meant not enforcing it at all.
//
// Found twice over, independently and then in review (PR #16, codex round 2),
// and each time by running it: \usepackage{fontspec} preceded by 1.2 MiB of
// comments outran the API's window and came back a finished PDF, against a
// document naming fontspec that the docs promise is refused.
//
// It inherits ScanTeXPreamble's contract exactly -- it may MISS (a package
// behind a macro or an \input is not visible to either gate) and never
// invents. Missing is survivable here in a way it is not for the resolvable
// set: this is a support boundary, not a safety one, and the fonts themselves
// are not in the image.
func TeXDeniedInSource(src []byte) string {
	var found string
	VisitTeXPreamblePackages(src, func(pkg string) bool {
		if _, denied := texDenied[pkg]; denied {
			found = pkg
			return false
		}
		return true
	})
	return found
}
