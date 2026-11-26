// Package convert typesets a LaTeX project to PDF with the tectonic CLI.
package convert

import (
	"context"
	"strings"
	"time"
)

// ErrOutputTooLarge is the read-limit sentinel. It is the typeset-PDF ceiling.
var ErrOutputTooLarge = ErrTeXOutputTooLarge

const (
	EngineTectonic             = "tectonic"
	OperationConvert           = "convert"
	OperationRender            = "render"
	DefaultEngineTimeout       = 271 * time.Second
	MaxInputBytes        int64 = 2 << 20
	MaxFileBytes               = DefaultTectonicMaxOutputBytes
	SniffWindowBytes           = 1 << 20
)

// Pair is one source format and one target format.
type Pair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Converter converts the file at inPath into outPath.
type Converter interface {
	Pairs() []Pair
	Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error
}

// OperationalConverter reports a job operation other than convert.
type OperationalConverter interface {
	Operation() string
}

var formatAliases = map[string]string{
	"tex": "latex",
}

// MIMEType is the content type stored for format.
func MIMEType(format string) string {
	switch NormalizeFormat(format) {
	case "pdf":
		return "application/pdf"
	case "png":
		return "image/png"
	case "latex":
		return "application/x-latex"
	default:
		return "application/octet-stream"
	}
}

// NormalizeFormat lowercases a format or extension, strips one leading dot, and folds tex to latex.
func NormalizeFormat(f string) string {
	f = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "."))
	if canonical, ok := formatAliases[f]; ok {
		return canonical
	}
	return f
}

func operationOf(c Converter) string {
	if oc, ok := c.(OperationalConverter); ok {
		return oc.Operation()
	}
	return OperationConvert
}

type opKey struct {
	Operation string
	From      string
	To        string
}

// Registry maps an operation and a format pair to a converter.
type Registry struct {
	m map[opKey]Converter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[opKey]Converter)}
}

// Register indexes each pair c reports.
func (r *Registry) Register(c Converter) {
	op := operationOf(c)
	for _, pair := range c.Pairs() {
		r.m[opKey{
			Operation: op,
			From:      NormalizeFormat(pair.From),
			To:        NormalizeFormat(pair.To),
		}] = c
	}
}

// Replace indexes each pair c reports, overwriting a previous converter for that pair.
func (r *Registry) Replace(c Converter) {
	r.Register(c)
}

// LookupOp reports the converter for op, from, and to.
func (r *Registry) LookupOp(op, from, to string) (Converter, bool) {
	c, ok := r.m[opKey{
		Operation: op,
		From:      NormalizeFormat(from),
		To:        NormalizeFormat(to),
	}]
	return c, ok
}

// Default is the process registry.
var Default = NewRegistry()

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isPDFSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}
