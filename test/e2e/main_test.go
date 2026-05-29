// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/utils"
)

var (
	testEnv        env.Environment
	dockerImage    = os.Getenv("E2E_DOCKER_IMAGE")
	kustomizeVer   = os.Getenv("E2E_KUSTOMIZE_VERSION")
	ctrlgenVer     = os.Getenv("E2E_CONTROLLER_TOOLS_VERSION")
	marklogicImage = os.Getenv("E2E_MARKLOGIC_IMAGE_VERSION")
	kubernetesVer  = os.Getenv("E2E_KUBERNETES_VERSION")
)

const (
	namespace = "marklogic-operator-system"

	// Keep CRD cleanup fast in local/CI e2e loops: do a short grace wait first,
	// then escalate quickly to finalizer cleanup when termination is stuck.
	crdCleanupInitialWaitAttempts      = 8
	crdCleanupInitialWaitInterval      = 1 * time.Second
	crdCleanupPostFinalizeWaitAttempts = 8
	crdCleanupPostFinalizeWaitInterval = 1 * time.Second
)

// namespaceLabels returns the labels to apply to test namespaces.
// When Istio ambient mode is enabled, includes the ambient dataplane label.
func namespaceLabels() map[string]string {
	if isIstioAmbientEnabled() {
		return map[string]string{
			"istio.io/dataplane-mode": "ambient",
		}
	}
	return nil
}

// ── Test summary tracker ──────────────────────────────────────────────────────

type testResult struct {
	name    string
	passed  bool
	skipped bool
}

var (
	trackMu      sync.Mutex
	trackedTests []testResult
)

// trackTest registers t in the global summary. Call it at the top of each Test* function.
// t.Cleanup runs after the test (and all its sub-tests) complete, so t.Failed()/t.Skipped() are final.
func trackTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		trackMu.Lock()
		trackedTests = append(trackedTests, testResult{
			name:    t.Name(),
			passed:  !t.Failed() && !t.Skipped(),
			skipped: t.Skipped(),
		})
		trackMu.Unlock()
	})
}

// printTestSummary logs a structured pass/fail banner of all tracked tests to stdout.
func printTestSummary() {
	trackMu.Lock()
	results := make([]testResult, len(trackedTests))
	copy(results, trackedTests)
	trackMu.Unlock()

	total := len(results)
	passed := 0
	var failed []string
	var skipped []string
	for _, r := range results {
		if r.skipped {
			skipped = append(skipped, r.name)
		} else if r.passed {
			passed++
		} else {
			failed = append(failed, r.name)
		}
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              E2E (CLUSTER-SCOPED) TEST SUITE SUMMARY         ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Total   : %-50d║\n", total)
	fmt.Printf("║  Passed  : %-50d║\n", passed)
	fmt.Printf("║  Skipped : %-50d║\n", len(skipped))
	fmt.Printf("║  Failed  : %-50d║\n", len(failed))
	if len(failed) > 0 {
		fmt.Println("╠══════════════════════════════════════════════════════════════╣")
		fmt.Println("║  Failed tests:                                               ║")
		for _, name := range failed {
			if len(name) > 49 {
				name = name[:46] + "..."
			}
			fmt.Printf("║    ✗ %-57s║\n", name)
		}
	}
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func TestMain(m *testing.M) {
	testEnv = env.New()
	path := conf.ResolveKubeConfigFile()
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		log.Fatalf("Failed to create config: %s", err)
	}
	cfg = cfg.WithKubeconfigFile(path)

	testEnv = env.NewWithConfig(cfg)

	log.Printf("Running tests with the following configurations: path=%s", path)

	log.Printf("Docker image: %s", dockerImage)
	log.Printf("Kustomize version: %s", kustomizeVer)
	log.Printf("Controller-gen version: %s", ctrlgenVer)
	log.Printf("MarkLogic image: %s", marklogicImage)
	log.Printf("Kubernetes version: %s", kubernetesVer)
	log.Printf("Istio ambient mode: %v", isIstioAmbientEnabled())

	// Use Environment.Setup to configure pre-test setup
	testEnv.Setup(
		// Delete namespace if it exists from previous run
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Printf("Ensuring clean namespace: %s", namespace)
			client := cfg.Client()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}

			// Try to get the namespace first
			err := client.Resources().Get(ctx, namespace, "", ns)
			if err == nil {
				// Namespace exists, delete it
				log.Printf("Deleting existing namespace: %s", namespace)
				if err := client.Resources().Delete(ctx, ns); err != nil {
					log.Printf("Error deleting namespace (may already be deleting): %v", err)
				}

				// Wait for namespace to be fully deleted (up to 60 seconds)
				log.Printf("Waiting for namespace deletion to complete...")
				for i := 0; i < 60; i++ {
					err := client.Resources().Get(ctx, namespace, "", ns)
					if err != nil {
						if apierrors.IsNotFound(err) {
							// Namespace is gone
							log.Printf("Namespace deleted successfully")
							break
						}
						// Other error - propagate it
						return ctx, fmt.Errorf("error checking namespace deletion status: %w", err)
					}
					if i == 59 {
						log.Printf("Namespace %s still deleting after initial wait; forcing namespace finalizer cleanup", namespace)
						if err := forceFinalizeNamespace(namespace); err != nil {
							return ctx, fmt.Errorf("timeout waiting for namespace %s to be deleted; force-finalize failed: %w", namespace, err)
						}
						if err := waitForNamespaceDeletionByName(ctx, client, namespace, 90*time.Second); err != nil {
							return ctx, err
						}
						log.Printf("Namespace %s force-deleted successfully", namespace)
						break
					}
					time.Sleep(1 * time.Second)
				}
			} else if apierrors.IsNotFound(err) {
				// Namespace does not exist, nothing to clean up
				log.Printf("Namespace does not exist, will create fresh")
			} else {
				// Other error - propagate it
				return ctx, fmt.Errorf("error checking if namespace exists: %w", err)
			}

			return ctx, nil
		},
		envfuncs.CreateNamespace(namespace),

		// When Istio ambient mode is enabled, label the operator namespace
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			if !isIstioAmbientEnabled() {
				return ctx, nil
			}
			log.Println("Istio ambient mode enabled: labeling operator namespace with istio.io/dataplane-mode=ambient")
			client := cfg.Client()

			// Patch the operator namespace to add the ambient label
			operatorNs := &corev1.Namespace{}
			if err := client.Resources().Get(ctx, namespace, "", operatorNs); err != nil {
				return ctx, fmt.Errorf("failed to get operator namespace: %w", err)
			}

			// Use Patch to avoid resourceVersion conflicts with other controllers
			patchData := []byte(`{"metadata":{"labels":{"istio.io/dataplane-mode":"ambient"}}}`)
			if err := client.Resources().Patch(ctx, operatorNs, k8s.Patch{PatchType: types.StrategicMergePatchType, Data: patchData}); err != nil {
				return ctx, fmt.Errorf("failed to label operator namespace: %w", err)
			}
			log.Printf("Labeled namespace %s with istio.io/dataplane-mode=ambient", namespace)

			return ctx, nil
		},

		// install tool dependencies
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Installing bin tools...")

			// change dir for Make file or it will fail
			if err := os.Chdir("../.."); err != nil {
				log.Printf("Unable to set working directory: %s", err)
				return ctx, err
			}
			wd, _ := os.Getwd()
			gobin := wd + "/bin"
			os.Setenv("GOBIN", gobin)
			os.Setenv("PATH", os.Getenv("PATH")+":"+gobin)

			resolveExistingKustomize := func() (string, error) {
				if kustomizeCmd, lookErr := exec.LookPath("kustomize"); lookErr == nil {
					return kustomizeCmd, nil
				}
				if kubectlCmd, lookErr := exec.LookPath("kubectl"); lookErr == nil {
					return kubectlCmd + " kustomize", nil
				}
				return "", errors.New("kustomize not found in PATH")
			}

			// Only download kustomize if it is not already present in bin/
			kustomizePath := gobin + "/kustomize"
			if _, err := os.Stat(kustomizePath); os.IsNotExist(err) {
				if p := utils.RunCommand(fmt.Sprintf("go install sigs.k8s.io/kustomize/kustomize/v5@%s", kustomizeVer)); p.Err() != nil {
					fallbackKustomize, fallbackErr := resolveExistingKustomize()
					if fallbackErr != nil {
						log.Printf("Failed to install kustomize binary: %s: %s", p.Err(), p.Result())
						return ctx, p.Err()
					}
					log.Printf("Failed to install kustomize binary (%s); falling back to existing command: %s", p.Err(), fallbackKustomize)
					os.Setenv("KUSTOMIZE", fallbackKustomize)
				}
				if _, statErr := os.Stat(kustomizePath); statErr == nil {
					os.Setenv("KUSTOMIZE", kustomizePath)
				}
			} else {
				log.Printf("kustomize already present at %s, skipping install", kustomizePath)
				os.Setenv("KUSTOMIZE", kustomizePath)
			}

			// Only download controller-gen if it is not already present in bin/
			ctrlgenPath := gobin + "/controller-gen"
			if _, err := os.Stat(ctrlgenPath); os.IsNotExist(err) {
				if p := utils.RunCommand(fmt.Sprintf("go install sigs.k8s.io/controller-tools/cmd/controller-gen@%s", ctrlgenVer)); p.Err() != nil {
					log.Printf("Failed to install controller-gen binary: %s: %s", p.Err(), p.Result())
					return ctx, p.Err()
				}
			} else {
				log.Printf("controller-gen already present at %s, skipping install", ctrlgenPath)
			}

			p := utils.RunCommand("kustomize version")
			if p.Err() != nil {
				// If kustomize is unavailable but KUSTOMIZE was set to "kubectl kustomize",
				// report it clearly for deploy diagnostics.
				if customKustomize := os.Getenv("KUSTOMIZE"); customKustomize != "" {
					log.Printf("kustomize version command failed (%s); KUSTOMIZE override is set to: %s", p.Err(), customKustomize)
				}
			}
			log.Printf("Kustomize version: %s", p.Result())
			return ctx, nil
		},

		// generate and deploy resource configurations
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Building source components...")

			log.Println("Cleaning stale MarkLogic custom resources before deploy...")
			if err := forceDeleteMarkLogicCustomResources(); err != nil {
				return ctx, fmt.Errorf("failed to clean stale MarkLogic custom resources before deploy: %w", err)
			}

			c := utils.RunCommand("controller-gen --version")
			log.Printf("controller-gen: %s", c.Result())

			// Deploy components
			log.Println("Deploying controller-manager resources...")
			p := utils.RunCommand(`kubectl version`)
			log.Printf("Output of kubectl: %s", p.Result())

			// Retry make deploy to handle the race where a concurrent test cleanup
			// deletes the namespace between our creation and the deploy step.
			client := cfg.Client()
			var deployErr error
			for attempt := 1; attempt <= 5; attempt++ {
				// Ensure the operator namespace is Active before attempting deploy.
				// If it disappeared or is Terminating (concurrent cleanup), wait and recreate.
				for i := 0; i < 60; i++ {
					ns := &corev1.Namespace{}
					nsErr := client.Resources().Get(ctx, namespace, "", ns)
					if apierrors.IsNotFound(nsErr) {
						log.Printf("Namespace %s not found before deploy attempt %d; recreating", namespace, attempt)
						newNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
						_ = client.Resources().Create(ctx, newNs)
						time.Sleep(2 * time.Second)
						continue
					}
					if nsErr != nil {
						time.Sleep(2 * time.Second)
						continue
					}
					if ns.Status.Phase == corev1.NamespaceTerminating || ns.DeletionTimestamp != nil {
						log.Printf("Namespace %s is Terminating before deploy attempt %d; waiting...", namespace, attempt)
						time.Sleep(2 * time.Second)
						continue
					}
					break // namespace is Active
				}

				p = utils.RunCommand(`make deploy`)
				log.Printf("Output of make deploy (attempt %d): %s", attempt, p.Result())
				if p.Err() == nil {
					deployErr = nil
					break
				}
				deployErr = p.Err()
				if !strings.Contains(p.Result(), "being terminated") {
					// Non-transient error — no point retrying.
					break
				}
				log.Printf("Deploy attempt %d failed with namespace-termination error; will retry after namespace clears", attempt)
				// Wait for the namespace to fully terminate so we can recreate it cleanly.
				for i := 0; i < 90; i++ {
					ns := &corev1.Namespace{}
					nsErr := client.Resources().Get(ctx, namespace, "", ns)
					if apierrors.IsNotFound(nsErr) {
						break
					}
					time.Sleep(2 * time.Second)
				}
			}
			if deployErr != nil {
				log.Printf("Failed to deploy resource configurations after retries: %s: %s", deployErr, p.Result())
				return ctx, deployErr
			}

			// wait for controller-manager to be ready
			log.Println("Waiting for controller-manager deployment to be available...")
			if err := wait.For(
				conditions.New(client.Resources()).DeploymentConditionMatch(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "marklogic-operator-controller-manager", Namespace: namespace}},
					appsv1.DeploymentProgressing,
					corev1.ConditionTrue),
				wait.WithTimeout(3*time.Minute),
				wait.WithInterval(10*time.Second),
			); err != nil {
				log.Printf("Timed out while waiting for deployment: %s", err)
				return ctx, err
			}

			p = utils.RunCommand(`kubectl get nodes`)
			log.Printf("Kubernetes Nodes: %s", p.Result())

			return ctx, nil
		},
	)

	// Use the Environment.Finish method to define clean up steps
	testEnv.Finish(
		// Clean up Istio ambient label from operator namespace
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			if !isIstioAmbientEnabled() {
				return ctx, nil
			}

			log.Println("Removing Istio ambient label from operator namespace...")
			client := cfg.Client()

			// Remove label from operator namespace using Patch to avoid conflicts
			operatorNs := &corev1.Namespace{}
			if err := client.Resources().Get(ctx, namespace, "", operatorNs); err == nil {
				if operatorNs.Labels != nil && operatorNs.Labels["istio.io/dataplane-mode"] != "" {
					// Use Patch to remove label, avoiding resourceVersion conflicts
					patchData := []byte(`{"metadata":{"labels":{"istio.io/dataplane-mode":null}}}`)
					if err := client.Resources().Patch(ctx, operatorNs, k8s.Patch{PatchType: types.StrategicMergePatchType, Data: patchData}); err != nil {
						log.Printf("Warning: failed to remove label from operator namespace: %v", err)
					} else {
						log.Printf("Removed Istio ambient label from %s namespace", namespace)
					}
				}
			} else if !apierrors.IsNotFound(err) {
				log.Printf("Warning: failed to get operator namespace: %v", err)
			}

			return ctx, nil
		},
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Finishing tests, cleaning cluster ...")
			if err := forceDeleteMarkLogicCustomResources(); err != nil {
				log.Printf("Warning: failed to force-delete MarkLogic custom resources during cleanup: %v", err)
			}
			utils.RunCommand(`bash -c "kustomize build config/default | kubectl delete -f -"`)
			return ctx, nil
		},
		envfuncs.DeleteNamespace(namespace),
	)

	// Use Environment.Run to launch the test
	exitCode := testEnv.Run(m)
	printTestSummary()
	os.Exit(exitCode)
}

// forceDeleteMarkLogicCustomResources removes stale MarkLogic custom resources
// (and strips finalizers when needed) so CRD deletion does not get stuck in
// Terminating between e2e runs.
func forceDeleteMarkLogicCustomResources() error {
	kinds := []string{
		"marklogicgroups.marklogic.progress.com",
		"marklogicclusters.marklogic.progress.com",
	}

	for _, kind := range kinds {
		check := utils.RunCommand(fmt.Sprintf("kubectl get crd %s --ignore-not-found", kind))
		if check.Err() != nil {
			continue
		}

		// Use jsonpath to get "namespace/name" pairs so we can patch+delete with the
		// correct namespace. Plain `-o name` with `-A` omits namespace information and
		// causes kubectl patch/delete to target the wrong namespace.
		objects := utils.RunCommand(fmt.Sprintf(
			`kubectl get %s -A --ignore-not-found -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`,
			kind))
		if objects.Err() != nil {
			continue
		}

		for _, nsName := range strings.Fields(objects.Result()) {
			nsName = strings.TrimSpace(nsName)
			if nsName == "" || !strings.Contains(nsName, "/") {
				continue
			}
			parts := strings.SplitN(nsName, "/", 2)
			ns, name := parts[0], parts[1]
			utils.RunCommand(fmt.Sprintf("kubectl patch %s %s -n %s --type=merge -p '{\"metadata\":{\"finalizers\":[]}}'", kind, name, ns))
			utils.RunCommand(fmt.Sprintf("kubectl delete %s %s -n %s --ignore-not-found --wait=false", kind, name, ns))
		}
	}

	if err := waitForCRDTerminationToClear(kinds, crdCleanupInitialWaitAttempts, crdCleanupInitialWaitInterval); err == nil {
		return nil
	}

	log.Printf("CRD termination is still pending after initial wait, forcing CRD finalizer cleanup")
	for _, kind := range kinds {
		status := utils.RunCommand(fmt.Sprintf("kubectl get crd %s --ignore-not-found -o jsonpath='{.metadata.deletionTimestamp}'", kind))
		if status.Err() != nil {
			return fmt.Errorf("failed to query %s CRD termination status: %w: %s", kind, status.Err(), status.Result())
		}
		if strings.TrimSpace(status.Result()) == "" {
			continue
		}
		// Re-clear any CR instances that might still have finalizers (e.g. from a
		// concurrent test run that left resources in a different namespace).
		objects := utils.RunCommand(fmt.Sprintf(
			`kubectl get %s -A --ignore-not-found -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`,
			kind))
		if objects.Err() == nil {
			for _, nsName := range strings.Fields(objects.Result()) {
				nsName = strings.TrimSpace(nsName)
				if nsName == "" || !strings.Contains(nsName, "/") {
					continue
				}
				parts := strings.SplitN(nsName, "/", 2)
				ns, name := parts[0], parts[1]
				utils.RunCommand(fmt.Sprintf("kubectl patch %s %s -n %s --type=merge -p '{\"metadata\":{\"finalizers\":[]}}'", kind, name, ns))
				utils.RunCommand(fmt.Sprintf("kubectl delete %s %s -n %s --ignore-not-found --wait=false", kind, name, ns))
			}
		}
		patch := utils.RunCommand(fmt.Sprintf("kubectl patch crd %s --type=merge -p '{\"metadata\":{\"finalizers\":[]}}'", kind))
		if patch.Err() != nil {
			log.Printf("Warning: failed to clear finalizers on CRD %s: %v: %s", kind, patch.Err(), patch.Result())
		}
	}

	// Give Kubernetes up to 30 s to complete the deletion after finalizer removal.
	if err := waitForCRDTerminationToClear(kinds, crdCleanupPostFinalizeWaitAttempts, crdCleanupPostFinalizeWaitInterval); err == nil {
		return nil
	}

	// Last resort: force-delete with grace-period=0 to bypass the normal GC cycle.
	log.Printf("CRD still terminating after finalizer cleanup; force-deleting remaining CRDs")
	for _, kind := range kinds {
		status := utils.RunCommand(fmt.Sprintf("kubectl get crd %s --ignore-not-found -o jsonpath='{.metadata.deletionTimestamp}'", kind))
		if status.Err() != nil || strings.TrimSpace(status.Result()) == "" {
			continue
		}
		forceDelete := utils.RunCommand(fmt.Sprintf("kubectl delete crd %s --ignore-not-found --force --grace-period=0 --wait=false", kind))
		if forceDelete.Err() != nil {
			log.Printf("Warning: force-delete of CRD %s failed: %v: %s", kind, forceDelete.Err(), forceDelete.Result())
		} else {
			log.Printf("Force-deleted CRD %s", kind)
		}
	}

	if err := waitForCRDTerminationToClear(kinds, crdCleanupPostFinalizeWaitAttempts, crdCleanupPostFinalizeWaitInterval); err != nil {
		return err
	}

	return nil
}

func waitForCRDTerminationToClear(crds []string, attempts int, interval time.Duration) error {
	terminating := make([]string, 0)

	for i := 0; i < attempts; i++ {
		terminating = terminating[:0]
		for _, crd := range crds {
			status := utils.RunCommand(fmt.Sprintf("kubectl get crd %s --ignore-not-found -o jsonpath='{.metadata.deletionTimestamp}'", crd))
			if status.Err() != nil {
				return fmt.Errorf("failed to query %s CRD termination status: %w: %s", crd, status.Err(), status.Result())
			}
			if strings.TrimSpace(status.Result()) != "" {
				terminating = append(terminating, crd)
			}
		}

		if len(terminating) == 0 {
			return nil
		}

		time.Sleep(interval)
	}

	return fmt.Errorf("timeout waiting for CRD terminating state to clear: %s", strings.Join(terminating, ", "))
}

func waitForNamespaceDeletionByName(ctx context.Context, client klient.Client, nsName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ns := &corev1.Namespace{}
		err := client.Resources().Get(ctx, nsName, "", ns)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("error checking namespace %s deletion status: %w", nsName, err)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for namespace %s to be deleted after force-finalize", nsName)
}

func forceFinalizeNamespace(nsName string) error {
	cmd := fmt.Sprintf(
		"kubectl get namespace %s -o json | python3 -c \"import sys, json; d=json.load(sys.stdin); d['spec']['finalizers']=[]; print(json.dumps(d))\" | kubectl replace --raw /api/v1/namespaces/%s/finalize -f -",
		nsName,
		nsName,
	)
	p := utils.RunCommand(cmd)
	if p.Err() != nil {
		return fmt.Errorf("%w: %s", p.Err(), p.Result())
	}
	return nil
}
