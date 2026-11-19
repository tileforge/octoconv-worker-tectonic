package convert

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TECTONIC_ENGINE_TIMEOUT is ceil(25.76 s/pass x 7 passes x 1.5), and the 7 is
// the ENGINE's, not ours. This asserts it still is.
//
// The term matters more than the others in that formula: the per-pass cost is
// measured on a document we generate and the margin is the operator's, but 7 is
// a property of a binary we pin by digest and could raise. If a future tectonic
// reruns further before giving up, the derivation silently starts describing a
// different bound, and nothing would say so.
//
// A CEILING OF OUR OWN WOULD BE BETTER, AND DOES NOT EXIST. The follow-up this
// test comes from proposed "tectonic -r N, making the 7 ours". Measured against
// the real CLI, -r is not a ceiling: "Rerun the TeX engine exactly this many
// times after the first" FORCES that many passes. On a document that converges
// in two, -r 6 took 1.076s -> 2.437s for the same output, so adopting it would
// have made every ordinary job pay the worst case. -Zhelp lists no bounding
// option either (0.17.0). So the 7 stays the engine's, and this test is what
// keeps that honest.
func TestEngineStopsAtSevenPasses(t *testing.T) {
	bundle := requireTectonicBundle(t)
	dir := t.TempDir()

	// A bibliography forces reruns: biblatex on backend=bibtex never converges
	// within the engine's cap, which is exactly why the corpus uses it to
	// measure the worst case.
	doc := filepath.Join(dir, "passes.tex")
	src := `\documentclass{article}
\usepackage[backend=bibtex]{biblatex}
\begin{filecontents}{\jobname.bib}
@book{k, author = {D. Knuth}, title = {The TeXbook}, publisher = {AW}, year = {1984}}
\end{filecontents}
\addbibresource{\jobname.bib}
\begin{document}
Cited~\cite{k}. Section~\ref{s} is below.
\section{S}\label{s}
\printbibliography
\end{document}
`
	if err := os.WriteFile(doc, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := runCommand(context.Background(), "tectonic", tectonicArgs(doc, dir, bundle)...)

	// The engine says so itself, and the message is what 64-05's derivation
	// quotes. Matching its own words rather than counting passes keeps this
	// test measuring the ENGINE's statement, not our arithmetic about it.
	const says = "stopping at 6 passes"
	if !bytes.Contains(out, []byte(says)) {
		t.Fatalf("the engine no longer says %q on a document that cannot converge.\n\n"+
			"TECTONIC_ENGINE_TIMEOUT = ceil(25.76 s/pass x 7 passes x 1.5) = 271s rests on that "+
			"number being the engine's own maximum. If it reruns further now, the timeout describes "+
			"a smaller bound than it claims and 64-05's measurement has to be re-run.\n\nengine tail:\n%s",
			says, tailLines(out, 12))
	}

	// Seven TeX invocations: the initial pass plus six reruns. The engine's
	// message counts reruns; the formula counts passes, and the two differing
	// by one is exactly the kind of off-by-one a derivation should not carry
	// implicitly.
	if got := bytes.Count(out, []byte("Transcript written")); got != 7 {
		t.Fatalf("transcripts = %d, want 7 (one initial pass + six reruns): the formula's pass count "+
			"and the engine's rerun count have drifted apart", got)
	}
}

func tailLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
