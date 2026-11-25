// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadCandidateFromAtomicProjectedVolume(t *testing.T) {
	dir := t.TempDir()
	revision := filepath.Join(dir, "..2026_11_18_00_00_00")
	if err := os.Mkdir(revision, 0700); err != nil {
		t.Fatal(err)
	}
	config := []byte("global\n")
	files := map[string]string{
		"phase": "prepare", "transaction": "1-abcd", "hash": Hash(config),
		"haproxy.cfg": string(config), "previous.cfg": "global\n",
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(revision, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Base(revision), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	candidate, err := readCandidate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.phase != "prepare" || candidate.transaction != "1-abcd" || string(candidate.config) != string(config) {
		t.Fatalf("unexpected projected candidate: %+v", candidate)
	}
}

func TestReadCandidateRejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	revision := filepath.Join(dir, "..revision")
	if err := os.Mkdir(revision, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"phase": "prepare", "transaction": "1-abcd", "hash": strings.Repeat("0", 64),
		"haproxy.cfg": "global\n", "previous.cfg": "global\n",
	} {
		if err := os.WriteFile(filepath.Join(revision, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Base(revision), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := readCandidate(dir); err == nil {
		t.Fatal("candidate with mismatched hash was accepted")
	}
}

func TestPatchPodStatusUsesOnlyNamedStatusEntry(t *testing.T) {
	var request *http.Request
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("short-lived-token"), 0600); err != nil {
		t.Fatal(err)
	}
	status := PodStatus{Transaction: "1-abcd", Phase: "prepared", Hash: "hash"}
	if err := patchPodStatus(context.Background(), server.Client(), server.URL, tokenPath, "haproxy-0", status); err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodPatch || request.Header.Get("Authorization") != "Bearer short-lived-token" ||
		request.Header.Get("Content-Type") != "application/merge-patch+json" {
		t.Fatalf("unexpected status patch request: method=%s headers=%v", request.Method, request.Header)
	}
	var patch struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch.Data) != 1 || patch.Data["haproxy-0"] == "" {
		t.Fatalf("status patch contained unexpected keys: %s", body)
	}
}

func TestPrepareValidationFailureDoesNotApplyConfiguration(t *testing.T) {
	config := []byte("global\n")
	status, done := processCandidate(context.Background(), Reloader{
		Dir: t.TempDir(), Binary: filepath.Join(t.TempDir(), "missing-haproxy"),
	}, candidate{
		phase: "prepare", transaction: "1-abcd", hash: Hash(config), config: config,
	})
	if done || status == nil || status.Phase != "failed" || status.Message == "" {
		t.Fatalf("invalid candidate was not reported during prepare: status=%+v done=%v", status, done)
	}
}

func TestAbortCandidateDoesNotApplyConfiguration(t *testing.T) {
	status, done := processCandidate(context.Background(), Reloader{
		Dir: t.TempDir(), Binary: filepath.Join(t.TempDir(), "missing-haproxy"),
	}, candidate{phase: "abort", transaction: "1-abcd", hash: strings.Repeat("0", 64)})
	if !done || status == nil || status.Phase != "aborted" {
		t.Fatalf("abort did not finish without applying: status=%+v done=%v", status, done)
	}
}

func TestIdleCandidateReappliesCommittedConfigurationAfterRestart(t *testing.T) {
	r := Reloader{Dir: testDir(t), Binary: "true"}
	stale, committed := []byte("stale projected configuration"), []byte("committed configuration")
	// HAProxy restarted before the active ConfigMap projection was refreshed.
	if err := r.Initialize(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", r.path("master.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan string, 4)
	go func() {
		for _, response := range []string{"Success=1\n", "456 worker\n"} {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			command, _ := io.ReadAll(conn)
			commands <- strings.TrimSpace(string(command))
			io.WriteString(conn, response)
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idle := candidate{phase: "idle", transaction: "2-abcd-1", hash: Hash(committed), config: committed, previous: committed}
	if err := reconcileIdle(ctx, r, idle); err != nil {
		t.Fatal(err)
	}
	if got := <-commands; got != "reload" {
		t.Fatalf("first master command = %q, want reload", got)
	}
	for _, name := range []string{"active.cfg", "committed.cfg"} {
		data, err := os.ReadFile(r.path(name))
		if err != nil || string(data) != string(committed) {
			t.Fatalf("%s = %q, err=%v; want committed configuration", name, data, err)
		}
	}
	// Once converged, idle reconciliation must not reload again.
	if err := reconcileIdle(ctx, r, idle); err != nil {
		t.Fatal(err)
	}
	<-commands
	select {
	case command := <-commands:
		t.Fatalf("converged idle candidate issued %q", command)
	default:
	}
}

func TestIdleCandidateWaitsForHAProxyStartup(t *testing.T) {
	r := Reloader{Dir: testDir(t), Binary: "false"}
	config := []byte("committed configuration")
	idle := candidate{phase: "idle", transaction: "bootstrap", hash: Hash(config), config: config, previous: config}
	if err := reconcileIdle(context.Background(), r, idle); err != nil {
		t.Fatalf("idle reconciliation before startup = %v", err)
	}
}

func TestSteadyStatusReportsWhatThePodActuallyRuns(t *testing.T) {
	r := Reloader{Dir: t.TempDir()}
	config := []byte("committed configuration")
	c := candidate{phase: "idle", transaction: "bootstrap", hash: Hash(config), config: config}
	if status := steadyStatus(r, c, "", 7, 0); status != nil {
		t.Fatalf("an uninitialized HAProxy must not be reported: %+v", status)
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "committed.cfg"), config, 0600); err != nil {
		t.Fatal(err)
	}
	if status := steadyStatus(r, c, "", 7, 0); status == nil || status.Phase != "active" || status.Hash != Hash(config) || status.WorkerPID != 7 {
		t.Fatalf("converged pod not reported active: %+v", status)
	}
	stale := []byte("stale configuration")
	if err := os.WriteFile(filepath.Join(r.Dir, "committed.cfg"), stale, 0600); err != nil {
		t.Fatal(err)
	}
	status := steadyStatus(r, c, "bind failed", 7, 0)
	if status == nil || status.Phase != "diverged" || status.Hash != Hash(stale) || status.Message != "bind failed" {
		t.Fatalf("diverged pod must report its real hash and the apply error: %+v", status)
	}
}

func TestSteadyReporterPublishesOnlyChanges(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	report := func(s *steadyReporter, status *PodStatus) {
		s.report(context.Background(), server.Client(), server.URL, tokenPath, "haproxy-0", status)
	}
	reporter := &steadyReporter{}
	active := &PodStatus{Transaction: "bootstrap", Phase: "active", Hash: "h1"}
	report(reporter, nil)
	report(reporter, active)
	report(reporter, active)
	if calls != 1 {
		t.Fatalf("unchanged or empty status must not be republished: %d calls", calls)
	}
	report(reporter, &PodStatus{Transaction: "bootstrap", Phase: "active", Hash: "h2"})
	if calls != 2 {
		t.Fatalf("a changed status must be republished: %d calls", calls)
	}
	// A transaction acknowledgement overwrites the pod's entry; the reporter is
	// reset so the same steady-state status is published again afterwards.
	reporter.last = ""
	report(reporter, &PodStatus{Transaction: "bootstrap", Phase: "active", Hash: "h2"})
	if calls != 3 {
		t.Fatalf("status must be republished after a transaction: %d calls", calls)
	}
}

func TestSteadyReporterRetriesAfterAPIFailure(t *testing.T) {
	fail := true
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if fail {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	reporter := &steadyReporter{}
	status := &PodStatus{Transaction: "bootstrap", Phase: "active", Hash: "h1"}
	reporter.report(context.Background(), server.Client(), server.URL, tokenPath, "haproxy-0", status)
	fail = false
	reporter.report(context.Background(), server.Client(), server.URL, tokenPath, "haproxy-0", status)
	if calls != 2 || reporter.last == "" {
		t.Fatalf("a rejected report must be retried and eventually recorded: calls=%d", calls)
	}
}

func TestWorkerPIDChangesAreNotRestarts(t *testing.T) {
	// Regression (found on a live cluster): treating a new worker PID as a container
	// restart re-ran a failing activation every few seconds, pausing the pod's
	// listeners each time. Only a new boot ID is a restart.
	r := Reloader{Dir: t.TempDir()}
	boot := &BootWatcher{}
	if boot.Restarted(r) {
		t.Fatal("a missing boot ID (HAProxy not initialized yet) is not a restart")
	}
	write := func(id string) {
		if err := os.WriteFile(filepath.Join(r.Dir, bootIDFile), []byte(id), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("boot-1")
	if boot.Restarted(r) {
		t.Fatal("the first observation must not count as a restart")
	}
	if boot.Restarted(r) {
		t.Fatal("an unchanged boot ID (for example across reloads) must not count as a restart")
	}
	write("boot-2")
	if !boot.Restarted(r) {
		t.Fatal("a new boot ID means the container restarted")
	}
	if boot.Restarted(r) {
		t.Fatal("a restart must be reported once")
	}
	if err := os.Remove(filepath.Join(r.Dir, bootIDFile)); err != nil {
		t.Fatal(err)
	}
	if boot.Restarted(r) {
		t.Fatal("a vanished boot ID must not count as a restart")
	}
	write("boot-2")
	if boot.Restarted(r) {
		t.Fatal("reappearing with the same ID is not a restart")
	}
}

func TestInitializeWritesAFreshBootID(t *testing.T) {
	r := Reloader{Dir: testDir(t), Binary: "true"}
	read := func() string {
		data, err := os.ReadFile(filepath.Join(r.Dir, bootIDFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if err := r.Initialize(context.Background(), []byte("global\n")); err != nil {
		t.Fatal(err)
	}
	first := read()
	if err := r.Initialize(context.Background(), []byte("global\n")); err != nil {
		t.Fatal(err)
	}
	if second := read(); len(first) != 32 || second == first {
		t.Fatalf("every container start needs a new ID: %q then %q", first, second)
	}
}

func TestIdleRetryBackoffGrowsAndIsCapped(t *testing.T) {
	d := time.Duration(0)
	var seen []time.Duration
	for i := 0; i < 8; i++ {
		d = nextIdleBackoff(d)
		seen = append(seen, d)
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, idleBackoffMax, idleBackoffMax, idleBackoffMax}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("backoff step %d: got %s want %s (all: %v)", i, seen[i], want[i], seen)
		}
	}
}

// candidateServer serves a candidate ConfigMap and counts requests.
type candidateServer struct {
	*httptest.Server
	hits   int
	status int
	data   map[string]string
}

func newCandidateServer(t *testing.T, data map[string]string) *candidateServer {
	t.Helper()
	cs := &candidateServer{status: http.StatusOK, data: data}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(cs.status)
		if cs.status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": cs.data})
		} else {
			_, _ = io.WriteString(w, `{"message":"configmaps is forbidden"}`)
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

func candidateData(phase, transaction, config string) map[string]string {
	return map[string]string{
		"phase": phase, "transaction": transaction, "hash": Hash([]byte(config)),
		"haproxy.cfg": config, "previous.cfg": "global\n",
	}
}

func newSource(t *testing.T, server *candidateServer, dir string, now *time.Time) *candidateSource {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &candidateSource{
		client: server.Client(), url: server.URL, tokenPath: tokenPath, dir: dir, now: func() time.Time { return *now },
	}
}

// writeMountedCandidate creates a projected-volume copy of a candidate.
func writeMountedCandidate(t *testing.T, data map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	revision := filepath.Join(dir, "..rev")
	if err := os.Mkdir(revision, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range data {
		if err := os.WriteFile(filepath.Join(revision, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("..rev", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFetchCandidateReadsTheAPIAndVerifiesIt(t *testing.T) {
	now := time.Now()
	server := newCandidateServer(t, candidateData("commit", "7-abc-1", "global\n"))
	source := newSource(t, server, t.TempDir(), &now)
	c, err := fetchCandidate(context.Background(), server.Client(), server.URL, source.tokenPath)
	if err != nil || c.phase != "commit" || c.transaction != "7-abc-1" || string(c.config) != "global\n" {
		t.Fatalf("candidate not read from the API: %+v %v", c, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"hash mismatch (torn update)": func(d map[string]string) { d["hash"] = strings.Repeat("0", 64) },
		"missing key":                 func(d map[string]string) { delete(d, "previous.cfg") },
		"empty transaction":           func(d map[string]string) { d["transaction"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			data := candidateData("commit", "7-abc-1", "global\n")
			mutate(data)
			bad := newCandidateServer(t, data)
			if _, err := fetchCandidate(context.Background(), bad.Client(), bad.URL, source.tokenPath); err == nil {
				t.Fatal("an inconsistent candidate must be rejected")
			}
		})
	}
	server.status = http.StatusForbidden
	if _, err := fetchCandidate(context.Background(), server.Client(), server.URL, source.tokenPath); err == nil ||
		!strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("an authorization failure must be explained: %v", err)
	}
}

func TestCandidateSourcePrefersTheAPIOverAStaleMountedCopy(t *testing.T) {
	// The kubelet refreshes the mounted copy only every minute or so.
	now := time.Now()
	server := newCandidateServer(t, candidateData("commit", "9-new-1", "global\n"))
	stale := writeMountedCandidate(t, candidateData("idle", "8-old-1", "global\n"))
	source := newSource(t, server, stale, &now)
	c, err := source.get(context.Background())
	if err != nil || c.phase != "commit" || c.transaction != "9-new-1" {
		t.Fatalf("the API's newer phase must win over the stale mounted copy: %+v %v", c, err)
	}
}

func TestCandidateSourceKeepsAcceptedAPIStateWhenMountedCopyLags(t *testing.T) {
	now := time.Now()
	server := newCandidateServer(t, candidateData("commit", "9-new-1", "global\n"))
	stale := writeMountedCandidate(t, candidateData("idle", "8-old-1", "global\n"))
	source := newSource(t, server, stale, &now)
	if c, err := source.get(context.Background()); err != nil || c.transaction != "9-new-1" {
		t.Fatalf("the API candidate was not accepted: %+v %v", c, err)
	}

	server.status = http.StatusServiceUnavailable
	now = now.Add(candidatePollActive)
	c, err := source.get(context.Background())
	if err != nil || c.phase != "commit" || c.transaction != "9-new-1" {
		t.Fatalf("a stale mounted candidate replaced the accepted API state: %+v %v", c, err)
	}
}

func TestCandidateSourcePollsFastDuringATransactionAndSlowlyWhenIdle(t *testing.T) {
	now := time.Now()
	server := newCandidateServer(t, candidateData("prepare", "1-a-1", "global\n"))
	source := newSource(t, server, t.TempDir(), &now)
	get := func() candidate {
		t.Helper()
		c, err := source.get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	get()
	get() // same instant: served from memory
	if server.hits != 1 {
		t.Fatalf("repeated reads within the poll interval must not hit the API: %d requests", server.hits)
	}
	now = now.Add(candidatePollActive)
	get()
	if server.hits != 2 {
		t.Fatalf("a running transaction must be re-read every %s: %d requests", candidatePollActive, server.hits)
	}
	server.data = candidateData("idle", "1-a-1", "global\n")
	now = now.Add(candidatePollActive)
	if c := get(); c.phase != "idle" || server.hits != 3 {
		t.Fatalf("idle state not picked up: %+v hits=%d", c, server.hits)
	}
	now = now.Add(candidatePollIdle - time.Millisecond)
	get()
	if server.hits != 3 {
		t.Fatalf("an idle pod must poll slowly: %d requests", server.hits)
	}
	server.data = candidateData("prepare", "2-b-1", "global\n")
	now = now.Add(time.Millisecond)
	if c := get(); c.transaction != "2-b-1" || server.hits != 4 {
		t.Fatalf("a new transaction must be noticed within the idle interval: %+v hits=%d", c, server.hits)
	}
}

func TestCandidateSourceFallsBackToTheMountedCopyAndRecovers(t *testing.T) {
	now := time.Now()
	server := newCandidateServer(t, candidateData("commit", "5-x-1", "global\n"))
	server.status = http.StatusForbidden // for example a Role that does not yet allow the read
	mounted := writeMountedCandidate(t, candidateData("prepare", "5-x-1", "global\n"))
	source := newSource(t, server, mounted, &now)
	c, err := source.get(context.Background())
	if err != nil || c.phase != "prepare" {
		t.Fatalf("the mounted copy must keep the reloader working when the API read is denied: %+v %v", c, err)
	}
	now = now.Add(time.Second)
	hits := server.hits
	source.get(context.Background())
	if server.hits != hits {
		t.Fatal("a denied API read must not be retried every second")
	}
	server.status = http.StatusOK
	now = now.Add(candidateFallbackRetry)
	if c, err := source.get(context.Background()); err != nil || c.phase != "commit" {
		t.Fatalf("the API must be used again once it works: %+v %v", c, err)
	}
}

func TestCandidateSourceReportsWhenNeitherSourceWorks(t *testing.T) {
	now := time.Now()
	server := newCandidateServer(t, nil)
	server.status = http.StatusServiceUnavailable
	source := newSource(t, server, t.TempDir(), &now)
	if _, err := source.get(context.Background()); err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "mounted copy") {
		t.Fatalf("both failures must be reported: %v", err)
	}
}

func TestParseProcessesCountsDrainingWorkers(t *testing.T) {
	for name, tc := range map[string]struct {
		output       string
		current, old int
	}{
		"single worker": {
			"#<PID>          <type>          <reloads>       <uptime>        <version>\n1  master  0  0d00h01m00s  3.4.7\n# workers\n12  worker  0  0d00h01m00s  3.4.7\n",
			12, 0,
		},
		"one draining": {
			"#<PID> <type> <reloads> <uptime> <version>\n1 master 2 0d00h05m00s 3.4.7\n# workers\n30 worker 0 0d00h00m02s 3.4.7\n# old workers\n12 worker 1 0d00h05m00s 3.4.7\n",
			30, 1,
		},
		"three draining, then programs": {
			"1 master 3 0d00h09m00s 3.4.7\n# workers\n44 worker 0 0d00h00m01s 3.4.7\n# old workers\n12 worker 2 0d00h09m00s 3.4.7\n30 worker 1 0d00h04m00s 3.4.7\n38 worker 1 0d00h02m00s 3.4.7\n# programs\n77 worker 0 0d00h01m00s x\n",
			44, 3,
		},
		"only old workers (a rejected activation)": {
			"1 master 1 0d00h01m00s 3.4.7\n# workers\n# old workers\n12 worker 0 0d00h01m00s 3.4.7\n",
			0, 1,
		},
		"garbage": {"unexpected output", 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if current, old := parseProcesses(tc.output); current != tc.current || old != tc.old {
				t.Fatalf("got current=%d old=%d, want current=%d old=%d", current, old, tc.current, tc.old)
			}
		})
	}
}

func TestSteadyStatusReportsDrainingWorkers(t *testing.T) {
	r := Reloader{Dir: t.TempDir()}
	config := []byte("committed configuration")
	if err := os.WriteFile(filepath.Join(r.Dir, "committed.cfg"), config, 0600); err != nil {
		t.Fatal(err)
	}
	c := candidate{phase: "idle", transaction: "t", hash: Hash(config), config: config}
	status := steadyStatus(r, c, "", 9, 2)
	if status == nil || status.OldWorkers != 2 {
		t.Fatalf("draining workers must be visible: %+v", status)
	}
	raw, _ := json.Marshal(steadyStatus(r, c, "", 9, 0))
	if strings.Contains(string(raw), "oldWorkers") {
		t.Fatalf("a pod with nothing draining must not carry the field: %s", raw)
	}
	a, _ := json.Marshal(steadyStatus(r, c, "", 9, 2))
	b, _ := json.Marshal(steadyStatus(r, c, "", 9, 1))
	if string(a) == string(b) {
		t.Fatal("a change in draining workers must change what is reported")
	}
}

func TestTransactionStateSettlesEachOutcomeCorrectly(t *testing.T) {
	applied := &PodStatus{Phase: "applied"}
	rollbackFailed := &PodStatus{Phase: "rollback-failed"}
	for name, tc := range map[string]struct {
		before                   transactionState
		phase                    string
		report                   *PodStatus
		done, failed             bool
		wantRetryPhase           string
		wantTransactionForgotten bool
	}{
		"a delivered report marks the phase handled": {
			phase: "commit", report: applied, wantRetryPhase: "t:commit",
		},
		"an abort completes the transaction and forgets it": {
			before: transactionState{lastReported: "x", retryPhase: "t:abort", retryTransaction: "t"},
			phase:  "abort", report: &PodStatus{Phase: "aborted"}, done: true, wantTransactionForgotten: true,
		},
		"a report that could not be delivered stays queued and the phase stays handled": {
			before: transactionState{pending: applied}, phase: "commit", report: applied, failed: true, wantRetryPhase: "t:commit",
		},
		"a failed rollback is recomputed on the next pass, not queued": {
			before: transactionState{retryPhase: "t:rollback", retryTransaction: "t"},
			phase:  "rollback", report: rollbackFailed, wantRetryPhase: "t:rollback",
		},
		"a failed delivery with nothing queued retries the whole phase": {
			before: transactionState{retryPhase: "t:rollback"}, phase: "rollback", report: rollbackFailed, failed: true,
		},
		"no report for a new transaction clears the handled phase": {
			before: transactionState{retryPhase: "old:idle", retryTransaction: "old"}, phase: "idle",
		},
		"no report within the same transaction keeps the handled phase": {
			before: transactionState{retryPhase: "t:idle", retryTransaction: "t"}, phase: "idle", wantRetryPhase: "t:idle",
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := tc.before
			state.settle("t:"+tc.phase, "t", tc.report, tc.done, tc.failed)
			if state.retryPhase != tc.wantRetryPhase {
				t.Fatalf("retryPhase = %q, want %q", state.retryPhase, tc.wantRetryPhase)
			}
			if tc.wantTransactionForgotten && (state.lastReported != "" || state.retryTransaction != "") {
				t.Fatalf("a finished transaction must be forgotten: %+v", state)
			}
		})
	}
}

func TestForgetMakesThePhaseNewAfterARestart(t *testing.T) {
	state := transactionState{lastReported: "x", retryPhase: "t:commit", retryTransaction: "t", pending: &PodStatus{}, pendingFingerprint: "y"}
	state.forget()
	if state.retryPhase != "" || state.lastReported != "" || state.pending != nil || state.pendingFingerprint != "" {
		t.Fatalf("restart must clear what was remembered: %+v", state)
	}
}

func TestErrorLogPrintsEachDistinctErrorOncePerCategory(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	var log errorLog
	for _, message := range []string{"a", "a", "a", "b", "b", "a"} {
		log.report("x", errors.New(message), "doing x")
	}
	log.report("y", errors.New("a"), "doing y") // another category is independent
	log.clear("x")
	log.report("x", errors.New("a"), "doing x") // after the condition cleared, the same error is news again
	write.Close()
	os.Stderr = original
	output, _ := io.ReadAll(read)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 5 { // x:a, x:b, x:a, y:a, x:a
		t.Fatalf("expected 5 lines, got %d:\n%s", len(lines), output)
	}
}
