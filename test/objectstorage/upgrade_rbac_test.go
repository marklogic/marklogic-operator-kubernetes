// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestNamespaceUpgradeRBACRequiresNarrowStorageClassAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding, *corev1.ServiceAccount)
		valid  bool
	}{
		{name: "storageclass reader", valid: true},
		{name: "restricted named storageclass", valid: true, mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules[0].ResourceNames = []string{"standard"}
			r.Rules[0].Verbs = []string{"get"}
		}},
		{name: "persistentvolume and volumeattributesclass readers", valid: true, mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules = append(r.Rules,
				rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get"}},
				rbacv1.PolicyRule{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattributesclasses"}, Verbs: []string{"get"}})
		}},
		{name: "persistentvolume write privilege", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules = append(r.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get", "delete"}})
		}},
		{name: "write privilege", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules[0].Verbs = append(r.Rules[0].Verbs, "patch")
		}},
		{name: "wildcard verb", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules[0].Verbs = []string{"*"}
		}},
		{name: "other resource", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules[0].Resources = append(r.Rules[0].Resources, "persistentvolumes")
		}},
		{name: "other API group", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules[0].APIGroups = []string{""}
		}},
		{name: "nonresource URL", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules[0].NonResourceURLs = []string{"*"}
		}},
		{name: "aggregation", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.AggregationRule = &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{{}}}
		}},
		{name: "extra rule", mutate: func(r *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			r.Rules = append(r.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
		}},
		{name: "external role", mutate: func(_ *rbacv1.ClusterRole, b *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			b.RoleRef.Name = "cluster-admin"
		}},
		{name: "other namespace", mutate: func(_ *rbacv1.ClusterRole, b *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			b.Subjects[0].Namespace = "another"
		}},
		{name: "unrendered account", mutate: func(_ *rbacv1.ClusterRole, b *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			b.Subjects[0].Name = "other"
		}},
		{name: "account rendered elsewhere", mutate: func(_ *rbacv1.ClusterRole, _ *rbacv1.ClusterRoleBinding, a *corev1.ServiceAccount) {
			a.Namespace = "another"
		}},
		{name: "user subject", mutate: func(_ *rbacv1.ClusterRole, b *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			b.Subjects[0].Kind = "User"
		}},
		{name: "extra subject", mutate: func(_ *rbacv1.ClusterRole, b *rbacv1.ClusterRoleBinding, _ *corev1.ServiceAccount) {
			b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: "Group", Name: "system:authenticated", APIGroup: rbacv1.GroupName})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"}, ObjectMeta: metav1.ObjectMeta{Name: "storageclass-reader"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: []string{"get", "list", "watch"}}}}
			account := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "test-upgrade"}}
			binding := &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "storageclass-reader"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account.Name, Namespace: account.Namespace}}}
			if tc.mutate != nil {
				tc.mutate(role, binding, account)
			}
			var objects []*unstructured.Unstructured
			// Deliberately put the binding before its role to check order independence.
			for _, obj := range []any{binding, account, role} {
				data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
				if err != nil {
					t.Fatal("cannot encode RBAC fixture")
				}
				objects = append(objects, &unstructured.Unstructured{Object: data})
			}
			if err := validateNamespaceUpgradeRBAC(objects, "test-upgrade"); (err == nil) != tc.valid {
				t.Fatal("namespace upgrade RBAC decision did not match expected scope")
			}
		})
	}
}

func TestNamespaceUpgradeRBACAcceptsCurrentHelmChart(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is unavailable; local chart rendering regression not exercised")
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "charts", "marklogic-operator-kubernetes"))
	if err != nil {
		t.Fatal("cannot resolve local chart")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Helm template with no --validate, --dry-run=server or dependency update is local only.
	data, err := exec.CommandContext(ctx, "helm", "template", "rbac-test", chart, "--namespace", "test-upgrade", "--set", "scope.type=namespace", "--set", "metrics.secure=false").Output()
	if err != nil {
		t.Fatal("cannot render local namespace chart (output suppressed)")
	}
	objects, err := decodeUpgradeManifests(data)
	if err != nil {
		t.Fatal("cannot decode local namespace chart")
	}
	clusterRoles, bindings := 0, 0
	for _, obj := range objects {
		switch obj.GetKind() {
		case "ServiceAccount":
			// The render helper supplies the release namespace for namespaced objects.
			if obj.GetNamespace() == "" {
				obj.SetNamespace("test-upgrade")
			}
		case "ClusterRole":
			clusterRoles++
		case "ClusterRoleBinding":
			bindings++
		}
	}
	if clusterRoles == 0 || bindings == 0 {
		t.Fatal("local namespace chart no longer exercises StorageClass cluster RBAC")
	}
	if validateNamespaceUpgradeRBAC(objects, "test-upgrade") != nil {
		t.Fatal("namespace chart StorageClass RBAC was rejected")
	}
}
