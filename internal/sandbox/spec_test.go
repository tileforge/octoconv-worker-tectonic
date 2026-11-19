package sandbox

import (
	"slices"
	"testing"
)

// The two ends of this format live in different binaries -- convert renders it,
// cmd/tectonic-worker's sandbox-exec mode reads it -- and nothing but a
// round-trip holds them equal.

func TestSpecRoundTrips(t *testing.T) {
	want := Paths{
		ReadWrite: []string{"/tmp/job-1", "/tmp"},
		ReadOnly:  []string{"/opt/tectonic/octoconv-bundle.ttb", "/usr", "/var/cache/tectonic"},
	}
	cmd := []string{"/usr/local/bin/tectonic", "-X", "compile", "--untrusted", "/tmp/job-1/in.tex"}

	got, gotLimits, gotCmd, err := ParseSpec(append(append(want.SpecWith(Limits{CPUSeconds: 271}), "--"), cmd...))
	if err != nil {
		t.Fatalf("ParseSpec on Spec's own output: %v", err)
	}
	if !slices.Equal(got.ReadWrite, want.ReadWrite) {
		t.Errorf("ReadWrite = %v, want %v", got.ReadWrite, want.ReadWrite)
	}
	if !slices.Equal(got.ReadOnly, want.ReadOnly) {
		t.Errorf("ReadOnly = %v, want %v", got.ReadOnly, want.ReadOnly)
	}
	if !slices.Equal(gotCmd, cmd) {
		t.Errorf("command = %v, want %v", gotCmd, cmd)
	}
	if gotLimits.CPUSeconds != 271 {
		t.Errorf("CPUSeconds = %d, want 271: the bound was rendered and did not survive the "+
			"parse, so the engine would run with no CPU limit at all", gotLimits.CPUSeconds)
	}
}

// A path is never granted MORE than it was rendered with. The two prefixes are
// three characters apart and the parse is a prefix match, which is exactly the
// shape that silently promotes read-only to writable if it is ever loosened.
func TestParseSpecDoesNotPromoteReadOnlyToWritable(t *testing.T) {
	p, _, _, err := ParseSpec([]string{"ro:/opt/tectonic", "--", "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ReadWrite) != 0 {
		t.Fatalf("a ro: token produced ReadWrite = %v", p.ReadWrite)
	}
}

// An unrecognised token is refused, not skipped. Skipping drops a rule, and a
// dropped rule is a directory the engine cannot read -- which arrives as an
// engine error nobody can trace back to here.
func TestParseSpecRefusesUnknownTokens(t *testing.T) {
	if _, _, _, err := ParseSpec([]string{"rw:/tmp", "x:/etc", "--", "/bin/true"}); err == nil {
		t.Fatal("ParseSpec accepted an unrecognised token; the rule it named was silently dropped")
	}
}

// No command means there is nothing to confine, and execing nothing would
// leave the caller to decide what to do next. It must not be able to conclude
// "sandbox applied".
func TestParseSpecRequiresACommand(t *testing.T) {
	for _, argv := range [][]string{{}, {"rw:/tmp"}, {"rw:/tmp", "--"}} {
		if _, _, _, err := ParseSpec(argv); err == nil {
			t.Fatalf("ParseSpec(%v) returned no error but has no command to exec", argv)
		}
	}
}

// The socket right is a fourth token kind. It must survive the wire as itself:
// a sock: token parsed into ReadWrite would grant a job directory that was
// meant to grant one socket, and one dropped would be an engine that cannot
// start with no error pointing here.
func TestSpecRoundTripsSockets(t *testing.T) {
	want := Paths{
		ReadWrite: []string{"/tmp/job-1"},
		Socket:    []string{"/tmp"},
		ReadOnly:  []string{"/usr/lib", "/etc/ld.so.cache"},
	}
	spec := want.Spec()
	wantSpec := []string{"rw:/tmp/job-1", "sock:/tmp", "ro:/usr/lib", "ro:/etc/ld.so.cache"}
	if !slices.Equal(spec, wantSpec) {
		t.Fatalf("Spec() = %v, want %v: sock: tokens go after rw: and before ro:", spec, wantSpec)
	}
	got, _, cmd, err := ParseSpec(append(spec, "--", "/bin/true"))
	if err != nil {
		t.Fatalf("ParseSpec on Spec's own output: %v", err)
	}
	if !slices.Equal(got.Socket, want.Socket) {
		t.Errorf("Socket = %v, want %v", got.Socket, want.Socket)
	}
	if !slices.Equal(got.ReadWrite, want.ReadWrite) || !slices.Equal(got.ReadOnly, want.ReadOnly) {
		t.Errorf("rw/ro = %v / %v, want %v / %v", got.ReadWrite, got.ReadOnly, want.ReadWrite, want.ReadOnly)
	}
	if !slices.Equal(cmd, []string{"/bin/true"}) {
		t.Errorf("command = %v", cmd)
	}
}

func TestParseSpecAcceptsSockToken(t *testing.T) {
	p, _, _, err := ParseSpec([]string{"sock:/tmp", "--", "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Socket, []string{"/tmp"}) || len(p.ReadWrite) != 0 || len(p.ReadOnly) != 0 {
		t.Fatalf("a sock: token produced Socket=%v ReadWrite=%v ReadOnly=%v; it must grant a socket and nothing else",
			p.Socket, p.ReadWrite, p.ReadOnly)
	}
}

// A Paths with no sockets renders exactly what it rendered before the socket
// right existed, so every already-confined class's helper argv is unchanged.
func TestSpecWithoutSocketsIsUnchanged(t *testing.T) {
	p := Paths{ReadWrite: []string{"/tmp/job-1", "/tmp"}, ReadOnly: []string{"/usr", "/lib"}}
	want := []string{"cpu:271", "rw:/tmp/job-1", "rw:/tmp", "ro:/usr", "ro:/lib"}
	if got := p.SpecWith(Limits{CPUSeconds: 271}); !slices.Equal(got, want) {
		t.Fatalf("SpecWith = %v, want %v", got, want)
	}
}
