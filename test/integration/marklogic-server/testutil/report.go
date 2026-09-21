// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type stageResult struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}
type caseResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}
type resourceSummary struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}
type runResult struct {
	SchemaVersion int               `json:"schemaVersion"`
	ID            string            `json:"runID"`
	Scenario      string            `json:"scenario"`
	Test          string            `json:"test"`
	Context       string            `json:"context"`
	Server        string            `json:"server,omitempty"`
	Namespace     string            `json:"namespace,omitempty"`
	Commit        string            `json:"commit,omitempty"`
	WorkingTree   string            `json:"workingTree"`
	Started       time.Time         `json:"started"`
	Finished      *time.Time        `json:"finished,omitempty"`
	Outcome       string            `json:"outcome"`
	FailureStage  string            `json:"failureStage,omitempty"`
	Cleanup       string            `json:"cleanup"`
	Stages        []stageResult     `json:"stages"`
	Cases         []caseResult      `json:"cases,omitempty"`
	Versions      map[string]string `json:"versions,omitempty"`
	Resources     []resourceSummary `json:"resources,omitempty"`
}

type runReport struct {
	mu       sync.Mutex
	dir      string
	result   runResult
	redactor *redactor
}

func newRunReport(id, scenario, testName string) (*runReport, error) {
	root := os.Getenv("INTEGRATION_RESULTS_DIR")
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		// go test runs in the package directory. Keep the default at the repo root.
		for {
			if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(cwd)
			if parent == cwd {
				return nil, fmt.Errorf("cannot find go.mod; set INTEGRATION_RESULTS_DIR")
			}
			cwd = parent
		}
		root = filepath.Join(cwd, "test", "test_results", "integration")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return nil, err
	}
	r := &runReport{dir: dir, redactor: newRedactor(), result: runResult{
		SchemaVersion: 1, ID: id, Scenario: scenario, Test: testName, Context: os.Getenv("INTEGRATION_CONTEXT"),
		Started: time.Now().UTC(), Outcome: "running", Cleanup: "not_created", WorkingTree: "unknown", Versions: map[string]string{},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
		r.result.Commit = trimOutput(output)
	}
	if output, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output(); err == nil {
		r.result.WorkingTree = "clean"
		if len(output) > 0 {
			r.result.WorkingTree = "modified"
		}
	}
	return r, r.writeLocked()
}

func (r *runReport) writeLocked() error { return writeJSON(r.dir, "run.json", r.result) }

// Writes are atomic so readers never observe a partially written result.
func writeJSON(dir, name string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".report-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(append(contents, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(dir, name))
}

func (r *Run) updateReport(t *testing.T, update func(*runResult)) {
	t.Helper()
	if r.report == nil {
		return
	}
	r.report.mu.Lock()
	defer r.report.mu.Unlock()
	update(&r.report.result)
	if err := r.report.writeLocked(); err != nil {
		t.Errorf("Write integration result: %v", err)
	}
}

// Stage records progress before an operation that may fail or time out.
func (r *Run) Stage(t *testing.T, name string) {
	t.Helper()
	r.updateReport(t, func(result *runResult) {
		result.Stages = append(result.Stages, stageResult{Name: name, At: time.Now().UTC()})
	})
}

func (r *Run) recordVersion(t *testing.T, name, value string) {
	t.Helper()
	r.updateReport(t, func(result *runResult) { result.Versions[name] = value })
}

// Case records a subtest's result after its cleanup callbacks have run.
func (r *Run) Case(t *testing.T, name string, body func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Cleanup(func() {
			r.updateReport(t, func(result *runResult) {
				result.Cases = append(result.Cases, caseResult{Name: name, Outcome: testOutcome(t)})
			})
		})
		body(t)
	})
}

func testOutcome(t *testing.T) string {
	if t.Failed() {
		return "failed"
	}
	if t.Skipped() {
		return "skipped"
	}
	return "passed"
}

func (r *Run) finishReport(t *testing.T) {
	t.Helper()
	r.updateReport(t, func(result *runResult) {
		now := time.Now().UTC()
		result.Finished = &now
		if result.Outcome != "prerequisites_unmet" {
			result.Outcome = testOutcome(t)
		}
		if result.Outcome == "passed" && len(result.Cases) > 0 {
			allSkipped := true
			for _, entry := range result.Cases {
				if entry.Outcome != "skipped" {
					allSkipped = false
				}
			}
			if allSkipped {
				result.Outcome = "skipped"
			}
		}
		if result.Outcome == "failed" && result.FailureStage == "" && len(result.Stages) > 0 {
			result.FailureStage = result.Stages[len(result.Stages)-1].Name
		}
	})
	if r.report != nil {
		r.report.mu.Lock()
		defer r.report.mu.Unlock()
		if err := r.report.writeJUnit(); err != nil {
			t.Errorf("Write JUnit result: %v", err)
			r.report.result.Outcome = "failed"
			if r.report.result.FailureStage == "" {
				r.report.result.FailureStage = "write_report"
			}
			if err := r.report.writeLocked(); err != nil {
				t.Errorf("Record report failure: %v", err)
			}
		}
	}
}
