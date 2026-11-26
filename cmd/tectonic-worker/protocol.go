package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tileforge/octoconv-worker-tectonic/internal/convert"
)

const (
	envProtocolCoreURL  = "TECTONIC_PROTOCOL_CORE_URL"
	envProtocolCert     = "TECTONIC_PROTOCOL_CLIENT_CERT"
	envProtocolKey      = "TECTONIC_PROTOCOL_CLIENT_KEY"
	envProtocolCA       = "TECTONIC_PROTOCOL_CLIENT_CA"
	envProtocolWorkerID = "TECTONIC_PROTOCOL_WORKER_ID"
	envProtocolManifest = "TECTONIC_PROTOCOL_MANIFEST_PATH"

	maxProtocolFileBytes = convert.MaxFileBytes
	maxProtocolJSONBytes = 262144

	stoppedNotifyTimeout  = 5 * time.Second
	terminalReportTimeout = 5 * time.Second
	helloTimeout          = 10 * time.Second
	pullGrace             = 5 * time.Second

	protocolVersion = 2
)

var (
	errOriginRejected    = errors.New("origin_rejected")
	errSessionClosed     = errors.New("session_closed")
	errAttemptNotActive  = errors.New("attempt_not_active")
	errSessionTerminated = errors.New("terminate")
)

// protocolSuiteOK accepts the three TLS 1.3 suites the published protocol allows.
func protocolSuiteOK(state tls.ConnectionState) error {
	if state.Version != tls.VersionTLS13 {
		return errors.New("tls version")
	}
	switch state.CipherSuite {
	case tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384, tls.TLS_CHACHA20_POLY1305_SHA256:
		return nil
	default:
		return errors.New("tls suite")
	}
}

type protocolDial struct {
	ready        bool
	core         *url.URL
	certFile     string
	keyFile      string
	caFile       string
	manifestFile string
	workerID     uuid.UUID
	freeSlots    int
	engineLimit  time.Duration
}

func manifestOperation(manifest []byte) (string, error) {
	var doc struct {
		Capabilities []struct {
			Legacy *struct {
				Operation string `json:"operation"`
				Engine    string `json:"engine"`
			} `json:"legacy"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(manifest, &doc); err != nil || len(doc.Capabilities) != 1 || doc.Capabilities[0].Legacy == nil {
		return "", errors.New("tectonic protocol manifest")
	}
	legacy := doc.Capabilities[0].Legacy
	if legacy.Engine != convert.EngineTectonic || (legacy.Operation != convert.OperationConvert && legacy.Operation != convert.OperationRender) {
		return "", errors.New("tectonic protocol manifest")
	}
	return legacy.Operation, nil
}

func loadProtocolDial() (protocolDial, error) {
	values := []string{
		os.Getenv(envProtocolCoreURL),
		os.Getenv(envProtocolCert),
		os.Getenv(envProtocolKey),
		os.Getenv(envProtocolCA),
		os.Getenv(envProtocolWorkerID),
		os.Getenv(envProtocolManifest),
	}
	present := 0
	for _, value := range values {
		if value != "" {
			present++
		}
	}
	if present != len(values) {
		return protocolDial{}, errors.New("tectonic protocol dial environment is incomplete")
	}
	core, err := url.Parse(values[0])
	if err != nil || !core.IsAbs() || core.Scheme != "https" || core.Hostname() == "" {
		return protocolDial{}, errors.New("TECTONIC_PROTOCOL_CORE_URL must be an absolute https URL")
	}
	workerID, err := uuid.Parse(values[4])
	if err != nil || workerID.String() != values[4] {
		return protocolDial{}, errors.New("TECTONIC_PROTOCOL_WORKER_ID must be a UUID")
	}
	names := []string{envProtocolCert, envProtocolKey, envProtocolCA, envProtocolManifest}
	paths := []string{values[1], values[2], values[3], values[5]}
	for i, path := range paths {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return protocolDial{}, fmt.Errorf("%s must be a file", names[i])
		}
	}
	slots, err := tectonicFreeSlots()
	if err != nil {
		return protocolDial{}, err
	}
	limit := envDuration("TECTONIC_ENGINE_TIMEOUT", convert.DefaultEngineTimeout)
	if limit <= 0 {
		limit = convert.DefaultEngineTimeout
	}
	return protocolDial{
		ready:        true,
		core:         core,
		certFile:     values[1],
		keyFile:      values[2],
		caFile:       values[3],
		manifestFile: values[5],
		workerID:     workerID,
		freeSlots:    slots,
		engineLimit:  limit,
	}, nil
}

type protocolClient struct {
	core         *url.URL
	coreHTTP     *http.Client
	fileHTTP     *http.Client
	workerID     uuid.UUID
	manifestSHA  string
	freeSlots    int
	engineLimit  time.Duration
	operation    string
	lookupOp     func(op, from, to string) (convert.Converter, bool)
	sessionID    string
	sessionToken string
	leaseSeconds int
	pullWait     int
	beatEvery    time.Duration

	mu         sync.Mutex
	active     *liveAttempt
	draining   bool
	sessionErr error
	term       chan struct{}
}

func newProtocolClient(dial protocolDial) (*protocolClient, error) {
	cert, err := tls.LoadX509KeyPair(dial.certFile, dial.keyFile)
	if err != nil {
		return nil, errors.New("tectonic protocol client certificate")
	}
	caPEM, err := os.ReadFile(dial.caFile)
	if err != nil {
		return nil, errors.New("tectonic protocol client CA")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("tectonic protocol client CA")
	}
	manifest, err := os.ReadFile(dial.manifestFile)
	if err != nil {
		return nil, errors.New("tectonic protocol manifest")
	}
	operation, err := manifestOperation(manifest)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(manifest)
	coreTLS := protocolTLS(pool, cert, dial.core.Hostname())
	fileTLS := protocolTLS(pool, cert, "")
	return &protocolClient{
		core:        dial.core,
		coreHTTP:    protocolHTTP(coreTLS),
		fileHTTP:    protocolHTTP(fileTLS),
		workerID:    dial.workerID,
		manifestSHA: hex.EncodeToString(sum[:]),
		freeSlots:   dial.freeSlots,
		operation:   operation,
		engineLimit: dial.engineLimit,
		term:        make(chan struct{}),
	}, nil
}

func protocolTLS(pool *x509.CertPool, cert tls.Certificate, serverName string) *tls.Config {
	cfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		RootCAs:                pool,
		Certificates:           []tls.Certificate{cert},
		CurvePreferences:       []tls.CurveID{tls.X25519, tls.CurveP256},
		SessionTicketsDisabled: true,
		Renegotiation:          tls.RenegotiateNever,
		VerifyConnection:       protocolSuiteOK,
	}
	if serverName != "" {
		cfg.ServerName = serverName
	}
	return cfg
}

func protocolHTTP(cfg *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: cfg,
			Proxy:           nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errOriginRejected
		},
	}
}

func (c *protocolClient) close() {
	c.coreHTTP.CloseIdleConnections()
	c.fileHTTP.CloseIdleConnections()
}

func runProtocol(ctx context.Context, dial protocolDial) error {
	client, err := newProtocolClient(dial)
	if err != nil {
		return err
	}
	defer client.close()
	err = client.loop(ctx)
	if err == nil || (ctx.Err() != nil && !errors.Is(err, errSessionClosed)) {
		return nil
	}
	return scrubURL(err)
}

func (c *protocolClient) loop(ctx context.Context) error {
	hello, err := c.hello(ctx)
	if err != nil {
		return err
	}
	c.beatEvery = heartbeatEvery(hello.HeartbeatIntervalSeconds)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.heartbeatLoop(sessionCtx)
	}()
	defer func() {
		cancelSession()
		wg.Wait()
	}()
	if hello.Control.Drain {
		return c.deleteSession(ctx)
	}
	if hello.Control.Terminate {
		return nil
	}
	for {
		if err := c.sessionFailure(); err != nil {
			return err
		}
		if ctx.Err() != nil || c.sessionTerminating() {
			return nil
		}
		if c.sessionDraining() {
			return c.deleteSession(ctx)
		}
		pullCtx, cancelPull := context.WithCancel(ctx)
		go func() {
			select {
			case <-c.term:
				cancelPull()
			case <-pullCtx.Done():
			}
		}()
		pull, err := c.pull(pullCtx)
		cancelPull()
		if err := c.sessionFailure(); err != nil {
			return err
		}
		if c.sessionTerminating() {
			return nil
		}
		if ctx.Err() != nil && !errors.Is(err, errSessionClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(pull.Assignments) == 0 {
			if pull.Control.Terminate {
				return nil
			}
			if pull.Control.Drain || c.sessionDraining() {
				return c.deleteSession(ctx)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-c.term:
				if err := c.sessionFailure(); err != nil {
					return err
				}
				return nil
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		for _, asgn := range pull.Assignments {
			if err := c.runAssignment(ctx, asgn, pull.Control); err != nil {
				return err
			}
			if err := c.sessionFailure(); err != nil {
				return err
			}
			if pull.Control.Terminate || c.sessionTerminating() {
				return nil
			}
			if pull.Control.Drain || c.sessionDraining() {
				return c.deleteSession(ctx)
			}
		}
	}
}

func heartbeatEvery(seconds int) time.Duration {
	if seconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func (c *protocolClient) heartbeatLoop(ctx context.Context) {
	body := []byte(`{"protocol_version":2}`)
	timer := time.NewTimer(c.beatEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
			if errors.Is(c.postHeartbeat(ctx, body), errSessionClosed) {
				return
			}
			timer.Reset(c.beatEvery)
		}
	}
}

func (c *protocolClient) postHeartbeat(ctx context.Context, body []byte) error {
	raw, err := c.postCore(
		ctx,
		"/worker/v1/sessions/"+url.PathEscape(c.sessionID)+"/heartbeat",
		body,
		c.sessionToken,
		"",
		false,
		terminalReportTimeout,
	)
	if errors.Is(err, errSessionClosed) {
		c.noteSessionClosed()
		return errSessionClosed
	}
	if err != nil {
		return nil
	}
	var resp struct {
		Control controlJSON `json:"control"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	c.applyHeartbeat(resp.Control)
	return nil
}

func (c *protocolClient) noteSessionClosed() {
	c.mu.Lock()
	if c.sessionErr == nil {
		c.sessionErr = errSessionClosed
	}
	c.closeTermLocked()
	run := c.active
	c.mu.Unlock()
	if run == nil {
		return
	}
	run.flag.claim("session_closed", errSessionClosed)
	run.cancel(errSessionClosed)
}

func (c *protocolClient) sessionFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionErr
}

func (c *protocolClient) applyHeartbeat(ctrl controlJSON) {
	c.mu.Lock()
	if ctrl.Drain {
		c.draining = true
	}
	run := c.active
	if ctrl.Terminate {
		c.closeTermLocked()
	}
	c.mu.Unlock()
	if ctrl.Terminate {
		if run != nil {
			c.stopAttempt(run.parent, run.cancel, run.flag, run.asgn, "terminate", errSessionTerminated)
		}
		return
	}
	if run == nil {
		return
	}
	if containsID(ctrl.CancelAttemptIDs, run.asgn.AttemptID) {
		c.stopAttempt(run.parent, run.cancel, run.flag, run.asgn, "cancel", errors.New("cancel"))
	}
}

func (c *protocolClient) markTerminated() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeTermLocked()
}

func (c *protocolClient) closeTermLocked() {
	select {
	case <-c.term:
	default:
		close(c.term)
	}
}

func (c *protocolClient) sessionTerminating() bool {
	select {
	case <-c.term:
		return true
	default:
		return false
	}
}

func (c *protocolClient) sessionDraining() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.draining
}

func (c *protocolClient) setActive(run *liveAttempt) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionErr != nil {
		return c.sessionErr
	}
	select {
	case <-c.term:
		return errSessionTerminated
	default:
	}
	c.active = run
	return nil
}

func (c *protocolClient) clearActive() {
	c.mu.Lock()
	c.active = nil
	c.mu.Unlock()
}

func (c *protocolClient) deleteSession(ctx context.Context) error {
	target, err := url.JoinPath(c.core.String(), "worker/v1/sessions", c.sessionID)
	if err != nil {
		return err
	}
	tryCtx, cancel := context.WithTimeout(ctx, terminalReportTimeout)
	defer cancel()
	_, err = c.do(
		tryCtx,
		c.coreHTTP,
		http.MethodDelete,
		target,
		[]byte(`{"protocol_version":2}`),
		c.sessionToken,
		"",
		"",
		maxProtocolJSONBytes,
	)
	return err
}

type liveAttempt struct {
	asgn   assignmentJSON
	parent context.Context
	cancel context.CancelCauseFunc
	flag   *stopFlag
}

type controlJSON struct {
	CancelAttemptIDs []string `json:"cancel_attempt_ids"`
	Drain            bool     `json:"drain"`
	Terminate        bool     `json:"terminate"`
}

type helloJSON struct {
	SessionID                string      `json:"session_id"`
	SessionToken             string      `json:"session_token"`
	LeaseSeconds             int         `json:"lease_seconds"`
	PullWaitSeconds          int         `json:"pull_wait_seconds"`
	HeartbeatIntervalSeconds int         `json:"heartbeat_interval_seconds"`
	Control                  controlJSON `json:"control"`
}

type helloRequestJSON struct {
	ProtocolVersion int    `json:"protocol_version"`
	WorkerID        string `json:"worker_id"`
	ManifestSHA256  string `json:"manifest_sha256"`
	FreeSlots       int    `json:"free_slots"`
}

func (c *protocolClient) hello(ctx context.Context) (helloJSON, error) {
	body, err := json.Marshal(helloRequestJSON{
		ProtocolVersion: protocolVersion,
		WorkerID:        c.workerID.String(),
		ManifestSHA256:  c.manifestSHA,
		FreeSlots:       c.freeSlots,
	})
	if err != nil {
		return helloJSON{}, err
	}
	raw, err := c.postCore(ctx, "/worker/v1/sessions", body, "", "", false, helloTimeout)
	if err != nil {
		return helloJSON{}, err
	}
	var hello helloJSON
	if err := json.Unmarshal(raw, &hello); err != nil || hello.SessionID == "" || hello.SessionToken == "" {
		return helloJSON{}, errors.New("hello")
	}
	c.sessionID = hello.SessionID
	c.sessionToken = hello.SessionToken
	c.leaseSeconds = hello.LeaseSeconds
	c.pullWait = hello.PullWaitSeconds
	return hello, nil
}

type pullRequestJSON struct {
	ProtocolVersion int    `json:"protocol_version"`
	PullRequestID   string `json:"pull_request_id"`
	FreeSlots       int    `json:"free_slots"`
	WaitSeconds     int    `json:"wait_seconds"`
}

type fileLinkJSON struct {
	Role      string `json:"role"`
	Ordinal   int    `json:"ordinal"`
	Format    string `json:"format"`
	Href      string `json:"href"`
	ExpiresAt string `json:"expires_at"`
}

type assignmentJSON struct {
	JobID             string          `json:"job_id"`
	StepID            string          `json:"step_id"`
	AttemptID         string          `json:"attempt_id"`
	AttemptToken      string          `json:"attempt_token"`
	CapabilityID      string          `json:"capability_id"`
	CapabilityVersion int             `json:"capability_version"`
	AttemptDeadline   string          `json:"attempt_deadline"`
	LeaseExpiresAt    string          `json:"lease_expires_at"`
	Limits            limitsJSON      `json:"limits"`
	Inputs            []fileLinkJSON  `json:"inputs"`
	Outputs           []fileLinkJSON  `json:"outputs"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
}

type limitsJSON struct {
	MaxOutputBytes int64 `json:"max_output_bytes"`
}

type pullResponseJSON struct {
	Assignments []assignmentJSON `json:"assignments"`
	Control     controlJSON      `json:"control"`
}

func (c *protocolClient) pull(ctx context.Context) (pullResponseJSON, error) {
	wait := c.pullWait
	if wait < 0 {
		wait = 0
	}
	if wait > 20 {
		wait = 20
	}
	body, err := json.Marshal(pullRequestJSON{
		ProtocolVersion: protocolVersion,
		PullRequestID:   uuid.NewString(),
		FreeSlots:       c.freeSlots,
		WaitSeconds:     wait,
	})
	if err != nil {
		return pullResponseJSON{}, err
	}
	budget := time.Duration(wait)*time.Second + pullGrace
	raw, err := c.postCore(
		ctx,
		"/worker/v1/sessions/"+url.PathEscape(c.sessionID)+"/pull",
		body,
		c.sessionToken,
		"",
		true,
		budget,
	)
	if err != nil {
		var status *statusError
		if errors.As(err, &status) && status.code == "rate_limited" {
			select {
			case <-ctx.Done():
				return pullResponseJSON{}, ctx.Err()
			case <-time.After(time.Second):
			}
			return c.pull(ctx)
		}
		return pullResponseJSON{}, err
	}
	var pull pullResponseJSON
	if err := json.Unmarshal(raw, &pull); err != nil {
		return pullResponseJSON{}, errors.New("pull")
	}
	if pull.Assignments == nil {
		pull.Assignments = []assignmentJSON{}
	}
	return pull, nil
}

func (c *protocolClient) runAssignment(ctx context.Context, asgn assignmentJSON, ctrl controlJSON) error {
	if ctrl.Terminate {
		return c.postStopped(ctx, asgn, "terminate")
	}
	if containsID(ctrl.CancelAttemptIDs, asgn.AttemptID) {
		return c.postStopped(ctx, asgn, "cancel")
	}
	if asgn.AttemptID == "" || asgn.AttemptToken == "" {
		return errors.New("assignment")
	}
	primary, ok := assignmentOutputs(asgn.Outputs)
	if !ok {
		return c.fail(ctx, asgn, "engine_error", "assignment")
	}
	source, extras, err := classifyInputs(asgn.JobID, asgn.Inputs)
	if err != nil {
		if errors.Is(err, errOriginRejected) {
			return errOriginRejected
		}
		return c.fail(ctx, asgn, "engine_error", "assignment")
	}
	out := primary
	if _, err := requireHTTPS(out.Href); err != nil {
		return err
	}
	deadline, err := time.Parse(time.RFC3339Nano, asgn.AttemptDeadline)
	if err != nil {
		return c.fail(ctx, asgn, "engine_error", "assignment")
	}
	leaseUntil, err := time.Parse(time.RFC3339Nano, asgn.LeaseExpiresAt)
	if err != nil {
		return c.fail(ctx, asgn, "engine_error", "assignment")
	}
	opts, err := assignmentParameters(asgn.Parameters)
	if err != nil {
		if c.operation != convert.OperationRender && errors.Is(err, errTexOptions) {
			return c.fail(ctx, asgn, "invalid_options", "stored conversion options are invalid")
		}
		return c.fail(ctx, asgn, "engine_error", "assignment")
	}
	if c.operation == convert.OperationRender && opts != nil {
		return c.fail(ctx, asgn, "engine_error", "assignment")
	}

	bounded, cancelDeadline := context.WithDeadline(ctx, deadline)
	attemptCtx, cancelAttempt := context.WithCancelCause(bounded)
	var flag stopFlag
	if err := c.setActive(&liveAttempt{asgn: asgn, parent: ctx, cancel: cancelAttempt, flag: &flag}); err != nil {
		cancelAttempt(err)
		cancelDeadline()
		if errors.Is(err, errSessionClosed) {
			return err
		}
		return c.postStopped(ctx, asgn, "terminate")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.renewLoop(ctx, attemptCtx, cancelAttempt, &flag, asgn, leaseUntil, deadline)
	}()
	defer func() {
		c.clearActive()
		cancelAttempt(context.Canceled)
		cancelDeadline()
		wg.Wait()
	}()

	if done, err := c.alreadyStopped(ctx, &flag); done {
		return err
	}
	src, err := c.getFile(attemptCtx, source.Href)
	if done, stopErr := c.alreadyStopped(ctx, &flag); done {
		return stopErr
	}
	if err != nil {
		if errors.Is(err, errOriginRejected) {
			return errOriginRejected
		}
		return c.fail(ctx, asgn, transportCode(err), safeMessage(err))
	}
	if stopped, err := c.reportProgress(ctx, attemptCtx, &flag, asgn, 0); stopped || err != nil {
		return err
	}

	files := make([]extraFile, 0, len(extras))
	for _, extra := range extras {
		body, err := c.getFile(attemptCtx, extra.link.Href)
		if done, stopErr := c.alreadyStopped(ctx, &flag); done {
			return stopErr
		}
		if err != nil {
			if errors.Is(err, errOriginRejected) {
				return errOriginRejected
			}
			return c.fail(ctx, asgn, transportCode(err), safeMessage(err))
		}
		files = append(files, extraFile{name: extra.name, body: body})
	}
	produced, err := c.convert(attemptCtx, source.Format, out.Format, src, files, deadline, asgn.Limits.MaxOutputBytes, opts)
	if done, stopErr := c.alreadyStopped(ctx, &flag); done {
		return stopErr
	}
	if err != nil {
		if errors.Is(err, errOriginRejected) {
			return errOriginRejected
		}
		return failConverted(c, ctx, asgn, err)
	}
	if stopped, err := c.reportProgress(ctx, attemptCtx, &flag, asgn, 99); stopped || err != nil {
		return err
	}
	if err := c.putFile(attemptCtx, out.Href, produced, convert.MIMEType(out.Format)); err != nil {
		if done, stopErr := c.alreadyStopped(ctx, &flag); done {
			return stopErr
		}
		if errors.Is(err, errOriginRejected) {
			return errOriginRejected
		}
		return c.fail(ctx, asgn, transportCode(err), safeMessage(err))
	}
	artifacts := []artifactJSON{artifactFor("primary", out.Ordinal, out.Format, produced)}
	if !flag.claim("complete", nil) {
		if errors.Is(flag.cause(), errSessionClosed) {
			return errSessionClosed
		}
		return nil
	}
	completeBody, err := json.Marshal(completeJSON{
		ProtocolVersion: protocolVersion,
		Artifacts:       artifacts,
	})
	if err != nil {
		return err
	}
	_, err = c.postCore(
		ctx,
		"/worker/v1/attempts/"+url.PathEscape(asgn.AttemptID)+"/complete",
		completeBody,
		c.sessionToken,
		asgn.AttemptToken,
		true,
		terminalReportTimeout,
	)
	if errors.Is(err, errAttemptNotActive) {
		return c.lostLease(ctx, &flag, asgn)
	}
	return err
}

func artifactFor(role string, ordinal int, format string, body []byte) artifactJSON {
	sum := sha256.Sum256(body)
	return artifactJSON{
		Role:        role,
		Ordinal:     ordinal,
		Format:      format,
		ContentType: convert.MIMEType(format),
		SHA256:      hex.EncodeToString(sum[:]),
		ByteSize:    int64(len(body)),
	}
}

func assignmentParameters(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return nil, nil
	}
	var opts map[string]any
	if err := json.Unmarshal(trimmed, &opts); err != nil {
		return nil, err
	}
	if _, err := convert.TeXOptsFromMap(opts); err != nil {
		return nil, errTexOptions
	}
	return nil, nil
}

var errTexOptions = errors.New("tex options")

func failConverted(c *protocolClient, ctx context.Context, asgn assignmentJSON, err error) error {
	code, message := tectonicEngineCode(err)
	return c.fail(ctx, asgn, code, message)
}

func assignmentOutputs(links []fileLinkJSON) (fileLinkJSON, bool) {
	if len(links) != 1 {
		return fileLinkJSON{}, false
	}
	link := links[0]
	if link.Role != "primary" || link.Ordinal != 0 || link.Href == "" || link.Format == "" {
		return fileLinkJSON{}, false
	}
	return link, true
}

func (c *protocolClient) convert(ctx context.Context, from, to string, src []byte, extras []extraFile, deadline time.Time, maxOut int64, opts map[string]any) ([]byte, error) {
	lookup := c.lookupOp
	if lookup == nil {
		lookup = convert.Default.LookupOp
	}
	conv, ok := lookup(c.operation, from, to)
	if !ok {
		return nil, fmt.Errorf("no converter for %s %s to %s", c.operation, from, to)
	}
	dir, err := os.MkdirTemp("", "octoconv-tectonic-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	inPath := filepath.Join(dir, "in."+from)
	outPath := filepath.Join(dir, "out."+to)
	if err := os.WriteFile(inPath, src, 0o600); err != nil {
		return nil, err
	}
	for _, extra := range extras {
		path, err := extraPath(dir, extra.name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, extra.body, 0o600); err != nil {
			return nil, err
		}
	}
	budget := c.engineLimit
	if until := time.Until(deadline); until < budget {
		budget = until
	}
	if budget <= 0 {
		return nil, context.DeadlineExceeded
	}
	convertCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	callOpts := opts
	if c.operation == convert.OperationRender {
		callOpts = nil
	}
	if err := conv.Convert(convertCtx, inPath, outPath, callOpts); err != nil {
		return nil, err
	}
	return readLimitedJobFile(outPath, maxOut)
}

func readLimitedJobFile(path string, maxOut int64) ([]byte, error) {
	f, _, err := convert.OpenJobFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limit := int64(maxProtocolFileBytes)
	if maxOut > 0 && maxOut < limit {
		limit = maxOut
	}
	payload, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, convert.ErrOutputTooLarge
	}
	return payload, nil
}

func (c *protocolClient) renewLoop(parent, attempt context.Context, cancelAttempt context.CancelCauseFunc, flag *stopFlag, asgn assignmentJSON, leaseUntil, deadline time.Time) {
	wait := renewEvery(c.leaseSeconds)
	if leaseCovers(leaseUntil, deadline) {
		wait = untilFire(leaseUntil)
	} else {
		wait = cappedWait(leaseUntil, wait)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-parent.Done():
			return
		case <-attempt.Done():
			return
		case <-timer.C:
			if parent.Err() != nil {
				return
			}
			if !leaseUntil.IsZero() && !time.Now().Before(leaseUntil) {
				c.stopAttempt(parent, cancelAttempt, flag, asgn, "lease_lost", errAttemptNotActive)
				return
			}
			if leaseCovers(leaseUntil, deadline) {
				timer.Reset(untilFire(leaseUntil))
				continue
			}
			next, ctrl, err := c.renewUntil(attempt, asgn, leaseUntil)
			if parent.Err() != nil {
				return
			}
			if !leaseUntil.IsZero() && !time.Now().Before(leaseUntil) {
				c.stopAttempt(parent, cancelAttempt, flag, asgn, "lease_lost", errAttemptNotActive)
				return
			}
			if attempt.Err() != nil {
				return
			}
			if errors.Is(err, errSessionClosed) {
				flag.claim("session_closed", errSessionClosed)
				cancelAttempt(errSessionClosed)
				return
			}
			if errors.Is(err, errAttemptNotActive) {
				c.stopAttempt(parent, cancelAttempt, flag, asgn, "lease_lost", errAttemptNotActive)
				return
			}
			if err != nil {
				failures++
				timer.Reset(renewRetryWait(leaseUntil, failures))
				continue
			}
			failures = 0
			if ctrl.Drain {
				c.mu.Lock()
				c.draining = true
				c.mu.Unlock()
			}
			if ctrl.Terminate {
				c.markTerminated()
				c.stopAttempt(parent, cancelAttempt, flag, asgn, "terminate", errSessionTerminated)
				return
			}
			if containsID(ctrl.CancelAttemptIDs, asgn.AttemptID) {
				c.stopAttempt(parent, cancelAttempt, flag, asgn, "cancel", errors.New("cancel"))
				return
			}
			if !next.IsZero() {
				leaseUntil = next
			}
			if leaseCovers(leaseUntil, deadline) {
				timer.Reset(untilFire(leaseUntil))
				continue
			}
			timer.Reset(cappedWait(leaseUntil, renewEvery(c.leaseSeconds)))
		}
	}
}

func renewRetryWait(leaseUntil time.Time, failures int) time.Duration {
	backoff := time.Second
	for n := 1; n < failures && backoff < 8*time.Second; n++ {
		backoff *= 2
	}
	if leaseUntil.IsZero() {
		return backoff
	}
	remaining := time.Until(leaseUntil)
	if remaining <= 0 {
		return 0
	}
	latest := remaining - leaseRenewSlack
	if latest < time.Millisecond {
		latest = remaining / 2
	}
	if latest < time.Millisecond {
		return 0
	}
	if backoff < latest {
		return backoff
	}
	return latest
}

// The renew timer has to fire while the lease is still open. The loop treats
// the expiry instant as already lost and does not POST /lease.
const leaseRenewSlack = 20 * time.Millisecond

func cappedWait(leaseUntil time.Time, interval time.Duration) time.Duration {
	if leaseUntil.IsZero() {
		return interval
	}
	remaining := time.Until(leaseUntil)
	if remaining <= 0 {
		return 0
	}
	latest := remaining - leaseRenewSlack
	if latest < time.Millisecond {
		latest = remaining / 2
	}
	if latest < time.Millisecond {
		return 0
	}
	if interval < latest {
		return interval
	}
	return latest
}

func leaseCovers(leaseUntil, deadline time.Time) bool {
	return !deadline.IsZero() && !leaseUntil.IsZero() && !leaseUntil.Before(deadline)
}

func untilFire(at time.Time) time.Duration {
	d := time.Until(at)
	if d < 0 {
		return 0
	}
	return d
}

func renewEvery(seconds int) time.Duration {
	if seconds <= 0 {
		return time.Millisecond
	}
	half := time.Duration(seconds) * time.Second / 2
	if half <= 0 {
		return time.Millisecond
	}
	return half
}

func (c *protocolClient) stopAttempt(parent context.Context, cancelAttempt context.CancelCauseFunc, flag *stopFlag, asgn assignmentJSON, reason string, cause error) {
	if parent.Err() != nil {
		return
	}
	claimed := flag.claim(reason, cause)
	cancelAttempt(cause)
	if !claimed {
		return
	}
	_ = c.postStopped(parent, asgn, reason)
}

func (c *protocolClient) lostLease(ctx context.Context, flag *stopFlag, asgn assignmentJSON) error {
	if flag.outcome() == "lease_lost" || flag.outcome() == "terminate" || flag.outcome() == "cancel" {
		return nil
	}
	if flag.claim("lease_lost", errAttemptNotActive) {
		return c.postStopped(ctx, asgn, "lease_lost")
	}
	if errors.Is(flag.cause(), errSessionClosed) {
		return errSessionClosed
	}
	return nil
}

func (c *protocolClient) alreadyStopped(ctx context.Context, flag *stopFlag) (bool, error) {
	if errors.Is(flag.cause(), errSessionClosed) {
		return true, errSessionClosed
	}
	if flag.outcome() != "" && flag.outcome() != "complete" {
		return true, nil
	}
	if ctx.Err() != nil {
		return true, nil
	}
	return false, nil
}

type leaseResponseJSON struct {
	LeaseExpiresAt string      `json:"lease_expires_at"`
	Control        controlJSON `json:"control"`
}

func (c *protocolClient) renewUntil(attempt context.Context, asgn assignmentJSON, leaseUntil time.Time) (time.Time, controlJSON, error) {
	ctx := attempt
	if !leaseUntil.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(attempt, leaseUntil)
		defer cancel()
	}
	return c.renew(ctx, asgn)
}

func (c *protocolClient) renew(ctx context.Context, asgn assignmentJSON) (time.Time, controlJSON, error) {
	body := []byte(`{"protocol_version":2}`)
	raw, err := c.postCore(
		ctx,
		"/worker/v1/attempts/"+url.PathEscape(asgn.AttemptID)+"/lease",
		body,
		c.sessionToken,
		asgn.AttemptToken,
		false,
		0,
	)
	if err != nil {
		return time.Time{}, controlJSON{}, err
	}
	var lease leaseResponseJSON
	if err := json.Unmarshal(raw, &lease); err != nil {
		return time.Time{}, controlJSON{}, errors.New("lease")
	}
	when, err := time.Parse(time.RFC3339Nano, lease.LeaseExpiresAt)
	if err != nil {
		return time.Time{}, lease.Control, nil
	}
	return when, lease.Control, nil
}

type progressJSON struct {
	ProtocolVersion int `json:"protocol_version"`
	Percent         int `json:"percent"`
}

func (c *protocolClient) reportProgress(ctx, attemptCtx context.Context, flag *stopFlag, asgn assignmentJSON, percent int) (stopped bool, err error) {
	err = c.postProgress(attemptCtx, asgn, percent)
	if err == nil {
		return false, nil
	}
	if done, stopErr := c.alreadyStopped(ctx, flag); done {
		return true, stopErr
	}
	if errors.Is(err, errAttemptNotActive) {
		return true, c.lostLease(ctx, flag, asgn)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true, failConverted(c, ctx, asgn, err)
	}
	return false, err
}

func (c *protocolClient) postProgress(ctx context.Context, asgn assignmentJSON, percent int) error {
	if percent < 0 || percent > 99 {
		return errors.New("progress")
	}
	body, err := json.Marshal(progressJSON{ProtocolVersion: protocolVersion, Percent: percent})
	if err != nil {
		return err
	}
	_, err = c.postCore(
		ctx,
		"/worker/v1/attempts/"+url.PathEscape(asgn.AttemptID)+"/progress",
		body,
		c.sessionToken,
		asgn.AttemptToken,
		false,
		0,
	)
	return err
}

type failJSON struct {
	ProtocolVersion int    `json:"protocol_version"`
	EngineCode      string `json:"engine_code"`
	Message         string `json:"message"`
}

func (c *protocolClient) fail(ctx context.Context, asgn assignmentJSON, code, message string) error {
	if ctx.Err() != nil {
		return nil
	}
	switch code {
	case "timeout", "engine_error", "invalid_options", "output_too_large", "unsupported_package":
	default:
		code = "engine_error"
	}
	if message == "" {
		message = "conversion failed"
	}
	body, err := json.Marshal(failJSON{ProtocolVersion: protocolVersion, EngineCode: code, Message: message})
	if err != nil {
		return err
	}
	_, err = c.postCore(
		ctx,
		"/worker/v1/attempts/"+url.PathEscape(asgn.AttemptID)+"/fail",
		body,
		c.sessionToken,
		asgn.AttemptToken,
		false,
		terminalReportTimeout,
	)
	if errors.Is(err, errAttemptNotActive) || errors.Is(err, errSessionClosed) {
		return err
	}
	return err
}

type stoppedJSON struct {
	ProtocolVersion int    `json:"protocol_version"`
	Reason          string `json:"reason"`
}

func (c *protocolClient) postStopped(ctx context.Context, asgn assignmentJSON, reason string) error {
	if ctx.Err() != nil {
		return nil
	}
	notify, cancel := context.WithTimeout(ctx, stoppedNotifyTimeout)
	defer cancel()
	body, err := json.Marshal(stoppedJSON{ProtocolVersion: protocolVersion, Reason: reason})
	if err != nil {
		return err
	}
	_, err = c.postCore(
		notify,
		"/worker/v1/attempts/"+url.PathEscape(asgn.AttemptID)+"/stopped",
		body,
		c.sessionToken,
		asgn.AttemptToken,
		false,
		0,
	)
	if errors.Is(err, errAttemptNotActive) {
		return nil
	}
	return err
}

type completeJSON struct {
	ProtocolVersion int            `json:"protocol_version"`
	Artifacts       []artifactJSON `json:"artifacts"`
}

type artifactJSON struct {
	Role        string `json:"role"`
	Ordinal     int    `json:"ordinal"`
	Format      string `json:"format"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	ByteSize    int64  `json:"byte_size"`
}

func (c *protocolClient) getFile(ctx context.Context, href string) ([]byte, error) {
	if _, err := requireHTTPS(href); err != nil {
		return nil, err
	}
	return c.do(ctx, c.fileHTTP, http.MethodGet, href, nil, "", "", "", convert.MaxInputBytes)
}

func (c *protocolClient) putFile(ctx context.Context, href string, body []byte, contentType string) error {
	if _, err := requireHTTPS(href); err != nil {
		return err
	}
	_, err := c.do(ctx, c.fileHTTP, http.MethodPut, href, body, "", "", contentType, maxProtocolJSONBytes)
	return err
}

func (c *protocolClient) postCore(ctx context.Context, path string, body []byte, bearer, attempt string, retry bool, perTry time.Duration) ([]byte, error) {
	target, err := url.JoinPath(c.core.String(), strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, err
	}
	attempts := 1
	if retry {
		attempts = 2
	}
	var last error
	for try := 0; try < attempts; try++ {
		raw, err := func() ([]byte, error) {
			tryCtx := ctx
			if perTry > 0 {
				var cancel context.CancelFunc
				tryCtx, cancel = context.WithTimeout(ctx, perTry)
				defer cancel()
			}
			return c.do(tryCtx, c.coreHTTP, http.MethodPost, target, body, bearer, attempt, "", maxProtocolJSONBytes)
		}()
		if err == nil {
			return raw, nil
		}
		last = err
		if ctx.Err() != nil || errors.Is(err, errSessionClosed) || errors.Is(err, errAttemptNotActive) || errors.Is(err, errOriginRejected) {
			return nil, err
		}
		var status *statusError
		if errors.As(err, &status) {
			return nil, err
		}
	}
	return nil, last
}

type statusError struct {
	status int
	code   string
}

func (e *statusError) Error() string {
	if e.code != "" {
		return e.code
	}
	return fmt.Sprintf("status %d", e.status)
}

func (c *protocolClient) do(ctx context.Context, client *http.Client, method, rawURL string, body []byte, bearer, attempt, contentType string, limit int64) ([]byte, error) {
	if _, err := requireHTTPS(rawURL); err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if attempt != "" {
		req.Header.Set("Octoconv-Attempt", attempt)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("response too large")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return payload, nil
	}
	code := wireCode(payload)
	if resp.StatusCode == http.StatusUnauthorized && code == "session_closed" {
		return nil, errSessionClosed
	}
	if resp.StatusCode == http.StatusConflict && code == "attempt_not_active" {
		return nil, errAttemptNotActive
	}
	return nil, &statusError{status: resp.StatusCode, code: code}
}

func requireHTTPS(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errOriginRejected
	}
	return parsed, nil
}

func wireCode(payload []byte) string {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	return body.Code
}

func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func scrubURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return scrubURL(urlErr.Err)
	}
	return err
}

func safeMessage(err error) string {
	err = scrubURL(err)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg == "" {
		return "conversion failed"
	}
	if len(msg) > 400 {
		return msg[:400]
	}
	return msg
}

type stopFlag struct {
	mu     sync.Mutex
	reason string
	err    error
}

func (s *stopFlag) claim(reason string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" || s.err != nil {
		return false
	}
	s.reason = reason
	s.err = err
	return true
}

func (s *stopFlag) outcome() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *stopFlag) cause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
