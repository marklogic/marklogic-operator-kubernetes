package k8sutil

import (
	"strings"
	"testing"
)

func TestClusterConfigSetsBasicAuthForPathBasedAppServers(t *testing.T) {
	t.Parallel()

	script, err := scriptsFolder.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatalf("read cluster configuration script: %v", err)
	}
	contents := string(script)
	for _, expected := range []string{
		`app_services_authentication='"authentication":"basic", '`,
		`-d "${app_services_payload}"`,
		`function configure_app_server_basic_auth`,
		`manage_tls_options=(--cacert /run/secrets/marklogic-certs/cacert.pem)`,
		`200|202|204)`,
		`retry 5 configure_app_server_basic_auth "$app_server"`,
	} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("cluster configuration script is missing %q", expected)
		}
	}
}

func TestNormalizeYAMLIndentationPreservesNestedOutputProcessors(t *testing.T) {
	t.Parallel()

	input := `- name: gelf
  match: kube.marklogic.logs.error
  processors:
    logs:
      - name: modify
        match: "*"
        add:
          - Tenant why`

	want := `    - name: gelf
      match: kube.marklogic.logs.error
      processors:
        logs:
          - name: modify
            match: "*"
            add:
              - Tenant why`

	got := normalizeYAMLIndentation(input, 4, 6)
	if got != want {
		t.Fatalf("normalized YAML mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}
