package convert

// This file (texpreamble.go): a bounded scan of a LaTeX preamble for the
// packages it loads, used by BOTH gates in TEX-03 -- cmd/api's admission
// refusal and cmd/tectonic-worker's terminal classification.
//
// IT IS A SCANNER, NOT A PARSER, AND THAT IS NOT A CAVEAT -- IT IS THE DESIGN
//
// TeX is a macro language whose expansion is Turing-complete. This scanner
// does NOT follow \input or \include, does NOT expand macros, and does NOT
// understand \if.../\else/\fi. Every one of those is a way for a \usepackage
// to be invisible to it, and none of them is a bug here:
//
//	\newcommand{\loadit}[1]{\usepackage{#1}}   % invisible: not expanded
//	\input{preamble}                            % invisible: not followed
//	\ifpdf\usepackage{pgfplots}\fi              % visible, but may not execute
//
// The scanner is allowed to MISS packages, and the worker gate exists exactly
// for what it misses. What it must never do is INVENT one: a false positive is
// a valid document refused at admission, which is the failure 63-02 removed a
// whole gate for.
//
// This project has a recorded history of a bounded scanner being quietly
// treated as authoritative -- 63-02's page-count gate abstained on precisely
// the long documents it existed to refuse, because a PDF page tree is a tree.
// Hence `complete`, returned rather than kept internal: a caller that ignores
// it has made a decision, not an oversight.

import (
	"errors"
	"fmt"
	"strings"
)

// ErrTeXUnsupportedPackage is the sentinel both gates carry. Wrap it with
// TeXUnsupportedPackageError so the message names the package -- a refusal
// that does not say WHICH package leaves the client removing \usepackage lines
// one at a time, one upload each.
var ErrTeXUnsupportedPackage = errors.New("tex: package is not in the supported set")

// TeXUnsupportedPackageError wraps ErrTeXUnsupportedPackage with the offending
// name.
func TeXUnsupportedPackageError(pkg string) error {
	return fmt.Errorf("%w: %s", ErrTeXUnsupportedPackage, pkg)
}

// TeXUnsupportedPackageName extracts the package name from an error produced
// by TeXUnsupportedPackageError, or "" if err is not one.
//
// Exists so the worker's failure message and the API's can be built from the
// same value rather than each formatting its own string around the same
// sentinel -- two messages for one condition drift, and this project has a
// name for that shape.
func TeXUnsupportedPackageName(err error) string {
	if !errors.Is(err, ErrTeXUnsupportedPackage) {
		return ""
	}
	msg := err.Error()
	i := strings.LastIndex(msg, ErrTeXUnsupportedPackage.Error()+": ")
	if i < 0 {
		return ""
	}
	name := msg[i+len(ErrTeXUnsupportedPackage.Error())+2:]
	if j := strings.IndexAny(name, " :\n"); j >= 0 {
		name = name[:j]
	}
	return name
}

// beginDocument ends the preamble. A \usepackage after it is not a preamble
// entry (LaTeX will error on it anyway), and treating it as one would let a
// document past the window extend its own preamble arbitrarily.
const beginDocument = `\begin{document}`

// ScanTeXPreamble collects the package names a LaTeX preamble loads, from an
// already-read prefix of the file.
//
// It returns the names in source order (duplicates preserved -- a document
// that loads a package twice says so twice, and a caller counting occurrences
// is not this function's business) and `complete`, which is true only when
// \begin{document} was seen inside the prefix.
//
// complete == false means the preamble ran past the window: some of it was not
// read at all, so the absence of a violation says nothing. That is the whole
// reason the worker gate exists, which is why it is a return value.
//
// THE PREFIX FORM, NOT AN io.ReaderAt FORM. Phase 59 D-12 split every markup
// matcher in two precisely so a caller needing several answers about one
// upload reads ONE window instead of one per matcher, and handleCreateJob is
// already holding that window for LooksLikeLaTeXFromPrefix. A second read here
// would be a second answer to one question about one file.
func ScanTeXPreamble(prefix []byte) (packages []string, complete bool) {
	complete = VisitTeXPreamblePackages(prefix, func(pkg string) bool {
		packages = append(packages, pkg)
		return true
	})
	return packages, complete
}

func VisitTeXPreamblePackages(prefix []byte, visit func(pkg string) bool) (complete bool) {
	src := string(prefix)

	// Cut at \begin{document} first, so everything below scans the preamble
	// only. Done on the raw text rather than after comment-stripping because
	// the marker itself cannot be inside a comment and still open a document.
	if i := strings.Index(src, beginDocument); i >= 0 {
		src = src[:i]
		complete = true
	}

	// Walk the preamble once, tracking two things a raw substring search
	// cannot see: whether we are inside a BRACE GROUP (a macro definition
	// body) and whether we are inside a CONDITIONAL (\if...\fi). A
	// \usepackage in either place is text the engine may never execute, so
	// reporting it is a REFUSAL OF A VALID DOCUMENT -- the one thing this
	// scanner must never do.
	//
	// Found in review, and it was not hypothetical:
	//
	//	\newif\iffoo \foofalse
	//	\iffoo\usepackage{pgfplots}\fi
	//
	// compiles cleanly (verified in the built image), and the first version of
	// this scanner reported pgfplots for it -- so the API answered 422
	// "does not provide the package pgfplots" to a document that does not use
	// it. The same shape applies to \newcommand{\x}{\usepackage{y}}.
	//
	// Skipping is not enough on its own: silence would then mean "no
	// violation" when it really means "there is a \usepackage here I refused
	// to evaluate". So every skip also clears `complete`, which routes the
	// document to the worker -- the gate abstains exactly as it does when the
	// preamble outruns the window.
	var (
		braceDepth int
		ifDepth    int
	)
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '%':
			// A comment runs to the end of the line. No lookback for an escape:
			// \% is a control SYMBOL and is consumed as one token below, so a
			// percent reached HERE is always a live comment character.
			//
			// The lookback this replaces asked whether the previous byte was a
			// backslash, which gets \\% wrong -- \\ is itself a control symbol,
			// so the percent after it starts a comment. Found in review (PR #16,
			// codex round 3) and confirmed by running the scanner:
			//
			//	\let\foo\\% \usepackage{pgfplots}
			//
			// returned packages=[pgfplots] complete=true, so the API refused a
			// document in which TeX never loads pgfplots at all.
			if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = len(src)
			}
			continue
		case '{':
			braceDepth++
			continue
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
			continue
		case '\\':
		default:
			continue
		}

		rest := src[i:]
		// A TeX control word is a backslash followed by a MAXIMAL run of
		// letters, so the comparisons below are on the whole word and never on
		// a prefix of it. Prefix matching here was a real defect (PR #16, codex
		// round 2), and it failed in the dangerous direction:
		//
		//	\iffoo\finalfoo\usepackage{pgfplots}\fi
		//
		// \finalfoo starts with "fi", so a prefix test closed the conditional,
		// and the \usepackage TeX never executes was reported as executed --
		// the API then refused, with "unsupported_package", a document that
		// compiles. Confirmed by running the scanner before this was changed:
		// packages=[pgfplots] complete=true.
		//
		// The same applies to the two directives below: \usepackagefoo{bar} is
		// a macro named usepackagefoo, not \usepackage{bar}, and reading it as
		// one would invent a package name out of a macro's argument.
		word, wordLen := texControlWord(rest)
		// A control SYMBOL -- a backslash and one non-letter -- is a single
		// token, and consuming it here is what makes the comment branch above
		// safe: \% never reaches it, and \\ does not leave a trailing
		// backslash for anything to misread. It also stops \{ and \} from
		// moving braceDepth, which they never should have.
		if len(word) == 1 && !isASCIILetter(word[0]) {
			i += wordLen - 1
			continue
		}
		// \newif declares a conditional; the \if<name> that follows is not a
		// control word this scanner can evaluate, so track the span rather
		// than the name.
		switch {
		case word == "fi":
			if ifDepth > 0 {
				ifDepth--
			}
			i += wordLen - 1
			continue
		case word == "newif":
			// \newif\ifdraft DECLARES a conditional; it does not open one, and
			// the \ifdraft directly after it is part of the declaration.
			//
			// Consuming only the \newif was a real bug, caught by the test that
			// asserts packages AROUND a conditional are still seen: the trailing
			// \ifdraft was then read as an opening \if, ifDepth never returned
			// to zero, and every \usepackage after the declaration was skipped.
			// An abstention rule that swallows the rest of the preamble is safe
			// and useless -- it turns the gate off while looking like it works.
			i += wordLen - 1
			for i+1 < len(src) && (src[i+1] == ' ' || src[i+1] == '\t') {
				i++
			}
			if strings.HasPrefix(src[i+1:], `\if`) {
				i++ // the backslash
				for i+1 < len(src) && isASCIILetter(src[i+1]) {
					i++
				}
			}
			continue
		case strings.HasPrefix(word, "if"):
			// Deliberately over-inclusive, and deliberately NOT symmetrical
			// with the close above. \ifthenelse (ifthen.sty, which IS in this
			// bundle) is a macro rather than a TeX conditional and is never
			// closed by an \fi, so this opens a conditional that stays open --
			// measured, not assumed: the scan returns complete=false for the
			// rest of the preamble and the worker decides.
			//
			// That is the safe direction. Over-opening costs the gate its
			// early refusal; over-CLOSING refuses a valid document, which is
			// the failure this whole file exists to avoid. There is no name
			// test that separates \ifdraft (a conditional \newif made) from
			// \ifthenelse (not one), so the asymmetry stays.
			ifDepth++
			i += wordLen - 1
			continue
		}

		for _, cmd := range []string{"usepackage", "RequirePackage"} {
			if word != cmd {
				continue
			}
			args, ok := takeTeXBraceGroup(skipTeXOptionGroup(rest[wordLen:]))
			if !ok {
				// A brace group the window cut in half, or a \usepackage whose
				// argument is a macro. Not an error and not a guess: skipped,
				// and the scan is no longer authoritative.
				complete = false
				i += wordLen - 1
				break
			}
			if braceDepth > 0 || ifDepth > 0 {
				// Inside a macro body or a conditional. It may never execute;
				// the engine decides.
				complete = false
				i += wordLen - 1
				break
			}
			for more := true; more; {
				var name string
				name, args, more = strings.Cut(args, ",")
				if name = strings.TrimSpace(name); looksLikeTeXPackageName(name) {
					if !visit(name) {
						return false
					}
				} else if name != "" {
					// A name that cannot be a filename (\usepackage{#1} out of
					// a macro body, or an unexpanded control sequence). Never
					// reported, never guessed at.
					complete = false
				}
			}
			i += wordLen - 1
			break
		}
	}
	return complete
}

// texControlWord returns the TeX control sequence at the start of src, which
// must begin with a backslash, together with how many bytes it occupies.
//
// TeX's own rule, and the reason this exists: a control WORD is a backslash
// followed by a maximal run of letters (\finalfoo is one token, not \fi
// followed by text), while a control SYMBOL is a backslash and exactly one
// non-letter (\%, \{, \\). Anything comparing with strings.HasPrefix is
// asking a different question than TeX does, and the difference is not
// theoretical -- see the call site.
func texControlWord(src string) (name string, n int) {
	if len(src) < 2 || src[0] != '\\' {
		return "", 1
	}
	if !isASCIILetter(src[1]) {
		return src[1:2], 2
	}
	j := 1
	for j < len(src) && isASCIILetter(src[j]) {
		j++
	}
	return src[1:j], j
}

// looksLikeTeXPackageName reports whether name could be a package at all.
//
// A package name is a FILENAME: \usepackage{x} resolves x.sty, so anything
// that cannot be part of a filename cannot be a package. Anything else is
// dropped, silently and deliberately -- the alternative is refusing a valid
// document over a string the engine would never have looked up.
//
// The case that produced this function is not hypothetical; it was found by
// the test that asserts the scanner never invents a name:
//
//	\newcommand{\loadit}[1]{\usepackage{#1}}
//
// The scanner does not expand macros, so it read the literal "#1" out of the
// definition body -- and an admission gate would then have refused the document
// for loading a package called "#1". Missing the real package here is correct
// (the worker gate is the backstop); naming a fictional one is not.
func looksLikeTeXPackageName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// stripTeXComment removes a `%` comment from a line, honouring `\%`.
//
// Without the escape handling, a line containing a literal percent sign would
// have everything after it discarded -- which only ever LOSES packages, never
// invents them, but a scanner that silently truncates on a common character
// would make `complete` mean less than it says.
func stripTeXComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] != '%' {
			continue
		}
		if i > 0 && line[i-1] == '\\' {
			continue
		}
		return line[:i]
	}
	return line
}

// skipTeXOptionGroup skips a leading [...] option group, if present, so
// \usepackage[T1]{fontenc} reaches {fontenc}.
//
// Only a leading one, and only if it opens before any brace: options come
// before the argument in every form LaTeX accepts here.
func skipTeXOptionGroup(s string) string {
	t := strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(t, "[") {
		return s
	}
	depth := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return t[i+1:]
			}
		case '{':
			// An unterminated option group; do not swallow the argument.
			return s
		}
	}
	return s
}

// takeTeXBraceGroup returns the contents of a leading {...} group.
func takeTeXBraceGroup(s string) (string, bool) {
	t := strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(t, "{") {
		return "", false
	}
	if end := strings.Index(t, "}"); end > 0 {
		return t[1:end], true
	}
	return "", false
}
