// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/scenarios"
)

func loadCatalog(t *testing.T) []scenarios.Scenario {
	t.Helper()
	c, err := scenarios.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func events(s scenarios.Scenario, actions ...string) string {
	var output strings.Builder
	for _, action := range actions {
		name := s.Test
		if strings.HasPrefix(action, "package:") {
			action = strings.TrimPrefix(action, "package:")
			name = ""
		}
		data, _ := json.Marshal(testEvent{Action: action, Test: name, Package: "example.org/repo/" + strings.TrimPrefix(s.Package, "./")})
		output.Write(data)
		output.WriteByte('\n')
	}
	return output.String()
}

func TestRunnerSelectsPackageAndIsolatesAllGates(t *testing.T) {
	catalog := loadCatalog(t)
	for _, s := range catalog {
		t.Run(s.Name, func(t *testing.T) {
			env := []string{"SCENARIO=" + s.Name, "INTEGRATION_CONTEXT=test-only", "INTEGRATION_OPERATOR_NAMESPACE=operator", "INTEGRATION_OPERATOR_DEPLOYMENT=operator", "MARKLOGIC_IMAGE=custom:image", "MARKLOGIC_VERSION=12.1.0"}
			for _, other := range catalog {
				env = append(env, other.Gate+"=true")
			}
			calls := 0
			fake := func(_ context.Context, args, childEnv []string, stdout, stderr io.Writer) error {
				selected := ""
				if calls == 1 {
					selected = s.Name
				}
				if !reflect.DeepEqual(childEnv, scenarios.Environment(catalog, env, selected)) {
					return fmt.Errorf("gates leaked into child")
				}
				if calls == 0 {
					if !reflect.DeepEqual(args, []string{"test", "-list", "^" + s.Test + "$", s.Package}) {
						return fmt.Errorf("discovery args: %v", args)
					}
					fmt.Fprintln(stdout, s.Test)
				} else {
					if !reflect.DeepEqual(args, []string{"test", "-json", "-count=1", "-timeout", "45m", "-run", "^" + s.Test + "$", s.Package}) {
						return fmt.Errorf("run args: %v", args)
					}
					io.WriteString(stdout, events(s, "run", "pass", "package:pass"))
				}
				calls++
				return nil
			}
			if err := runCLI(nil, env, catalog, fake, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("got %d commands", calls)
			}
		})
	}
}

func TestRunnerRejectsInvalidInputsBeforeGoTest(t *testing.T) {
	catalog := loadCatalog(t)
	for _, tc := range []struct{ args, env []string }{
		{nil, nil}, {[]string{"run", "typo"}, nil}, {[]string{"list", "extra"}, nil}, {[]string{"describe"}, nil},
		{[]string{"run", "platform-smoke"}, nil},
		{[]string{"run", "platform-smoke"}, []string{"INTEGRATION_CONTEXT=x", "INTEGRATION_TIMEOUT=0"}},
		{[]string{"run", "oauth-resource-server"}, []string{"INTEGRATION_CONTEXT=x"}},
		{[]string{"run", "oauth-authorization-code"}, []string{"INTEGRATION_CONTEXT=x", "INTEGRATION_OPERATOR_NAMESPACE=x", "INTEGRATION_OPERATOR_DEPLOYMENT=x", "MARKLOGIC_IMAGE=x", "MARKLOGIC_VERSION=12.0"}},
	} {
		fake := func(context.Context, []string, []string, io.Writer, io.Writer) error {
			t.Error("executed go test for invalid input")
			return nil
		}
		if err := runCLI(tc.args, tc.env, catalog, fake, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted args=%v env=%v", tc.args, tc.env)
		}
	}
}

func TestDiscoveryNeedsNoClusterConfiguration(t *testing.T) {
	catalog := loadCatalog(t)
	for _, args := range [][]string{{"list"}, {"describe", "platform-smoke"}} {
		var output bytes.Buffer
		fake := func(context.Context, []string, []string, io.Writer, io.Writer) error {
			t.Error("discovery executed a command")
			return nil
		}
		if err := runCLI(args, nil, catalog, fake, &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "platform-smoke") {
			t.Fatal("missing scenario")
		}
	}
	calls := 0
	fake := func(_ context.Context, args, env []string, stdout, stderr io.Writer) error {
		if !reflect.DeepEqual(env, scenarios.Environment(catalog, nil, "")) {
			t.Fatal("check enabled a gate")
		}
		fmt.Fprintln(stdout, catalog[calls].Test)
		calls++
		return nil
	}
	if err := runCLI([]string{"check"}, nil, catalog, fake, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls != len(catalog) {
		t.Fatal("check missed a scenario")
	}
}

func TestRunnerRejectsMissingSelectionAndCommandFailures(t *testing.T) {
	catalog := loadCatalog(t)
	s := catalog[len(catalog)-1]
	for _, mode := range []string{"zero matches", "duplicate matches", "list failure", "test failure", "skipped", "empty output", "invalid JSON"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			fake := func(_ context.Context, args, env []string, stdout, stderr io.Writer) error {
				calls++
				if calls == 1 {
					switch mode {
					case "zero matches":
						return nil
					case "duplicate matches":
						fmt.Fprintln(stdout, s.Test)
					case "list failure":
						return fmt.Errorf("compile failed")
					}
					fmt.Fprintln(stdout, s.Test)
					return nil
				}
				switch mode {
				case "test failure":
					io.WriteString(stdout, events(s, "run", "pass", "package:pass"))
					return fmt.Errorf("exit 1")
				case "skipped":
					io.WriteString(stdout, events(s, "run", "skip", "package:pass"))
				case "empty output":
				case "invalid JSON":
					io.WriteString(stdout, "not JSON\n")
				default:
					t.Error("ran suite after discovery failure")
				}
				return nil
			}
			if err := runCLI([]string{"run", s.Name}, []string{"INTEGRATION_CONTEXT=test-only"}, catalog, fake, io.Discard, io.Discard); err == nil {
				t.Fatal("false success")
			}
		})
	}
}

func TestEventContract(t *testing.T) {
	s := loadCatalog(t)[0]
	for _, actions := range [][]string{{"package:pass"}, {"run", "skip", "package:pass"}, {"run", "pass"}, {"run", "fail", "package:fail"}} {
		if err := consumeEvents(strings.NewReader(events(s, actions...)), s, io.Discard); err == nil {
			t.Errorf("accepted %v", actions)
		}
	}
	child := s
	child.Test += "/case"
	for _, outcome := range []string{"skip", "fail", "pass"} {
		stream := events(s, "run") + events(child, "run", outcome) + events(s, "pass", "package:pass")
		err := consumeEvents(strings.NewReader(stream), s, io.Discard)
		if (err == nil) != (outcome == "pass") {
			t.Errorf("subcase %s: %v", outcome, err)
		}
	}
}

func TestNestedSkippedCasesAndWrongTargetsCannotPass(t *testing.T) {
	s := loadCatalog(t)[0]
	group, leaf, other := s, s, s
	group.Test += "/group"
	leaf.Test += "/group/leaf"
	other.Test = "TestUnrelated"
	for _, stream := range []string{
		events(s, "run") + events(group, "run") + events(leaf, "run", "skip") + events(group, "pass") + events(s, "pass", "package:pass"),
		events(other, "run", "pass", "package:pass"),
		events(s, "run", "pass") + events(other, "run", "pass", "package:pass"),
	} {
		if err := consumeEvents(strings.NewReader(stream), s, io.Discard); err == nil {
			t.Fatal("false success")
		}
	}
}

// Exercise the real compiled suites, with an unusable kubeconfig and no context.
// A gate regression must fail before it can contact a cluster or create reports.
func TestDisabledLiveSuitesSkipBeforeSetup(t *testing.T) {
	catalog := loadCatalog(t)
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	results := t.TempDir()
	env := scenarios.Environment(catalog, os.Environ(), "")
	env = append(env, "INTEGRATION_CONTEXT=", "KUBECONFIG="+filepath.Join(t.TempDir(), "missing"), "INTEGRATION_RESULTS_DIR="+results)
	groups := map[string][]string{}
	for _, s := range catalog {
		groups[s.Package] = append(groups[s.Package], s.Test)
	}
	for pkg, names := range groups {
		cmd := exec.Command("go", "test", "-json", "-count=1", "-timeout", "30s", "-run", "^("+strings.Join(names, "|")+")$", pkg)
		cmd.Dir, cmd.Env = root, env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("disabled suite failed: %v\n%s", err, output)
		}
		skipped := map[string]bool{}
		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var event testEvent
			err := decoder.Decode(&event)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if event.Action == "skip" {
				skipped[event.Test] = true
			}
		}
		for _, name := range names {
			if !skipped[name] {
				t.Errorf("%s did not skip", name)
			}
		}
	}
	entries, err := os.ReadDir(results)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled suites created reports: %v %v", entries, err)
	}
}

func TestLocalCommandClearsEveryLiveGate(t *testing.T) {
	catalog := loadCatalog(t)
	env := []string{}
	for _, s := range catalog {
		env = append(env, s.Gate+"=true")
	}
	called := false
	fake := func(_ context.Context, args, childEnv []string, stdout, stderr io.Writer) error {
		called = true
		if !reflect.DeepEqual(childEnv, scenarios.Environment(catalog, env, "")) {
			t.Fatal("local command retained a gate")
		}
		if !reflect.DeepEqual(args, []string{"test", "-count=1", "-timeout", "2m", "./test/integration/..."}) {
			t.Fatalf("wrong local command: %v", args)
		}
		return nil
	}
	if err := runCLI([]string{"local"}, env, catalog, fake, io.Discard, io.Discard); err != nil || !called {
		t.Fatalf("local: %v called=%t", err, called)
	}
}
