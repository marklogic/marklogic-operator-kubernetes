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
		`manage_tls_options=(--cacert /run/secrets/marklogic-bootstrap-certs/cacert.pem)`,
		`200|202|204)`,
		`retry 5 configure_app_server_basic_auth "$app_server"`,
	} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("cluster configuration script is missing %q", expected)
		}
	}
}

func TestClusterConfigPersistsStaticHostJoinForRestartRecovery(t *testing.T) {
	t.Parallel()

	script, err := scriptsFolder.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatalf("read cluster configuration script: %v", err)
	}
	contents := string(script)
	for _, expected := range []string{
		`function local_host_uses_shared_security`,
		`function wait_for_local_shared_security`,
		`function bootstrap_host_registration_status`,
		`if local_host_uses_shared_security; then`,
		`if wait_for_local_shared_security; then`,
		`local retry_count=12`,
		`local cluster_config_file="$ML_KUBERNETES_FILE_PATH/cluster-config.zip"`,
		`Resuming the interrupted join with the persisted cluster configuration.`,
		`Skipping host initialization; join recovery will use persisted cluster configuration.`,
		`Refusing duplicate registration; remove the stale host registration before retrying.`,
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

	joinBranch := contents[strings.Index(contents, `function join_cluster`):strings.Index(contents, `function configure_group`)]
	persistConfig := strings.Index(joinBranch, `"-o" "$cluster_config_file"`)
	applyConfig := strings.Index(joinBranch, `"--data-binary" "@${cluster_config_file}"`)
	removeConfig := strings.LastIndex(joinBranch, `rm -f "$cluster_config_file"`)
	if persistConfig < 0 || applyConfig < 0 || removeConfig < 0 || persistConfig > applyConfig || applyConfig > removeConfig {
		t.Fatal("static host join must persist, apply, and remove cluster configuration in order")
	}
}

func TestClusterConfigWaitsForBootstrapBeforeInitializingJoiningHosts(t *testing.T) {
	t.Parallel()

	script, err := scriptsFolder.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatalf("read cluster configuration script: %v", err)
	}
	contents := string(script)
	mainStart := strings.Index(contents, `# Only the designated bootstrap group's first host`)
	mainEnd := strings.Index(contents, `if [[ $MARKLOGIC_JOIN_TLS_ENABLED == "true" ]]`)
	if mainStart < 0 || mainEnd < 0 || mainStart >= mainEnd {
		t.Fatal("cluster configuration script is missing its bootstrap decision block")
	}
	main := contents[mainStart:mainEnd]
	if !strings.Contains(main, `if [[ "$IS_BOOTSTRAP_HOST" == "true" ]] && [[ "$MARKLOGIC_CLUSTER_TYPE" == "bootstrap" ]]; then`) {
		t.Fatal("cluster Security initialization must be restricted to the designated bootstrap host")
	}

	joiningStart := strings.Index(main, "else \n    check_status_file_for_nonbootstrap")
	if joiningStart < 0 {
		t.Fatal("cluster configuration script is missing the joining-host branch")
	}
	joining := main[joiningStart:]
	waitForBootstrap := strings.Index(joining, `wait_bootstrap_ready`)
	initializeHost := strings.Index(joining, `init_marklogic "$HOST_FQDN"`)
	configureGroup := strings.Index(joining, `retry 5 configure_group`)
	joinCluster := strings.Index(joining, `join_cluster $HOST_FQDN`)
	if waitForBootstrap < 0 || initializeHost < 0 || waitForBootstrap > initializeHost {
		t.Fatal("joining hosts must wait for the designated bootstrap host before initialization")
	}
	if configureGroup < 0 || joinCluster < 0 || configureGroup > joinCluster {
		t.Fatal("a joining group's first host must configure its group before joining")
	}
}

func TestClusterConfigUsesInsertedNamedCertificate(t *testing.T) {
	t.Parallel()

	script, err := scriptsFolder.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatalf("read cluster configuration script: %v", err)
	}
	contents := string(script)
	expected := `if [[ "$certType" == "self-signed" ]]; then
                log "Info:  Generating Temporary CA Certificate"`
	if !strings.Contains(contents, expected) {
		t.Fatal("template CA generation must be limited to self-signed certificate mode")
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
		"init_security_db\n    configure_bootstrap_tls_before_operator_handoff\n    activate_operator_credentials\n    retry 5 configure_group",
		"wait_bootstrap_ready\n    activate_operator_credentials",
	} {
		if !strings.Contains(clusterContents, expectedSequence) {
			t.Fatalf("cluster configuration script is missing handoff sequence %q", expectedSequence)
		}
	}
	for _, expected := range []string{
		`username=${3:-$MARKLOGIC_ADMIN_USERNAME}`,
		`password=${4:-$MARKLOGIC_ADMIN_PASSWORD}`,
		`BOOTSTRAP_HTTPS_OPTION="--cacert /run/secrets/marklogic-bootstrap-certs/cacert.pem"`,
		`function configure_bootstrap_tls_before_operator_handoff`,
		`MARKLOGIC_MANAGEMENT_USERNAME="$MARKLOGIC_ADMIN_USERNAME"`,
		`function current_host_group`,
		`tls_group=$(current_host_group)`,
		`group-id=${tls_group}`,
		`Bootstrap TLS verification failed before operator credential handoff`,
	} {
		if !strings.Contains(clusterContents, expected) {
			t.Fatalf("cluster configuration script is missing %q", expected)
		}
	}
	tlsFunctionEnd := strings.Index(clusterContents, `function configure_app_server_basic_auth`)
	tlsCleanup := strings.Index(clusterContents, "rm -f /run/secrets/marklogic-certs/*.key\n}")
	preHandoffFunction := strings.Index(clusterContents, `function configure_bootstrap_tls_before_operator_handoff`)
	if preHandoffFunction < 0 || tlsCleanup < 0 || tlsFunctionEnd < 0 || preHandoffFunction < tlsCleanup || preHandoffFunction > tlsFunctionEnd {
		t.Fatal("bootstrap pre-handoff TLS helper must be defined after configure_tls and before its first call")
	}
	tlsFunction := clusterContents[strings.Index(clusterContents, `function configure_tls`):preHandoffFunction]
	if strings.Contains(tlsFunction, `group-id=${MARKLOGIC_GROUP}`) || !strings.Contains(tlsFunction, `group-id=${tls_group}`) {
		t.Fatal("configure_tls must target the host's resolved current group")
	}
	preHandoffBody := clusterContents[preHandoffFunction:tlsFunctionEnd]
	verification := strings.Index(preHandoffBody, `Bootstrap TLS verification failed before operator credential handoff`)
	markCurrent := strings.Index(preHandoffBody, `TLS_CONFIGURATION_CURRENT=true`)
	if verification < 0 || markCurrent < verification {
		t.Fatal("bootstrap TLS must be verified before it is marked current")
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
