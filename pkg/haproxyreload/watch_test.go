// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"context"
	"encoding/json"
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
