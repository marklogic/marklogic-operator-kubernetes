// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestNamespaceScopedRBAC verifies that the Helm chart installs the correct RBAC for
// namespace-scoped mode and does NOT install cluster-wide permissions.
//
// In namespace-scoped mode (scope.type=namespace) the chart must:
//   - Create a Role in each watched namespace (not a ClusterRole)
//   - Create a RoleBinding in each watched namespace (not a ClusterRoleBinding)
//   - NOT install a ClusterRole named "marklogic-operator-manager-role"
//   - NOT install a ClusterRoleBinding named "marklogic-operator-manager-rolebinding"
//
// This is the key test that the kustomize-based suite (test/e2e) cannot perform,
// because make deploy always applies ClusterRole/ClusterRoleBinding regardless of
// any WATCH_NAMESPACE patch made afterwards.
func TestNamespaceScopedRBAC(t *testing.T) {
	trackTest(t)
	feature := features.New("Namespace-Scoped RBAC").
		WithLabel("type", "rbac")

	// Verify no ClusterRole for the manager exists.
	// Helm does not render manager-rbac.yaml when scope.type=namespace.
	feature.Assess("No ClusterRole installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var cr rbacv1.ClusterRole
		err := client.Resources().Get(ctx, "marklogic-operator-manager-role", "", &cr)
		if err == nil {
			t.Fatal("ClusterRole 'marklogic-operator-manager-role' exists but must NOT be present in namespace-scoped mode")
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Unexpected error checking for ClusterRole: %v", err)
		}
		t.Log("Confirmed: no manager ClusterRole present")
		return ctx
	})

	// Verify no ClusterRoleBinding for the manager exists.
	feature.Assess("No ClusterRoleBinding installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var crb rbacv1.ClusterRoleBinding
		err := client.Resources().Get(ctx, "marklogic-operator-manager-rolebinding", "", &crb)
		if err == nil {
			t.Fatal("ClusterRoleBinding 'marklogic-operator-manager-rolebinding' exists but must NOT be present in namespace-scoped mode")
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Unexpected error checking for ClusterRoleBinding: %v", err)
		}
		t.Log("Confirmed: no manager ClusterRoleBinding present")
		return ctx
	})

	// Verify a Role exists in each watched namespace.
	feature.Assess("Role exists in each watched namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		for _, ns := range strings.Split(watchedNamespaces, ",") {
			ns = strings.TrimSpace(ns)
			var role rbacv1.Role
			if err := client.Resources(ns).Get(ctx, "marklogic-operator-manager-role", ns, &role); err != nil {
				t.Fatalf("Role 'marklogic-operator-manager-role' not found in namespace %q: %v", ns, err)
			}
			t.Logf("Role found in namespace %q", ns)
		}
		return ctx
	})

	// Verify a RoleBinding exists in each watched namespace and its subject points
	// to the operator ServiceAccount in the operator install namespace (helmNS).
	feature.Assess("RoleBinding exists in each watched namespace with correct subject", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		for _, ns := range strings.Split(watchedNamespaces, ",") {
			ns = strings.TrimSpace(ns)
			var rb rbacv1.RoleBinding
			if err := client.Resources(ns).Get(ctx, "marklogic-operator-manager-rolebinding", ns, &rb); err != nil {
				t.Fatalf("RoleBinding 'marklogic-operator-manager-rolebinding' not found in namespace %q: %v", ns, err)
			}
			// The subject must reference the SA in the operator install namespace, not the watched namespace.
			found := false
			for _, subj := range rb.Subjects {
				if subj.Kind == "ServiceAccount" && subj.Namespace == helmNS {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("RoleBinding in namespace %q has no ServiceAccount subject in %q (subjects: %v)", ns, helmNS, rb.Subjects)
			}
			t.Logf("RoleBinding in namespace %q has correct subject (SA in %q)", ns, helmNS)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
