// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	coreV1 "k8s.io/api/core/v1"
	apiextensionsV1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/utils"
)

func TestOperatorReady(t *testing.T) {
	trackTest(t)
	runTopLevelParallel(t)
	var podCreationSig <-chan *coreV1.Pod
	var cancelWatch context.CancelFunc
	t.Cleanup(func() {
		if cancelWatch != nil {
			cancelWatch()
		}
	})

	feature := features.New("Operator Ready")
	// Use feature.Setup to define pre-test configuration
	feature.Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()
		watchCtx, cancel := context.WithCancel(ctx)
		cancelWatch = cancel
		var notifyPod func(interface{})
		podCreationSig, notifyPod = testutils.OperatorPodNotifications(watchCtx)

		var podList coreV1.PodList
		if err := client.Resources(namespace).List(ctx, &podList); err != nil {
			t.Fatalf("Cannot list operator Pods: %v", err)
		}
		for index := range podList.Items {
			if strings.HasPrefix(podList.Items[index].Name, "marklogic-operator-controller") {
				notifyPod(&podList.Items[index])
				return ctx
			}
		}
		if err := client.Resources(namespace).Watch(&coreV1.PodList{}).WithAddFunc(notifyPod).Start(watchCtx); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Assessment to check for CRD in cluster
	feature.Assess("CRD installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		p := utils.RunCommand(`kubectl get ns`)
		t.Logf("Kubernetes namespace: %s", p.Result())
		client := c.Client()
		name := "marklogicclusters.marklogic.progress.com"
		var crd apiextensionsV1.CustomResourceDefinition
		if err := client.Resources().Get(ctx, name, "", &crd); err != nil {
			t.Fatalf("CRD not found: %s", err)
		}
		if condition := crd.Spec.Names.Kind; condition != "MarklogicCluster" {
			t.Fatalf("MarklogicCluster CRD has unexpected kind: %s", condition)
		}
		return ctx
	})

	// Assessment to check for the creation of the pod
	feature.Assess("Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		select {
		case <-time.After(60 * time.Second):
			t.Error("Timed out waiting for controller-manager pod")
		case pod := <-podCreationSig:
			t.Log("Pod created by MarklogicCluster controller")
			if len(pod.OwnerReferences) == 0 {
				t.Fatalf("Pod %s has no owner references", pod.Name)
			}
			refname := pod.GetOwnerReferences()[0].Name
			if !strings.HasPrefix(refname, "marklogic-operator") {
				t.Fatalf("Pod has unexpected owner ref: %#v", refname)
			}
		}
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if cancelWatch != nil {
			cancelWatch()
		}
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}
