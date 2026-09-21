// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runner selection and prerequisite regressions live with the Go runner. This
// test protects the original shell entry point and its argument forwarding.
func TestRunnerShellCompatibility(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
printf 'GO_ARGS:%s\nSCENARIO:%s\n' "$*" "$SCENARIO"
`
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"list"}, {"describe", "platform-smoke"}} {
		cmd := exec.Command("bash", append([]string{"../../scripts/run.sh"}, args...)...)
		cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "SCENARIO=oauth-resource-server"}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, output)
		}
		want := "GO_ARGS:run ./test/integration/cmd/integration-runner"
		if len(args) > 0 {
			want += " " + strings.Join(args, " ")
		}
		if !strings.Contains(string(output), want+"\n") {
			t.Fatalf("lost arguments: %s", output)
		}
		if !strings.Contains(string(output), "SCENARIO:oauth-resource-server") {
			t.Fatalf("lost SCENARIO: %s", output)
		}
	}
}
