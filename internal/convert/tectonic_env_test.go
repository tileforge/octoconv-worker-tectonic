package convert

import (
	"os"
	"strings"
	"testing"
)

// The engine's environment is an allowlist, because a .tex can read files.
//
// --untrusted refuses \write18 (TEX-04, proven by running a hostile document)
// and does NOT confine reads. Measured on the shipped image: a document
// containing
//
//	\newread\f \openin\f=/etc/hostname \read\f to \line ... \line
//
// typeset the container's hostname INTO THE RETURNED PDF, and the same read
// against /proc/self/environ returned this process's environment -- which
// carries DATABASE_URL, S3_ACCESS_KEY, S3_SECRET_KEY, API_KEY_SALT and
// WEBHOOK_SIGNING_SECRET. A .tex was a credential read.
//
// This test is the half that can be asserted without the engine: whatever the
// document can read, the environment it reads must not be worth reading.
func TestTectonicEnvCarriesNoSecrets(t *testing.T) {
	// The names this service actually puts in a worker's environment.
	secrets := []string{
		"DATABASE_URL", "REDIS_ADDR",
		"S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_ENDPOINT",
		"API_KEY_SALT", "WEBHOOK_SIGNING_SECRET",
	}
	for _, name := range secrets {
		t.Setenv(name, "CANARY-"+name)
	}
	// And one the engine legitimately needs, to prove the allowlist is not
	// simply empty -- an env of nothing would pass every assertion below while
	// breaking the engine.
	t.Setenv("TECTONIC_CACHE_DIR", "/var/cache/tectonic")

	env := tectonicEnv("/tmp/octoconv-job-1")

	joined := strings.Join(env, "\n")
	for _, name := range secrets {
		if strings.Contains(joined, "CANARY-"+name) {
			t.Fatalf("%s reaches the engine's environment; a document that reads /proc/self/environ "+
				"gets it, and this service puts credentials there", name)
		}
	}
	if !strings.Contains(joined, "TECTONIC_CACHE_DIR=/var/cache/tectonic") {
		t.Fatal("TECTONIC_CACHE_DIR did not survive the allowlist: the engine would recompile its " +
			"format on every job, and an empty environment would pass the assertions above while " +
			"proving nothing")
	}
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			t.Fatalf("malformed environment entry %q", kv)
		}
	}
}

// TestTectonicEnvOmitsWhatIsUnset keeps the allowlist from inventing values.
// An engine given HOME="" behaves differently from one given no HOME at all,
// and the difference is the kind that shows up once, in production.
func TestTectonicEnvOmitsWhatIsUnset(t *testing.T) {
	if _, ok := os.LookupEnv("OCTOCONV_DEFINITELY_UNSET_XYZ"); ok {
		t.Skip("the variable this test relies on being unset is set")
	}
	for _, kv := range tectonicEnv("") {
		if strings.HasPrefix(kv, "SOURCE_DATE_EPOCH=") && os.Getenv("SOURCE_DATE_EPOCH") == "" {
			t.Fatal("SOURCE_DATE_EPOCH was invented for a child that had none")
		}
	}
}

// TestTectonicEnvPinsTMPDIRToTheJob is the other half of the sandbox's
// per-job grant, and the reason TMPDIR left the allowlist.
//
// Landlock grants are path-beneath and additive: whatever TMPDIR names has to
// be granted, so an INHERITED TMPDIR of /tmp forced a grant on the whole of
// /tmp and made the per-job grant beside it meaningless -- every job could
// read and write every other job's directory, and could leave files outside
// the workDir its deferred cleanup removes (codex, PR #21).
func TestTectonicEnvPinsTMPDIRToTheJob(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")

	const jobDir = "/tmp/octoconv-job-42"
	var got string
	for _, kv := range tectonicEnv(jobDir) {
		if strings.HasPrefix(kv, "TMPDIR=") {
			got = strings.TrimPrefix(kv, "TMPDIR=")
		}
	}
	if got != jobDir {
		t.Fatalf("TMPDIR = %q, want %q. An inherited TMPDIR forces the sandbox to grant the "+
			"SHARED temporary directory, which grants every other job's files along with it.", got, jobDir)
	}
}
