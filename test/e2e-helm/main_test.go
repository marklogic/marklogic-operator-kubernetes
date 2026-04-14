// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package e2ehelm contains end-to-end tests for the MarkLogic Operator installed via Helm
// in namespace-scoped mode (scope.type=namespace).
//
// Unlike the test/e2e package — which deploys the operator via `make deploy` (kustomize,
// cluster-scoped ClusterRole/ClusterRoleBinding) and then patches WATCH_NAMESPACE at
// runtime — this package installs the operator using the Helm chart with:
//
//	scope.type=namespace
//	scope.watchNamespaces=<watched namespaces>
//	metrics.secure=false
//
// This means the operator runs with only Role/RoleBinding per watched namespace and
// NO ClusterRole backstop, which is the only way to truly validate namespace-scoped RBAC.
package e2ehelm

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	e2eutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

// helmNS is the namespace the operator is installed into.
const helmNS = "marklogic-operator-system"

// helmRelease is the Helm release name used for install/uninstall.
const helmRelease = "marklogic-operator"

// helmChart is the path to the local chart (relative to repo root, where make runs).
const helmChart = "charts/marklogic-operator-kubernetes"

// watchedNamespaces is the comma-separated list of namespaces the operator watches.
// Every test namespace in this suite must appear here — the namespace-scoped operator
// only has a Role/RoleBinding in these namespaces (no ClusterRole backstop).
const watchedNamespaces = "ml-ns-test,ml-ns-ednode,ml-ns-tls,ml-ns-tls-named,ml-ns-tls-ednode,ml-ns-haproxy-path,ml-ns-haproxy,ml-ns-log"

var (
	testEnv        env.Environment
	dockerImage    = os.Getenv("E2E_DOCKER_IMAGE")
	marklogicImage = os.Getenv("E2E_MARKLOGIC_IMAGE_VERSION")

	// Shared credentials and container name used across all test files in this package.
	adminUsername   = "admin"
	adminPassword   = "Admin@8001"
	mlContainerName = "marklogic-server"
	replicas        = int32(1)
)

// namespaceLabels returns labels to apply to test namespaces.
// Istio ambient mode is not used in this suite, so no extra labels are needed.
func namespaceLabels() map[string]string {
	return nil
}

// ── Test summary tracker ──────────────────────────────────────────────────────

type testResult struct {
	name   string
	passed bool
}

var (
	trackMu      sync.Mutex
	trackedTests []testResult
)

// trackTest registers t in the global summary. Call it at the top of each Test* function.
// t.Cleanup runs after the test (and all its sub-tests) complete, so t.Failed() is final.
func trackTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		trackMu.Lock()
		trackedTests = append(trackedTests, testResult{name: t.Name(), passed: !t.Failed()})
		trackMu.Unlock()
	})
}

// printTestSummary logs a structured summary of all tracked tests to stdout.
func printTestSummary() {
	trackMu.Lock()
	results := make([]testResult, len(trackedTests))
	copy(results, trackedTests)
	trackMu.Unlock()

	total := len(results)
	passed := 0
	var failed []string
	for _, r := range results {
		if r.passed {
			passed++
		} else {
			failed = append(failed, r.name)
		}
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║               E2E-HELM TEST SUITE SUMMARY                   ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Total  : %-51d║\n", total)
	fmt.Printf("║  Passed : %-51d║\n", passed)
	fmt.Printf("║  Failed : %-51d║\n", len(failed))
	if len(failed) > 0 {
		fmt.Println("╠══════════════════════════════════════════════════════════════╣")
		fmt.Println("║  Failed tests:                                               ║")
		for _, name := range failed {
			// Truncate long names to fit the box
			if len(name) > 49 {
				name = name[:46] + "..."
			}
			fmt.Printf("║    ✗ %-57s║\n", name)
		}
	}
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

// logDiagnostics dumps pods, statefulsets, services, and MarklogicCluster resources
// for the given namespace, plus the last 50 lines of the operator pod logs.
// Call this immediately before a fatal pod-readiness assertion to get actionable context.
func logDiagnostics(t *testing.T, ns string) {
	t.Helper()
	for _, cmd := range []string{
		fmt.Sprintf("kubectl get pods -n %s -o wide", ns),
		fmt.Sprintf("kubectl get statefulsets -n %s", ns),
		fmt.Sprintf("kubectl get services -n %s", ns),
		fmt.Sprintf("kubectl get marklogicclusters -n %s", ns),
	} {
		p := e2eutils.RunCommand(cmd)
		t.Logf("$ %s\n%s", cmd, p.Result())
	}
	// Operator logs give context on reconciliation failures and CrashLoopBackOff.
	p := e2eutils.RunCommand(
		fmt.Sprintf("kubectl logs -n %s -l control-plane=controller-manager --tail=50 --prefix=true", helmNS),
	)
	t.Logf("Operator logs (last 50 lines):\n%s", p.Result())
}

func TestMain(m *testing.M) {
	path := conf.ResolveKubeConfigFile()
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		log.Fatalf("Failed to create config: %s", err)
	}
	cfg = cfg.WithKubeconfigFile(path)
	testEnv = env.NewWithConfig(cfg)

	log.Printf("e2e-helm: docker image: %s", dockerImage)
	log.Printf("e2e-helm: marklogic image: %s", marklogicImage)
	log.Printf("e2e-helm: watched namespaces: %s", watchedNamespaces)

	testEnv.Setup(
		// Ensure the operator namespace is clean before installing.
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Printf("Ensuring clean operator namespace: %s", helmNS)
			client := cfg.Client()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: helmNS}}
			if err := client.Resources().Get(ctx, helmNS, "", ns); err == nil {
				log.Printf("Deleting existing namespace %s", helmNS)
				_ = client.Resources().Delete(ctx, ns)
				for i := 0; i < 60; i++ {
					if err := client.Resources().Get(ctx, helmNS, "", ns); apierrors.IsNotFound(err) {
						log.Printf("Namespace %s deleted", helmNS)
						break
					} else if i == 59 {
						return ctx, fmt.Errorf("timeout waiting for namespace %s deletion", helmNS)
					}
					time.Sleep(1 * time.Second)
				}
			}
			return ctx, nil
		},
		envfuncs.CreateNamespace(helmNS),

		// Pre-create all watched namespaces so the Helm chart can create
		// Role/RoleBinding in them during install.
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			client := cfg.Client()
			for _, ns := range strings.Split(watchedNamespaces, ",") {
				if err := client.Resources().Create(ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: ns},
				}); err != nil && !apierrors.IsAlreadyExists(err) {
					return ctx, fmt.Errorf("failed to create watched namespace %s: %w", ns, err)
				}
				log.Printf("Watched namespace ready: %s", ns)
			}
			return ctx, nil
		},

		// Change working directory to the repo root (same pattern as test/e2e).
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			if err := os.Chdir("../.."); err != nil {
				return ctx, fmt.Errorf("failed to chdir to repo root: %w", err)
			}
			wd, _ := os.Getwd()
			log.Printf("Working directory: %s", wd)
			return ctx, nil
		},

		// Install the operator via Helm in namespace-scoped mode.
		// scope.type=namespace  → Role/RoleBinding per watched namespace (no ClusterRole)
		// metrics.secure=false  → HTTP :8080 (ClusterRole for TokenReview not available)
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Printf("Installing operator via Helm (scope=namespace, metrics.secure=false)...")

			args := []string{
				"upgrade", "--install", helmRelease, helmChart,
				"--namespace", helmNS,
				"--create-namespace",
				"--set", "scope.type=namespace",
				"--set", fmt.Sprintf("scope.watchNamespaces=%s", strings.ReplaceAll(watchedNamespaces, ",", "\\,")),
				"--set", "metrics.secure=false",
				"--wait",
				"--timeout", "3m",
			}
			if dockerImage != "" {
				// Reject shell metacharacters before incorporating the env var into args.
				if strings.ContainsAny(dockerImage, ";|&$`(){}[]<>") {
					return ctx, fmt.Errorf("E2E_DOCKER_IMAGE contains invalid characters: %s", dockerImage)
				}
				// Split on the last ':' that appears after the last '/' so that registry
				// ports (e.g. registry:5000/repo/image:tag) are handled correctly.
				repo := dockerImage
				tag := ""
				if idx := strings.LastIndex(dockerImage, ":"); idx > strings.LastIndex(dockerImage, "/") {
					repo = dockerImage[:idx]
					tag = dockerImage[idx+1:]
				}
				args = append(args, "--set", "controllerManager.manager.image.repository="+repo)
				if tag != "" {
					args = append(args, "--set", "controllerManager.manager.image.tag="+tag)
				}
				args = append(args, "--set", "controllerManager.manager.image.pullPolicy=Never")
			}

			// #nosec G204 — fixed args are package constants; E2E_DOCKER_IMAGE is validated above
			cmd := exec.Command("helm", args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return ctx, fmt.Errorf("helm install failed: %w", err)
			}
			log.Printf("Helm install succeeded")
			return ctx, nil
		},

		// Wait for the operator Deployment to be available.
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Waiting for operator deployment to be available...")
			client := cfg.Client()
			if err := wait.For(
				conditions.New(client.Resources()).DeploymentConditionMatch(
					&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
						Name:      "marklogic-operator-controller-manager",
						Namespace: helmNS,
					}},
					appsv1.DeploymentAvailable,
					corev1.ConditionTrue,
				),
				wait.WithTimeout(3*time.Minute),
				wait.WithInterval(5*time.Second),
			); err != nil {
				return ctx, fmt.Errorf("operator deployment not available: %w", err)
			}
			log.Println("Operator deployment is available")
			return ctx, nil
		},
	)

	testEnv.Finish(
		// Uninstall the Helm release.
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Printf("Uninstalling Helm release %s", helmRelease)
			// #nosec G204 — helmRelease and helmNS are package constants
			cmd := exec.Command("helm", "uninstall", helmRelease, "--namespace", helmNS, "--ignore-not-found")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				log.Printf("Warning: helm uninstall failed: %v", err)
			}
			return ctx, nil
		},
		envfuncs.DeleteNamespace(helmNS),

		// Delete all watched namespaces created during Setup.
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			client := cfg.Client()
			for _, ns := range strings.Split(watchedNamespaces, ",") {
				if err := client.Resources().Delete(ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: ns},
				}); err != nil && !apierrors.IsNotFound(err) {
					log.Printf("Warning: failed to delete watched namespace %s: %v", ns, err)
				}
			}
			return ctx, nil
		},
	)

	exitCode := testEnv.Run(m)
	printTestSummary()
	os.Exit(exitCode)
}
