package convert

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func referenceScanTeXPreamble(prefix []byte) (packages []string, complete bool) {
	src := string(prefix)

	if i := strings.Index(src, beginDocument); i >= 0 {
		src = src[:i]
		complete = true
	}

	var (
		braceDepth int
		ifDepth    int
	)
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '%':
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
		word, wordLen := referenceTeXControlWord(rest)
		if len(word) == 1 && !referenceIsASCIILetter(word[0]) {
			i += wordLen - 1
			continue
		}
		switch {
		case word == "fi":
			if ifDepth > 0 {
				ifDepth--
			}
			i += wordLen - 1
			continue
		case word == "newif":
			i += wordLen - 1
			for i+1 < len(src) && (src[i+1] == ' ' || src[i+1] == '\t') {
				i++
			}
			if strings.HasPrefix(src[i+1:], `\if`) {
				i++
				for i+1 < len(src) && referenceIsASCIILetter(src[i+1]) {
					i++
				}
			}
			continue
		case strings.HasPrefix(word, "if"):
			ifDepth++
			i += wordLen - 1
			continue
		case word == "endinput":
			if braceDepth == 0 && ifDepth == 0 {
				if nl := strings.IndexByte(src[i+wordLen:], '\n'); nl >= 0 {
					src = src[:i+wordLen+nl+1]
				}
			}
			i += wordLen - 1
			continue
		case word == "verb":
			i = skipVerbBody(src, i, wordLen)
			continue
		case word == "begin":
			if next, ok := skipVerbatimEnv(src, i, wordLen); ok {
				i = next
				continue
			}
			next, closed := referenceSkipFilecontentsBody(src, i, wordLen)
			if next < 0 {
				i += wordLen - 1
				continue
			}
			if !closed {
				complete = false
			}
			i = next
			continue
		}

		for _, cmd := range []string{"usepackage", "RequirePackage"} {
			if word != cmd {
				continue
			}
			args, ok := referenceTakeTeXBraceGroup(referenceSkipTeXOptionGroup(rest[wordLen:]))
			if !ok {
				complete = false
				i += wordLen - 1
				break
			}
			if braceDepth > 0 || ifDepth > 0 {
				complete = false
				i += wordLen - 1
				break
			}
			for _, name := range strings.Split(args, ",") {
				if name = strings.TrimSpace(name); referenceLooksLikeTeXPackageName(name) {
					packages = append(packages, name)
				} else if name != "" {
					complete = false
				}
			}
			i += wordLen - 1
			break
		}
	}
	return packages, complete
}

func referenceTeXControlWord(src string) (name string, n int) {
	if len(src) < 2 || src[0] != '\\' {
		return "", 1
	}
	if !referenceIsASCIILetter(src[1]) {
		return src[1:2], 2
	}
	j := 1
	for j < len(src) && referenceIsASCIILetter(src[j]) {
		j++
	}
	return src[1:j], j
}

func referenceIsASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func referenceLooksLikeTeXPackageName(name string) bool {
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

func referenceSkipTeXOptionGroup(s string) string {
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
			return s
		}
	}
	return s
}

func referenceSkipFilecontentsBody(text string, i, wordLen int) (next int, closed bool) {
	rest := text[i+wordLen:]
	k := 0
	for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t' || rest[k] == '\n') {
		k++
	}
	if k >= len(rest) || rest[k] != '{' {
		return -1, false
	}
	end := strings.IndexByte(rest[k:], '}')
	if end <= 1 {
		return -1, false
	}
	name := rest[k+1 : k+end]
	if name != "filecontents" && name != "filecontents*" {
		return -1, false
	}
	marker := `\end{` + name + `}`
	start := i + wordLen + k + end + 1
	rel := strings.Index(text[start:], marker)
	if rel < 0 {
		if len(text) == 0 {
			return 0, false
		}
		return len(text) - 1, false
	}
	j := start + rel + len(marker)
	for j < len(text) && text[j] != '\n' {
		j++
	}
	if j >= len(text) {
		return len(text) - 1, true
	}
	return j, true
}

func referenceTakeTeXBraceGroup(s string) (string, bool) {
	t := strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(t, "{") {
		return "", false
	}
	if end := strings.Index(t, "}"); end > 0 {
		return t[1:end], true
	}
	return "", false
}

func texDiffLiterals(t *testing.T, files ...string) []string {
	t.Helper()
	var concat func(e ast.Expr) (string, bool)
	concat = func(e ast.Expr) (string, bool) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		case *ast.ParenExpr:
			return concat(x.X)
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return "", false
			}
			l, ok := concat(x.X)
			if !ok {
				return "", false
			}
			r, ok := concat(x.Y)
			return l + r, ok
		}
		return "", false
	}
	var out []string
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		n := 0
		ast.Inspect(f, func(node ast.Node) bool {
			e, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			if s, ok := concat(e); ok {
				out = append(out, s)
				n++
			}
			return true
		})
		if n == 0 {
			t.Fatalf("no string literal found in %s: the corpus would not contain its inputs", path)
		}
	}
	return out
}

func texDiffSynthetic() []string {
	window := SniffWindowBytes
	head := `\documentclass{article}\usepackage{`
	fill := func(name string) string {
		return head + strings.Repeat(name+",", (window-len(head)-len(name)-1)/(len(name)+1)) + name + "}"
	}
	return []string{
		fill("a"),
		fill("graphicx"),
		fill("a")[:window-1],
		fill("a") + `\begin{document}`,
		`\begin{document}` + fill("a"),
		fill("ab")[:window/2] + `}\usepackage{x}\begin{document}`,
		`\usepackage[a,b]{x}\begin{document}`,
		`\usepackage[[nested],x]{y}\begin{document}`,
		`\usepackage[unterminated{x}\begin{document}`,
		`\usepackage [opt] {x}\begin{document}`,
		"\\usepackage\t[opt]\t{x}\\begin{document}",
		`\usepackage[a]b]{x}\begin{document}`,
		`\usepackage{x}[opt]\begin{document}`,
		`\usepackage{}\begin{document}`,
		`\usepackage{ }\begin{document}`,
		`\usepackage{,}\begin{document}`,
		`\usepackage{a,,b, ,c,}\begin{document}`,
		`\usepackage{ a , b ,	c	}\begin{document}`,
		`\usepackage{a,#1,b}\begin{document}`,
		`\usepackage{a,\foo,b}\begin{document}`,
		`\usepackage{a b,c}\begin{document}`,
		`\usepackage{a.b,c-d,e_f,G9}\begin{document}`,
		`\usepackage{a,b`,
		`\usepackage{a,b}`,
		`\usepackage`,
		`\usepackage\x{a}\begin{document}`,
		`\usepackage{a}\usepackage{a}\begin{document}`,
		`{\usepackage{x}}\usepackage{y}\begin{document}`,
		`}}}\usepackage{y}\begin{document}`,
		`\newcommand{\a}{\usepackage{b}}\usepackage{c}\begin{document}`,
		`\iffoo\usepackage{a}\fi\usepackage{b}\begin{document}`,
		`\iffoo\iffoo\usepackage{a}\fi\usepackage{b}\fi\usepackage{c}\begin{document}`,
		`\fi\fi\usepackage{a}\begin{document}`,
		`\newif\iffoo\usepackage{a}\begin{document}`,
		"\\newif \t\\iffoo\\usepackage{a}\\begin{document}",
		`\newif\fi\usepackage{a}\begin{document}`,
		`\newif`,
		`\ifthenelse{x}{\usepackage{a}}{}\usepackage{b}\begin{document}`,
		`\finalfoo\usepackage{a}\begin{document}`,
		`\usepackagefoo{a}\RequirePackagefoo{b}\begin{document}`,
		`\RequirePackage{a,b}\RequirePackage[x]{c}\begin{document}`,
		"% \\usepackage{a}\n\\usepackage{b}\\begin{document}",
		`% \usepackage{a}`,
		`\%\usepackage{a}\begin{document}`,
		`\\%\usepackage{a}` + "\n" + `\usepackage{b}\begin{document}`,
		`\{\usepackage{a}\}\begin{document}`,
		`\usepackage{a}\begin{documen`,
		`\begin{document}\usepackage{a}`,
		`\`,
		``,
		`\\`,
		`\usepackage{a}\begin{document}\usepackage{b}\begin{document}`,
	}
}

func texDiffRandom(rng *rand.Rand, n int) []string {
	vocab := []string{`\usepackage`, `\RequirePackage`, `{`, `}`, `[`, `]`, `,`, ` `, "\t", "\n", `%`, `\%`, `\\`, `\iffoo`, `\fi`, `\newif`, `\begin{document}`, `a`, `graphicx`, `#1`, `\x`, `\usepackagefoo`, `\ifthenelse`, `\{`, `\}`, `.`, `-`, `_`}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		for j, m := 0, 1+rng.IntN(40); j < m; j++ {
			b.WriteString(vocab[rng.IntN(len(vocab))])
		}
		out = append(out, b.String())
	}
	return out
}

func texDiffCorpus(t *testing.T) []string {
	t.Helper()
	var corpus []string
	for _, s := range texDiffLiterals(t, "texpreamble_test.go", "texresolvable_test.go", "texdenied_test.go") {
		corpus = append(corpus, s, `\documentclass{article}`+"\n"+`\usepackage{`+s+"}\n"+`\begin{document}`+"\n")
	}
	corpus = append(corpus, texDiffSynthetic()...)
	corpus = append(corpus, texDiffRandom(rand.New(rand.NewPCG(78, 13)), 2000)...)
	return corpus
}

func TestVisitTeXPreamblePackagesMatchesTheReference(t *testing.T) {
	corpus := texDiffCorpus(t)
	names, incomplete, stops := 0, 0, 0
	for i, src := range corpus {
		want, wantComplete := referenceScanTeXPreamble([]byte(src))
		names += len(want)
		if !wantComplete {
			incomplete++
		}
		got, gotComplete := ScanTeXPreamble([]byte(src))
		if !slices.Equal(got, want) || gotComplete != wantComplete {
			t.Errorf("input %d (%d bytes, %.60q): ScanTeXPreamble = %d names, complete %v; the reference implementation = %d names, complete %v",
				i, len(src), src, len(got), gotComplete, len(want), wantComplete)
		}
		var visited []string
		visitedComplete := VisitTeXPreamblePackages([]byte(src), func(pkg string) bool {
			visited = append(visited, pkg)
			return true
		})
		if !slices.Equal(visited, want) || visitedComplete != wantComplete {
			t.Errorf("input %d (%d bytes, %.60q): VisitTeXPreamblePackages visited %d names, complete %v; the reference implementation = %d names, complete %v",
				i, len(src), src, len(visited), visitedComplete, len(want), wantComplete)
		}
		for _, k := range []int{1, (len(want) + 1) / 2} {
			if k < 1 || k > len(want) {
				continue
			}
			var first []string
			stoppedComplete := VisitTeXPreamblePackages([]byte(src), func(pkg string) bool {
				first = append(first, pkg)
				return len(first) < k
			})
			stops++
			if !slices.Equal(first, want[:k]) || stoppedComplete {
				t.Errorf("input %d (%.60q): a visitor stopping at name %d received %d names, complete %v; want the reference's first %d, complete false",
					i, src, k, len(first), stoppedComplete, k)
			}
		}
	}
	t.Logf("corpus: %d inputs, %d names reported by the reference, %d inputs incomplete, %d early-stopping visits", len(corpus), names, incomplete, stops)
	if names == 0 || incomplete == 0 || incomplete == len(corpus) || stops == 0 {
		t.Errorf("the corpus does not exercise the scanner: %d names, %d of %d inputs incomplete, %d early stops", names, incomplete, len(corpus), stops)
	}
}
