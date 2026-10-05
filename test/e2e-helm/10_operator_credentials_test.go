// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestOperatorCredentialHandoff(t *testing.T) {
	trackTest(t)

	const (
		namespaceName = "ml-ns-credentials-test"
		clusterName   = "credential-handoff-cluster"
		groupName     = "credential-node"
	)
	if !strings.Contains(","+watchedNamespaces+",", ","+namespaceName+",") {
		t.Fatalf("namespace %q must be included in E2E_WATCHED_NAMESPACES", namespaceName)
	}
	replicas := int32(1)
	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespaceName},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{{
				Name:        groupName,
				Replicas:    &replicas,
				IsBootstrap: true,
			}},
		},
	}

	feature := features.New("Operator Credential Handoff Namespace-Scoped").WithLabel("type", "credential-handoff")
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(namespaceName).GetScheme())
		if err := client.Resources(namespaceName).Create(ctx, cluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cluster, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatalf("MarklogicCluster was not created: %v", err)
		}
		return ctx
	})

	feature.Assess("Operator Secret replaces the bootstrap admin Secret mount", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForOperatorCredentialHandoff(ctx, c.Client(), namespaceName, clusterName, groupName, 10*time.Minute); err != nil {
			logDiagnostics(t, namespaceName)
			t.Fatalf("Operator credential handoff did not complete: %v", err)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := c.Client().Resources(namespaceName).Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("Failed to delete MarklogicCluster: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
