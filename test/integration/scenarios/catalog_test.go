// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package scenarios

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogRejectsInvalidRegistrations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]Scenario) []Scenario
	}{
		{"empty", func(c []Scenario) []Scenario { return nil }},
		{"duplicate name", func(c []Scenario) []Scenario { c[1].Name = c[0].Name; return c }},
		{"duplicate gate", func(c []Scenario) []Scenario { c[1].Gate = c[0].Gate; return c }},
		{"duplicate selection", func(c []Scenario) []Scenario { c[1].Test = c[0].Test; return c }},
		{"test regex", func(c []Scenario) []Scenario { c[0].Test = "Test.*"; return c }},
		{"subtest", func(c []Scenario) []Scenario { c[0].Test = "TestOne/subcase"; return c }},
		{"recursive package", func(c []Scenario) []Scenario { c[0].Package = "./test/integration/..."; return c }},
		{"outside integration", func(c []Scenario) []Scenario { c[0].Package = "./internal/controller"; return c }},
		{"traversal", func(c []Scenario) []Scenario { c[0].Package = "./test/integration/../e2e"; return c }},
		{"noncanonical package", func(c []Scenario) []Scenario { c[0].Package = "./test/integration//oauth"; return c }},
		{"gate config collision", func(c []Scenario) []Scenario { c[0].Gate = "INTEGRATION_CONTEXT"; return c }},
		{"optional config collision", func(c []Scenario) []Scenario { c[0].Gate = "INTEGRATION_RETAIN_NAMESPACE"; return c }},
		{"unsupported patch minimum", func(c []Scenario) []Scenario { c[1].MinMarkLogicVersion = "12.1.4"; return c }},
		{"invalid gate", func(c []Scenario) []Scenario { c[0].Gate = "PATH"; return c }},
		{"missing context", func(c []Scenario) []Scenario { c[0].RequiredEnv = nil; return c }},
		{"missing prerequisites", func(c []Scenario) []Scenario { c[0].Prerequisites = nil; return c }},
		{"invalid version", func(c []Scenario) []Scenario { c[1].MinMarkLogicVersion = "latest"; return c }},
		{"missing version declaration", func(c []Scenario) []Scenario { c[0].MinMarkLogicVersion = "12.1"; return c }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(tc.mutate(catalog))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decode(data); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
	for _, data := range [][]byte{[]byte(`[{"typo":true}]`), append(append([]byte{}, catalogJSON...), []byte(` []`)...), []byte(`{`)} {
		if _, err := decode(data); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}

func TestVersionDeclaration(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := catalog[1]
	for _, value := range []string{"12.1", "12.1.0", "12.2.1-custom", "13.0.0", "012.01.0"} {
		if err := s.ValidateEnvironment(func(key string) string {
			if key == "MARKLOGIC_VERSION" {
				return value
			}
			return "configured"
		}); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"", "12.0.3", "11.9", "latest", "12.1garbage", "999999999999999999999.1"} {
		if err := s.ValidateEnvironment(func(key string) string {
			if key == "MARKLOGIC_VERSION" {
				return value
			}
			return "configured"
		}); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestEnvironmentDisablesInheritedGates(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	inherited := []string{"KUBECONFIG=/tmp/example", "INTEGRATION_CONTEXT=example"}
	for _, s := range catalog {
		inherited = append(inherited, s.Gate+"=true", s.Gate+"=1")
	}
	for _, selected := range append([]Scenario{{}}, catalog...) {
		env := Environment(catalog, inherited, selected.Name)
		for _, s := range catalog {
			count := 0
			for _, entry := range env {
				if strings.HasPrefix(entry, s.Gate+"=") {
					count++
					want := s.Gate + "=false"
					if s.Name == selected.Name {
						want = s.Gate + "=true"
					}
					if entry != want {
						t.Errorf("got %s, want %s", entry, want)
					}
				}
			}
			if count != 1 {
				t.Errorf("%s appears %d times", s.Gate, count)
			}
		}
		if env[0] != inherited[0] || env[1] != inherited[1] {
			t.Fatal("lost Kubernetes configuration")
		}
	}
}
