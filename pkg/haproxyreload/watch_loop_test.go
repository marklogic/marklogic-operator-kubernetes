// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKubeAPI serves the candidate ConfigMap and records the status patches a sidecar sends.
type fakeKubeAPI struct {
	*httptest.Server
	mu        sync.Mutex
	candidate map[string]string
	reports   []PodStatus
}

func newFakeKubeAPI(t *testing.T) *fakeKubeAPI {
	t.Helper()
	api := &fakeKubeAPI{}
	api.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer loop-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/configmaps/"+CandidateMap):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.candidate})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/configmaps/"+StatusMap):
			body, _ := io.ReadAll(r.Body)
			var patch struct {
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(body, &patch); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var report PodStatus
			if err := json.Unmarshal([]byte(patch.Data["pod-0"]), &report); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			api.reports = append(api.reports, report)
			_, _ = io.WriteString(w, "{}")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *fakeKubeAPI) set(phase, transaction string, config, previous []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.candidate = map[string]string{
		"phase": phase, "transaction": transaction, "hash": Hash(config),
		"haproxy.cfg": string(config), "previous.cfg": string(previous),
	}
}

func (a *fakeKubeAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reports)
}

// await returns the first report after index `from` that has this phase and hash.
func (a *fakeKubeAPI) await(t *testing.T, from int, phase string, config []byte) PodStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for i := from; i < len(a.reports); i++ {
			if a.reports[i].Phase == phase && a.reports[i].Hash == Hash(config) {
				report := a.reports[i]
				a.mu.Unlock()
				return report
			}
		}
		a.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t.Fatalf("no %q report for hash %.8s after #%d; reports: %+v", phase, Hash(config), from, a.reports)
	return PodStatus{}
}

func masterReloads(t *testing.T, r Reloader) int {
	t.Helper()
	output, err := r.cli(context.Background(), "show proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[1] == "master" {
			n, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no master line in %q", output)
	return 0
}

func servedVersion(t *testing.T, addr string) string {
	t.Helper()
	response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return strings.TrimSpace(string(body))
}

// TestWatchLoopRunsATransactionAgainstRealHAProxy drives the real sidecar loop through a full
// transaction, a failed activation and a container restart, against a fake API server.
func TestWatchLoopRunsATransactionAgainstRealHAProxy(t *testing.T) {
	r, v1, httpAddr, _ := testProxy(t)
	api := newFakeKubeAPI(t)
	tokenDir, healthDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "token"), []byte("loop-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(tokenDir, "ca.crt"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(api.Listener.Addr().String())
	api.set("idle", "bootstrap", v1, v1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- WatchWith(ctx, r, WatchConfig{
			APIHost: host, APIPort: port, Namespace: "ns", Pod: "pod-0", APITokenDir: tokenDir,
			CandidateDir: t.TempDir(), HealthDir: healthDir, Tick: 30 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the watcher did not stop")
		}
	})

	// Steady state: a pod that runs the committed configuration says so.
	if active := api.await(t, 0, "active", v1); active.Transaction != "bootstrap" || active.WorkerPID == 0 {
		t.Fatalf("steady-state report incomplete: %+v", active)
	}
	if err := CheckHeartbeat(healthDir, time.Minute, time.Now()); err != nil {
		t.Fatalf("the loop must keep its liveness heartbeat: %v", err)
	}

	// A normal transaction: prepare validates, commit reloads, idle returns to steady state.
	v2 := bytes.ReplaceAll(v1, []byte("version-one"), []byte("version-two"))
	mark := api.count()
	api.set("prepare", "tx-1", v2, v1)
	api.await(t, mark, "prepared", v2)
	if got := servedVersion(t, httpAddr); got != "version-one" {
		t.Fatalf("prepare must not change what is served: %q", got)
	}
	api.set("commit", "tx-1", v2, v1)
	applied := api.await(t, mark, "applied", v2)
	if got := servedVersion(t, httpAddr); got != "version-two" || applied.WorkerPID == 0 {
		t.Fatalf("commit must reload HAProxy onto the new configuration: served %q, report %+v", got, applied)
	}
	mark = api.count()
	api.set("idle", "tx-1", v2, v2)
	api.await(t, mark, "active", v2)

	// A configuration that passes `haproxy -c` but cannot bind (the port is taken) must be
	// reported once and restored, not re-applied in a loop while the commit phase lasts.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	unbindable := append(append([]byte{}, v2...), []byte(fmt.Sprintf(
		"\nfrontend unbindable\n  mode http\n  bind %s\n  http-request return status 200 content-type text/plain string x\n", taken.Addr()))...)
	mark, reloadsBefore := api.count(), masterReloads(t, r)
	api.set("prepare", "tx-2", unbindable, v2)
	api.await(t, mark, "prepared", unbindable)
	api.set("commit", "tx-2", unbindable, v2)
	failed := api.await(t, mark, "failed", unbindable)
	if !strings.Contains(failed.Message, "Success=0") && !strings.Contains(strings.ToLower(failed.Message), "bind") {
		t.Fatalf("the failure must say why: %q", failed.Message)
	}
	// One re-applied attempt takes about three seconds (HAProxy pauses its listeners while it
	// tries), so wait long enough for a second one to show if the loop were to repeat.
	time.Sleep(4500 * time.Millisecond)
	api.mu.Lock()
	failedReports := 0
	for _, report := range api.reports {
		if report.Phase == "failed" && report.Hash == Hash(unbindable) {
			failedReports++
		}
	}
	api.mu.Unlock()
	if extra := masterReloads(t, r) - reloadsBefore; extra > 3 || failedReports != 1 {
		t.Fatalf("a failed activation was re-applied in a loop: %d reloads and %d failure reports in 4.5s "+
			"(expected the attempt and one restore, reported once)", extra, failedReports)
	}
	if got := servedVersion(t, httpAddr); got != "version-two" {
		t.Fatalf("after the failed activation the previous configuration must still serve: %q", got)
	}
	mark = api.count()
	api.set("rollback", "tx-2", unbindable, v2)
	api.await(t, mark, "rolledback", v2)
	api.set("idle", "tx-2", v2, v2)

	// Only a new boot ID (the container restarted) makes the loop process a phase again.
	mark = api.count()
	api.set("commit", "tx-3", v2, v1)
	api.await(t, mark, "applied", v2)
	settled := api.count()
	time.Sleep(600 * time.Millisecond)
	if api.count() != settled {
		t.Fatal("an unchanged phase must not be reported again")
	}
	if err := os.WriteFile(filepath.Join(r.Dir, bootIDFile), []byte("a-new-boot"), 0660); err != nil {
		t.Fatal(err)
	}
	api.await(t, settled, "applied", v2)
}
