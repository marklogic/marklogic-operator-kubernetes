// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CandidateDir = "/etc/marklogic-haproxy-candidate"
	APITokenDir  = "/var/run/marklogic-haproxy-api"
	StatusMap    = "marklogic-haproxy-reload-status"
	CandidateMap = "marklogic-haproxy-candidate"
)

type PodStatus struct {
	Transaction string `json:"transaction"`
	Phase       string `json:"phase"`
	Hash        string `json:"hash"`
	WorkerPID   int    `json:"workerPID,omitempty"`
	// OldWorkers counts workers of earlier configurations that are still draining
	// connections; hard-stop-after bounds how long they may linger.
	OldWorkers int    `json:"oldWorkers,omitempty"`
	Message    string `json:"message,omitempty"`
}

// WatchConfig says where the Kubernetes API, the pod's identity and the files the loop
// reads and writes are. Production uses the in-cluster values; tests inject their own.
type WatchConfig struct {
	APIHost, APIPort string
	Namespace, Pod   string
	APITokenDir      string
	CandidateDir     string
	HealthDir        string
	Tick             time.Duration
}

// Watch runs the sidecar loop with the configuration the pod provides.
func Watch(ctx context.Context, r Reloader) error {
	return WatchWith(ctx, r, WatchConfig{
		APIHost: os.Getenv("KUBERNETES_SERVICE_HOST"), APIPort: os.Getenv("KUBERNETES_SERVICE_PORT"),
		Namespace: os.Getenv("POD_NAMESPACE"), Pod: os.Getenv("POD_NAME"),
		APITokenDir: APITokenDir, CandidateDir: CandidateDir, HealthDir: HealthDir, Tick: time.Second,
	})
}

// errorLog prints each distinct error once per category, so a condition that persists for
// minutes produces one line instead of one per second.
type errorLog struct{ last map[string]string }

func (l *errorLog) report(category string, err error, what string) {
	if l.last == nil {
		l.last = map[string]string{}
	}
	if err.Error() != l.last[category] {
		fmt.Fprintf(os.Stderr, "haproxy-reloader: %s: %v\n", what, err)
		l.last[category] = err.Error()
	}
}

func (l *errorLog) clear(category string) { delete(l.last, category) }

// transactionState remembers what has already been processed and reported, so that a phase
// that stays in place for minutes is acted on once, not on every iteration.
type transactionState struct {
	lastReported       string
	retryPhase         string // "<transaction>:<phase>" that has been processed
	retryTransaction   string
	pending            *PodStatus // a report that could not be delivered yet
	pendingFingerprint string
}

// forget makes the loop treat the current phase as new, after a container restart.
func (t *transactionState) forget() {
	t.retryPhase, t.lastReported = "", ""
	t.pending, t.pendingFingerprint = nil, ""
}

// settle decides whether the phase just handled needs no further action.
func (t *transactionState) settle(phaseKey, transaction string, report *PodStatus, done, reportFailed bool) {
	switch {
	case done:
		t.lastReported, t.retryPhase, t.retryTransaction = "", "", ""
	case report != nil && report.Phase != "rollback-failed" && !reportFailed:
		t.retryPhase, t.retryTransaction = phaseKey, transaction
	case reportFailed && t.pending != nil:
		// Keep the phase marked as handled; only the undelivered report is retried.
		t.retryPhase, t.retryTransaction = phaseKey, transaction
	case reportFailed:
		t.retryPhase = ""
	case transaction != t.retryTransaction:
		t.retryPhase = ""
	}
}

// idleState throttles re-applying a committed configuration that keeps failing.
type idleState struct {
	err      string
	failedID string
	retryAt  time.Time
	backoff  time.Duration
}

type watcher struct {
	r         Reloader
	cfg       WatchConfig
	client    *http.Client
	statusURL string
	tokenPath string
	source    *candidateSource
	steady    *steadyReporter
	boot      *BootWatcher
	log       errorLog
	tx        transactionState
	idle      idleState

	workerPID, oldWorkers int
}

func newWatcher(ctx context.Context, r Reloader, cfg WatchConfig) (*watcher, error) {
	if cfg.APIHost == "" || cfg.APIPort == "" || cfg.Namespace == "" || cfg.Pod == "" {
		return nil, errors.New("Kubernetes service address and pod identity are required")
	}
	ca, err := os.ReadFile(filepath.Join(cfg.APITokenDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("reading Kubernetes API CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("Kubernetes API CA bundle contains no certificates")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	configMapURL := func(name string) string {
		return "https://" + netJoinHostPort(cfg.APIHost, cfg.APIPort) + "/api/v1/namespaces/" +
			url.PathEscape(cfg.Namespace) + "/configmaps/" + name
	}
	w := &watcher{
		r: r, cfg: cfg, client: client,
		statusURL: configMapURL(StatusMap),
		tokenPath: filepath.Join(cfg.APITokenDir, "token"),
		steady:    &steadyReporter{},
		boot:      &BootWatcher{},
	}
	w.source = &candidateSource{
		client: client, url: configMapURL(CandidateMap), tokenPath: w.tokenPath, dir: cfg.CandidateDir, now: time.Now,
	}
	w.workerPID, w.oldWorkers = currentWorker(ctx, r)
	return w, nil
}

// WatchWith runs the sidecar loop until the context ends.
func WatchWith(ctx context.Context, r Reloader, cfg WatchConfig) error {
	w, err := newWatcher(ctx, r, cfg)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(cfg.Tick)
	defer ticker.Stop()
	for {
		if err := w.iterate(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// iterate is one pass of the loop. It returns an error only when the context has ended.
func (w *watcher) iterate(ctx context.Context) error {
	w.heartbeat()
	if err := w.recoverInterrupted(ctx); err != nil {
		return err
	}
	w.observeHAProxy(ctx)
	candidate, err := w.source.get(ctx)
	if err != nil {
		w.log.report("candidate", err, "reading candidate")
		return nil
	}
	w.log.clear("candidate")
	if candidate.phase == "idle" {
		w.convergeIdle(ctx, candidate)
	} else {
		// Transaction acknowledgements overwrite this pod's entry, so the steady-state
		// report must be repeated once the candidate is idle again.
		w.steady.last = ""
	}
	w.processTransaction(ctx, candidate)
	return nil
}

// heartbeat lets the liveness probe restart the sidecar if this loop stops making progress.
func (w *watcher) heartbeat() {
	if err := WriteHeartbeat(w.cfg.HealthDir, time.Now()); err != nil {
		w.log.report("heartbeat", err, "writing heartbeat")
	} else {
		w.log.clear("heartbeat")
	}
}

// recoverInterrupted restores the last confirmed configuration after an interrupted
// activation, so a running but unready HAProxy regains a current worker without the
// operator's help.
func (w *watcher) recoverInterrupted(ctx context.Context) error {
	err := w.r.WithLock(ctx, func() error {
		opCtx, cancel := context.WithTimeout(ctx, 2*Timeout)
		defer cancel()
		return w.r.Recover(opCtx)
	})
	if err == nil || errors.Is(err, os.ErrNotExist) {
		w.log.clear("recover")
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w.log.report("recover", err, "recovering interrupted reload")
	return nil
}

// observeHAProxy records the worker and detects a restart of the HAProxy container.
func (w *watcher) observeHAProxy(ctx context.Context) {
	if pid, old := currentWorker(ctx, w.r); pid != 0 {
		w.workerPID, w.oldWorkers = pid, old
	}
	// Only a container restart justifies processing a transaction again. A new worker PID
	// is what every reload produces, including a failed activation, and treating it as a
	// restart re-runs that failing activation in a loop.
	if w.boot.Restarted(w.r) {
		w.tx.forget()
		w.steady.last = ""
	}
}

// convergeIdle re-applies the committed configuration if this pod does not run it (for
// example after a restart) and reports what the pod actually runs.
func (w *watcher) convergeIdle(ctx context.Context, c candidate) {
	id := c.transaction + ":" + c.hash
	if id != w.idle.failedID || !time.Now().Before(w.idle.retryAt) {
		err := w.r.WithLock(ctx, func() error { return reconcileIdle(ctx, w.r, c) })
		if err != nil && ctx.Err() == nil {
			w.log.report("idle", err, "applying committed configuration")
			w.idle.err = err.Error()
			if id != w.idle.failedID {
				w.idle.backoff = 0
			}
			w.idle.backoff = nextIdleBackoff(w.idle.backoff)
			w.idle.failedID, w.idle.retryAt = id, time.Now().Add(w.idle.backoff)
		} else {
			w.log.clear("idle")
			w.idle = idleState{}
		}
	}
	w.steady.report(ctx, w.client, w.statusURL, w.tokenPath, w.cfg.Pod,
		steadyStatus(w.r, c, w.idle.err, w.workerPID, w.oldWorkers))
}

// processTransaction acts on the candidate's phase once and reports the outcome.
func (w *watcher) processTransaction(ctx context.Context, c candidate) {
	phaseKey := c.transaction + ":" + c.phase
	if phaseKey == w.tx.retryPhase {
		w.deliverPendingReport(ctx)
		return
	}
	w.tx.pending, w.tx.pendingFingerprint = nil, ""
	var report *PodStatus
	done := false
	if err := w.r.WithLock(ctx, func() error {
		report, done = processCandidate(ctx, w.r, c)
		return nil
	}); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "haproxy-reloader: acquiring reload lock: %v\n", err)
	}
	reportFailed := w.publish(ctx, report)
	w.tx.settle(phaseKey, c.transaction, report, done, reportFailed)
}

// deliverPendingReport retries a report that the API did not accept.
func (w *watcher) deliverPendingReport(ctx context.Context) {
	if w.tx.pending == nil {
		return
	}
	if err := patchPodStatus(ctx, w.client, w.statusURL, w.tokenPath, w.cfg.Pod, *w.tx.pending); err != nil {
		w.log.report("report", err, "reporting reload status")
		return
	}
	w.tx.lastReported = w.tx.pendingFingerprint
	w.tx.pending, w.tx.pendingFingerprint = nil, ""
	w.log.clear("report")
}

// publish sends the report unless it is identical to the last one, and reports whether
// delivery failed. A failed report is kept for retry, except a rollback failure, which is
// recomputed on the next attempt anyway.
func (w *watcher) publish(ctx context.Context, report *PodStatus) (failed bool) {
	if report == nil {
		return false
	}
	fingerprint, _ := json.Marshal(report)
	if string(fingerprint) == w.tx.lastReported {
		return false
	}
	if err := patchPodStatus(ctx, w.client, w.statusURL, w.tokenPath, w.cfg.Pod, *report); err != nil {
		w.log.report("report", err, "reporting reload status")
		if report.Phase != "rollback-failed" {
			copied := *report
			w.tx.pending, w.tx.pendingFingerprint = &copied, string(fingerprint)
		}
		return true
	}
	w.tx.lastReported = string(fingerprint)
	w.log.clear("report")
	return false
}

func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

func processCandidate(ctx context.Context, r Reloader, c candidate) (*PodStatus, bool) {
	status := &PodStatus{Transaction: c.transaction, Hash: c.hash}
	switch c.phase {
	case "prepare":
		status.Phase = "prepared"
		opCtx, cancel := context.WithTimeout(ctx, Timeout)
		err := r.Validate(opCtx, c.config)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
		}
		return status, false
	case "commit":
		status.Phase = "applied"
		opCtx, cancel := context.WithTimeout(ctx, Timeout)
		err := r.Validate(opCtx, c.config)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
			return status, false
		}
		opCtx, cancel = context.WithTimeout(ctx, 2*Timeout+5*time.Second)
		err = r.Apply(opCtx, c.config)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
			return status, false
		}
		opCtx, cancel = context.WithTimeout(ctx, Timeout)
		pid, err := r.Worker(opCtx)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
			return status, false
		}
		status.WorkerPID = pid
		return status, false
	case "rollback":
		status.Phase = "rolledback"
		opCtx, cancel := context.WithTimeout(ctx, 2*Timeout+5*time.Second)
		err := r.Apply(opCtx, c.previous)
		cancel()
		if err != nil {
			status.Phase, status.Message = "rollback-failed", err.Error()
			return status, false
		}
		status.Hash = Hash(c.previous)
		opCtx, cancel = context.WithTimeout(ctx, Timeout)
		pid, err := r.Worker(opCtx)
		cancel()
		if err != nil {
			status.Phase, status.Message = "rollback-failed", err.Error()
			return status, false
		}
		status.WorkerPID = pid
		return status, false
	case "abort":
		status.Phase = "aborted"
		return status, true
	default:
		return nil, false
	}
}

const (
	idleBackoffStart = 10 * time.Second
	idleBackoffMax   = 5 * time.Minute
)

// nextIdleBackoff spaces out attempts to apply a committed configuration that keeps
// failing. A reload that fails at bind time pauses the listeners for a moment, so
// retrying every few seconds would keep disrupting the pod's traffic.
func nextIdleBackoff(previous time.Duration) time.Duration {
	if previous <= 0 {
		return idleBackoffStart
	}
	return min(previous*2, idleBackoffMax)
}

// steadyStatus describes the configuration this pod actually runs while no
// transaction is in progress, so the operator can verify convergence instead of
// assuming it. It returns nil while HAProxy has not been initialized yet.
func steadyStatus(r Reloader, c candidate, applyError string, workerPID, oldWorkers int) *PodStatus {
	committed, err := os.ReadFile(r.path("committed.cfg"))
	if err != nil {
		return nil
	}
	status := &PodStatus{Transaction: c.transaction, Phase: "active", Hash: Hash(committed), WorkerPID: workerPID, OldWorkers: oldWorkers}
	if status.Hash != c.hash {
		status.Phase, status.Message = "diverged", "applying the committed configuration"
		if applyError != "" {
			status.Message = applyError
		}
	}
	return status
}

// steadyReporter publishes steady-state status only when it changes.
type steadyReporter struct {
	last    string
	lastErr string
}

func (s *steadyReporter) report(ctx context.Context, client *http.Client, statusURL, tokenPath, pod string, status *PodStatus) {
	if status == nil {
		return
	}
	fingerprint, _ := json.Marshal(status)
	if string(fingerprint) == s.last {
		return
	}
	if err := patchPodStatus(ctx, client, statusURL, tokenPath, pod, *status); err != nil {
		if err.Error() != s.lastErr {
			fmt.Fprintf(os.Stderr, "haproxy-reloader: reporting steady-state status: %v\n", err)
			s.lastErr = err.Error()
		}
		return
	}
	s.last, s.lastErr = string(fingerprint), ""
}

// reconcileIdle converges a restarted HAProxy on the committed configuration.
// Its startup configuration can predate the commit while the active ConfigMap
// projection is refreshed, and no further transaction would otherwise apply it.
func reconcileIdle(ctx context.Context, r Reloader, c candidate) error {
	committed, err := os.ReadFile(r.path("committed.cfg"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if Hash(committed) == c.hash {
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, 2*Timeout+5*time.Second)
	defer cancel()
	return r.Apply(opCtx, c.config)
}

func currentWorker(ctx context.Context, r Reloader) (pid, old int) {
	workerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pid, old, err := r.Processes(workerCtx)
	if err != nil {
		return 0, 0
	}
	return pid, old
}

func patchPodStatus(ctx context.Context, client *http.Client, endpoint, tokenPath, pod string, status PodStatus) error {
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("reading projected Kubernetes token: %w", err)
	}
	value, err := json.Marshal(status)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(struct {
		Data map[string]string `json:"data"`
	}{Data: map[string]string{pod: string(value)}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(patch))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Kubernetes API returned %s: %s", response.Status, body)
	}
	return nil
}
