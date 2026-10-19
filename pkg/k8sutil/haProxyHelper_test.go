// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

func TestGenerateFrontendConfigExcludesQueryConsoleFromHeaderRewrite(t *testing.T) {
	cr := &marklogicv1.MarklogicCluster{
		Spec: marklogicv1.MarklogicClusterSpec{
			HAProxy: &marklogicv1.HAProxy{FrontendPort: 8443},
		},
	}
	config := &HAProxyConfig{
		IsPathBased: true,
		BackendConfigMap: map[string][]BackendConfig{
			"8000-console-path": {
				{Port: 8000, TargetPort: 8000, Path: "/console", IsPathBased: true},
			},
		},
		FrontEndConfigMap: map[string]FrontEndConfig{},
	}

	generated := generateFrontendConfig(cr, config)
	expectedLines := []string{
		"bind :8443",
		"acl is_console path /console",
		"acl is_console path_beg /console/",
		"http-request set-header Host marklogic:8443 unless is_console",
		"http-request set-header Referer http://marklogic:8443 unless is_console",
	}
	for _, expected := range expectedLines {
		if !strings.Contains(generated, expected) {
			t.Errorf("expected generated frontend to contain %q, got:\n%s", expected, generated)
		}
	}
}
