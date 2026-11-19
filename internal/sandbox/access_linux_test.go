//go:build linux

package sandbox

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The access bit sets ARE the sandbox. Every property measured live in the
// shipped image comes from these three constants, so they are pinned here --
// a live result cannot run on a developer's machine, and the constants can.
//
// Measured in the image under exactly this ruleset:
//
//	inside the job directory : create a file yes, create a directory yes,
//	                           remove a file yes
//	                           create a symlink NO, create a fifo NO,
//	                           execute a file it wrote NO,
//	                           execute a copied tectonic binary NO
//	read-only trees          : write NO, delete the bundle NO

// TestJobDirectoryCannotExecute is the one worth stating on its own. The
// engine's input is a client-authored program, and the job directory is where
// that client's files land. Nothing there may ever be executable.
//
// --untrusted already refuses \write18, so this is defence in depth rather
// than the primary control -- which is exactly why it needs a test: a control
// that is not the one anybody is watching is the one that gets widened.
func TestJobDirectoryCannotExecute(t *testing.T) {
	if accessReadWrite&unix.LANDLOCK_ACCESS_FS_EXECUTE != 0 {
		t.Fatal("the job directory grants EXECUTE. A client's uploaded file would be executable " +
			"by anything the engine can reach, and the job directory is where client-controlled " +
			"bytes are written by design.")
	}
}

// TestNothingMayCreateSymlinksOrSpecialFiles: these are HANDLED by the ruleset
// and granted nowhere, which is what makes them refused. An access left out of
// handledAccess is not restricted at all, so "granted nowhere" is only half of
// the statement and the other half lives in handledAccess.
func TestNothingMayCreateSymlinksOrSpecialFiles(t *testing.T) {
	forbidden := map[string]uint64{
		"symlink":         unix.LANDLOCK_ACCESS_FS_MAKE_SYM,
		"fifo":            unix.LANDLOCK_ACCESS_FS_MAKE_FIFO,
		"socket":          unix.LANDLOCK_ACCESS_FS_MAKE_SOCK,
		"block device":    unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK,
		"char device":     unix.LANDLOCK_ACCESS_FS_MAKE_CHAR,
		"cross-dir refer": unix.LANDLOCK_ACCESS_FS_REFER,
	}
	for name, bit := range forbidden {
		if handledAccess&bit == 0 {
			t.Errorf("creating a %s is not HANDLED by the ruleset, so it is not restricted at "+
				"all -- granting it nowhere means nothing unless the ruleset takes an opinion "+
				"about it", name)
		}
		if accessReadWrite&bit != 0 || accessReadOnly&bit != 0 {
			t.Errorf("creating a %s is GRANTED", name)
		}
	}
}

// TestReadOnlyIsReadOnly. The bundle, the format cache and the system trees are
// shared across every job, so one document being able to write there is one
// document poisoning the next.
func TestReadOnlyIsReadOnly(t *testing.T) {
	for name, bit := range map[string]uint64{
		"write":    unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
		"truncate": unix.LANDLOCK_ACCESS_FS_TRUNCATE,
		"create":   unix.LANDLOCK_ACCESS_FS_MAKE_REG,
		"mkdir":    unix.LANDLOCK_ACCESS_FS_MAKE_DIR,
		"unlink":   unix.LANDLOCK_ACCESS_FS_REMOVE_FILE,
		"rmdir":    unix.LANDLOCK_ACCESS_FS_REMOVE_DIR,
	} {
		if accessReadOnly&bit != 0 {
			t.Errorf("the read-only set grants %s: one job could alter what the next one is "+
				"typeset with -- its bundle, its format cache, or the engine binary itself", name)
		}
	}
}

// TestEverythingGrantedIsAlsoHandled is the invariant that makes the other two
// meaningful: a rule may only grant rights its ruleset handles, and the kernel
// rejects the whole add_rule with EINVAL otherwise. A grant outside
// handledAccess is not a wider sandbox, it is a worker that cannot start.
func TestEverythingGrantedIsAlsoHandled(t *testing.T) {
	if accessReadWrite&^handledAccess != 0 {
		t.Errorf("the read-write set grants %#x, which the ruleset does not handle; add_rule "+
			"would fail with EINVAL and the worker would refuse every job",
			accessReadWrite&^handledAccess)
	}
	if accessReadOnly&^handledAccess != 0 {
		t.Errorf("the read-only set grants %#x, which the ruleset does not handle",
			accessReadOnly&^handledAccess)
	}
}

// TestSocketGrantIsSocketOnly pins the socket right (phase 70, route
// scoped-socket): creating a socket is granted on a sock: path and NOWHERE
// else. It is its own constant, equal to MAKE_SOCK exactly, inside what the
// ruleset handles (or the add_rule fails with EINVAL and the document worker
// cannot start), and sharing no bit with either general-purpose grant --
// TestNothingMayCreateSymlinksOrSpecialFiles keeps saying that a job directory
// and a read-only tree cannot create a socket.
func TestSocketGrantIsSocketOnly(t *testing.T) {
	if accessMakeSocket != unix.LANDLOCK_ACCESS_FS_MAKE_SOCK {
		t.Errorf("accessMakeSocket = %#x, want exactly MAKE_SOCK (%#x): a sock: path would "+
			"grant more than creating a socket", uint64(accessMakeSocket), uint64(unix.LANDLOCK_ACCESS_FS_MAKE_SOCK))
	}
	if handledAccess&accessMakeSocket != accessMakeSocket {
		t.Error("the socket right is not handled by the ruleset: add_rule would fail with EINVAL, " +
			"and an unhandled access is not restricted anywhere")
	}
	if accessReadWrite&accessMakeSocket != 0 || accessReadOnly&accessMakeSocket != 0 {
		t.Error("a general-purpose grant carries the socket right: every class's job directory " +
			"or system tree could then create a socket")
	}
}
