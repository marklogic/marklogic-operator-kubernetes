// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerSelectsOneScenarioAndRejectsInvalidPrerequisites(t *testing.T) {
	dir := t.TempDir()
	fakeGo := `#!/bin/sh
printf 'GO_ARGS:%s\n' "$*"
printf 'GATES:%s,%s,%s\n' "$MARKLOGIC_OAUTH_RESOURCE_SERVER" "$MARKLOGIC_OAUTH_AUTHORIZATION_CODE" "$MARKLOGIC_HAPROXY_SESSION_AFFINITY"
`
	for name, script := range map[string]string{"go": fakeGo, "kubectl": "#!/bin/sh\nexit 99\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, scenario, version string
		wantErr                 bool
		selection, gates        string
	}{
		{"resource server", "oauth-resource-server", "", false, "^TestOAuthResourceServerInfrastructure$", "true,false,false"},
		{"affinity", "haproxy-session-affinity", "", false, "^TestHAProxySessionIDAffinityContract$", "false,false,true"},
		{"authcode", "oauth-authorization-code", "12.1.0", false, "^TestOAuthAuthorizationCodeInfrastructure$", "false,true,false"},
		{"unsupported version", "oauth-authorization-code", "12.0.3", true, "", ""},
		{"undeclared version", "oauth-authorization-code", "", true, "", ""},
		{"unknown scenario", "typo", "", true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "../../scripts/run.sh")
			// Deliberately enable every inherited gate: the runner must clear them.
			cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "SCENARIO=" + tc.scenario, "INTEGRATION_CONTEXT=test-only", "INTEGRATION_OPERATOR_NAMESPACE=operator", "INTEGRATION_OPERATOR_DEPLOYMENT=operator", "MARKLOGIC_IMAGE=custom:image", "MARKLOGIC_VERSION=" + tc.version, "MARKLOGIC_OAUTH_RESOURCE_SERVER=true", "MARKLOGIC_OAUTH_AUTHORIZATION_CODE=true", "MARKLOGIC_HAPROXY_SESSION_AFFINITY=true"}
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v output=%s", err, output)
			}
			if tc.wantErr {
				if strings.Contains(string(output), "GO_ARGS:") {
					t.Fatal("runner executed tests despite invalid prerequisites")
				}
				return
			}
			if !strings.Contains(string(output), "-run "+tc.selection) || !strings.Contains(string(output), "GATES:"+tc.gates) {
				t.Fatalf("incorrect test selection: %s", output)
			}
		})
	}
}
