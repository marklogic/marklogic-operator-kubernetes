//go:build upgradee2e

// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package upgradee2e

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	defaultSourceChart          = "marklogic-operator/marklogic-operator-kubernetes"
	defaultSourceVersion        = "1.2.0"
	defaultTargetChart          = "charts/marklogic-operator-kubernetes"
	defaultMarkLogicImage       = "progressofficial/marklogic-db:12.0.0-ubi9-rootless-2.2.2"
	defaultHelmTimeout          = "10m"
	defaultWorkloadWaitTimeout  = 25 * time.Minute
	defaultNamespaceWaitTimeout = 2 * time.Minute
	defaultClusterSuiteTimeout  = "60m"
	defaultNamespaceSuiteTimeout = "60m"
	defaultOperatorDeployment   = "marklogic-operator-controller-manager"

	cleanupProbeSleep = 30 * time.Second
)

var (
	operatorClusterRoles = []string{
		"marklogic-operator-manager-role",
		"marklogic-operator-metrics-auth-role",
		"marklogic-operator-metrics-reader",
	}

	operatorClusterRoleBindings = []string{
		"marklogic-operator-manager-rolebinding",
		"marklogic-operator-metrics-auth-rolebinding",
	}

	e2eHelmWatchedNamespaces = []string{
		"ml-ns-test",
		"ml-ns-ednode",
		"ml-ns-tls",
		"ml-ns-tls-named",
		"ml-ns-tls-ednode",
		"ml-ns-haproxy-path",
		"ml-ns-haproxy",
		"ml-ns-log",
		"ml-ns-resize-a",
		"ml-ns-resize-b",
	}

	e2eClusterCleanupNamespaces = []string{
		"ml-dynamic-host",
		"ml-cluster-test",
		"ednode",
		"tls-self-signed",
		"marklogic-tlsnamed",
		"marklogic-tlsednode",
		"haproxy-pathbased",
		"haproxy-test",
		"log-test",
		"ml-resize-a",
		"ml-resize-b",
		"loki",
		"grafana",
	}

	e2eHelmCleanupNamespaces = []string{
		"ml-ns-test",
		"ml-ns-ednode",
		"ml-ns-tls",
		"ml-ns-tls-named",
		"ml-ns-tls-ednode",
		"ml-ns-haproxy-path",
		"ml-ns-haproxy",
		"ml-ns-log",
		"ml-ns-resize-a",
		"ml-ns-resize-b",
	}

	marklogicCRDs = []string{
		"marklogicclusters.marklogic.progress.com",
		"marklogicgroups.marklogic.progress.com",
	}
)

type chartValues struct {
	ControllerManager struct {
		Manager struct {
			Image struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
			} `yaml:"image"`
		} `yaml:"manager"`
	} `yaml:"controllerManager"`
}

type scenarioState struct {
	name              string
	release           string
	operatorNamespace string
	namespaces        []string
}

type upgradeRunner struct {
	testingT              *testing.T
	repoRoot              string
	sourceChart           string
	sourceVersion         string
	targetChart           string
	targetImageRef        string
	targetImageRepository string
	targetImageTag        string
	targetImageFromChart  bool
	marklogicImage        string
	helmTimeout           string
	workloadWaitTimeout   time.Duration
	clusterSuiteTimeout   string
	namespaceSuiteTimeout string
	keepResources         bool
	skipHelmRepoUpdate    bool
	runID                 string
	current               scenarioState
}

func TestUpgradeClusterScope(t *testing.T) {
	runner := newUpgradeRunner(t)
	runner.runClusterUpgradeScenario()
}

func TestUpgradeNamespaceScope(t *testing.T) {
	runner := newUpgradeRunner(t)
	runner.runNamespaceUpgradeScenario()
}

func TestCleanupUpgradeResources(t *testing.T) {
	runner := newUpgradeRunner(t)
	runner.cleanupUpgradeTestResources()
}

func newUpgradeRunner(t *testing.T) *upgradeRunner {
	t.Helper()

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	runner := &upgradeRunner{
		testingT:              t,
		repoRoot:              repoRoot,
		sourceChart:           envOrDefault("E2E_UPGRADE_SOURCE_CHART", defaultSourceChart),
		sourceVersion:         envOrDefault("E2E_UPGRADE_SOURCE_VERSION", defaultSourceVersion),
		targetChart:           envOrDefault("E2E_UPGRADE_TARGET_CHART", defaultTargetChart),
		marklogicImage:        envOrDefault("E2E_MARKLOGIC_IMAGE_VERSION", defaultMarkLogicImage),
		helmTimeout:           envOrDefault("E2E_UPGRADE_HELM_TIMEOUT", defaultHelmTimeout),
		workloadWaitTimeout:   durationEnvOrDefault("E2E_UPGRADE_WORKLOAD_TIMEOUT", defaultWorkloadWaitTimeout),
		clusterSuiteTimeout:   envOrDefault("E2E_TEST_TIMEOUT", defaultClusterSuiteTimeout),
		namespaceSuiteTimeout: envOrDefault("E2E_HELM_TEST_TIMEOUT", defaultNamespaceSuiteTimeout),
		keepResources:         boolEnv("E2E_UPGRADE_KEEP_RESOURCES"),
		skipHelmRepoUpdate:    boolEnv("E2E_UPGRADE_SKIP_HELM_REPO_UPDATE"),
		runID:                 os.Getenv("E2E_UPGRADE_RUN_ID"),
	}

	runner.targetImageRef = envOrDefault("E2E_UPGRADE_TARGET_IMAGE", os.Getenv("E2E_DOCKER_IMAGE"))

	return runner
}

func (r *upgradeRunner) runClusterUpgradeScenario() {
	r.requireTools("kubectl", "helm")
	r.parseTargetImage()
	r.ensureRunID()
	r.ensureHelmRepo()
	r.verifyClusterIsClean()
	r.verifyChartInputs()
	r.verifyTargetImageAvailable()

	operatorNamespace := "mlop-upg-cluster-" + r.runID
	baselineNamespace := "ml-upg-base-" + r.runID
	postNamespace := "ml-upg-post-" + r.runID
	release := "mlop-upg-cluster-" + r.runID

	r.registerScenario("cluster", release, operatorNamespace, operatorNamespace, baselineNamespace, postNamespace)
	r.enableFailureDiagnosticsAndCleanup()

	r.testingT.Logf("Starting cluster-scope upgrade scenario with run-id %s", r.runID)
	r.createNamespace(operatorNamespace)
	r.createNamespace(baselineNamespace)
	r.createNamespace(postNamespace)
	r.adoptExistingCRDsForRelease(release, operatorNamespace)

	r.runCommand("helm", "upgrade", "--install", release, r.sourceChart,
		"--version", r.sourceVersion,
		"--namespace", operatorNamespace,
		"--create-namespace",
		"--wait",
		"--timeout", r.helmTimeout,
	)

	r.waitForOperatorReady(operatorNamespace)
	r.deployMarkLogicCluster(baselineNamespace, "ml-upgrade-baseline")

	r.runCommand("helm", r.buildTargetUpgradeArgs(release, operatorNamespace, "cluster", "")...)
	r.waitForOperatorReady(operatorNamespace)

	r.assertClusterResourcePresent("clusterrole", "marklogic-operator-manager-role")
	r.assertClusterResourcePresent("clusterrolebinding", "marklogic-operator-manager-rolebinding")

	argsString := r.deploymentArgs(operatorNamespace)
	r.assertContains(argsString, "--metrics-secure=true", "cluster-scope metrics must stay secure after upgrade")
	r.assertContains(argsString, "--metrics-bind-address=:8443", "cluster-scope metrics port must stay at 8443")
	r.assertNotContains(argsString, "--metrics-secure=false", "cluster-scope deployment args are incorrect")

	r.waitForPodReady(baselineNamespace, "node-0")
	r.deployMarkLogicCluster(postNamespace, "ml-upgrade-post")

	r.appendCurrentNamespaces(e2eClusterCleanupNamespaces...)
	r.runClusterE2ESuite()

	r.testingT.Log("Cluster-scope upgrade scenario passed")
}

func (r *upgradeRunner) runNamespaceUpgradeScenario() {
	r.requireTools("kubectl", "helm")
	r.parseTargetImage()
	r.ensureRunID()
	r.ensureHelmRepo()
	r.verifyClusterIsClean()
	r.verifyChartInputs()
	r.verifyTargetImageAvailable()

	operatorNamespace := "mlop-upg-ns-" + r.runID
	primaryWatchedNamespace := "ml-upg-watch-a-" + r.runID
	secondaryWatchedNamespace := "ml-upg-watch-b-" + r.runID
	unwatchedNamespace := "ml-upg-unwatched-" + r.runID
	release := "mlop-upg-ns-" + r.runID

	watchedNamespaces := []string{primaryWatchedNamespace, secondaryWatchedNamespace}
	watchedNamespaces = append(watchedNamespaces, e2eHelmWatchedNamespaces...)
	watchCSV := strings.Join(watchedNamespaces, ",")

	r.registerScenario("namespace", release, operatorNamespace, operatorNamespace, unwatchedNamespace)
	r.appendCurrentNamespaces(watchedNamespaces...)
	r.enableFailureDiagnosticsAndCleanup()

	r.testingT.Logf("Starting namespace-scope upgrade scenario with run-id %s", r.runID)
	r.createNamespace(operatorNamespace)
	for _, namespace := range watchedNamespaces {
		r.createNamespace(namespace)
	}
	r.createNamespace(unwatchedNamespace)
	r.adoptExistingCRDsForRelease(release, operatorNamespace)

	r.runCommand("helm", "upgrade", "--install", release, r.sourceChart,
		"--version", r.sourceVersion,
		"--namespace", operatorNamespace,
		"--create-namespace",
		"--wait",
		"--timeout", r.helmTimeout,
	)

	r.waitForOperatorReady(operatorNamespace)
	r.deployMarkLogicCluster(primaryWatchedNamespace, "ml-upgrade-baseline")

	r.runCommand("helm", r.buildTargetUpgradeArgs(release, operatorNamespace, "namespace", watchCSV)...)
	r.waitForOperatorReady(operatorNamespace)

	r.assertClusterResourceAbsent("clusterrole", "marklogic-operator-manager-role")
	r.assertClusterResourceAbsent("clusterrolebinding", "marklogic-operator-manager-rolebinding")
	r.assertClusterResourceAbsent("clusterrole", "marklogic-operator-metrics-auth-role")
	r.assertClusterResourceAbsent("clusterrolebinding", "marklogic-operator-metrics-auth-rolebinding")
	r.assertClusterResourceAbsent("clusterrole", "marklogic-operator-metrics-reader")

	r.assertNamespacedResourcePresent("role", "marklogic-operator-manager-role", primaryWatchedNamespace)
	r.assertNamespacedResourcePresent("rolebinding", "marklogic-operator-manager-rolebinding", primaryWatchedNamespace)
	r.assertNamespacedResourcePresent("role", "marklogic-operator-manager-role", secondaryWatchedNamespace)
	r.assertNamespacedResourcePresent("rolebinding", "marklogic-operator-manager-rolebinding", secondaryWatchedNamespace)
	r.assertNamespacedResourcePresent("role", "marklogic-operator-manager-role", operatorNamespace)
	r.assertNamespacedResourcePresent("rolebinding", "marklogic-operator-manager-rolebinding", operatorNamespace)

	argsString := r.deploymentArgs(operatorNamespace)
	envString := r.deploymentEnv(operatorNamespace)
	r.assertContains(argsString, "--metrics-secure=false", "namespace-scope metrics must switch to insecure mode")
	r.assertContains(argsString, "--metrics-bind-address=:8080", "namespace-scope metrics port must switch to 8080")
	r.assertContains(envString, "WATCH_NAMESPACE="+watchCSV, "namespace-scope WATCH_NAMESPACE is incorrect")

	r.waitForPodReady(primaryWatchedNamespace, "node-0")
	r.deployMarkLogicCluster(secondaryWatchedNamespace, "ml-upgrade-post")
	r.applyClusterManifest(unwatchedNamespace, "ml-upgrade-unwatched")
	time.Sleep(cleanupProbeSleep)
	if r.commandSucceeds("kubectl", "get", "statefulset", "node", "-n", unwatchedNamespace) {
		r.testingT.Fatalf("unwatched namespace %s should not be reconciled, but statefulset/node was created", unwatchedNamespace)
	}

	r.appendCurrentNamespaces(e2eHelmCleanupNamespaces...)
	r.runNamespaceE2ESuite(watchCSV)

	r.testingT.Log("Namespace-scope upgrade scenario passed")
}

func (r *upgradeRunner) cleanupUpgradeTestResources() {
	r.requireTools("kubectl", "helm")
	r.testingT.Logf("Cleaning upgrade-test resources for run-id filter %q", r.runID)
	found := false

	for _, release := range r.findUpgradeTestReleases() {
		found = true
		r.testingT.Logf("Removing Helm release %s from namespace %s", release.name, release.namespace)
		r.runCommandAllowFailure("helm", "uninstall", release.name, "--namespace", release.namespace, "--ignore-not-found")
	}

	r.cleanupClusterScopedRBAC()

	for _, namespace := range r.findUpgradeTestNamespaces() {
		found = true
		r.testingT.Logf("Deleting namespace %s", namespace)
		r.deleteNamespaceAndWait(namespace)
	}

	if !found {
		r.testingT.Log("No upgrade-test resources found")
	}
}

func (r *upgradeRunner) ensureRunID() {
	if strings.TrimSpace(r.runID) == "" {
		r.runID = time.Now().Format("0102150405")
	}
}

func (r *upgradeRunner) ensureHelmRepo() {
	if r.skipHelmRepoUpdate || !strings.HasPrefix(r.sourceChart, "marklogic-operator/") {
		return
	}

	r.runCommandAllowFailure("helm", "repo", "add", "marklogic-operator", "https://marklogic.github.io/marklogic-operator-kubernetes/")
	r.runCommand("helm", "repo", "update", "marklogic-operator")
}

func (r *upgradeRunner) verifyClusterIsClean() {
	deploymentNamespaces := strings.TrimSpace(r.runCommand("kubectl", "get", "deploy", "-A", "-o", `jsonpath={range .items[?(@.metadata.name=="marklogic-operator-controller-manager")]}{.metadata.namespace}{"\n"}{end}`))
	if deploymentNamespaces != "" {
		r.testingT.Fatalf("existing operator deployment found in namespace(s): %s", strings.ReplaceAll(deploymentNamespaces, "\n", ", "))
	}

	for _, name := range operatorClusterRoles {
		if r.commandSucceeds("kubectl", "get", "clusterrole", name) {
			r.testingT.Fatalf("existing ClusterRole %s found; use a clean cluster first", name)
		}
	}

	for _, name := range operatorClusterRoleBindings {
		if r.commandSucceeds("kubectl", "get", "clusterrolebinding", name) {
			r.testingT.Fatalf("existing ClusterRoleBinding %s found; use a clean cluster first", name)
		}
	}

	releases := strings.Fields(r.runCommand("helm", "list", "-A", "-q"))
	var existing []string
	for _, release := range releases {
		if strings.Contains(release, "marklogic-operator") || strings.Contains(release, "mlop-upg") {
			existing = append(existing, release)
		}
	}
	if len(existing) > 0 {
		r.testingT.Fatalf("existing Helm release(s) detected that look like operator installs: %s", strings.Join(existing, ", "))
	}
}

func (r *upgradeRunner) verifyChartInputs() {
	r.runCommand("kubectl", "cluster-info")
	r.runCommand("helm", "show", "chart", r.sourceChart, "--version", r.sourceVersion)
	r.runCommand("helm", "show", "chart", r.targetChart)
}

func (r *upgradeRunner) parseTargetImage() {
	if r.targetImageRef == "" {
		valuesOutput := r.runCommand("helm", "show", "values", r.targetChart)
		var values chartValues
		if err := yaml.Unmarshal([]byte(valuesOutput), &values); err != nil {
			r.testingT.Fatalf("parse target chart values: %v", err)
		}
		repository := strings.TrimSpace(values.ControllerManager.Manager.Image.Repository)
		tag := strings.TrimSpace(values.ControllerManager.Manager.Image.Tag)
		if repository == "" || tag == "" {
			r.testingT.Fatalf("unable to resolve controllerManager.manager.image.repository/tag from target chart values")
		}
		r.targetImageRepository = repository
		r.targetImageTag = tag
		r.targetImageRef = repository + ":" + tag
		r.targetImageFromChart = true
		return
	}

	separator := strings.LastIndex(r.targetImageRef, ":")
	if separator <= strings.LastIndex(r.targetImageRef, "/") {
		r.testingT.Fatalf("E2E_UPGRADE_TARGET_IMAGE must be repository:tag, got %q", r.targetImageRef)
	}

	r.targetImageRepository = r.targetImageRef[:separator]
	r.targetImageTag = r.targetImageRef[separator+1:]
	r.targetImageFromChart = false
}

func (r *upgradeRunner) verifyTargetImageAvailable() {
	r.requireTools("docker")

	if r.commandSucceeds("docker", "manifest", "inspect", r.targetImageRef) {
		return
	}

	if r.commandSucceeds("docker", "image", "inspect", r.targetImageRef) {
		return
	}

	if r.targetImageFromChart {
		r.testingT.Fatalf("target chart default image %s is not available in a registry or local Docker cache; build/load a local image and rerun with E2E_UPGRADE_TARGET_IMAGE or E2E_DOCKER_IMAGE set", r.targetImageRef)
	}

	r.testingT.Fatalf("target upgrade image %s is not available in a registry or local Docker cache; build/load the image first or rerun with a valid local image such as marklogic-operator-kubernetes:e2e-local", r.targetImageRef)
}

func (r *upgradeRunner) registerScenario(name, release, operatorNamespace string, namespaces ...string) {
	r.current = scenarioState{
		name:              name,
		release:           release,
		operatorNamespace: operatorNamespace,
		namespaces:        append([]string{}, namespaces...),
	}
}

func (r *upgradeRunner) appendCurrentNamespaces(namespaces ...string) {
	r.current.namespaces = append(r.current.namespaces, namespaces...)
}

func (r *upgradeRunner) enableFailureDiagnosticsAndCleanup() {
	r.testingT.Cleanup(func() {
		if r.testingT.Failed() && r.current.name != "" {
			r.collectDiagnostics()
		}
		if !r.keepResources && r.current.name != "" {
			r.cleanupCurrentScenario()
		}
	})
}

func (r *upgradeRunner) collectDiagnostics() {
	r.testingT.Logf("Collecting diagnostics for scenario %s", r.current.name)
	r.runCommandAllowFailure("kubectl", "get", "pods", "-A")
	r.runCommandAllowFailure("kubectl", "get", "clusterrole,clusterrolebinding")

	if r.current.operatorNamespace != "" {
		r.runCommandAllowFailure("kubectl", "get", "deploy,pods,svc", "-n", r.current.operatorNamespace)
		r.runCommandAllowFailure("kubectl", "logs", "deployment/"+defaultOperatorDeployment, "-n", r.current.operatorNamespace, "--tail=200")
	}

	for _, namespace := range r.current.namespaces {
		r.runCommandAllowFailure("kubectl", "get", "all", "-n", namespace)
		r.runCommandAllowFailure("kubectl", "get", "role,rolebinding", "-n", namespace)
		r.runCommandAllowFailure("kubectl", "get", "marklogicclusters,marklogicgroups", "-n", namespace, "-o", "yaml")
	}
}

func (r *upgradeRunner) cleanupCurrentScenario() {
	if r.current.release != "" && r.current.operatorNamespace != "" {
		r.testingT.Logf("Cleaning Helm release %s", r.current.release)
		r.runCommandAllowFailure("helm", "uninstall", r.current.release, "--namespace", r.current.operatorNamespace, "--ignore-not-found")
	}

	r.cleanupClusterScopedRBAC()

	for _, namespace := range r.current.namespaces {
		r.deleteNamespaceAndWait(namespace)
	}

	r.current = scenarioState{}
}

func (r *upgradeRunner) cleanupClusterScopedRBAC() {
	for _, name := range operatorClusterRoleBindings {
		r.runCommandAllowFailure("kubectl", "delete", "clusterrolebinding", name, "--ignore-not-found")
	}
	for _, name := range operatorClusterRoles {
		r.runCommandAllowFailure("kubectl", "delete", "clusterrole", name, "--ignore-not-found")
	}
}

func (r *upgradeRunner) adoptExistingCRDsForRelease(release, namespace string) {
	for _, crd := range marklogicCRDs {
		if !r.commandSucceeds("kubectl", "get", "crd", crd) {
			continue
		}

		r.runCommand("kubectl", "label", "crd", crd, "app.kubernetes.io/managed-by=Helm", "--overwrite")
		r.runCommand("kubectl", "annotate", "crd", crd,
			"meta.helm.sh/release-name="+release,
			"meta.helm.sh/release-namespace="+namespace,
			"--overwrite",
		)
	}
}

func (r *upgradeRunner) waitForOperatorReady(namespace string) {
	r.runCommand("kubectl", "rollout", "status", "deployment/"+defaultOperatorDeployment, "-n", namespace, "--timeout", r.helmTimeout)
}

func (r *upgradeRunner) waitForPodReady(namespace, podName string) {
	deadline := time.Now().Add(r.workloadWaitTimeout)
	for time.Now().Before(deadline) {
		if r.commandSucceeds("kubectl", "get", "pod", podName, "-n", namespace) {
			remaining := time.Until(deadline).Round(time.Second)
			if remaining < time.Second {
				remaining = time.Second
			}
			r.runCommand("kubectl", "wait", "--for=condition=Ready", "pod/"+podName, "-n", namespace, "--timeout", remaining.String())
			return
		}
		time.Sleep(5 * time.Second)
	}

	r.testingT.Fatalf("timed out waiting for pod %s in namespace %s", podName, namespace)
}

func (r *upgradeRunner) deployMarkLogicCluster(namespace, clusterName string) {
	r.applyClusterManifest(namespace, clusterName)
	r.waitForPodReady(namespace, "node-0")
}

func (r *upgradeRunner) deleteNamespaceAndWait(namespace string) {
	r.runCommandAllowFailure("kubectl", "delete", "namespace", namespace, "--ignore-not-found", "--wait=false")
	deadline := time.Now().Add(defaultNamespaceWaitTimeout)
	for time.Now().Before(deadline) {
		if !r.namespaceExists(namespace) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	r.testingT.Fatalf("timed out waiting for namespace %s to be deleted", namespace)
}

func (r *upgradeRunner) namespaceExists(namespace string) bool {
	cmd := exec.Command("kubectl", "get", "namespace", namespace, "--ignore-not-found", "-o", "name")
	cmd.Dir = r.repoRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.testingT.Logf("command failed while checking namespace %s existence: %s", namespace, strings.TrimSpace(string(out)))
		return true
	}
	return strings.TrimSpace(string(out)) != ""
}

func (r *upgradeRunner) applyClusterManifest(namespace, clusterName string) {
	manifest, err := os.CreateTemp("", "marklogic-upgrade-*.yaml")
	if err != nil {
		r.testingT.Fatalf("create temp manifest: %v", err)
	}
	defer os.Remove(manifest.Name())

	content := fmt.Sprintf(`apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  persistence:
    enabled: false
    size: 10Gi
  markLogicGroups:
  - name: node
    replicas: 1
    isBootstrap: true
`, clusterName, namespace, r.marklogicImage)

	if _, err := manifest.WriteString(content); err != nil {
		_ = manifest.Close()
		r.testingT.Fatalf("write temp manifest: %v", err)
	}
	if err := manifest.Close(); err != nil {
		r.testingT.Fatalf("close temp manifest: %v", err)
	}

	r.runCommand("kubectl", "apply", "-f", manifest.Name())
}

func (r *upgradeRunner) createNamespace(namespace string) {
	r.runCommandAllowFailure("kubectl", "create", "namespace", namespace)
}

func (r *upgradeRunner) runClusterE2ESuite() {
	env := map[string]string{
		"E2E_USE_EXISTING_OPERATOR":   "true",
		"E2E_OPERATOR_NAMESPACE":      r.current.operatorNamespace,
		"E2E_MARKLOGIC_IMAGE_VERSION": r.marklogicImage,
	}
	if r.targetImageRef != "" {
		env["E2E_DOCKER_IMAGE"] = r.targetImageRef
	}

	r.testingT.Log("Running cluster-scoped e2e suite against upgraded operator")
	r.runStreamingCommand(env, "go", "test", "-count=1", "-timeout", r.clusterSuiteTimeout, "-v", "./test/e2e")
}

func (r *upgradeRunner) runNamespaceE2ESuite(watchedNamespacesCSV string) {
	env := map[string]string{
		"E2E_USE_EXISTING_OPERATOR":   "true",
		"E2E_OPERATOR_NAMESPACE":      r.current.operatorNamespace,
		"E2E_OPERATOR_RELEASE":        r.current.release,
		"E2E_WATCHED_NAMESPACES":      watchedNamespacesCSV,
		"E2E_MARKLOGIC_IMAGE_VERSION": r.marklogicImage,
	}
	if r.targetImageRef != "" {
		env["E2E_DOCKER_IMAGE"] = r.targetImageRef
	}

	r.testingT.Log("Running namespace-scoped e2e suite against upgraded operator")
	r.runStreamingCommand(env, "go", "test", "-count=1", "-timeout", r.namespaceSuiteTimeout, "-v", "./test/e2e-helm")
}

func (r *upgradeRunner) buildTargetUpgradeArgs(release, operatorNamespace, scope, watchedNamespaces string) []string {
	args := []string{
		"upgrade",
		release,
		r.targetChart,
		"--namespace", operatorNamespace,
		"--wait",
		"--timeout", r.helmTimeout,
		"--set", "scope.type=" + scope,
	}

	if watchedNamespaces != "" {
		args = append(args, "--set-string", "scope.watchNamespaces="+watchedNamespaces)
	}

	if scope == "namespace" {
		args = append(args, "--set", "metrics.secure=false")
	} else {
		args = append(args, "--set", "metrics.secure=true")
	}

	if r.targetImageRepository != "" {
		args = append(args,
			"--set", "controllerManager.manager.image.repository="+r.targetImageRepository,
			"--set", "controllerManager.manager.image.tag="+r.targetImageTag,
		)
	}

	return args
}

func (r *upgradeRunner) assertClusterResourcePresent(kind, name string) {
	if !r.commandSucceeds("kubectl", "get", kind, name) {
		r.testingT.Fatalf("expected %s/%s to exist", kind, name)
	}
}

func (r *upgradeRunner) assertClusterResourceAbsent(kind, name string) {
	if r.commandSucceeds("kubectl", "get", kind, name) {
		r.testingT.Fatalf("expected %s/%s to be absent", kind, name)
	}
}

func (r *upgradeRunner) assertNamespacedResourcePresent(kind, name, namespace string) {
	if !r.commandSucceeds("kubectl", "get", kind, name, "-n", namespace) {
		r.testingT.Fatalf("expected %s/%s in namespace %s", kind, name, namespace)
	}
}

func (r *upgradeRunner) deploymentArgs(namespace string) string {
	return strings.TrimSpace(r.runCommand("kubectl", "get", "deployment", defaultOperatorDeployment, "-n", namespace, "-o", `jsonpath={.spec.template.spec.containers[0].args[*]}`))
}

func (r *upgradeRunner) deploymentEnv(namespace string) string {
	return strings.TrimSpace(r.runCommand("kubectl", "get", "deployment", defaultOperatorDeployment, "-n", namespace, "-o", `jsonpath={range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}`))
}

func (r *upgradeRunner) assertContains(value, expected, description string) {
	if !strings.Contains(value, expected) {
		r.testingT.Fatalf("%s. expected to find %q in %q", description, expected, value)
	}
}

func (r *upgradeRunner) assertNotContains(value, unexpected, description string) {
	if strings.Contains(value, unexpected) {
		r.testingT.Fatalf("%s. did not expect to find %q in %q", description, unexpected, value)
	}
}

func (r *upgradeRunner) findUpgradeTestReleases() []releaseRef {
	output := strings.TrimSpace(r.runCommandAllowFailure("helm", "list", "-A", "--no-headers"))
	if output == "" {
		return nil
	}

	var releases []releaseRef
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, namespace := fields[0], fields[1]
		if matchesUpgradeReleaseName(name, r.runID) {
			releases = append(releases, releaseRef{name: name, namespace: namespace})
		}
	}

	return releases
}

func (r *upgradeRunner) findUpgradeTestNamespaces() []string {
	output := strings.TrimSpace(r.runCommandAllowFailure("kubectl", "get", "ns", "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	if output == "" {
		return nil
	}

	var namespaces []string
	for _, namespace := range strings.Split(output, "\n") {
		if matchesUpgradeNamespace(namespace, r.runID) {
			namespaces = append(namespaces, namespace)
		}
	}

	return namespaces
}

type releaseRef struct {
	name      string
	namespace string
}

func matchesUpgradeReleaseName(name, runID string) bool {
	if !(strings.HasPrefix(name, "mlop-upg-cluster-") || strings.HasPrefix(name, "mlop-upg-ns-")) {
		return false
	}
	return runID == "" || strings.HasSuffix(name, "-"+runID)
}

func matchesUpgradeNamespace(namespace, runID string) bool {
	prefixes := []string{
		"mlop-upg-cluster-",
		"mlop-upg-ns-",
		"ml-upg-base-",
		"ml-upg-post-",
		"ml-upg-watch-a-",
		"ml-upg-watch-b-",
		"ml-upg-unwatched-",
	}
	matched := false
	for _, prefix := range prefixes {
		if strings.HasPrefix(namespace, prefix) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	return runID == "" || strings.HasSuffix(namespace, "-"+runID)
}

func (r *upgradeRunner) requireTools(names ...string) {
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			r.testingT.Fatalf("required tool not found: %s", name)
		}
	}
}

func (r *upgradeRunner) commandSucceeds(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Dir = r.repoRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		r.testingT.Logf("command failed (ignored): %s %s\n%s", name, strings.Join(args, " "), strings.TrimSpace(string(out)))
		return false
	}
	return true
}

func (r *upgradeRunner) runCommand(name string, args ...string) string {
	r.testingT.Helper()
	out, err := r.execute(nil, false, name, args...)
	if err != nil {
		r.testingT.Fatalf("command failed: %s %s\n%s", name, strings.Join(args, " "), strings.TrimSpace(out))
	}
	return out
}

func (r *upgradeRunner) runCommandAllowFailure(name string, args ...string) string {
	r.testingT.Helper()
	out, err := r.execute(nil, false, name, args...)
	if err != nil {
		r.testingT.Logf("command failed (ignored): %s %s\n%s", name, strings.Join(args, " "), strings.TrimSpace(out))
	}
	return out
}

func (r *upgradeRunner) runStreamingCommand(extraEnv map[string]string, name string, args ...string) {
	r.testingT.Helper()
	out, err := r.execute(extraEnv, true, name, args...)
	if err != nil {
		r.testingT.Fatalf("command failed: %s %s\n%s", name, strings.Join(args, " "), strings.TrimSpace(out))
	}
}

func (r *upgradeRunner) execute(extraEnv map[string]string, stream bool, name string, args ...string) (string, error) {
	r.testingT.Helper()
	r.testingT.Logf("running: %s %s", name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	cmd.Dir = r.repoRoot
	cmd.Env = mergeEnv(os.Environ(), extraEnv)

	if !stream {
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	var buffer bytes.Buffer
	multiWriter := io.MultiWriter(os.Stdout, &buffer)
	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter
	err := cmd.Run()
	return buffer.String(), err
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		candidate := filepath.Join(wd, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			return wd, nil
		}

		parent := filepath.Dir(wd)
		if parent == wd {
			return "", fmt.Errorf("could not find go.mod above %s", wd)
		}
		wd = parent
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func boolEnv(key string) bool {
	value := strings.TrimSpace(os.Getenv(key))
	return strings.EqualFold(value, "true") || value == "1" || strings.EqualFold(value, "yes")
}

func durationEnvOrDefault(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}

	result := append([]string{}, base...)
	for key, value := range overrides {
		prefix := key + "="
		replaced := false
		for i, entry := range result {
			if strings.HasPrefix(entry, prefix) {
				result[i] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, prefix+value)
		}
	}

	return result
}