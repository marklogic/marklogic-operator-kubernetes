// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// integration-runner is invoked from the repository root by scripts/run.sh.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/scenarios"
)

type goCommand func(context.Context, []string, []string, io.Writer, io.Writer) error

func executeGo(ctx context.Context, args, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func main() {
	catalog, err := scenarios.Load()
	if err == nil {
		err = runCLI(os.Args[1:], os.Environ(), catalog, executeGo, os.Stdout, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Integration runner:", err)
		os.Exit(1)
	}
}

func runCLI(args, env []string, catalog []scenarios.Scenario, command goCommand, stdout, stderr io.Writer) error {
	getenv := func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if value, ok := strings.CutPrefix(env[i], key+"="); ok {
				return value
			}
		}
		return ""
	}
	if len(args) == 0 {
		args = []string{"run", getenv("SCENARIO")}
	}
	usage := "usage: run.sh list | describe NAME | check | local | run NAME (or SCENARIO=NAME run.sh)"
	switch args[0] {
	case "local":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		return command(context.Background(), []string{"test", "-count=1", "-timeout", "2m", "./test/integration/..."}, scenarios.Environment(catalog, env, ""), stdout, stderr)
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		for _, s := range catalog {
			fmt.Fprintf(stdout, "%s\t%s\n", s.Name, s.Description)
		}
		return nil
	case "check":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		// Check each registered exact selection without enabling any suite.
		for _, s := range catalog {
			if err := checkSelection(s, catalog, env, command, stderr); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "OK %s: %s %s\n", s.Name, s.Package, s.Test)
		}
		return nil
	case "describe", "run":
		if len(args) != 2 || args[1] == "" {
			return fmt.Errorf("%s", usage)
		}
	default:
		return fmt.Errorf("%s", usage)
	}
	var selected *scenarios.Scenario
	for i := range catalog {
		if catalog[i].Name == args[1] {
			selected = &catalog[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown scenario %q; use run.sh list", args[1])
	}
	s := *selected
	if args[0] == "describe" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(s)
	}
	if err := s.ValidateEnvironment(getenv); err != nil {
		return err
	}
	timeout := getenv("INTEGRATION_TIMEOUT")
	if timeout == "" {
		timeout = "45m"
	}
	duration, err := time.ParseDuration(timeout)
	if err != nil || duration <= 0 {
		return fmt.Errorf("INTEGRATION_TIMEOUT must be a positive Go duration")
	}
	if err := checkSelection(s, catalog, env, command, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Scenario: %s\nContext: %s\nPackage: %s\nTest: %s\n", s.Name, getenv("INTEGRATION_CONTEXT"), s.Package, s.Test)
	printProvenance(stdout, getenv("MARKLOGIC_IMAGE"))
	// JSON events let us reject passing packages that never ran the target test,
	// including a disabled gate or a renamed test. Human-readable output streams.
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), duration+2*time.Minute)
	defer cancel()
	go func() {
		err := command(ctx, []string{"test", "-json", "-count=1", "-timeout", timeout, "-run", "^" + s.Test + "$", s.Package}, scenarios.Environment(catalog, env, s.Name), writer, stderr)
		writer.Close()
		result <- err
	}()
	eventErr := consumeEvents(reader, s, stdout)
	// On invalid JSON, unblock the writer and stop the child process.
	reader.Close()
	if eventErr != nil {
		cancel()
	}
	commandErr := <-result
	if commandErr != nil {
		return fmt.Errorf("scenario %s: go test failed: %w", s.Name, commandErr)
	}
	return eventErr
}

func printProvenance(stdout io.Writer, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if commit, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
		fmt.Fprintf(stdout, "Commit: %s\n", strings.TrimSpace(string(commit)))
	}
	if status, err := exec.CommandContext(ctx, "git", "status", "--porcelain", "--untracked-files=normal").Output(); err == nil && len(status) > 0 {
		fmt.Fprintln(stdout, "Working tree contains changes; the commit alone does not identify this run.")
	}
	if image != "" {
		fmt.Fprintf(stdout, "MarkLogic image: %s\n", image)
	}
}

func checkSelection(s scenarios.Scenario, catalog []scenarios.Scenario, env []string, command goCommand, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var output bytes.Buffer
	if err := command(ctx, []string{"test", "-list", "^" + s.Test + "$", s.Package}, scenarios.Environment(catalog, env, ""), &output, stderr); err != nil {
		return fmt.Errorf("discover %s: %w", s.Name, err)
	}
	matches := 0
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.TrimSpace(line) == s.Test {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("scenario %s: expected exactly one %s in %s, found %d", s.Name, s.Test, s.Package, matches)
	}
	return nil
}

type testEvent struct{ Action, Package, Test, Output string }

func consumeEvents(reader io.Reader, s scenarios.Scenario, stdout io.Writer) error {
	decoder := json.NewDecoder(reader)
	started, passed, packagePassed := false, false, false
	subcases := map[string]string{}
	groups := map[string]bool{}
	unexpected := false
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("invalid go test JSON: %w", err)
		}
		if event.Output != "" {
			if _, err := io.WriteString(stdout, event.Output); err != nil {
				return err
			}
		}
		if !strings.HasSuffix(event.Package, "/"+strings.TrimPrefix(s.Package, "./")) {
			continue
		}
		if event.Test == "" && event.Action == "pass" {
			packagePassed = true
		}
		if event.Test == s.Test {
			switch event.Action {
			case "run":
				started = true
			case "pass":
				passed = true
			case "skip", "fail":
				unexpected = true
			}
		} else if strings.HasPrefix(event.Test, s.Test+"/") {
			if event.Action != "output" {
				subcases[event.Test] = event.Action
			}
			for parent := event.Test; strings.Contains(parent, "/"); {
				parent = parent[:strings.LastIndex(parent, "/")]
				groups[parent] = true
			}
			if event.Action == "fail" {
				unexpected = true
			}
		} else if event.Action == "run" {
			unexpected = true
		}
	}
	passedLeaf := false
	for name, outcome := range subcases {
		if !groups[name] && outcome == "pass" {
			passedLeaf = true
		}
	}
	if !started || !passed || !packagePassed || unexpected || (len(subcases) > 0 && !passedLeaf) {
		return fmt.Errorf("scenario %s did not execute and pass the exact target %s (or all subcases were skipped); inspect test output and live gate %s", s.Name, s.Test, s.Gate)
	}
	return nil
}
