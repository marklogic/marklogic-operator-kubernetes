// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
)

func TestS3BackupRestore(t *testing.T) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(liveGate)), "true") {
		t.Skipf("set %s=true to run S3 backup/restore", liveGate)
	}
	c, err := configFromEnvironment(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	run := testutil.NewRun(t, "backup-s3", true)
	runURI := c.BaseURI + "/" + run.ID
	// Backups are useful evidence and outlive the namespace intentionally. The
	// report must not imply namespace cleanup also deleted objects in S3.
	run.RecordRetainedArtifact(t, "s3-backup-prefix", runURI+"/")
	t.Logf("S3 backup evidence is retained at %s/; remove this exact run prefix after review using your bucket retention policy", runURI)
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	objects, err := buildObjects(c, run.Namespace, run.StorageClass, "Backup!"+hex.EncodeToString(random))
	if err != nil {
		t.Fatal(err)
	}
	run.ApplyObjects(t, objects...)
	run.Stage(t, "readiness")
	testutil.WaitForStatefulSetReady(t, run.Namespace, clusterName, 15*time.Minute)
	testutil.WaitForPodReady(t, run.Namespace, clientName, 2*time.Minute)
	run.LogImages(t)
	step := func(name string, body func(*testing.T)) bool {
		run.Stage(t, name)
		run.Case(t, name, body)
		return !t.Failed()
	}
	if !step("credentials_applied", func(t *testing.T) { waitForCredentials(t, run, c) }) {
		return
	}
	var topology struct {
		Forests []forest `json:"forests"`
	}
	eval(t, run, "Schemas", forestsQuery, nil, &topology)
	if len(topology.Forests) == 0 {
		t.Fatal("Documents database has no forests")
	}
	if !step("seed_and_verify", func(t *testing.T) { writeAndVerify(t, run, "original") }) {
		return
	}
	backupPath := ""
	if !step("backup_completed", func(t *testing.T) {
		var created struct {
			Created bool `json:"created"`
		}
		eval(t, run, "Schemas", directoryQuery, map[string]string{"directory": runURI}, &created)
		if !created.Created {
			t.Fatal("backup directory was not created")
		}
		id := startJob(t, run, backupQuery, runURI)
		backupPath = waitForJob(t, run, id, topology.Forests, true, runURI)
		run.RecordRetainedArtifact(t, "s3-backup", backupPath)
	}) {
		return
	}
	// Prove restore reads the backup rather than simply observing seeded data.
	if !step("change_and_verify", func(t *testing.T) { writeAndVerify(t, run, "changed-after-backup") }) {
		return
	}
	step("restore_original_data", func(t *testing.T) {
		id := startJob(t, run, restoreQuery, backupPath)
		waitForJob(t, run, id, topology.Forests, false, runURI)
		verifyDocument(t, run, "original")
	})
}

func eval(t *testing.T, run *testutil.Run, database, query string, variables map[string]string, result any) {
	t.Helper()
	vars, err := json.Marshal(variables)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"curl", "--silent", "--show-error", "--fail", "--max-time", "60", "--config", "/etc/backup/auth/curl.conf", "--cacert", "/etc/backup/ca/cacert.pem", "--output", "/work/eval-body", "--dump-header", "/work/eval-headers", "--header", "Accept: multipart/mixed", "--data-urlencode", "xquery=" + query}
	if variables != nil {
		args = append(args, "--data-urlencode", "vars="+string(vars))
	}
	args = append(args, "https://"+serverHost(run.Namespace, 0)+":8000/v1/eval?database="+database)
	testutil.ExecuteInPod(t, run.Namespace, clientName, "curl", args...)
	headers := testutil.ExecuteInPod(t, run.Namespace, clientName, "curl", "cat", "/work/eval-headers")
	body := testutil.ExecuteInPod(t, run.Namespace, clientName, "curl", "cat", "/work/eval-body")
	if err := decodeEval(headers, body, result); err != nil {
		t.Fatal(err)
	}
}

func writeAndVerify(t *testing.T, run *testutil.Run, value string) {
	t.Helper()
	var result struct {
		Written bool `json:"written"`
	}
	eval(t, run, "Documents", writeQuery, map[string]string{"run": run.ID, "value": value}, &result)
	if !result.Written {
		t.Fatal("test document was not written")
	}
	verifyDocument(t, run, value)
}
func verifyDocument(t *testing.T, run *testutil.Run, value string) {
	t.Helper()
	var result struct {
		Run   string `json:"run"`
		Value string `json:"value"`
	}
	eval(t, run, "Documents", readQuery, nil, &result)
	if result.Run != run.ID || result.Value != value {
		t.Fatal("document does not match this run's expected content")
	}
}
func startJob(t *testing.T, run *testutil.Run, query, directory string) string {
	t.Helper()
	var result struct {
		Job string `json:"job"`
	}
	eval(t, run, "Schemas", query, map[string]string{"directory": directory}, &result)
	if id, err := strconv.ParseUint(result.Job, 10, 64); err != nil || id == 0 {
		t.Fatal("backup/restore did not return a valid job ID")
	}
	return result.Job
}
func waitForJob(t *testing.T, run *testutil.Run, id string, forests []forest, backup bool, runURI string) string {
	t.Helper()
	query := restoreStatusQuery
	if backup {
		query = backupStatusQuery
	}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var status jobStatus
		eval(t, run, "Schemas", query, map[string]string{"job": id}, &status)
		done, path, err := jobComplete(status, forests, backup, runURI)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return path
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("backup/restore did not complete within ten minutes")
	return ""
}
func waitForCredentials(t *testing.T, run *testutil.Run, c config) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "kubectl", "--context", os.Getenv("INTEGRATION_CONTEXT"), "get", "marklogiccluster", clusterName, "-n", run.Namespace, "-o", "json")
		output, err := cmd.Output()
		cancel()
		if err != nil {
			t.Fatalf("read object-storage status: %v (response omitted)", err)
		}
		var cluster marklogicv1.MarklogicCluster
		if err := json.Unmarshal(output, &cluster); err != nil {
			t.Fatal("invalid MarklogicCluster status JSON")
		}
		applied, err := credentialsApplied(cluster, c)
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("AWS credentials did not reach Applied with this run's fingerprint within five minutes")
}
