package convert_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
)

// TestScanTeXPreamble is a table over shapes that occur in real preambles,
// not over shapes that are convenient to parse. Every fixture below is either
// taken from a document this repository already compiles (64-04's end-to-end
// paper, pandoc's default template) or is a form LaTeX documents them as legal.
func TestScanTeXPreamble(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantPackages []string
		wantComplete bool
	}{
		{
			name:         "plain",
			src:          "\\documentclass{article}\n\\usepackage{amsmath}\n\\begin{document}\nx\n\\end{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name:         "options",
			src:          "\\documentclass{article}\n\\usepackage[T1]{fontenc}\n\\begin{document}\n",
			wantPackages: []string{"fontenc"},
			wantComplete: true,
		},
		{
			name:         "several packages in one call",
			src:          "\\usepackage{amsmath,amssymb, booktabs}\n\\begin{document}\n",
			wantPackages: []string{"amsmath", "amssymb", "booktabs"},
			wantComplete: true,
		},
		{
			name:         "options with a comma inside",
			src:          "\\usepackage[backend=bibtex,style=numeric]{biblatex}\n\\begin{document}\n",
			wantPackages: []string{"biblatex"},
			wantComplete: true,
		},
		{
			name:         "RequirePackage",
			src:          "\\RequirePackage{fix-cm}\n\\documentclass{article}\n\\begin{document}\n",
			wantPackages: []string{"fix-cm"},
			wantComplete: true,
		},
		{
			name:         "commented out",
			src:          "% \\usepackage{pgfplots}\n\\usepackage{amsmath}\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name:         "trailing comment on a real line",
			src:          "\\usepackage{amsmath} % why: display equations\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name:         "escaped percent is not a comment",
			src:          "\\usepackage{siunitx} \\newcommand{\\pct}{100\\%} \\usepackage{url}\n\\begin{document}\n",
			wantPackages: []string{"siunitx", "url"},
			wantComplete: true,
		},
		{
			// The preamble ENDS at \begin{document}. A \usepackage after it is
			// not a preamble entry, and treating it as one would let a
			// document extend its own preamble past the window.
			name:         "usepackage after begin document is not the preamble",
			src:          "\\usepackage{amsmath}\n\\begin{document}\n\\usepackage{pgfplots}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			// The case the whole `complete` return value exists for.
			name:         "truncated prefix",
			src:          "\\documentclass{article}\n\\usepackage{amsmath}\n\\usepackage{grap",
			wantPackages: []string{"amsmath"},
			wantComplete: false,
		},
		{
			name:         "two on one line",
			src:          "\\usepackage{amsmath}\\usepackage{amssymb}\n\\begin{document}\n",
			wantPackages: []string{"amsmath", "amssymb"},
			wantComplete: true,
		},
		{
			name:         "no packages at all",
			src:          "\\documentclass{article}\n\\begin{document}\nhello\n",
			wantPackages: nil,
			wantComplete: true,
		},
		{
			// Pandoc's own standalone output opens with a comment block and
			// loads five packages the declared set does not name -- the exact
			// document 64-03's measurement was about.
			name: "pandoc default template head",
			src: "% Options for packages loaded elsewhere\n" +
				"\\PassOptionsToPackage{unicode}{hyperref}\n" +
				"\\documentclass[]{article}\n" +
				"\\usepackage{lmodern}\n" +
				"\\usepackage[T1]{fontenc}\n" +
				"\\usepackage[utf8]{inputenc}\n" +
				"\\usepackage{textcomp}\n" +
				"\\usepackage{microtype}\n" +
				"\\begin{document}\n",
			wantPackages: []string{"lmodern", "fontenc", "inputenc", "textcomp", "microtype"},
			wantComplete: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := convert.ScanTeXPreamble([]byte(tc.src))
			if complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", complete, tc.wantComplete)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantPackages, ",") {
				t.Errorf("packages = %v, want %v", got, tc.wantPackages)
			}
		})
	}
}

// TestScanTeXPreambleNeverInventsAPackage states the asymmetry the scanner is
// built around, as executable text rather than as a comment.
//
// Missing a package is TOLERATED -- the worker gate is the backstop, and
// `complete` reports when the window ran out. Inventing one is not: it is a
// valid document refused at admission, which is the failure 63-02 removed a
// whole gate for.
//
// The three inputs below are the macro/conditional/input forms the doc comment
// names. Each hides a package from the scanner, and each must produce NOTHING
// rather than a guess.
func TestScanTeXPreambleNeverInventsAPackage(t *testing.T) {
	hidden := []struct {
		name         string
		src          string
		wantAbstains bool
	}{
		{
			name:         "macro definition body",
			src:          "\\newcommand{\\loadit}[1]{\\usepackage{#1}}\n\\loadit{pgfplots}\n\\begin{document}\n",
			wantAbstains: true,
		},
		{
			// Found in review (PR #16), and verified against the engine first:
			// this document COMPILES CLEANLY in the built image, because the
			// conditional is false. The scanner used to report pgfplots for it,
			// so the API answered 422 "does not provide the package pgfplots"
			// to a document that does not use pgfplots at all.
			name:         "usepackage inside a false conditional",
			src:          "\\newif\\iffoo\n\\foofalse\n\\iffoo\\usepackage{pgfplots}\\fi\n\\begin{document}\n",
			wantAbstains: true,
		},
		{
			// Round 2 of the same review, and the same failure one level down:
			// the conditional WAS tracked, but \fi was matched as a PREFIX, so
			// \finalfoo -- a perfectly ordinary macro name that happens to
			// begin with those two letters -- closed it. The scanner then read
			// the skipped \usepackage as executed and the API refused a
			// document that compiles. Confirmed by running it before the fix:
			// packages=[pgfplots], complete=true.
			//
			// TeX reads a control WORD as a maximal run of letters, so the only
			// correct comparison is on the whole word.
			name:         "control word that merely starts with fi",
			src:          "\\iffoo\\finalfoo\\usepackage{pgfplots}\\fi\n\\begin{document}\n",
			wantAbstains: true,
		},
		{
			// The same prefix defect on the directive itself: \usepackagefoo is
			// a macro, and reading it as \usepackage would invent "bar" -- a
			// package name taken out of some other macro's argument.
			name:         "control word that merely starts with usepackage",
			src:          "\\usepackagefoo{bar}\n\\begin{document}\n",
			wantAbstains: false,
		},
		{
			// Round 3, and the same shape a third time: a single-byte lookback
			// standing in for tokenization. \\ is a control SYMBOL, so the
			// percent after it starts a comment and TeX never sees the
			// \usepackage -- but the scanner asked only whether the previous
			// BYTE was a backslash and reported pgfplots with complete=true.
			// Confirmed by running it before the fix.
			name:         "percent after an escaped backslash still comments",
			src:          "\\let\\foo\\\\% \\usepackage{pgfplots}\n\\begin{document}\n",
			wantAbstains: false,
		},
		{
			name: "input",
			src:  "\\input{preamble}\n\\begin{document}\n",
		},
		{
			name: "empty conditional",
			src:  "\\ifpdf\\else\\fi\n\\begin{document}\n",
		},
	}
	for _, tc := range hidden {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := convert.ScanTeXPreamble([]byte(tc.src))
			for _, pkg := range got {
				if pkg == "#1" || pkg == "pgfplots" || pkg == "preamble" || pkg == "bar" {
					t.Errorf("scanner reported %q from a construct it does not evaluate: an admission "+
						"gate would refuse this valid document", pkg)
				}
			}
			// Skipping silently is not enough. A skip that left complete==true
			// would mean "no violation found" when it really means "there is a
			// \usepackage here I refused to evaluate" -- and the worker, which
			// is the authority, would never be told to look.
			if tc.wantAbstains && complete {
				t.Errorf("complete = true after skipping an unevaluated \\usepackage: the API would " +
					"treat this scan as authoritative instead of abstaining to the worker")
			}
		})
	}
}

// TestScanTeXPreambleStillSeesRealPackagesAroundConditionals is the other half
// of the fix above. An abstention rule that gave up on any document containing
// an \if would be safe and useless: the gate exists to catch the ordinary case,
// and pandoc's own template ships conditionals in its preamble.
func TestScanTeXPreambleStillSeesRealPackagesAroundConditionals(t *testing.T) {
	src := "\\documentclass{article}\n" +
		"\\usepackage{amsmath}\n" +
		"\\newif\\ifdraft\\draftfalse\n" +
		"\\ifdraft\\usepackage{pgfplots}\\fi\n" +
		"\\usepackage{booktabs}\n" +
		"\\begin{document}\n"

	got, complete := convert.ScanTeXPreamble([]byte(src))

	found := map[string]bool{}
	for _, p := range got {
		found[p] = true
	}
	for _, want := range []string{"amsmath", "booktabs"} {
		if !found[want] {
			t.Errorf("%s was NOT reported -- the conditional made the scanner blind to packages "+
				"outside it, which is a gate that has stopped gating", want)
		}
	}
	if found["pgfplots"] {
		t.Error("pgfplots reported from inside a false conditional")
	}
	if complete {
		t.Error("complete = true, but a \\usepackage inside the conditional was skipped")
	}
}

// TestScanTeXPreambleSkipsFilecontentsBodies is the filecontents form of the
// same rule: the kernel writes the body to a file and does not execute it.
// Reporting a package from that body refuses a document that never loads it.
func TestScanTeXPreambleSkipsFilecontentsBodies(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantPackages []string
		wantComplete bool
		wantDenied   string
	}{
		{
			name: "starred body is not a load",
			src: "\\documentclass{article}\n" +
				"\\begin{filecontents*}{sample.sty}\n" +
				"\\RequirePackage{fontspec}\n" +
				"\\end{filecontents*}\n" +
				"\\begin{document}\n",
			wantComplete: true,
		},
		{
			name: "unstarred body is not a load",
			src: "\\begin{filecontents}{sample.sty}\n" +
				"\\RequirePackage{fontspec}\n" +
				"\\end{filecontents}\n" +
				"\\begin{document}\n",
			wantComplete: true,
		},
		{
			name: "options and a package after the environment",
			src: "\\begin{filecontents*}[overwrite]{sample.sty}\n" +
				"\\RequirePackage{fontspec}\n" +
				"\\end{filecontents*}\n" +
				"\\usepackage{amsmath}\n" +
				"\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			// The kernel ignores the rest of the line that holds the end
			// marker. A package there is not loaded. The next line is.
			name: "package on the end line is ignored",
			src: "\\begin{filecontents*}{sample.sty}\n" +
				"\\end{filecontents*}\\usepackage{fontspec}\n" +
				"\\usepackage{booktabs}\n" +
				"\\begin{document}\n",
			wantPackages: []string{"booktabs"},
			wantComplete: true,
			wantDenied:   "",
		},
		{
			name: "unstarred end does not close the starred environment",
			src: "\\begin{filecontents*}{sample.sty}\n" +
				"\\end{filecontents}\n" +
				"\\RequirePackage{fontspec}\n" +
				"\\end{filecontents*}\n" +
				"\\usepackage{amsmath}\n" +
				"\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name: "unclosed environment abstains",
			src: "\\begin{filecontents*}{sample.sty}\n" +
				"\\RequirePackage{fontspec}\n" +
				"\\usepackage{amsmath}\n" +
				"\\begin{document}\n",
			wantComplete: false,
		},
		{
			name: "package before the environment is still a load",
			src: "\\usepackage{amsmath}\n" +
				"\\begin{filecontents*}{sample.sty}\n" +
				"\\RequirePackage{fontspec}\n" +
				"\\end{filecontents*}\n" +
				"\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := convert.ScanTeXPreamble([]byte(tc.src))
			if complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", complete, tc.wantComplete)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantPackages, ",") {
				t.Errorf("packages = %v, want %v", got, tc.wantPackages)
			}
			for _, pkg := range got {
				if pkg == "fontspec" {
					t.Errorf("scanner reported fontspec from a filecontents body")
				}
			}
			if denied := convert.TeXDeniedInSource([]byte(tc.src)); denied != tc.wantDenied {
				t.Errorf("TeXDeniedInSource = %q, want %q", denied, tc.wantDenied)
			}
		})
	}
}

// TestScanTeXPreambleSkipsVerbatim checks that an example of a package load
// inside verbatim or \verb is not itself a load. An included chapter has no
// \begin{document} to cut the scan, so the skip has to happen here.
func TestScanTeXPreambleSkipsVerbatim(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantPackages []string
	}{
		{
			name: "verbatim example is not a load",
			src:  "\\begin{verbatim}\n\\usepackage{fontspec}\n\\end{verbatim}\n",
		},
		{
			name: "verbatim star example is not a load",
			src:  "\\begin{verbatim*}\n\\RequirePackage{fontspec}\n\\end{verbatim*}\n",
		},
		{
			name:         "a package after verbatim is still a load",
			src:          "\\begin{verbatim}\n\\usepackage{fontspec}\n\\end{verbatim}\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "verb example is not a load",
			src:          "\\verb|\\usepackage{fontspec}|\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "lstlisting example is not a load",
			src:          "\\begin{lstlisting}\n\\usepackage{fontspec}\n\\end{lstlisting}\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "lstinline example is not a load",
			src:          "\\lstinline|\\usepackage{fontspec}|\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "braced lstinline example is not a load",
			src:          "\\lstinline{\\usepackage{fontspec}}\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "lstinline options do not hide a later package",
			src:          "\\lstinline[morekeywords={end}]|\\usepackage{fontspec}|\n\\usepackage{booktabs}\n",
			wantPackages: []string{"booktabs"},
		},
		{
			name:         "lstlisting options do not hide a later package",
			src:          "\\begin{lstlisting}[language=TeX]\n\\usepackage{fontspec}\n\\end{lstlisting}\n\\usepackage{booktabs}\n",
			wantPackages: []string{"booktabs"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := convert.ScanTeXPreamble([]byte(tc.src))
			if strings.Join(got, ",") != strings.Join(tc.wantPackages, ",") {
				t.Errorf("packages = %v, want %v", got, tc.wantPackages)
			}
			if denied := convert.TeXDeniedInSource([]byte(tc.src)); denied != "" {
				t.Errorf("TeXDeniedInSource = %q, want empty", denied)
			}
		})
	}
}

// TestScanTeXPreambleDocumentBoundary checks that only an executable
// \begin{document} ends the preamble. A marker in a comment used to truncate
// the raw text and hide a later load.
func TestScanTeXPreambleDocumentBoundary(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantPackages []string
		wantComplete bool
		wantDenied   string
	}{
		{
			name:         "a commented marker does not hide the following package",
			src:          "% \\begin{document}\n\\usepackage{fontspec}\n",
			wantPackages: []string{"fontspec"},
			wantDenied:   "fontspec",
		},
		{
			name:         "the real marker still ends the preamble",
			src:          "% \\begin{document}\n\\usepackage{fontspec}\n\\begin{document}\n\\usepackage{amsmath}\n",
			wantPackages: []string{"fontspec"},
			wantComplete: true,
			wantDenied:   "fontspec",
		},
		{
			name:         "a marker inside a macro does not end the preamble",
			src:          "\\newcommand{\\x}{\\begin{document}}\n\\usepackage{amsmath}\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name:         "a marker inside a conditional does not end the preamble",
			src:          "\\iffalse\\begin{document}\\fi\n\\usepackage{amsmath}\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name: "a marker written by filecontents does not end the preamble",
			src: "\\begin{filecontents*}{sample.tex}\n\\begin{document}\n\\usepackage{fontspec}\n\\end{filecontents*}\n" +
				"\\usepackage{amsmath}\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := convert.ScanTeXPreamble([]byte(tc.src))
			if complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", complete, tc.wantComplete)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantPackages, ",") {
				t.Errorf("packages = %v, want %v", got, tc.wantPackages)
			}
			if denied := convert.TeXDeniedInSource([]byte(tc.src)); denied != tc.wantDenied {
				t.Errorf("TeXDeniedInSource = %q, want %q", denied, tc.wantDenied)
			}
		})
	}
}

// TestScanTeXPreambleStopsAtEndDocument checks an included chapter, which
// has no \begin{document}. LaTeX still ends the job at \end{document}.
func TestScanTeXPreambleStopsAtEndDocument(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantPackages []string
	}{
		{
			name: "a package after the terminator is not a load",
			src:  "chapter text\n\\end{document}\n\\usepackage{fontspec}\n",
		},
		{
			name: "a package on the terminator line is not a load",
			src:  "\\end{document}\\usepackage{fontspec}\n",
		},
		{
			name:         "a package before the terminator is still a load",
			src:          "\\usepackage{amsmath}\n\\end{document}\n\\usepackage{fontspec}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "a terminator inside a conditional does not hide a later package",
			src:          "\\iffalse\\end{document}\\fi\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "a terminator inside a macro does not hide a later package",
			src:          "\\newcommand{\\x}{\\end{document}}\n\\usepackage{amsmath}\n",
			wantPackages: []string{"amsmath"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := convert.ScanTeXPreamble([]byte(tc.src))
			if complete {
				t.Errorf("complete = true, want false")
			}
			if strings.Join(got, ",") != strings.Join(tc.wantPackages, ",") {
				t.Errorf("packages = %v, want %v", got, tc.wantPackages)
			}
			if denied := convert.TeXDeniedInSource([]byte(tc.src)); denied != "" {
				t.Errorf("TeXDeniedInSource = %q, want empty", denied)
			}
		})
	}
}

// TestScanTeXPreambleStopsAfterEndinput checks the primitive's own rule: TeX
// finishes the line that holds a top-level \endinput and does not read the
// rest of the file.
func TestScanTeXPreambleStopsAfterEndinput(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantPackages []string
		wantComplete bool
	}{
		{
			name: "the next line of a style is not a load",
			src:  "\\ProvidesPackage{custom}\n\\endinput\n\\RequirePackage{fontspec}\n",
		},
		{
			name:         "a declaration on the same line is still a load",
			src:          "\\endinput\\RequirePackage{amsmath}\n\\RequirePackage{fontspec}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name:         "a package before endinput is still a load",
			src:          "\\RequirePackage{amsmath}\n\\endinput\n\\RequirePackage{fontspec}\n",
			wantPackages: []string{"amsmath"},
		},
		{
			name: "begin document after endinput does not revive the trailing package",
			src:  "\\endinput\n\\usepackage{fontspec}\n\\begin{document}\n",
		},
		{
			name:         "endinput inside a conditional does not hide the package after fi",
			src:          "\\iffalse\\endinput\\fi\n\\usepackage{amsmath}\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
		{
			name:         "endinput inside a macro does not hide a later package",
			src:          "\\newcommand{\\x}{\\endinput}\n\\usepackage{amsmath}\n\\begin{document}\n",
			wantPackages: []string{"amsmath"},
			wantComplete: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := convert.ScanTeXPreamble([]byte(tc.src))
			if complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", complete, tc.wantComplete)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantPackages, ",") {
				t.Errorf("packages = %v, want %v", got, tc.wantPackages)
			}
			if denied := convert.TeXDeniedInSource([]byte(tc.src)); denied != "" {
				t.Errorf("TeXDeniedInSource = %q, want empty", denied)
			}
		})
	}
}

// TestTeXUnsupportedPackageErrorRoundTrips holds the sentinel and its name
// extraction together. Both gates format their client-facing message from
// TeXUnsupportedPackageName, so a change that broke extraction would produce
// two refusals that name nothing, in two places, with every other test green.
func TestTeXUnsupportedPackageErrorRoundTrips(t *testing.T) {
	err := convert.TeXUnsupportedPackageError("pgfplots")
	if !errors.Is(err, convert.ErrTeXUnsupportedPackage) {
		t.Fatalf("errors.Is(err, ErrTeXUnsupportedPackage) = false for %v", err)
	}
	if got := convert.TeXUnsupportedPackageName(err); got != "pgfplots" {
		t.Fatalf("TeXUnsupportedPackageName = %q, want %q", got, "pgfplots")
	}
	if got := convert.TeXUnsupportedPackageName(errors.New("something else")); got != "" {
		t.Fatalf("TeXUnsupportedPackageName of an unrelated error = %q, want \"\"", got)
	}
	// Wrapped one layer deeper, which is how the worker will carry it.
	wrapped := errors.Join(errors.New("convert"), err)
	if got := convert.TeXUnsupportedPackageName(wrapped); got != "pgfplots" {
		t.Fatalf("TeXUnsupportedPackageName of a wrapped error = %q, want %q", got, "pgfplots")
	}
}
