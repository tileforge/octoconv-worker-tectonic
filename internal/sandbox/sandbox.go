// Package sandbox confines what a conversion engine may TOUCH and what it may
// CONSUME: the files its job actually needs, and the CPU its job is allowed to
// cost.
//
// # WHY IT EXISTS
//
// PR #19 measured what --untrusted does and does not do: it refuses \write18,
// and it does NOT confine reads. A .tex containing
//
//	\newread\f \openin\f=/etc/hostname \read\f to \line ... \line
//
// typeset the container's hostname into the PDF the client got back, and the
// same read against /proc/self/environ returned the worker's environment. That
// PR removed what the reads were WORTH by giving the engine an allowlist
// environment, and said plainly that it did not remove the reads. This does.
//
// # WHY LANDLOCK
//
// The todo left behind by #19 costed three options and called a mount namespace
// per job "the only one that makes the guarantee structural" -- while noting it
// needs a capability the chart does not grant. Landlock needs none: an
// unprivileged process restricts ITSELF, the restriction is inherited across
// execve and cannot be lifted. Measured in the shipped image as nobody: ABI
// version 8.
//
// The rules are applied by a process that then execs the engine, because they
// cannot be applied to someone else -- see cmd/tectonic-worker's sandbox-exec
// mode.
package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

// Paths is what a sandboxed engine may touch. Everything else in the container
// -- /etc, /proc, /run, any future mounted secret -- becomes unreachable, which
// is the point.
type Paths struct {
	// ReadWrite is the job's own directory: the input the worker materialised,
	// the output the engine writes, and whatever intermediates it keeps.
	ReadWrite []string
	// ReadOnly is what the engine needs to run at all: its bundle, its
	// precompiled format cache, and the system directories holding the binary
	// and its shared libraries.
	ReadOnly []string
	// Socket is the directories beneath which the engine may CREATE a
	// Unix-domain socket, and nothing else: no read, no write, no remove, no
	// execute. It exists for an engine whose single-instance IPC binds one
	// (LibreOffice binds /tmp/OSL_PIPE_<uid>_SingleOfficeIPC_<hash>, and picks
	// its directory from /tmp then /var/tmp only). Every other class leaves it
	// empty, so no class's rendered spec changes.
	//
	// A separate list rather than a bit in ReadWrite's mask on purpose: a
	// read-write grant is the job directory, where client-controlled bytes
	// land, and creating a socket there is a right no engine has asked for.
	Socket []string
}

// Limits are the resource bounds applied alongside the filesystem ruleset.
//
// Here rather than in the worker because they share the one property that
// makes this package exist: they can only be applied by the process that then
// BECOMES the engine, and the only thing that survives that exec is argv. A
// second owner of the same argv is a second thing to keep in step.
type Limits struct {
	// CPUSeconds bounds the CPU the engine may consume, via RLIMIT_CPU. Zero
	// means unbounded.
	//
	// CPU and not wall clock, because they are different quantities and only
	// one of them is the document's fault. Measured in the shipped image at a
	// 2-CPU quota, the same document twice: idle it took 0.96s wall for 0.95s
	// of CPU; with the node oversubscribed 4x it took 3.67s wall for 1.08s of
	// CPU. Wall clock inflated 4x while consumption did not move. A bound on
	// wall clock therefore refuses documents for the sins of their neighbours.
	CPUSeconds int
}

// Spec renders p as argv tokens.
//
// # WHY A WIRE FORMAT AT ALL
//
// Landlock restricts the CALLING process, so the rules must be applied by
// whatever then becomes the engine -- across an exec boundary, where the only
// thing that survives is argv. Rendering and parsing live together here
// because they are one format with two ends in two binaries, and a format
// whose halves live apart is a format that drifts.
func (p Paths) Spec() []string { return p.SpecWith(Limits{}) }

// SpecWith renders the paths and the limits together.
func (p Paths) SpecWith(l Limits) []string {
	spec := make([]string, 0, len(p.ReadWrite)+len(p.Socket)+len(p.ReadOnly)+1)
	if l.CPUSeconds > 0 {
		spec = append(spec, "cpu:"+strconv.Itoa(l.CPUSeconds))
	}
	for _, path := range p.ReadWrite {
		spec = append(spec, "rw:"+path)
	}
	for _, path := range p.Socket {
		spec = append(spec, "sock:"+path)
	}
	for _, path := range p.ReadOnly {
		spec = append(spec, "ro:"+path)
	}
	return spec
}

// ParseSpec reads Spec's output back, and returns whatever followed "--" as
// the command to exec.
//
// An unrecognised token is an ERROR rather than something to skip: a dropped
// token is a rule that was meant to be granted and was not, which surfaces
// much later as an engine failing to read a file nobody can explain.
func ParseSpec(argv []string) (Paths, Limits, []string, error) {
	var p Paths
	var l Limits
	i := 0
	for ; i < len(argv) && argv[i] != "--"; i++ {
		switch {
		case strings.HasPrefix(argv[i], "cpu:"):
			n, err := strconv.Atoi(strings.TrimPrefix(argv[i], "cpu:"))
			if err != nil || n <= 0 {
				return p, l, nil, fmt.Errorf("sandbox: bad cpu limit %q", argv[i])
			}
			l.CPUSeconds = n
		case strings.HasPrefix(argv[i], "ro:"):
			p.ReadOnly = append(p.ReadOnly, strings.TrimPrefix(argv[i], "ro:"))
		case strings.HasPrefix(argv[i], "rw:"):
			p.ReadWrite = append(p.ReadWrite, strings.TrimPrefix(argv[i], "rw:"))
		case strings.HasPrefix(argv[i], "sock:"):
			p.Socket = append(p.Socket, strings.TrimPrefix(argv[i], "sock:"))
		default:
			return p, l, nil, fmt.Errorf("sandbox: unrecognised argument %q", argv[i])
		}
	}
	if i >= len(argv)-1 {
		return p, l, nil, fmt.Errorf("sandbox: no command after --")
	}
	return p, l, argv[i+1:], nil
}
