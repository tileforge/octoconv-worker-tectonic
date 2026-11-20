package main

import (
	"context"
	"errors"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
)

func transportCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "engine_error"
}

func tectonicEngineCode(err error) (string, string) {
	if err == nil {
		return "engine_error", "conversion failed"
	}
	if errors.Is(err, convert.ErrTeXSandboxUnavailable) || errors.Is(err, convert.ErrTeXEngineStarved) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout", safeMessage(err)
	}
	if pkg := convert.TeXUnsupportedPackageName(err); pkg != "" {
		return "unsupported_package", convert.TeXUnsupportedPackageMessage(pkg)
	}
	if errors.Is(err, convert.ErrTeXOutputTooLarge) || errors.Is(err, convert.ErrOutputTooLarge) {
		return "output_too_large", "the typeset PDF is larger than this deployment accepts"
	}
	if errors.Is(err, convert.ErrTeXBundleUnusable) {
		return "engine_error", "the typesetting engine is not configured on this deployment"
	}
	if errors.Is(err, convert.ErrTeXCPUExhausted) {
		return "engine_error", "the document used more processing time than this deployment allows"
	}
	if errors.Is(err, convert.ErrTeXEngineTimeout) {
		return "engine_error", "the document did not finish typesetting within the time limit"
	}
	if errors.Is(err, convert.ErrTeXBiberUnavailable) {
		return "engine_error", "biblatex requires the option backend=bibtex on this deployment: no biber is available"
	}
	if errors.Is(err, convert.ErrTeXEngineKilled) {
		return "engine_error", "the document exhausted the memory available to the typesetting engine"
	}
	if errors.Is(err, convert.ErrTeXUndefinedReferences) {
		return "engine_error", "the document has undefined references or citations; the PDF would show ?? in their place"
	}
	return "engine_error", "the document could not be typeset"
}
