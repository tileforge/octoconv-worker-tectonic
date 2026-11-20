package main

import (
	"errors"
	"testing"
)

func TestInputNameFromHref(t *testing.T) {
	job := "11111111-1111-4111-8111-111111111111"
	good := "https://files.example/bucket/uploads/" + job + "/1-preamble.tex?X-Amz-Algorithm=AWS4&x=/uploads/" + job + "/1-evil.tex"
	name, err := inputNameFromHref(job, 1, good)
	if err != nil || name != "preamble.tex" {
		t.Fatalf("name %q err %v", name, err)
	}
	hyphen := "https://files.example/uploads/" + job + "/2-my-file.tex"
	name, err = inputNameFromHref(job, 2, hyphen)
	if err != nil || name != "my-file.tex" {
		t.Fatalf("hyphenated name %q err %v", name, err)
	}
	spaced := "https://files.example/uploads/" + job + "/3-my%20file.bib"
	name, err = inputNameFromHref(job, 3, spaced)
	if err != nil || name != "my file.bib" {
		t.Fatalf("spaced name %q err %v", name, err)
	}
	rejected := []string{
		"https://files.example/uploads/other/1-preamble.tex",
		"https://files.example/uploads/" + job + "/2-preamble.tex",
		"https://files.example/uploads/" + job + "/1-in.pdf",
		"https://files.example/uploads/" + job + "/1-out.log",
		"https://files.example/uploads/" + job + "/1--flag.tex",
		"https://files.example/uploads/" + job + "/1-..%2Fetc%2Fpasswd",
		"https://files.example/uploads/" + job + "/1-%2e%2e",
		"https://files.example/uploads/" + job + "/1-.hidden.tex",
		"https://files.example/object?key=uploads/" + job + "/1-preamble.tex",
		"https://files.example/uploads/" + job + "/01-preamble.tex",
		"https://files.example/uploads/" + job + "/1-preamble.tex/",
	}
	for _, href := range rejected {
		if _, err := inputNameFromHref(job, 1, href); err == nil {
			t.Fatalf("accepted %s", href)
		}
	}
}

func TestClassifyInputs(t *testing.T) {
	job := "11111111-1111-4111-8111-111111111111"
	source := fileLinkJSON{Role: "source", Ordinal: 0, Format: "latex", Href: "https://files.example/objects/in"}
	extra := fileLinkJSON{
		Role:    "source",
		Ordinal: 1,
		Format:  "latex",
		Href:    "https://files.example/bucket/uploads/" + job + "/1-preamble.tex",
	}
	got, extras, err := classifyInputs(job, []fileLinkJSON{extra, source})
	if err != nil || got.Ordinal != 0 || len(extras) != 1 || extras[0].name != "preamble.tex" {
		t.Fatalf("got %+v extras %+v err %v", got, extras, err)
	}
	dup := extra
	dup.Ordinal = 2
	dup.Href = "https://files.example/bucket/uploads/" + job + "/2-preamble.tex"
	if _, _, err := classifyInputs(job, []fileLinkJSON{source, extra, dup}); !errors.Is(err, errAssignmentInputs) {
		t.Fatalf("duplicate name err %v", err)
	}
	owned := fileLinkJSON{Role: "source", Ordinal: 1, Format: "pdf", Href: "https://files.example/bucket/uploads/" + job + "/1-in.pdf"}
	if _, _, err := classifyInputs(job, []fileLinkJSON{source, owned}); !errors.Is(err, errUnsafeInputFilename) {
		t.Fatalf("owned name err %v", err)
	}
	many := []fileLinkJSON{source}
	for i := 1; i <= maxInputParts; i++ {
		many = append(many, fileLinkJSON{
			Role:    "source",
			Ordinal: i,
			Format:  "latex",
			Href:    hrefFor(job, i),
		})
	}
	if _, _, err := classifyInputs(job, many); !errors.Is(err, errAssignmentInputs) {
		t.Fatalf("too many err %v", err)
	}
	if _, _, err := classifyInputs(job, []fileLinkJSON{extra}); !errors.Is(err, errAssignmentInputs) {
		t.Fatalf("missing document err %v", err)
	}
	plain, extras, err := classifyInputs("", []fileLinkJSON{source})
	if err != nil || plain.Href != source.Href || len(extras) != 0 {
		t.Fatalf("single input %+v %+v %v", plain, extras, err)
	}
}

func hrefFor(job string, ordinal int) string {
	return "https://files.example/uploads/" + job + "/" + itoa(ordinal) + "-part" + itoa(ordinal) + ".tex"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
