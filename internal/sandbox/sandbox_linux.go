//go:build linux

package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// What a job directory may do: read, write, create and remove within
	// itself. TRUNCATE is included because tectonic rewrites its outputs
	// across passes.
	accessReadWrite = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR

	// What the system directories may do: be read and executed. No write, so
	// an engine cannot alter its own binary, its bundle or its cache.
	accessReadOnly = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_EXECUTE

	// What a Socket path may do: create a Unix-domain socket beneath itself,
	// and nothing else. Its own constant, so that no read-write or read-only
	// grant can ever carry it by accident: it is granted only on a path named
	// by a sock: token (Paths.Socket).
	accessMakeSocket = unix.LANDLOCK_ACCESS_FS_MAKE_SOCK

	// The subset that is meaningful on a REGULAR FILE. Landlock refuses a rule
	// granting a directory-only right on a non-directory with EINVAL, and the
	// bundle is a single .ttb file -- so without this mask the ruleset cannot
	// be built at all and the worker refuses every job.
	//
	// Measured, not read: the first wiring granted accessReadOnly on the
	// bundle path and every compile came back
	// "add rule /opt/tectonic/octoconv-bundle.ttb: invalid argument".
	accessFileOnly = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

	// Every access this ruleset takes an opinion about. An access left out of
	// handledAccess is NOT restricted, so this deliberately lists more than the
	// two sets above grant -- symlink, fifo and device creation are handled here
	// and granted nowhere. Socket creation is handled everywhere too, and is
	// granted only by an explicit sock: path (accessMakeSocket).
	handledAccess = accessReadWrite | accessReadOnly |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// abiVersion is the Landlock ABI this kernel implements, or 0 if it offers
// none.
//
// Asked with the version query rather than by reading a kernel version string:
// the LSM can be compiled in and disabled at boot, and only the syscall knows.
func abiVersion() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
		unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// Available reports whether this kernel offers Landlock at all.
func Available() bool { return abiVersion() > 0 }

// supportedAccess masks handledAccess down to what this ABI knows about.
//
// This is not politeness, it is the difference between confined and dead: a
// ruleset that HANDLES an access the kernel has never heard of is rejected
// whole with EINVAL, so an unmasked handledAccess would turn every older
// kernel into "sandbox cannot be established" -- and since the caller fails
// closed, into a tex engine that refuses every job. The rights masked away are
// simply not restricted there, which is the honest degradation: a v2 kernel
// confines reads and writes and does not bound ioctls.
//
// The measured deployment reports ABI 8 and masks nothing.
func supportedAccess(abi int) uint64 {
	var access uint64 = handledAccess
	if abi < 5 {
		access &^= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	if abi < 3 {
		access &^= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi < 2 {
		access &^= unix.LANDLOCK_ACCESS_FS_REFER
	}
	return access
}

// Apply restricts THIS process to p, permanently, and the restriction is
// inherited by anything it execs.
//
// It cannot be undone and cannot be applied to another process: that is the
// property that makes it worth having, and the reason the caller has to be the
// thing that becomes the engine.
func Apply(p Paths) error {
	// LOCK THE THREAD, AND NEVER UNLOCK IT.
	//
	// Landlock and no_new_privs are properties of the calling THREAD, not of
	// the process. A goroutine may migrate to another OS thread at any
	// preemption point, so without this the rules can be established on thread
	// A while syscall.Exec runs on thread B -- and execve carries the security
	// state of the thread that calls it. The engine would then run with NO
	// sandbox at all, intermittently, while every test and every live probe
	// that happened to stay on one thread reported success.
	//
	// It is deliberately never unlocked: the only caller applies the rules and
	// then becomes the engine, so the lock must outlive this function. Found by
	// codex on PR #21; the live proof did not catch it, because a race that
	// usually goes the right way looks exactly like correctness.
	runtime.LockOSThread()

	abi := abiVersion()
	if abi < 1 {
		return fmt.Errorf("landlock: unavailable on this kernel")
	}
	handled := supportedAccess(abi)

	attr := unix.LandlockRulesetAttr{Handled_access_fs: handled}
	// Use only the original filesystem field. Newer x/sys versions also
	// expose network, scope and audit fields, which this sandbox does not use.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Handled_access_fs), 0)
	if errno != 0 {
		return fmt.Errorf("landlock: create ruleset: %w", errno)
	}
	defer unix.Close(int(fd))

	// A rule may only grant rights the ruleset handles, so the same mask has
	// to reach the grants -- otherwise the add-rule call is the thing that
	// fails with EINVAL instead of the create call.
	for _, path := range p.ReadOnly {
		if err := addPath(int(fd), path, accessReadOnly&handled); err != nil {
			return err
		}
	}
	for _, path := range p.ReadWrite {
		if err := addPath(int(fd), path, accessReadWrite&handled); err != nil {
			return err
		}
	}
	// MAKE_SOCK and nothing else. addPath narrows a non-directory to the file
	// mask, which shares no bit with it, so a sock: token naming a file fails
	// closed here instead of granting nothing silently.
	for _, path := range p.Socket {
		if err := addPath(int(fd), path, accessMakeSocket&handled); err != nil {
			return err
		}
	}

	// no_new_privs is required before restrict_self, and is wanted anyway: it
	// stops anything the engine execs from gaining privileges through setuid.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("landlock: no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: restrict self: %w", errno)
	}
	return nil
}

// addPath grants access beneath one directory or file.
//
// A path that does not exist is an ERROR rather than a skip: a rule silently
// dropped is a directory the engine cannot reach, and the failure would arrive
// later as an unexplained engine error instead of here.
func addPath(rulesetFD int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock: open %s: %w", path, err)
	}
	defer unix.Close(fd)

	// A regular file may only be granted file rights; see accessFileOnly.
	// Narrowing rather than erroring, because naming a file is legitimate --
	// the tex bundle is one, and granting its whole directory would hand the
	// engine more than it needs.
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("landlock: stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= accessFileOnly
		if access == 0 {
			return fmt.Errorf("landlock: %s is not a directory and none of the requested "+
				"access applies to a file", path)
		}
	}

	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(rulesetFD),
		unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("landlock: add rule %s: %w", path, errno)
	}
	return nil
}

// ThreadID is the OS thread the caller is running on.
//
// Exported so the process that applies the rules can prove, immediately before
// execve, that it is still on the thread it restricted. Apply locks the thread
// and that ought to be enough; this is the check that says so out loud rather
// than trusting it, because the failure it guards against is silent.
func ThreadID() int { return unix.Gettid() }
