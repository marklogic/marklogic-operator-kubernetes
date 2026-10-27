// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"strings"
	"testing"
	"time"

	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestOperatorReady verifies that the Helm-installed operator is running correctly:
// the CRD is registered and the controller-manager pod is present and owned by the
// marklogic-operator Deployment.
func TestOperatorReady(t *testing.T) {
	trackTest(t)
	var podCreationSig <-chan *corev1.Pod
	var cancelWatch context.CancelFunc
	t.Cleanup(func() {
		if cancelWatch != nil {
			cancelWatch()
		}
	})

	feature := features.New("Operator Ready (Helm Namespace-Scoped)").
		WithLabel("type", "operator-ready")

	feature.Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()
		watchCtx, cancel := context.WithCancel(ctx)
		cancelWatch = cancel
		var notifyPod func(interface{})
		podCreationSig, notifyPod = testutils.OperatorPodNotifications(watchCtx)

		// The operator was deployed by TestMain before this test runs, so the pod may
		// already exist. A Watch only fires for future Add events and will miss it.
		var podList corev1.PodList
		if err := client.Resources(helmNS).List(ctx, &podList); err == nil {
			for i := range podList.Items {
				if strings.HasPrefix(podList.Items[i].Name, "marklogic-operator-controller") {
					notifyPod(&podList.Items[i])
					return ctx
				}
			}
		}

		// If not yet visible, fall back to a Watch for the pod Add event.
		if err := client.Resources(helmNS).Watch(&corev1.PodList{}).WithAddFunc(notifyPod).Start(watchCtx); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("CRD installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		apiextensionsv1.AddToScheme(client.Resources().GetScheme())
		var crd apiextensionsv1.CustomResourceDefinition
		if err := client.Resources().Get(ctx, "marklogicclusters.marklogic.progress.com", "", &crd); err != nil {
			t.Fatalf("CRD not found: %v", err)
		}
		if crd.Spec.Names.Kind != "MarklogicCluster" {
			t.Fatalf("CRD has unexpected kind: %s", crd.Spec.Names.Kind)
		}
		return ctx
	})

	feature.Assess("Controller-manager pod running", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		select {
		case <-time.After(60 * time.Second):
			t.Error("Timed out waiting for controller-manager pod")
		case pod := <-podCreationSig:
			t.Logf("Controller-manager pod observed: %s", pod.Name)
			if len(pod.OwnerReferences) == 0 {
				t.Fatalf("Pod %s has no owner references", pod.Name)
			}
			ownerName := pod.OwnerReferences[0].Name
			if !strings.Contains(ownerName, "marklogic-operator") {
				t.Fatalf("Pod owner %q does not match expected prefix", ownerName)
			}
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if cancelWatch != nil {
			cancelWatch()
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
