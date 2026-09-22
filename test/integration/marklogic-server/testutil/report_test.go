// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportLifecycleChild(t *testing.T) {
	mode := os.Getenv("INTEGRATION_REPORT_TEST_MODE")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	if mode == "prerequisite" {
		t.Setenv("INTEGRATION_CONTEXT", "")
		NewRun(t, "local-report-test", false)
		return
	}
	if mode == "missing-kubectl" {
		t.Setenv("INTEGRATION_CONTEXT", "must-not-connect")
		t.Setenv("PATH", t.TempDir())
		NewRun(t, "local-report-test", false)
		return
	}
	report, err := newRunReport("test-id", "local-report-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{report: report}
	t.Cleanup(func() { run.finishReport(t) })
	run.Stage(t, "verify")
	run.updateReport(t, func(result *runResult) { result.Cleanup = "completed" })
	switch mode {
	case "pass":
		run.Case(t, "case", func(t *testing.T) {})
	case "fail":
		run.Case(t, "case", func(t *testing.T) { t.Fatal("intentional failure") })
	case "skip":
		run.Case(t, "case", func(t *testing.T) { t.Skip("intentional skip") })
	case "cleanup":
		run.Stage(t, "cleanup")
		run.updateReport(t, func(result *runResult) { result.Cleanup = "failed" })
		t.Fatal("intentional cleanup failure")
	case "interrupted":
		os.Exit(2)
	}
}

func TestReportLifecycle(t *testing.T) {
	for _, tc := range []struct {
		mode, outcome string
		exitOK        bool
	}{
		{"pass", "passed", true}, {"fail", "failed", false}, {"skip", "skipped", true},
		{"cleanup", "failed", false}, {"prerequisite", "prerequisites_unmet", false}, {"interrupted", "running", false},
		{"missing-kubectl", "prerequisites_unmet", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestReportLifecycleChild$")
			cmd.Env = append(os.Environ(), "INTEGRATION_REPORT_TEST_MODE="+tc.mode, "INTEGRATION_RESULTS_DIR="+root)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.exitOK {
				t.Fatalf("unexpected child outcome: %v\n%s", err, output)
			}
			if tc.mode == "missing-kubectl" && !strings.Contains(string(output), "kubectl is required") {
				t.Fatalf("missing tool did not fail before cluster access: %s", output)
			}
			dirs, err := os.ReadDir(root)
			if err != nil || len(dirs) != 1 {
				t.Fatalf("expected one run directory: %v %v", dirs, err)
			}
			dir := filepath.Join(root, dirs[0].Name())
			contents, err := os.ReadFile(filepath.Join(dir, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result runResult
			if err := json.Unmarshal(contents, &result); err != nil {
				t.Fatal(err)
			}
			if result.Outcome != tc.outcome {
				t.Fatalf("outcome=%q want=%q", result.Outcome, tc.outcome)
			}
			if tc.mode == "interrupted" {
				if result.Finished != nil {
					t.Fatal("interrupted run was marked finished")
				}
				return
			}
			if result.Finished == nil {
				t.Fatal("completed run missing finish time")
			}
			if tc.mode == "fail" && result.FailureStage != "verify" {
				t.Fatalf("lost failure stage: %q", result.FailureStage)
			}
			if tc.mode == "cleanup" && result.Cleanup != "failed" {
				t.Fatal("lost cleanup failure")
			}
			contents, err = os.ReadFile(filepath.Join(dir, "junit.xml"))
			if err != nil {
				t.Fatal(err)
			}
			var suite junitSuite
			if err := xml.Unmarshal(contents, &suite); err != nil {
				t.Fatal(err)
			}
			if suite.Tests != 1 {
				t.Fatalf("expected one test, got %d", suite.Tests)
			}
			if tc.mode == "skip" && suite.Skipped != 1 {
				t.Fatal("JUnit lost skipped status")
			}
			if !tc.exitOK && suite.Failures != 1 {
				t.Fatal("JUnit lost failure")
			}
			info, err := os.Stat(filepath.Join(dir, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Errorf("report file mode=%v", info.Mode().Perm())
			}
		})
	}
}

func TestRunDirectoriesAreUnique(t *testing.T) {
	t.Setenv("INTEGRATION_RESULTS_DIR", t.TempDir())
	a, err := newRunReport("one", "same", "test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRunReport("two", "same", "test")
	if err != nil {
		t.Fatal(err)
	}
	if a.dir == b.dir {
		t.Fatal("runs reused the results directory")
	}
}

func TestRetainedArtifactsAreDistinctFromNamespaceCleanup(t *testing.T) {
	t.Setenv("INTEGRATION_RESULTS_DIR", t.TempDir())
	report, err := newRunReport("test-id", "backup-s3", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{report: report}
	report.redactor.add("private-token")
	run.RecordRetainedArtifact(t, "backup", "s3://example/test/run-id/")
	run.RecordRetainedArtifact(t, "sensitive-location", "https://example/path?token=private-token")
	run.updateReport(t, func(result *runResult) { result.Cleanup = "completed" })
	contents, err := os.ReadFile(filepath.Join(report.dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "private-token") {
		t.Fatal("artifact location leaked a registered secret")
	}
	var result runResult
	if err := json.Unmarshal(contents, &result); err != nil {
		t.Fatal(err)
	}
	if result.Cleanup != "completed" || len(result.RetainedArtifacts) != 2 || result.RetainedArtifacts[0].Location != "s3://example/test/run-id/" {
		t.Fatal("namespace cleanup hid retained external evidence")
	}
}
