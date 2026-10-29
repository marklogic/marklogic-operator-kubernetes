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

func TestClusterConfigRejectsStaleStaticHostRegistration(t *testing.T) {
	t.Parallel()

	script, err := scriptsFolder.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatalf("read cluster configuration script: %v", err)
	}
	contents := string(script)
	for _, expected := range []string{
		`function local_host_uses_shared_security`,
		`function bootstrap_host_registration_status`,
		`if local_host_uses_shared_security; then`,
		`Refusing to create a duplicate static host registration`,
		`Refusing to reinitialize the static host and change its identity`,
		`Local shared Security authentication succeeded. Skipping host initialization.`,
	} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("cluster configuration script is missing %q", expected)
		}
	}

	nonBootstrapBranch := contents[strings.LastIndex(contents, `else `):]
	registrationCheck := strings.Index(nonBootstrapBranch, `response_code=$(bootstrap_host_registration_status "$HOST_FQDN")`)
	initialization := strings.Index(nonBootstrapBranch, `init_marklogic "$HOST_FQDN"`)
	if registrationCheck < 0 || initialization < 0 || registrationCheck > initialization {
		t.Fatal("non-bootstrap host registration must be checked before host initialization")
	}
}

func TestScriptsSelectCredentialsWithoutPodTemplateSwitch(t *testing.T) {
	t.Parallel()

	clusterConfig, err := scriptsFolder.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatalf("read cluster configuration script: %v", err)
	}
	clusterContents := string(clusterConfig)
	for _, expected := range []string{
		`MARKLOGIC_MANAGEMENT_USERNAME="marklogic-kubernetes-operator"`,
		`function activate_operator_credentials`,
		`local deadline=$((SECONDS + max_wait_seconds))`,
		`while ((SECONDS < deadline)); do`,
		`Dedicated operator credentials did not become active after`,
		`MARKLOGIC_MANAGEMENT_PASSWORD="$(< /run/secrets/ml-operator-secrets/password)"`,
		`activate_operator_credentials`,
		`"--anyauth" "--user" "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}"`,
	} {
		if !strings.Contains(clusterContents, expected) {
			t.Fatalf("cluster configuration script is missing %q", expected)
		}
	}
	if strings.Contains(clusterContents, `"--anyauth" "--user" "${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD}"`) {
		t.Fatal("cluster join must not use admin credentials")
	}
	for _, expectedSequence := range []string{
		"init_security_db\n        activate_operator_credentials\n        retry 5 configure_group",
		"wait_bootstrap_ready\n    activate_operator_credentials",
	} {
		if !strings.Contains(clusterContents, expectedSequence) {
			t.Fatalf("cluster configuration script is missing handoff sequence %q", expectedSequence)
		}
	}
	if strings.Contains(clusterContents, "MARKLOGIC_OPERATOR_CREDENTIALS_ACTIVE") {
		t.Fatal("cluster configuration must not depend on a pod-template credential switch")
	}

	prestopHook, err := scriptsFolder.ReadFile("scripts/prestop-hook.sh")
	if err != nil {
		t.Fatalf("read prestop hook: %v", err)
	}
	prestopContents := string(prestopHook)
	for _, expected := range []string{
		`if [[ -s /run/secrets/ml-operator-secrets/password ]]; then`,
		`res_code=$(request_shutdown "${MARKLOGIC_OPERATOR_USERNAME}" "${MARKLOGIC_OPERATOR_PASSWORD}")`,
	} {
		if !strings.Contains(prestopContents, expected) {
			t.Fatalf("prestop hook is missing %q", expected)
		}
	}
	if strings.Contains(prestopContents, "MARKLOGIC_OPERATOR_CREDENTIALS_ACTIVE") {
		t.Fatal("prestop hook must select credentials from mounted Secrets at runtime")
	}
	if strings.Contains(prestopContents, "MARKLOGIC_ADMIN_") {
		t.Fatal("prestop hook must not fall back to admin credentials")
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
