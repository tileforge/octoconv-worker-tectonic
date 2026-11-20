// Command tectonic-worker serves one approved tectonic capability over Worker Protocol v2.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
	"github.com/tileforge/octoconv-worker-tectonic/internal/sandbox"
)

const sandboxDisabledEnv = "TECTONIC_SANDBOX_DISABLED"

func main() {
	if sandbox.RunExecModeIfRequested(os.Args) {
		return
	}
	if err := refuseLegacyStores(); err != nil {
		log.Fatalf("tectonic protocol: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bundlePath := os.Getenv(convert.TectonicBundleEnv)
	if bundlePath == "" {
		bundlePath = convert.DefaultTectonicBundlePath
	}
	if err := convert.ValidateTectonicBundle(bundlePath); err != nil {
		log.Fatalf("tectonic bundle: %v (set %s)", err, convert.TectonicBundleEnv)
	}
	maxOutputBytes := envInt64("TECTONIC_MAX_OUTPUT_BYTES", convert.DefaultTectonicMaxOutputBytes)

	sandboxHelper, err := sandbox.ResolveHelper(sandboxDisabledEnv)
	if err != nil {
		log.Fatalf("tectonic sandbox: %v", err)
	}
	if len(sandboxHelper) > 0 {
		if err := convert.ValidateTectonicCacheDir(os.Getenv(convert.TectonicCacheEnv)); err != nil {
			log.Fatalf("tectonic format cache: %v", err)
		}
	}
	confineFS := len(sandboxHelper) > 0
	if !confineFS {
		self, serr := os.Executable()
		if serr != nil {
			log.Fatalf("locate this binary to use as the exec helper: %v", serr)
		}
		sandboxHelper = []string{self, sandbox.ExecMode}
		log.Printf("%s=true: the engine can read every file in this container", sandboxDisabledEnv)
	} else {
		log.Printf("tectonic sandbox: landlock via %s", strings.Join(sandboxHelper, " "))
	}

	if raw := os.Getenv("TECTONIC_ENGINE_CPU_SECONDS"); raw != "" {
		if _, err := time.ParseDuration(firstField(raw)); err != nil {
			log.Fatalf("TECTONIC_ENGINE_CPU_SECONDS=%q is not a duration: %v", raw, err)
		}
	}
	cpuBudget := envDuration("TECTONIC_ENGINE_CPU_SECONDS", 271*time.Second)
	if cpuBudget < time.Second {
		log.Fatalf("TECTONIC_ENGINE_CPU_SECONDS=%s is below one second", cpuBudget)
	}
	cpuSeconds := int(cpuBudget.Seconds())
	tectonicConverter := buildTectonicConverter(bundlePath, maxOutputBytes, sandboxHelper, cpuSeconds, confineFS)
	proofBytes, err := preflightEngine(ctx, tectonicConverter, sandboxHelper)
	if err != nil {
		log.Fatalf("tectonic sandbox preflight: %v", err)
	}
	if confineFS {
		log.Printf("tectonic sandbox proven: tectonic ran inside the job directory and was denied a file outside every grant (output %d bytes, no outside output)", proofBytes)
	}
	convert.Default.Replace(tectonicConverter)

	dial, err := loadProtocolDial()
	if err != nil {
		log.Fatalf("tectonic protocol: %v", err)
	}
	log.Printf("tectonic protocol worker starting")
	if err := runProtocol(ctx, dial); err != nil {
		log.Fatalf("tectonic protocol: %v", err)
	}
}

func refuseLegacyStores() error {
	if os.Getenv("DATABASE_URL") != "" || os.Getenv("REDIS_ADDR") != "" {
		return errors.New("DATABASE_URL and REDIS_ADDR are not used")
	}
	return nil
}

func tectonicFreeSlots() (int, error) {
	raw := os.Getenv("TECTONIC_WORKER_CONCURRENCY")
	if raw == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(firstField(raw))
	if err != nil || n != 1 {
		return 0, errors.New("TECTONIC_WORKER_CONCURRENCY must be 1")
	}
	return 1, nil
}

func buildTectonicConverter(bundlePath string, maxOutputBytes int64, sandboxHelper []string, cpuSeconds int, confineFS bool) convert.TectonicConverter {
	return convert.TectonicConverter{
		BundlePath:        bundlePath,
		MaxOutputBytes:    maxOutputBytes,
		SandboxHelper:     sandboxHelper,
		CPUSeconds:        cpuSeconds,
		ConfineFilesystem: confineFS,
	}
}

func preflightEngine(ctx context.Context, c convert.TectonicConverter, helper []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := c.TectonicPreflight(ctx, helper)
	if err != nil {
		return 0, fmt.Errorf("%v.\n\nThree causes are usual, and the engine's own output above says which:\n"+
			"  1. the format cache holds no format this engine can use.\n"+
			"  2. a runtime policy permits the Landlock ABI query but denies establishing a ruleset.\n"+
			"  3. the engine's runtime files sit outside the granted directories.\n\n"+
			"Set %s=true to run the engine unconfined deliberately", err, sandboxDisabledEnv)
	}
	return n, nil
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(firstField(v), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(firstField(v)); err == nil {
			return d
		}
	}
	return def
}

func firstField(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}
