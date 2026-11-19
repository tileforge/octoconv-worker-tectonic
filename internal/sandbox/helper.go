package sandbox

import (
	"fmt"
	"os"
)

// ResolveHelper returns the argv prefix a converter should run its engine
// through, or nil when confinement is explicitly disabled by disabledEnv.
//
// An error means the sandbox was WANTED and cannot be established, and the
// caller turns that into a refusal to start. A worker that cannot confine its
// engine must not consume a queue: it would either fail every job or, far
// worse, run the engine unconfined while its logs say otherwise.
//
// Lifted out of cmd/tectonic-worker when the second worker needed it. Copying
// it per worker is the defect this repository has already shipped once in
// .gitignore -- one instance fixed, the class left open.
func ResolveHelper(disabledEnv string) ([]string, error) {
	if os.Getenv(disabledEnv) == "true" {
		return nil, nil
	}
	if !Available() {
		return nil, fmt.Errorf("landlock is unavailable on this kernel; "+
			"the engine would run with read access to every file in this container. "+
			"Set %s=true to accept that deliberately", disabledEnv)
	}
	// os.Executable and not a hardcoded path: this binary is the helper, and
	// resolving it by name would depend on a PATH the engine's own allowlist
	// environment controls.
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate this binary to use as the sandbox helper: %w", err)
	}

	// THE ABI QUERY IS NOT PROOF THAT CONFINEMENT CAN BE ESTABLISHED.
	//
	// It answers "does this kernel implement Landlock", and a runtime policy
	// can allow that question and deny the answer: a seccomp profile
	// permitting landlock_create_ruleset while blocking landlock_restrict_self,
	// an LSM stacking limit, a container runtime filtering one syscall of the
	// three. Proving it is a preflight's job -- exec the REAL engine under the
	// REAL ruleset, which exercises create, add_rule, restrict_self and execve
	// at once.
	return []string{self, ExecMode}, nil
}
