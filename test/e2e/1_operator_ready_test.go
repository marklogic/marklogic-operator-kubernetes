// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	coreV1 "k8s.io/api/core/v1"
	apiextensionsV1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/utils"
)

func TestOperatorReady(t *testing.T) {
	podCreationSig := make(chan *coreV1.Pod)

	feature := features.New("Operator Ready")
	// Use feature.Setup to define pre-test configuration
	feature.Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		if err := client.Resources(namespace).Watch(&coreV1.PodList{}).WithAddFunc(func(obj interface{}) {
			pod := obj.(*coreV1.Pod)
			if strings.HasPrefix(pod.Name, "marklogic-operator-controller") {
				podCreationSig <- pod
			}
		}).Start(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Assessment to check for CRD in cluster
	feature.Assess("CRD installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		p := utils.RunCommand(`kubectl get ns`)
		t.Logf("Kubernetes namespace: %s", p.Result())
		client := c.Client()
		apiextensionsV1.AddToScheme(client.Resources().GetScheme())
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
			t.Error("Timed out wating for pod creation by MarklogicCluster contoller")
		case pod := <-podCreationSig:
			t.Log("Pod created by MarklogicCluster controller")
			refname := pod.GetOwnerReferences()[0].Name
			if !strings.HasPrefix(refname, "marklogic-operator") {
				t.Fatalf("Pod has unexpected owner ref: %#v", refname)
			}
		}
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		close(podCreationSig)
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}
