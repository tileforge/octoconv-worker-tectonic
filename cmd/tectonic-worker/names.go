package main

import (
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// maxInputParts matches the manifest and the core jobs.MaxInputParts ceiling.
const maxInputParts = 32

const maxInputFilenameLen = 255

var errUnsafeInputFilename = errors.New("unsafe input filename")

var errAssignmentInputs = errors.New("assignment inputs")

// extraFile is one attachment written beside the main document.
type extraFile struct {
	name string
	body []byte
}

type namedInput struct {
	link fileLinkJSON
	name string
}

// classifyInputs accepts one source document at ordinal 0 plus attachments.
// Attachment names come from the presigned object key. The file link has no
// filename field, and the protocol schema rejects unknown fields.
func classifyInputs(jobID string, links []fileLinkJSON) (fileLinkJSON, []namedInput, error) {
	if len(links) == 0 || len(links) > maxInputParts {
		return fileLinkJSON{}, nil, errAssignmentInputs
	}
	var source fileLinkJSON
	var haveSource bool
	seenOrdinal := map[int]bool{}
	seenName := map[string]bool{}
	extras := make([]namedInput, 0)
	for _, link := range links {
		if link.Role != "source" || link.Href == "" || link.Format == "" || link.Ordinal < 0 {
			return fileLinkJSON{}, nil, errAssignmentInputs
		}
		if seenOrdinal[link.Ordinal] {
			return fileLinkJSON{}, nil, errAssignmentInputs
		}
		seenOrdinal[link.Ordinal] = true
		if _, err := requireHTTPS(link.Href); err != nil {
			return fileLinkJSON{}, nil, err
		}
		if link.Ordinal == 0 {
			source = link
			haveSource = true
			continue
		}
		name, err := inputNameFromHref(jobID, link.Ordinal, link.Href)
		if err != nil {
			return fileLinkJSON{}, nil, err
		}
		if seenName[name] {
			return fileLinkJSON{}, nil, errAssignmentInputs
		}
		seenName[name] = true
		extras = append(extras, namedInput{link: link, name: name})
	}
	if !haveSource {
		return fileLinkJSON{}, nil, errAssignmentInputs
	}
	return source, extras, nil
}

// inputNameFromHref reads uploads/{job_id}/{ordinal}-{filename} from the URL
// path. The query string is ignored: a signature does not make a query a key.
func inputNameFromHref(jobID string, ordinal int, href string) (string, error) {
	if jobID == "" || ordinal <= 0 {
		return "", errUnsafeInputFilename
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return "", errUnsafeInputFilename
	}
	parts := strings.Split(parsed.EscapedPath(), "/")
	if len(parts) < 3 || parts[len(parts)-1] == "" {
		return "", errUnsafeInputFilename
	}
	if parts[len(parts)-3] != "uploads" {
		return "", errUnsafeInputFilename
	}
	gotJob, err := url.PathUnescape(parts[len(parts)-2])
	if err != nil || gotJob != jobID {
		return "", errUnsafeInputFilename
	}
	leaf, err := url.PathUnescape(parts[len(parts)-1])
	if err != nil {
		return "", errUnsafeInputFilename
	}
	gotOrdinal, name, err := splitObjectLeaf(leaf)
	if err != nil || gotOrdinal != ordinal {
		return "", errUnsafeInputFilename
	}
	if _, err := safeInputFilename(name); err != nil {
		return "", err
	}
	if workerOwnedName(name) {
		return "", errUnsafeInputFilename
	}
	return name, nil
}

func splitObjectLeaf(leaf string) (int, string, error) {
	ordinalText, name, ok := strings.Cut(leaf, "-")
	if !ok || ordinalText == "" || name == "" {
		return 0, "", errUnsafeInputFilename
	}
	ordinal, err := strconv.Atoi(ordinalText)
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != ordinalText {
		return 0, "", errUnsafeInputFilename
	}
	return ordinal, name, nil
}

func extraPath(dir, name string) (string, error) {
	if _, err := safeInputFilename(name); err != nil {
		return "", err
	}
	if workerOwnedName(name) {
		return "", errUnsafeInputFilename
	}
	cleaned := filepath.Join(dir, name)
	if filepath.Base(cleaned) != name || filepath.Dir(cleaned) != dir {
		return "", errUnsafeInputFilename
	}
	return cleaned, nil
}

// safeInputFilename is the core jobs.SafeInputFilename gate. A name that needs
// rewriting is refused: the engine resolves \input and \addbibresource by the
// name the client sent. Measured: a repaired name left the document reading a
// path the worker never created, and an attachment named in.pdf was replaced
// by the typeset PDF.
func safeInputFilename(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", errUnsafeInputFilename
	}
	if strings.ContainsAny(name, `/\`) || path.Base(name) != name {
		return "", errUnsafeInputFilename
	}
	if len(name) > maxInputFilenameLen {
		return "", errUnsafeInputFilename
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") ||
		strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return "", errUnsafeInputFilename
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return "", errUnsafeInputFilename
		}
	}
	return name, nil
}

// workerOwnedName reserves the in.* and out.* stems. Tectonic writes in.pdf
// beside in.latex, so reserving only the two worker names left an attachment
// called in.pdf overwritten by the compiled PDF.
func workerOwnedName(name string) bool {
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	return stem == "in" || stem == "out"
}
