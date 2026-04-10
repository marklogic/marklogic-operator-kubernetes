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
				"--set", fmt.Sprintf("scope.watchNamespaces=%s", watchedNamespaces),
				"--set", "metrics.secure=false",
				"--wait",
				"--timeout", "3m",
			}
			if dockerImage != "" {
				// IMAGE is of the form repo/name:tag — split into repository and tag.
				parts := strings.SplitN(dockerImage, ":", 2)
				args = append(args, "--set", "controllerManager.manager.image.repository="+parts[0])
				if len(parts) == 2 {
					args = append(args, "--set", "controllerManager.manager.image.tag="+parts[1])
				}
				args = append(args, "--set", "controllerManager.manager.image.pullPolicy=Never")
			}

			// #nosec G204 — args are constructed from known constants and validated env vars
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
	)

	os.Exit(testEnv.Run(m))
}
