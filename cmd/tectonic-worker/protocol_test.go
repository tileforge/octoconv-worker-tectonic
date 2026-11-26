package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
)

func TestProtocolDialReadsImageVariables(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	ca := filepath.Join(dir, "ca.pem")
	manifest := filepath.Join(dir, "manifest.json")
	for _, path := range []string{cert, key, ca, manifest} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.New()
	t.Setenv("TECTONIC_PROTOCOL_CORE_URL", "https://core.example")
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_CERT", cert)
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_KEY", key)
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_CA", ca)
	t.Setenv("TECTONIC_PROTOCOL_WORKER_ID", id.String())
	t.Setenv("TECTONIC_PROTOCOL_MANIFEST_PATH", manifest)
	t.Setenv("TECTONIC_WORKER_CONCURRENCY", "")
	t.Setenv("TECTONIC_ENGINE_TIMEOUT", "90s")
	dial, err := loadProtocolDial()
	if err != nil {
		t.Fatal(err)
	}
	if !dial.ready || dial.workerID != id || dial.freeSlots != 1 || dial.core.Host != "core.example" || dial.engineLimit != 90*time.Second {
		t.Fatalf("dial %+v", dial)
	}
	t.Setenv("TECTONIC_WORKER_CONCURRENCY", "1 # one slot")
	dial, err = loadProtocolDial()
	if err != nil || dial.freeSlots != 1 {
		t.Fatalf("dial %+v %v", dial, err)
	}
	t.Setenv("TECTONIC_ENGINE_TIMEOUT", "")
	dial, err = loadProtocolDial()
	if err != nil || dial.engineLimit != convert.DefaultEngineTimeout {
		t.Fatalf("default budget %+v %v", dial, err)
	}
}

func TestProtocolDialRejectsExtraSlots(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	ca := filepath.Join(dir, "ca.pem")
	manifest := filepath.Join(dir, "manifest.json")
	for _, path := range []string{cert, key, ca, manifest} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.New()
	t.Setenv("TECTONIC_PROTOCOL_CORE_URL", "https://core.example")
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_CERT", cert)
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_KEY", key)
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_CA", ca)
	t.Setenv("TECTONIC_PROTOCOL_WORKER_ID", id.String())
	t.Setenv("TECTONIC_PROTOCOL_MANIFEST_PATH", manifest)
	for _, value := range []string{"2", "0", "nope"} {
		t.Setenv("TECTONIC_WORKER_CONCURRENCY", value)
		if _, err := loadProtocolDial(); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestProtocolDialRejectsPartialEnvironment(t *testing.T) {
	for _, key := range []string{
		"TECTONIC_PROTOCOL_CORE_URL",
		"TECTONIC_PROTOCOL_CLIENT_CERT",
		"TECTONIC_PROTOCOL_CLIENT_KEY",
		"TECTONIC_PROTOCOL_CLIENT_CA",
		"TECTONIC_PROTOCOL_WORKER_ID",
		"TECTONIC_PROTOCOL_MANIFEST_PATH",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("TECTONIC_PROTOCOL_CORE_URL", "https://core.example")
	if _, err := loadProtocolDial(); err == nil {
		t.Fatal("partial dial")
	}
}

func TestProtocolDialRejectsCleartextAndUppercaseID(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	ca := filepath.Join(dir, "ca.pem")
	manifest := filepath.Join(dir, "manifest.json")
	for _, path := range []string{cert, key, ca, manifest} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.New()
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_CERT", cert)
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_KEY", key)
	t.Setenv("TECTONIC_PROTOCOL_CLIENT_CA", ca)
	t.Setenv("TECTONIC_PROTOCOL_MANIFEST_PATH", manifest)
	t.Setenv("TECTONIC_PROTOCOL_WORKER_ID", id.String())
	t.Setenv("TECTONIC_PROTOCOL_CORE_URL", "http://core.example")
	if _, err := loadProtocolDial(); err == nil {
		t.Fatal("http dial")
	}
	t.Setenv("TECTONIC_PROTOCOL_CORE_URL", "https://core.example")
	t.Setenv("TECTONIC_PROTOCOL_WORKER_ID", strings.ToUpper(id.String()))
	if _, err := loadProtocolDial(); err == nil {
		t.Fatal("uppercase worker id")
	}
}

func TestRefuseLegacyStores(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_ADDR", "")
	if err := refuseLegacyStores(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "postgres://db.example/octo")
	if err := refuseLegacyStores(); err == nil {
		t.Fatal("database url")
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_ADDR", "redis.example:6379")
	if err := refuseLegacyStores(); err == nil {
		t.Fatal("redis")
	}
}

func TestManifestOperation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-tectonic-convert.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := manifestOperation(raw)
	if err != nil || got != convert.OperationConvert {
		t.Fatalf("operation %q %v", got, err)
	}
	for _, body := range []string{
		`{"capabilities":[{"legacy":{"operation":"convert","engine":"cad"}}]}`,
		`{"capabilities":[{"legacy":{"operation":"render","engine":"document"}}]}`,
		`{"capabilities":[{"legacy":{"operation":"convert","engine":"document"}},{"legacy":{"operation":"convert","engine":"document"}}]}`,
	} {
		if _, err := manifestOperation([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestModuleDoesNotImportStores(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"github.com/jackc/pgx",
		"github.com/redis/go-redis",
		"github.com/hibiken/asynq",
		"github.com/minio/minio-go",
	}
	set := token.NewFileSet()
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
			return fs.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(set, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			for _, bad := range forbidden {
				if imported == bad || strings.HasPrefix(imported, bad+"/") {
					t.Fatalf("%s imports %s", path, imported)
				}
			}
		}
		if entry.Name() != "main.go" {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(body, []byte("DATABASE_URL")) || bytes.Contains(body, []byte("REDIS_ADDR")) {
				t.Fatalf("%s names a legacy store", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssignmentOutputs(t *testing.T) {
	primary := fileLinkJSON{Role: "primary", Ordinal: 0, Href: "https://files/primary", Format: "pdf"}
	got, ok := assignmentOutputs([]fileLinkJSON{primary})
	if !ok || got.Href != primary.Href {
		t.Fatalf("primary %+v ok=%v", got, ok)
	}
	if _, ok = assignmentOutputs([]fileLinkJSON{primary, {Role: "measurements", Ordinal: 0, Href: "https://files/m", Format: "json"}}); ok {
		t.Fatal("measurements accepted")
	}
	if _, ok = assignmentOutputs([]fileLinkJSON{primary, primary}); ok {
		t.Fatal("two primaries accepted")
	}
	if _, ok = assignmentOutputs([]fileLinkJSON{{Role: "primary", Ordinal: 1, Href: "https://files/primary", Format: "pdf"}}); ok {
		t.Fatal("ordinal accepted")
	}
}

func TestTectonicEngineCode(t *testing.T) {
	if code, _ := tectonicEngineCode(context.DeadlineExceeded); code != "timeout" {
		t.Fatal("timeout")
	}
	if code, _ := tectonicEngineCode(os.ErrNotExist); code != "engine_error" {
		t.Fatal("engine")
	}
	rasterTimeout := fmt.Errorf("pdftoppm: %w: %v", convert.ErrTeXEngineTimeout, fmt.Errorf("pdftoppm killed: %w", context.DeadlineExceeded))
	if code, _ := tectonicEngineCode(rasterTimeout); code != "engine_error" {
		t.Fatalf("raster timeout %s", code)
	}
	if code, _ := tectonicEngineCode(fmt.Errorf("pdftoppm: %w", convert.ErrTeXEngineStarved)); code != "timeout" {
		t.Fatal("raster starvation")
	}
	if transportCode(context.DeadlineExceeded) != "timeout" || transportCode(os.ErrNotExist) != "engine_error" {
		t.Fatal("transport")
	}
}

type deadlineTrip struct {
	base http.RoundTripper
	ok   bool
	left time.Duration
}

func (d *deadlineTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	if !d.ok {
		if dl, ok := r.Context().Deadline(); ok {
			d.ok = true
			d.left = time.Until(dl)
		}
	}
	return d.base.RoundTrip(r)
}

func protocolTestClient(t *testing.T, handler http.Handler, trip *deadlineTrip) *protocolClient {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	core, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := srv.Client()
	if trip != nil {
		trip.base = httpClient.Transport
		httpClient.Transport = trip
	}
	return &protocolClient{
		core:     core,
		coreHTTP: httpClient,
		term:     make(chan struct{}),
	}
}

func TestHelloRequestHasDeadline(t *testing.T) {
	trip := &deadlineTrip{}
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"sess","session_token":"token","lease_seconds":30,"pull_wait_seconds":0,"heartbeat_interval_seconds":10}`)
	}), trip)
	if _, err := c.hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !trip.ok {
		t.Fatal("hello has no deadline")
	}
	if trip.left > helloTimeout || trip.left < helloTimeout-2*time.Second {
		t.Fatalf("hello deadline %s", trip.left)
	}
}

func TestRenewRetryWaitStaysInsideTheLease(t *testing.T) {
	if got := renewRetryWait(time.Time{}, 1); got != time.Second {
		t.Fatalf("first backoff %s", got)
	}
	if got := renewRetryWait(time.Time{}, 2); got != 2*time.Second {
		t.Fatalf("second backoff %s", got)
	}
	if got := renewRetryWait(time.Time{}, 3); got != 4*time.Second {
		t.Fatalf("third backoff %s", got)
	}
	if got := renewRetryWait(time.Time{}, 6); got != 8*time.Second {
		t.Fatalf("capped backoff %s", got)
	}
	if got := renewRetryWait(time.Now().Add(-time.Second), 1); got != 0 {
		t.Fatalf("expired lease %s", got)
	}
	until := time.Now().Add(15 * time.Second)
	for failures := 1; failures <= 6; failures++ {
		wait := renewRetryWait(until, failures)
		if wait <= 0 || !time.Now().Add(wait).Before(until) {
			t.Fatalf("failures %d wait %s until %s", failures, wait, until)
		}
	}
	short := time.Now().Add(500 * time.Millisecond)
	wait := renewRetryWait(short, 4)
	if wait <= 0 || !time.Now().Add(wait).Before(short) {
		t.Fatalf("short lease wait %s", wait)
	}
}

func TestCappedWaitRenewsBeforeExpiry(t *testing.T) {
	if got := cappedWait(time.Time{}, 15*time.Second); got != 15*time.Second {
		t.Fatalf("open lease %s", got)
	}
	if got := cappedWait(time.Now().Add(-time.Second), renewEvery(30)); got != 0 {
		t.Fatalf("expired %s", got)
	}
	long := time.Now().Add(time.Minute)
	if got := cappedWait(long, renewEvery(30)); got != 15*time.Second {
		t.Fatalf("half interval %s", got)
	}
	until := time.Now().Add(10 * time.Second)
	before := time.Now()
	got := cappedWait(until, renewEvery(30))
	if got <= 0 || !before.Add(got).Before(until.Add(-10*time.Millisecond)) {
		t.Fatalf("short remainder %s until %s", got, until)
	}
}

func TestFailedRenewRetriesBeforeExpiry(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	leaseUntil := time.Now().Add(2 * time.Second)
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/lease") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		posts++
		n := posts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		raw, err := json.Marshal(map[string]string{
			"lease_expires_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}), nil)
	c.leaseSeconds = 0
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempt, cancelAttempt := context.WithCancelCause(parent)
	defer cancelAttempt(context.Canceled)
	var flag stopFlag
	done := make(chan struct{})
	go func() {
		c.renewLoop(parent, attempt, cancelAttempt, &flag, assignmentJSON{AttemptID: "attempt", AttemptToken: "tok"}, leaseUntil, time.Now().Add(time.Hour))
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := posts
		mu.Unlock()
		if n >= 2 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := posts
	mu.Unlock()
	if got < 2 || !time.Now().Before(leaseUntil) {
		t.Fatalf("posts %d at %s before %s", got, time.Now(), leaseUntil)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop")
	}
	if flag.outcome() == "lease_lost" {
		t.Fatal("lease lost after a recovered renew")
	}
}

func TestRenewFailuresStopAtExpiry(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	leaseUntil := time.Now().Add(600 * time.Millisecond)
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/lease") {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.leaseSeconds = 0
	parent := context.Background()
	attempt, cancelAttempt := context.WithCancelCause(parent)
	defer cancelAttempt(context.Canceled)
	var flag stopFlag
	done := make(chan struct{})
	go func() {
		c.renewLoop(parent, attempt, cancelAttempt, &flag, assignmentJSON{AttemptID: "attempt", AttemptToken: "tok"}, leaseUntil, time.Now().Add(time.Hour))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop")
	}
	since := time.Since(leaseUntil)
	if since < -50*time.Millisecond || since > 750*time.Millisecond {
		t.Fatalf("stopped %s after expiry", since)
	}
	mu.Lock()
	got := posts
	mu.Unlock()
	if got < 2 || flag.outcome() != "lease_lost" {
		t.Fatalf("posts %d outcome %s", got, flag.outcome())
	}
}

type blockConverter struct {
	entered chan struct{}
}

func (blockConverter) Pairs() []convert.Pair { return nil }

func (b blockConverter) Convert(ctx context.Context, _, _ string, _ map[string]any) error {
	close(b.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestHeartbeatSessionClosedStopsTheAttempt(t *testing.T) {
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("source"))
			return
		}
		if strings.Contains(r.URL.Path, "/heartbeat") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"session_closed"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	c.engineLimit = time.Minute
	c.sessionID = "sess"
	c.sessionToken = "token"
	c.leaseSeconds = 30
	entered := make(chan struct{})
	c.lookupOp = func(op, from, to string) (convert.Converter, bool) {
		if op == convert.OperationConvert && from == "docx" && to == "pdf" {
			return blockConverter{entered: entered}, true
		}
		return nil, false
	}
	later := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	href := c.core.String() + "/objects/docx"
	asgn := assignmentJSON{
		AttemptID:       "attempt",
		AttemptToken:    "attempt-token",
		AttemptDeadline: later,
		LeaseExpiresAt:  later,
		Limits:          limitsJSON{MaxOutputBytes: convert.MaxFileBytes},
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "docx"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "pdf"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		select {
		case <-entered:
		case <-ctx.Done():
			return
		}
		_ = c.postHeartbeat(ctx, []byte(`{"protocol_version":1}`))
	}()
	err := c.runAssignment(ctx, asgn, controlJSON{})
	if !errors.Is(err, errSessionClosed) {
		t.Fatalf("assignment %v", err)
	}
	if !errors.Is(c.sessionFailure(), errSessionClosed) {
		t.Fatal("session stayed open")
	}
}

func TestHeartbeatLoopStopsAfterSessionClosed(t *testing.T) {
	var mu sync.Mutex
	heartbeats := 0
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		heartbeats++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"session_closed"}`))
	}), nil)
	c.sessionID = "sess"
	c.sessionToken = "token"
	c.beatEvery = 15 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.heartbeatLoop(ctx)
	mu.Lock()
	got := heartbeats
	mu.Unlock()
	if got != 1 || !errors.Is(c.sessionFailure(), errSessionClosed) {
		t.Fatalf("heartbeats %d session %v", got, c.sessionFailure())
	}
}

func TestSetActiveRefusesARecordedStop(t *testing.T) {
	refused := func(t *testing.T, c *protocolClient, want error) {
		t.Helper()
		_, cancel := context.WithCancelCause(context.Background())
		defer cancel(context.Canceled)
		var flag stopFlag
		err := c.setActive(&liveAttempt{
			asgn:   assignmentJSON{AttemptID: "attempt"},
			parent: context.Background(),
			cancel: cancel,
			flag:   &flag,
		})
		if !errors.Is(err, want) {
			t.Fatalf("err %v", err)
		}
		c.mu.Lock()
		active := c.active
		c.mu.Unlock()
		if active != nil || flag.outcome() != "" {
			t.Fatalf("active %v outcome %s", active != nil, flag.outcome())
		}
	}
	terminated := protocolTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
	terminated.applyHeartbeat(controlJSON{Terminate: true})
	refused(t, terminated, errSessionTerminated)

	closed := protocolTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
	closed.noteSessionClosed()
	refused(t, closed, errSessionClosed)
}

func TestHeartbeatErrorLeavesTheAttemptRunning(t *testing.T) {
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}), nil)
	c.sessionID = "sess"
	c.sessionToken = "token"
	attempt, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	var flag stopFlag
	if err := c.setActive(&liveAttempt{
		asgn:   assignmentJSON{AttemptID: "attempt"},
		parent: context.Background(),
		cancel: cancel,
		flag:   &flag,
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.postHeartbeat(context.Background(), []byte(`{"protocol_version":1}`)); err != nil {
		t.Fatal(err)
	}
	if c.sessionFailure() != nil || attempt.Err() != nil || flag.outcome() != "" {
		t.Fatalf("session %v attempt %v outcome %s", c.sessionFailure(), attempt.Err(), flag.outcome())
	}
}

type fixedConverter struct {
	body []byte
}

func (fixedConverter) Pairs() []convert.Pair { return nil }

func (f fixedConverter) Convert(_ context.Context, _, outPath string, _ map[string]any) error {
	return os.WriteFile(outPath, f.body, 0o600)
}

type recordingConverter struct {
	opts map[string]any
}

func (recordingConverter) Pairs() []convert.Pair { return nil }

func (r *recordingConverter) Convert(_ context.Context, _, outPath string, opts map[string]any) error {
	r.opts = opts
	return os.WriteFile(outPath, []byte("<p>ok</p>"), 0o600)
}

func TestAssignmentParameters(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "  {}  "} {
		opts, err := assignmentParameters(json.RawMessage(raw))
		if err != nil || opts != nil {
			t.Fatalf("%q -> %#v %v", raw, opts, err)
		}
	}
	if _, err := assignmentParameters(json.RawMessage(`{"quality":1}`)); err == nil {
		t.Fatal("parameter key accepted")
	}
	if _, err := assignmentParameters(json.RawMessage(`[1]`)); err == nil {
		t.Fatal("array accepted")
	}
	if _, err := assignmentParameters(json.RawMessage(`"none"`)); err == nil {
		t.Fatal("string accepted")
	}
}

func TestImageParametersReachTheConverter(t *testing.T) {
	var mu sync.Mutex
	var fails [][]byte
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("# hi\n"))
			return
		}
		if strings.Contains(r.URL.Path, "/fail") {
			mu.Lock()
			fails = append(fails, append([]byte(nil), body...))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	c.engineLimit = time.Minute
	c.leaseSeconds = 30
	c.sessionToken = "token"
	rec := &recordingConverter{}
	c.lookupOp = func(op, from, to string) (convert.Converter, bool) {
		if op != convert.OperationConvert || from != "png" || to != "webp" {
			return nil, false
		}
		return rec, true
	}
	href := c.core.String() + "/objects/png"
	asgn := assignmentJSON{
		AttemptID:       "png-webp",
		AttemptToken:    "attempt-token",
		AttemptDeadline: time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Limits:          limitsJSON{MaxOutputBytes: convert.MaxFileBytes},
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "png"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "webp"}},
	}
	if err := c.runAssignment(context.Background(), asgn, controlJSON{}); err != nil {
		t.Fatal(err)
	}
	if rec.opts != nil {
		t.Fatalf("%#v", rec.opts)
	}

	rec.opts = map[string]any{"called": true}
	asgn.AttemptID = "bad-parameters"
	asgn.Parameters = json.RawMessage(`{"quality":1}`)
	if err := c.runAssignment(context.Background(), asgn, controlJSON{}); err != nil {
		t.Fatal(err)
	}
	if _, called := rec.opts["called"]; !called {
		t.Fatal("parameter key reached the converter")
	}
	rec.opts = map[string]any{"called": true}
	asgn.AttemptID = "bad-array"
	asgn.Parameters = json.RawMessage(`[1]`)
	if err := c.runAssignment(context.Background(), asgn, controlJSON{}); err != nil {
		t.Fatal(err)
	}
	if _, called := rec.opts["called"]; !called {
		t.Fatal("parameter array reached the converter")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fails) != 2 {
		t.Fatalf("fail bodies %d", len(fails))
	}
	if !bytes.Contains(fails[0], []byte(`"engine_code":"invalid_options"`)) || !bytes.Contains(fails[0], []byte(`"message":"stored conversion options are invalid"`)) {
		t.Fatalf("options body %s", fails[0])
	}
	if !bytes.Contains(fails[1], []byte(`"engine_code":"engine_error"`)) || !bytes.Contains(fails[1], []byte(`"message":"assignment"`)) {
		t.Fatalf("array body %s", fails[1])
	}
}

func TestExtraInputsLandBesideTheDocument(t *testing.T) {
	job := "11111111-1111-4111-8111-111111111111"
	var mu sync.Mutex
	snap := map[string][]byte{}
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch {
			case strings.HasSuffix(r.URL.Path, "/0-main.tex"):
				_, _ = w.Write([]byte("document"))
			case strings.HasSuffix(r.URL.Path, "/1-preamble.tex"):
				_, _ = w.Write([]byte("preamble"))
			case strings.HasSuffix(r.URL.Path, "/2-refs.bib"):
				_, _ = w.Write([]byte("bib"))
			default:
				http.NotFound(w, r)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	c.engineLimit = time.Minute
	c.leaseSeconds = 30
	c.sessionToken = "token"
	c.lookupOp = func(op, from, to string) (convert.Converter, bool) {
		if op != convert.OperationConvert || from != "latex" || to != "pdf" {
			return nil, false
		}
		return dirSnapConverter{mu: &mu, snap: &snap}, true
	}
	base := c.core.String()
	out := base + "/objects/out.pdf"
	asgn := assignmentJSON{
		JobID:           job,
		AttemptID:       "multi",
		AttemptToken:    "attempt-token",
		AttemptDeadline: time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Limits:          limitsJSON{MaxOutputBytes: convert.MaxFileBytes},
		Inputs: []fileLinkJSON{
			{Role: "source", Ordinal: 2, Format: "bib", Href: base + "/bucket/uploads/" + job + "/2-refs.bib"},
			{Role: "source", Ordinal: 0, Format: "latex", Href: base + "/bucket/uploads/" + job + "/0-main.tex"},
			{Role: "source", Ordinal: 1, Format: "latex", Href: base + "/bucket/uploads/" + job + "/1-preamble.tex"},
		},
		Outputs: []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: out, Format: "pdf"}},
	}
	if err := c.runAssignment(context.Background(), asgn, controlJSON{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(snap["in.latex"]) != "document" || string(snap["preamble.tex"]) != "preamble" || string(snap["refs.bib"]) != "bib" {
		t.Fatalf("files %#v", snap)
	}
	if _, ok := snap["in.pdf"]; ok {
		t.Fatal("attachment replaced the engine output name")
	}
}

func TestOwnedExtraInputFailsBeforeConvert(t *testing.T) {
	job := "11111111-1111-4111-8111-111111111111"
	var mu sync.Mutex
	var fails [][]byte
	called := false
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(r.URL.Path, "/fail") {
			mu.Lock()
			fails = append(fails, append([]byte(nil), body...))
			mu.Unlock()
		}
		if r.Method == http.MethodGet {
			t.Errorf("downloaded %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	c.engineLimit = time.Minute
	c.leaseSeconds = 30
	c.sessionToken = "token"
	c.lookupOp = func(string, string, string) (convert.Converter, bool) {
		called = true
		return fixedConverter{body: []byte("%PDF")}, true
	}
	href := "https://files.example/bucket/uploads/" + job + "/1-in.pdf"
	asgn := assignmentJSON{
		JobID:           job,
		AttemptID:       "owned",
		AttemptToken:    "attempt-token",
		AttemptDeadline: time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Inputs: []fileLinkJSON{
			{Role: "source", Ordinal: 0, Format: "latex", Href: "https://files.example/document"},
			{Role: "source", Ordinal: 1, Format: "pdf", Href: href},
		},
		Outputs: []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: "https://files.example/out", Format: "pdf"}},
	}
	if err := c.runAssignment(context.Background(), asgn, controlJSON{}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("converter ran")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fails) != 1 || !bytes.Contains(fails[0], []byte(`"engine_code":"engine_error"`)) || !bytes.Contains(fails[0], []byte(`"message":"assignment"`)) {
		t.Fatalf("fail bodies %d %s", len(fails), fails)
	}
}

type dirSnapConverter struct {
	mu   *sync.Mutex
	snap *map[string][]byte
}

func (dirSnapConverter) Pairs() []convert.Pair { return nil }

func (d dirSnapConverter) Convert(_ context.Context, inPath, outPath string, _ map[string]any) error {
	dir := filepath.Dir(inPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	got := map[string][]byte{}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		got[entry.Name()] = body
	}
	d.mu.Lock()
	*d.snap = got
	d.mu.Unlock()
	return os.WriteFile(outPath, []byte("%PDF"), 0o600)
}

func TestImagePairsPostAnAcceptedComplete(t *testing.T) {
	pairs := (convert.TectonicConverter{}).Pairs()
	if len(pairs) != 1 {
		t.Fatalf("pairs %d", len(pairs))
	}
	var mu sync.Mutex
	puts := map[string]putBody{}
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte("source"))
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			puts[r.URL.Path] = putBody{body: body, contentType: r.Header.Get("Content-Type")}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(r.URL.Path, "/complete") {
				if r.Header.Get("Octoconv-Attempt") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				mu.Lock()
				err = acceptImageComplete(raw, puts)
				mu.Unlock()
				if err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"code":"artifact_rejected"}`))
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	c.engineLimit = time.Minute
	c.leaseSeconds = 30
	c.sessionToken = "token"
	for _, pair := range pairs {
		body := []byte(pair.From + "->" + pair.To)
		c.lookupOp = func(op, from, to string) (convert.Converter, bool) {
			if op != convert.OperationConvert || from != pair.From || to != pair.To {
				return nil, false
			}
			return fixedConverter{body: body}, true
		}
		href := c.core.String() + "/objects/" + pair.From + "-" + pair.To
		asgn := assignmentJSON{
			AttemptID:       pair.From + "-" + pair.To,
			AttemptToken:    "attempt-token",
			AttemptDeadline: time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
			LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			Limits:          limitsJSON{MaxOutputBytes: convert.MaxFileBytes},
			Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: pair.From}},
			Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: pair.To}},
		}
		if err := c.runAssignment(context.Background(), asgn, controlJSON{}); err != nil {
			t.Fatalf("%s to %s: %v", pair.From, pair.To, err)
		}
		mu.Lock()
		put := puts["/objects/"+pair.From+"-"+pair.To]
		mu.Unlock()
		if !bytes.Equal(put.body, body) {
			t.Fatalf("%s to %s put %q", pair.From, pair.To, put.body)
		}
		if put.contentType != convert.MIMEType(pair.To) {
			t.Fatalf("%s to %s content type %s", pair.From, pair.To, put.contentType)
		}
	}
}

type putBody struct {
	body        []byte
	contentType string
}

func acceptImageComplete(raw []byte, puts map[string]putBody) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var body struct {
		ProtocolVersion int `json:"protocol_version"`
		Artifacts       []struct {
			Role        string `json:"role"`
			Ordinal     int    `json:"ordinal"`
			Format      string `json:"format"`
			ContentType string `json:"content_type"`
			SHA256      string `json:"sha256"`
			ByteSize    int64  `json:"byte_size"`
		} `json:"artifacts"`
	}
	if err := dec.Decode(&body); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return os.ErrInvalid
	}
	if body.ProtocolVersion != 2 || len(body.Artifacts) != 1 {
		return os.ErrInvalid
	}
	art := body.Artifacts[0]
	if art.Role != "primary" || art.Ordinal != 0 || art.Format == "" {
		return os.ErrInvalid
	}
	if art.ContentType != convert.MIMEType(art.Format) || art.ByteSize < 0 || art.ByteSize > convert.MaxFileBytes {
		return os.ErrInvalid
	}
	if len(art.SHA256) != 64 || strings.ToLower(art.SHA256) != art.SHA256 {
		return os.ErrInvalid
	}
	var matched bool
	for _, put := range puts {
		sum := sha256.Sum256(put.body)
		if hex.EncodeToString(sum[:]) != art.SHA256 {
			continue
		}
		if put.contentType != art.ContentType || int64(len(put.body)) != art.ByteSize {
			return os.ErrInvalid
		}
		matched = true
	}
	if !matched {
		return os.ErrInvalid
	}
	return nil
}

func TestAttemptDeadlineStopsStalledDownload(t *testing.T) {
	failed := make(chan []byte, 1)
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-r.Context().Done()
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(r.URL.Path, "/fail") {
			failed <- raw
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	c.engineLimit = time.Minute
	href := c.core.String() + "/objects"
	asgn := assignmentJSON{
		AttemptID:       "attempt",
		AttemptToken:    "tok",
		AttemptDeadline: time.Now().Add(200 * time.Millisecond).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "docx"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "pdf"}},
	}
	done := make(chan error, 1)
	go func() {
		done <- c.runAssignment(context.Background(), asgn, controlJSON{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attempt ignored the deadline")
	}
	select {
	case body := <-failed:
		if !strings.Contains(string(body), `"engine_code":"timeout"`) {
			t.Fatalf("fail body %s", body)
		}
	default:
		t.Fatal("fail was not posted")
	}
}

type renderCapture struct {
	mu      sync.Mutex
	gets    int
	puts    int
	putBody []byte
	putType string
	fails   [][]byte
}

type renderRecorder struct {
	body   []byte
	opts   map[string]any
	called int
}

func (renderRecorder) Pairs() []convert.Pair { return nil }

func (r *renderRecorder) Convert(_ context.Context, _, outPath string, opts map[string]any) error {
	r.called++
	r.opts = opts
	return os.WriteFile(outPath, r.body, 0o600)
}

func newRenderClient(t *testing.T) (*protocolClient, *renderCapture) {
	t.Helper()
	cap := &renderCapture{}
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cap.mu.Lock()
			cap.gets++
			cap.mu.Unlock()
			_, _ = w.Write([]byte("hello\n"))
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			cap.mu.Lock()
			cap.puts++
			cap.putBody = append([]byte(nil), body...)
			cap.putType = r.Header.Get("Content-Type")
			cap.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(r.URL.Path, "/fail") {
				cap.mu.Lock()
				cap.fails = append(cap.fails, append([]byte(nil), body...))
				cap.mu.Unlock()
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationRender
	c.engineLimit = time.Minute
	c.leaseSeconds = 30
	c.sessionToken = "token"
	return c, cap
}

func renderAssignment(c *protocolClient, id string, params json.RawMessage) assignmentJSON {
	href := c.core.String() + "/objects/" + id
	return assignmentJSON{
		AttemptID:       id,
		AttemptToken:    "attempt-token",
		AttemptDeadline: time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Limits:          limitsJSON{MaxOutputBytes: convert.MaxFileBytes},
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "latex"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "png"}},
		Parameters:      params,
	}
}

func TestTectonicRenderManifestOperation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-tectonic-render.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := manifestOperation(raw)
	if err != nil || got != convert.OperationRender {
		t.Fatalf("operation %q %v", got, err)
	}
	if _, err := manifestOperation([]byte(`{"capabilities":[{"legacy":{"operation":"render","engine":"html"}}]}`)); err == nil || err.Error() != "tectonic protocol manifest" {
		t.Fatal(err)
	}
}

func TestTectonicRenderAssignmentParameters(t *testing.T) {
	c, cap := newRenderClient(t)
	rec := &renderRecorder{body: []byte("render-png")}
	c.lookupOp = func(op, from, to string) (convert.Converter, bool) {
		if op != convert.OperationRender || from != "latex" || to != "png" {
			return nil, false
		}
		return rec, true
	}
	bad := []json.RawMessage{
		json.RawMessage(`{"size":256}`),
		json.RawMessage(`{"quality":1}`),
		json.RawMessage(`[1]`),
		json.RawMessage(`"none"`),
	}
	for i, params := range bad {
		if err := c.runAssignment(context.Background(), renderAssignment(c, fmt.Sprintf("bad-%d", i), params), controlJSON{}); err != nil {
			t.Fatal(err)
		}
		cap.mu.Lock()
		gets, puts := cap.gets, cap.puts
		fails := len(cap.fails)
		var body []byte
		if fails > 0 {
			body = append([]byte(nil), cap.fails[fails-1]...)
		}
		cap.mu.Unlock()
		if rec.called != 0 || gets != 0 || puts != 0 || fails != i+1 {
			t.Fatalf("called %d gets %d puts %d fails %d", rec.called, gets, puts, fails)
		}
		if !bytes.Contains(body, []byte(`"protocol_version":2`)) || !bytes.Contains(body, []byte(`"engine_code":"engine_error"`)) || !bytes.Contains(body, []byte(`"message":"assignment"`)) {
			t.Fatalf("fail body %s", body)
		}
	}
	for i, params := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("{}")} {
		rec.opts = map[string]any{"sentinel": true}
		if err := c.runAssignment(context.Background(), renderAssignment(c, fmt.Sprintf("empty-%d", i), params), controlJSON{}); err != nil {
			t.Fatal(err)
		}
		if rec.opts != nil {
			t.Fatalf("opts %#v", rec.opts)
		}
	}
	if rec.called != 3 {
		t.Fatalf("called %d", rec.called)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.putType != "image/png" || !bytes.Equal(cap.putBody, rec.body) {
		t.Fatalf("put %s %q", cap.putType, cap.putBody)
	}
}
