// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"encoding/xml"
	"os"
	"path/filepath"
)

type junitFailure struct {
	Message string `xml:"message,attr"`
}
type junitCase struct {
	Name    string        `xml:"name,attr"`
	Class   string        `xml:"classname,attr"`
	Failure *junitFailure `xml:"failure,omitempty"`
	Skipped *struct{}     `xml:"skipped,omitempty"`
}
type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

func (r *runReport) writeJUnit() error {
	suite := junitSuite{Name: r.result.Scenario}
	add := func(name, outcome, message string) {
		entry := junitCase{Name: name, Class: r.result.Scenario}
		switch outcome {
		case "failed", "prerequisites_unmet":
			entry.Failure = &junitFailure{Message: message}
			suite.Failures++
		case "skipped":
			entry.Skipped = &struct{}{}
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, entry)
		suite.Tests++
	}
	for _, result := range r.result.Cases {
		add(result.Name, result.Outcome, "See run.json and diagnostics.json for the failing scenario stage")
	}
	if len(suite.Cases) == 0 || r.result.Cleanup == "failed" || (r.result.Outcome != "passed" && r.result.Outcome != "skipped" && suite.Failures == 0) {
		add(r.result.Test+"/lifecycle", r.result.Outcome, "Scenario stage: "+r.result.FailureStage+"; cleanup: "+r.result.Cleanup)
	}
	contents, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, "junit.xml"), append([]byte(xml.Header), contents...), 0600)
}
