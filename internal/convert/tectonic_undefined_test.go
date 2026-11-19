package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUndefinedReferencesReadsLaTeXsOwnWarning is the round-2 half of a check
// this PR has now got wrong in both directions, each time caught by running it.
//
//	first version   matched the whole log  -> failed 64-04's paper and both
//	                                          bibliography corpus documents,
//	                                          which resolve everything by the
//	                                          last pass
//	second version  matched the phrase     -> a document can PRINT the phrase
//	                                          (\typeout), and a valid PDF was
//	                                          thrown away for it
//
// Both fixtures are real runs of the shipped image, captured before this test
// was written: testdata/tectonic/PROVENANCE.md.
func TestUndefinedReferencesReadsLaTeXsOwnWarning(t *testing.T) {
	t.Run("a document with genuinely undefined references", func(t *testing.T) {
		out := readTectonicFixture(t, "undefined-references.txt")
		if !strings.Contains(string(out), "LaTeX Warning: There were undefined references.") {
			t.Fatal("the fixture no longer carries LaTeX's warning -- it cannot prove the positive case")
		}
		if !tectonicUndefinedReferences(out) {
			t.Fatal("a PDF full of ?? would be published as a finished job")
		}
	})

	t.Run("a document that merely prints the phrase", func(t *testing.T) {
		out := readTectonicFixture(t, "forged-undefined-references.txt")
		// The fixture's value is exactly this: the phrase is present, and
		// LaTeX's warning is not.
		if !strings.Contains(string(out), "There were undefined references") {
			t.Fatal("the fixture no longer contains the phrase -- it cannot prove the negative case")
		}
		if strings.Contains(string(out), "LaTeX Warning: There were undefined") {
			t.Fatal("the fixture contains the real warning after all; it is the wrong capture for this test")
		}
		if tectonicUndefinedReferences(out) {
			t.Fatal("\\typeout of the phrase was believed: this document typesets correctly and its " +
				"PDF would be discarded with a terminal error")
		}
	})
}

// TestUndefinedReferencesStaysOnTheFinalPass keeps the scope that made the
// first version wrong. The warning is NORMAL on early passes -- the .aux does
// not exist yet -- so a check that reads the whole log fails every multi-pass
// document, including this phase's own end-to-end proof.
func TestUndefinedReferencesStaysOnTheFinalPass(t *testing.T) {
	early := "LaTeX Warning: There were undefined references.\n" +
		"Transcript written on doc.log.\n" +
		"note: Rerunning TeX ...\n" +
		"[1] (doc.aux)\n" +
		"Transcript written on doc.log.\n" +
		"note: Writing `doc.pdf`\n"
	if tectonicUndefinedReferences([]byte(early)) {
		t.Fatal("a warning from an EARLY pass was counted: every document with a \\ref would fail")
	}

	late := "[1] (doc.aux)\n" +
		"Transcript written on doc.log.\n" +
		"note: Rerunning TeX ...\n" +
		"LaTeX Warning: There were undefined references.\n" +
		"Transcript written on doc.log.\n"
	if !tectonicUndefinedReferences([]byte(late)) {
		t.Fatal("a warning from the FINAL pass was missed")
	}
}

// TestTectonicTailIsBoundedInBytes covers the path round 2 did not: the log
// BUFFER was capped, and the slice of it that gets written into
// job_events.detail for every failure was still bounded only by a line count.
// TeX wraps terminal output, so in practice twelve lines is small -- but a
// database row should not rest on "in practice".
func TestTectonicTailIsBoundedInBytes(t *testing.T) {
	oneHugeLine := make([]byte, 1<<20)
	for i := range oneHugeLine {
		oneHugeLine[i] = 'A'
	}
	got := tectonicTail(oneHugeLine)
	if len(got) > 4200 {
		t.Fatalf("tectonicTail returned %d bytes from a single 1 MiB line: that string is persisted "+
			"per failed job", len(got))
	}
	if !strings.HasSuffix(got, "AAA") {
		t.Fatalf("the END was not kept; the cause of a TeX failure is at the end of its log")
	}

	// The ordinary case must be untouched -- a truncating tail that also
	// mangles normal output would hide every real diagnostic.
	normal := "! LaTeX Error: File `pgfplots.sty' not found.\nEmergency stop.\n"
	if tail := tectonicTail([]byte(normal)); !strings.Contains(tail, "pgfplots.sty") {
		t.Fatalf("normal output was damaged: %q", tail)
	}
}

// TestValidateTectonicBundleRefusesWhatStatAccepts is the round-3 finding as an
// invariant. os.Stat succeeds for a directory and for a file this process
// cannot open; both make the worker start, log its bundle, and consume the
// queue while unable to typeset anything.
func TestValidateTectonicBundleRefusesWhatStatAccepts(t *testing.T) {
	dir := t.TempDir()

	t.Run("a directory", func(t *testing.T) {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("precondition: %v", err)
		}
		if err := ValidateTectonicBundle(dir); err == nil {
			t.Fatal("a directory passed validation -- os.Stat accepts it, which is the whole point")
		}
	})

	t.Run("an empty file", func(t *testing.T) {
		empty := filepath.Join(dir, "empty.ttb")
		if err := os.WriteFile(empty, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateTectonicBundle(empty); err == nil {
			t.Fatal("a zero-byte bundle passed validation")
		}
	})

	t.Run("an unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permission bits do not apply")
		}
		locked := filepath.Join(dir, "locked.ttb")
		if err := os.WriteFile(locked, []byte("bundle"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(locked); err != nil {
			t.Fatalf("precondition: stat should still succeed, got %v", err)
		}
		if err := ValidateTectonicBundle(locked); err == nil {
			t.Fatal("a file this process cannot open passed validation -- the worker runs as nobody, " +
				"and a bundle readable only by root stats perfectly")
		}
	})

	t.Run("a real file passes", func(t *testing.T) {
		ok := filepath.Join(dir, "ok.ttb")
		if err := os.WriteFile(ok, []byte("bundle bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateTectonicBundle(ok); err != nil {
			t.Fatalf("a readable regular file was refused: %v", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if err := ValidateTectonicBundle(""); err == nil {
			t.Fatal("an unset path passed validation; tectonic's fallback is a NETWORK FETCH")
		}
	})
}

// TestDeniedPackagesFollowWhatTheDocumentIncludes covers both failures this
// scan has had, one on each side.
//
// By EXTENSION was a bypass: \input{preamble} reads a file named exactly
// "preamble", and an extensionless upload carrying \usepackage{fontspec} was
// read, loaded from the bundle and typeset.
//
// By "looks like text" was the opposite: a project rendering a source listing
// with \lstinputlisting{sample.txt} would be REFUSED because the listing quotes
// the directive it is showing. Refusing valid work is the more expensive of the
// two, and it is the one this project has paid for most.
//
// So the walk follows what TeX follows.
func TestDeniedPackagesFollowWhatTheDocumentIncludes(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(main string) string {
		for _, p := range texSourceClosure(main) {
			src, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if pkg := TeXDeniedInSource(src); pkg != "" {
				return pkg
			}
		}
		return ""
	}

	t.Run("an extensionless file the document inputs is source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{noext}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "noext", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("denied package not found (got %q): a name is not what makes a file TeX source", got)
		}
	})

	t.Run("a listing the document renders is data", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{listings}\n"+
			"\\begin{document}\\lstinputlisting{sample.txt}\\end{document}\n")
		// A perfectly ordinary thing to show in a paper about LaTeX.
		write(t, dir, "sample.txt", "To use system fonts you would write \\usepackage{fontspec}.\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("a rendered listing was read as a preamble and refused the job over %q: the file "+
				"is data, and its contents are not directives", got)
		}
	})

	t.Run("transitively, through a chain of inputs", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{a}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "a.tex", "\\input{b}\n")
		write(t, dir, "b.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("the chain was not followed (got %q)", got)
		}
	})

	t.Run("a file nobody includes is not scanned", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "orphan.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("an uploaded-but-unused file refused the job over %q: TeX never reads it", got)
		}
	})

	t.Run("an include inside an unused macro is not source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\newcommand{\\unused}{\\input{legacy}}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "legacy.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("legacy.tex was scanned (got %q): the macro that inputs it is never called", got)
		}
	})

	t.Run("an empty package option still reaches the uploaded style", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage[]{custom}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "custom.sty", "\\RequirePackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("denied package not found (got %q): \\usepackage[]{custom} still loads custom.sty", got)
		}
	})

	t.Run("a comma inside an input name is one file", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{safe,legacy}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "safe,legacy.tex", "\\newcommand{\\x}{y}\n")
		write(t, dir, "legacy.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("legacy.tex was scanned (got %q): \\input{safe,legacy} reads one file", got)
		}
		write(t, dir, "safe,legacy.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("the comma-named source was not scanned (got %q)", got)
		}
	})

	t.Run("the first existing input file is the only one", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{chapter.tex}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "chapter.tex", "\\newcommand{\\x}{y}\n")
		write(t, dir, "chapter.tex.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("chapter.tex.tex was scanned (got %q): \\input{chapter.tex} opens chapter.tex", got)
		}
	})

	t.Run("a repeated include does not fall through to another extension", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{chapter}\n\\input{chapter}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "chapter.tex", "\\newcommand{\\x}{y}\n")
		write(t, dir, "chapter.ltx", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("chapter.ltx was scanned (got %q): the second \\input{chapter} already selected chapter.tex", got)
		}
	})

	t.Run("verbatim text is not source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\begin{verbatim}\n\\input{legacy}\n\\end{verbatim}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "legacy.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("legacy.tex was scanned (got %q): the include is inside verbatim", got)
		}
	})

	t.Run("a package after endinput in an uploaded style is not a load", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{custom}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "custom.sty", "\\ProvidesPackage{custom}\n\\endinput\n\\RequirePackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("fontspec was reported (got %q): TeX stops reading custom.sty at \\endinput", got)
		}
	})

	t.Run("a package on the endinput line of an uploaded style is a load", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{custom}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "custom.sty", "\\endinput\\RequirePackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("fontspec was not reported (got %q): TeX still reads the rest of the \\endinput line", got)
		}
	})

	t.Run("an include written by filecontents is not source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n"+
			"\\begin{filecontents*}{example.tex}\n\\input{legacy}\n\\end{filecontents*}\n"+
			"\\begin{document}x\\end{document}\n")
		write(t, dir, "legacy.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("legacy.tex was scanned (got %q): filecontents only writes the example", got)
		}
	})

	t.Run("an include after filecontents is still source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n"+
			"\\begin{filecontents*}[overwrite]{example.tex}\n\\input{legacy}\n\\end{filecontents*}\n"+
			"\\input{real}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "legacy.tex", "\\newcommand{\\x}{y}\n")
		write(t, dir, "real.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("real.tex was not scanned (got %q): the include after filecontents does execute", got)
		}
	})

	t.Run("an include inside a conditional is not source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\iffoo\\input{legacy}\\fi\n\\begin{document}x\\end{document}\n")
		write(t, dir, "legacy.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "" {
			t.Fatalf("legacy.tex was scanned (got %q): the include sits inside a conditional", got)
		}
	})
}

// TestIncludedNamesAreParsedNotMatched covers both directions a regexp got
// wrong, each measured against the real engine before this became a parser.
func TestIncludedNamesAreParsedNotMatched(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
		why  string
	}{
		{
			// "article" is here because \documentclass names a file TeX reads
			// AS SOURCE when it resolves locally -- an uploaded article.cls
			// would be read, and one carrying a denied package was how round
			// 5's second P1 reached the engine. The CALLER resolves the name;
			// a class the bundle provides matches no local file and costs
			// nothing.
			name: "the braceless primitive form",
			src:  "\\documentclass{article}\n\\input nobrace\n\\begin{document}x\\end{document}\n",
			want: []string{"article", "nobrace"},
			why:  "TeX reads nobrace.tex; a brace-only pattern missed it, so a preamble could hide there",
		},
		{
			name: "a commented-out include",
			src:  "\\documentclass{article}\n% \\input{old}\n\\begin{document}x\\end{document}\n",
			want: []string{"article"},
			why:  "TeX reads nothing from the comment, and pulling old.tex into the scan could refuse a job over a line that never executes",
		},
		{
			name: "a longer control word that merely starts with input",
			src:  "\\inputencoding{utf8}\n",
			want: nil,
			why:  "\\inputencoding is not \\input -- the same lesson \\fi and \\usepackage already cost this package",
		},
		{
			name: "the ordinary braced form, and \\include",
			src:  "\\input{preamble}\n\\include{chapter1}\n",
			want: []string{"preamble", "chapter1"},
		},
		{
			name: "data-reading commands are not source",
			src:  "\\lstinputlisting{sample.txt}\n\\addbibresource{refs.bib}\n",
			want: nil,
			why:  "neither makes its target TeX source, and treating them as if they did refused a paper for quoting a directive",
		},
		{
			name: "an include inside an unused macro body",
			src:  "\\newcommand{\\unused}{\\input{legacy}}\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "TeX does not read legacy.tex unless \\unused is called, and scanning it refused a valid document",
		},
		{
			name: "an include inside a conditional",
			src:  "\\iffoo\\input{legacy}\\fi\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "a false conditional still compiles, and the include inside it is not an executed dependency",
		},
		{
			name: "a newif declaration does not swallow the next include",
			src:  "\\newif\\ifdraft\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "\\newif\\ifdraft declares a conditional; treating \\ifdraft as an open \\if skipped every later include",
		},
		{
			name: "an empty package option is not the filename",
			src:  "\\usepackage[]{custom}\n",
			want: []string{"custom"},
			why:  "reading [] as the filename left custom.sty out of the closure while TeX still loaded it",
		},
		{
			name: "a class option is not the filename",
			src:  "\\documentclass[11pt,a4paper]{mine}\n",
			want: []string{"mine"},
			why:  "the commas belong to the option list, and the class name is the brace group after it",
		},
		{
			name: "a comma in an input argument is part of one filename",
			src:  "\\input{safe,legacy}\n",
			want: []string{"safe,legacy"},
			why:  "\\input reads one file; splitting on the comma followed legacy.tex, which TeX never opens",
		},
		{
			name: "a package list still names each package after its options",
			src:  "\\usepackage[draft]{alpha,beta}\n",
			want: []string{"alpha", "beta"},
			why:  "\\usepackage{a,b} loads both, and the option group is not one of the names",
		},
		{
			name: "an include shown in verbatim is not source",
			src:  "\\begin{verbatim}\n\\input{legacy}\n\\end{verbatim}\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "verbatim prints the example; TeX does not read legacy.tex",
		},
		{
			name: "an include after endinput is not source",
			src:  "\\input{preamble}\n\\endinput\n\\input{legacy}\n",
			want: []string{"preamble"},
			why:  "TeX finishes the \\endinput line and does not read legacy.tex",
		},
		{
			name: "an include on the endinput line is source",
			src:  "\\endinput\\input{legacy}\n\\input{preamble}\n",
			want: []string{"legacy"},
			why:  "TeX still executes tokens on the same line as \\endinput",
		},
		{
			name: "an include written by filecontents is not source",
			src:  "\\begin{filecontents*}{example.tex}\n\\input{legacy}\n\\end{filecontents*}\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "filecontents writes the example; TeX does not read legacy.tex",
		},
		{
			name: "an include on the filecontents end line is not source",
			src:  "\\begin{filecontents}{example.tex}\n\\end{filecontents}\\input{legacy}\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "the kernel discards the rest of the line that holds the end marker",
		},
		{
			name: "an include inside verb is not source",
			src:  "\\verb|\\input{legacy}|\n\\input{preamble}\n",
			want: []string{"preamble"},
			why:  "\\verb prints its body, and the \\input inside it is not executed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, inc := range texIncludedNames([]byte(tc.src)) {
				got = append(got, inc.name)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("texIncludedNames = %v, want %v\n  %s", got, tc.want, tc.why)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("texIncludedNames[%d] = %q, want %q\n  %s", i, got[i], tc.want[i], tc.why)
				}
			}
		})
	}
}

// TestClosureFollowsPackagesAndIsBoundedByFiles is round 5's two P1s, which are
// the same defect from opposite ends: what the walk follows, and what stops it.
func TestClosureFollowsPackagesAndIsBoundedByFiles(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(main string) string {
		for _, p := range texSourceClosure(main) {
			src, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if pkg := TeXDeniedInSource(src); pkg != "" {
				return pkg
			}
		}
		return ""
	}

	t.Run("an uploaded .sty is source, and reached through a preamble", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{preamble}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "preamble.tex", "\\usepackage{custom}\n")
		// \usepackage{custom} reads THIS file before it looks in the bundle.
		write(t, dir, "custom.sty", "\\RequirePackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("denied package not found (got %q): the walk followed \\input but not the package "+
				"loaders, so an uploaded .sty was invisible to it while TeX read it", got)
		}
	})

	t.Run("an uploaded .cls is source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{mine}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "mine.cls", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("a local document class was not followed (got %q)", got)
		}
	})

	t.Run("repeated directives do not exhaust the walk", func(t *testing.T) {
		dir := t.TempDir()
		var b strings.Builder
		b.WriteString("\\documentclass{article}\n")
		// Harmless repetition: 200 directives, one file.
		for i := 0; i < 200; i++ {
			b.WriteString("\\input{safe}\n")
		}
		b.WriteString("\\input{preamble}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "in.latex", b.String())
		write(t, dir, "safe.tex", "\\newcommand{\\x}{y}\n")
		write(t, dir, "preamble.tex", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("the walk gave up before reaching preamble.tex (got %q): it was bounded by "+
				"DIRECTIVES, and repeating one harmless include spends that bound on a single file", got)
		}
	})

	t.Run("a comma-separated package list names each one", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{alpha,beta}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "beta.sty", "\\usepackage{fontspec}\n")
		if got := scan(filepath.Join(dir, "in.latex")); got != "fontspec" {
			t.Fatalf("the second name in a list was not followed (got %q)", got)
		}
	})

	t.Run("a package the bundle provides is not a local file", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{amsmath}\n\\begin{document}x\\end{document}\n")
		// Nothing named amsmath.sty here: it resolves in the bundle, which is
		// ours, and the walk must not invent a file for it.
		if got := texSourceClosure(filepath.Join(dir, "in.latex")); len(got) != 1 {
			t.Fatalf("closure = %v, want just the document: a bundle package is not an uploaded file", got)
		}
	})
}

// TestEachDirectiveResolvesOnlyItsOwnExtensions is the third time this walk has
// been wrong in the over-refusal direction, and the reason it keeps happening
// is worth stating: every fix here closes a bypass, and closing a bypass means
// looking at MORE files, and looking at more files is how a data file gets read
// as a preamble.
//
// \usepackage{listings} loads listings.sty. It never loads listings.tex -- so
// an attached data listing named after a bundle package must not be pulled into
// the scan, however text-shaped it is and whatever it quotes.
func TestEachDirectiveResolvesOnlyItsOwnExtensions(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a data file named after a bundle package is not source", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{listings}\n"+
			"\\begin{document}\\lstinputlisting{listings.tex}\\end{document}\n")
		write(t, dir, "listings.tex", "To use system fonts you would write \\usepackage{fontspec}.\n")

		for _, p := range texSourceClosure(filepath.Join(dir, "in.latex")) {
			if filepath.Base(p) == "listings.tex" {
				t.Fatal("listings.tex was pulled into the closure by \\usepackage{listings}: that " +
					"directive loads listings.STY, and the .tex is a listing the paper renders")
			}
		}
	})

	t.Run("but an uploaded .sty for that name still is", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\usepackage{custom}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "custom.sty", "\\RequirePackage{fontspec}\n")
		found := false
		for _, p := range texSourceClosure(filepath.Join(dir, "in.latex")) {
			if filepath.Base(p) == "custom.sty" {
				found = true
			}
		}
		if !found {
			t.Fatal("custom.sty was not followed: \\usepackage reads an uploaded .sty before the bundle")
		}
	})

	t.Run("and \\input still tries the bare name and .tex", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "in.latex", "\\documentclass{article}\n\\input{noext}\n\\input{withext}\n\\begin{document}x\\end{document}\n")
		write(t, dir, "noext", "x\n")
		write(t, dir, "withext.tex", "y\n")
		var names []string
		for _, p := range texSourceClosure(filepath.Join(dir, "in.latex")) {
			names = append(names, filepath.Base(p))
		}
		for _, want := range []string{"noext", "withext.tex"} {
			found := false
			for _, n := range names {
				if n == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s missing from %v", want, names)
			}
		}
	})
}
