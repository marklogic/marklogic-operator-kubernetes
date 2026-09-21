// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package scenarios contains the live integration scenario catalog. It has no
// Kubernetes dependencies and never connects to a cluster.
package scenarios

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

//go:embed catalog.json
var catalogJSON []byte

type Scenario struct {
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Package             string   `json:"package"`
	Test                string   `json:"test"`
	Gate                string   `json:"gate"`
	RequiredEnv         []string `json:"requiredEnv"`
	MinMarkLogicVersion string   `json:"minMarkLogicVersion,omitempty"`
	Prerequisites       []string `json:"prerequisites"`
}

func Load() ([]Scenario, error) { return decode(catalogJSON) }

func decode(data []byte) ([]Scenario, error) {
	var catalog []Scenario
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("scenario catalog: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("scenario catalog must contain one JSON array")
	}
	if err := validate(catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,49}$`)
	testPattern    = regexp.MustCompile(`^Test[A-Z][A-Za-z0-9_]*$`)
	envPattern     = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	packagePattern = regexp.MustCompile(`^\./test/integration/[a-zA-Z0-9_/-]+$`)
	versionPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(\.[0-9]+)?([-+][A-Za-z0-9.-]+)?$`)
	minimumPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

func validate(catalog []Scenario) error {
	if len(catalog) == 0 {
		return fmt.Errorf("scenario catalog is empty")
	}
	names, gates, selections := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range catalog {
		invalid := func(reason string) error { return fmt.Errorf("scenario %q: %s", s.Name, reason) }
		if !namePattern.MatchString(s.Name) || strings.TrimSpace(s.Description) == "" {
			return invalid("requires a lowercase scenario name and description")
		}
		if !packagePattern.MatchString(s.Package) || "./"+path.Clean(s.Package) != s.Package || !testPattern.MatchString(s.Test) {
			return invalid("requires one local integration package and one exact top-level Test name (no patterns)")
		}
		if !envPattern.MatchString(s.Gate) || !(strings.HasPrefix(s.Gate, "INTEGRATION_") || strings.HasPrefix(s.Gate, "MARKLOGIC_")) {
			return invalid("gate must be an INTEGRATION_ or MARKLOGIC_ environment variable")
		}
		switch s.Gate {
		case "INTEGRATION_TIMEOUT", "INTEGRATION_RESULTS_DIR", "INTEGRATION_RETAIN_NAMESPACE", "INTEGRATION_STORAGE_CLASS", "MARKLOGIC_OAUTH_RETAIN_NAMESPACE":
			return invalid("gate collides with shared run configuration")
		}
		if names[s.Name] || gates[s.Gate] || selections[s.Package+"/"+s.Test] {
			return invalid("duplicate name, gate, or package/test selection")
		}
		names[s.Name], gates[s.Gate], selections[s.Package+"/"+s.Test] = true, true, true
		required := map[string]bool{}
		for _, key := range s.RequiredEnv {
			if !envPattern.MatchString(key) || required[key] {
				return invalid("invalid or duplicate requiredEnv entry")
			}
			required[key] = true
		}
		if !required["INTEGRATION_CONTEXT"] || len(s.Prerequisites) == 0 {
			return invalid("requires INTEGRATION_CONTEXT and documented prerequisites")
		}
		for _, prerequisite := range s.Prerequisites {
			if strings.TrimSpace(prerequisite) == "" {
				return invalid("empty prerequisite")
			}
		}
		if s.MinMarkLogicVersion != "" {
			if _, _, err := version(s.MinMarkLogicVersion); err != nil || !minimumPattern.MatchString(s.MinMarkLogicVersion) || !required["MARKLOGIC_VERSION"] {
				return invalid("minMarkLogicVersion requires major.minor and MARKLOGIC_VERSION in requiredEnv")
			}
		}
	}
	// A gate must never overwrite configuration (including another scenario's).
	for _, s := range catalog {
		for _, key := range s.RequiredEnv {
			if gates[key] {
				return fmt.Errorf("gate %s collides with required configuration", key)
			}
		}
	}
	return nil
}

func (s Scenario) ValidateEnvironment(getenv func(string) string) error {
	for _, key := range s.RequiredEnv {
		if strings.TrimSpace(getenv(key)) == "" {
			return fmt.Errorf("set %s for scenario %s", key, s.Name)
		}
	}
	if s.MinMarkLogicVersion != "" {
		major, minor, err := version(getenv("MARKLOGIC_VERSION"))
		if err != nil {
			return fmt.Errorf("MARKLOGIC_VERSION: %w", err)
		}
		minMajor, minMinor, _ := version(s.MinMarkLogicVersion)
		if major < minMajor || (major == minMajor && minor < minMinor) {
			return fmt.Errorf("scenario %s requires MarkLogic %s+", s.Name, s.MinMarkLogicVersion)
		}
	}
	return nil
}

func version(value string) (uint64, uint64, error) {
	parts := versionPattern.FindStringSubmatch(value)
	if parts == nil {
		return 0, 0, fmt.Errorf("must be a numeric major.minor[.patch] version")
	}
	major, errMajor := strconv.ParseUint(parts[1], 10, 64)
	minor, errMinor := strconv.ParseUint(parts[2], 10, 64)
	if errMajor != nil || errMinor != nil {
		return 0, 0, fmt.Errorf("version number is too large")
	}
	return major, minor, nil
}

// Environment clears every registered gate, then optionally enables one. Other
// configuration, including KUBECONFIG, is passed through unchanged.
func Environment(catalog []Scenario, inherited []string, selected string) []string {
	gates := map[string]bool{}
	for _, s := range catalog {
		gates[s.Gate] = true
	}
	env := make([]string, 0, len(inherited)+len(gates))
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if !gates[key] {
			env = append(env, entry)
		}
	}
	for _, s := range catalog {
		env = append(env, s.Gate+"="+strconv.FormatBool(s.Name == selected))
	}
	return env
}
