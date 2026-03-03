// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
						return ctx, fmt.Errorf("timeout waiting for namespace %s to be deleted", namespace)
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

			// Only download kustomize if it is not already present in bin/
			kustomizePath := gobin + "/kustomize"
			if _, err := os.Stat(kustomizePath); os.IsNotExist(err) {
				if p := utils.RunCommand(fmt.Sprintf("go install sigs.k8s.io/kustomize/kustomize/v5@%s", kustomizeVer)); p.Err() != nil {
					log.Printf("Failed to install kustomize binary: %s: %s", p.Err(), p.Result())
					return ctx, p.Err()
				}
			} else {
				log.Printf("kustomize already present at %s, skipping install", kustomizePath)
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
			log.Printf("Kustomize version: %s", p.Result())
			return ctx, nil
		},

		// generate and deploy resource configurations
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Building source components...")

			c := utils.RunCommand("controller-gen --version")
			log.Printf("controller-gen: %s", c.Result())

			// Deploy components
			log.Println("Deploying controller-manager resources...")
			p := utils.RunCommand(`kubectl version`)
			log.Printf("Output of kubectl: %s", p.Result())
			p = utils.RunCommand(`make deploy`)
			log.Printf("Output of make deploy: %s", p.Result())
			if p.Err() != nil {
				log.Printf("Failed to deploy resource configurations: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}

			// wait for controller-manager to be ready
			log.Println("Waiting for controller-manager deployment to be available...")
			client := cfg.Client()
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
			utils.RunCommand(`bash -c "kustomize build config/default | kubectl delete -f -"`)
			return ctx, nil
		},
		envfuncs.DeleteNamespace(namespace),
	)

	// Use Environment.Run to launch the test
	os.Exit(testEnv.Run(m))
}
