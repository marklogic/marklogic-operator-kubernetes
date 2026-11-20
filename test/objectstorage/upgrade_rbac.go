// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"errors"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Namespace installations need cluster-level reads of these storage resources for PVC
// expansion and volume attributes validation. Permit only that exception; a broad cluster
// binding could hide a namespace-scope RBAC regression.
var namespaceClusterReadResources = map[[2]string]bool{
	{"", "persistentvolumes"}:                     true,
	{"storage.k8s.io", "storageclasses"}:          true,
	{"storage.k8s.io", "volumeattributesclasses"}: true,
}

func validateNamespaceUpgradeRBAC(objects []*unstructured.Unstructured, namespace string) error {
	invalid := errors.New("namespace upgrade artifacts contain unsupported cluster RBAC")
	if namespace == "" {
		return invalid
	}
	accounts := map[string]bool{}
	roles := map[string]bool{}
	for _, obj := range objects {
		if obj == nil {
			return invalid
		}
		switch obj.GetKind() {
		case "ServiceAccount":
			if obj.GetNamespace() == namespace && obj.GetName() != "" {
				accounts[obj.GetName()] = true
			}
		case "ClusterRole":
			role := &rbacv1.ClusterRole{}
			if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, role) != nil || role.Name == "" || role.AggregationRule != nil || len(role.Rules) == 0 {
				return invalid
			}
			for _, rule := range role.Rules {
				if len(rule.APIGroups) != 1 || len(rule.Resources) != 1 ||
					!namespaceClusterReadResources[[2]string{rule.APIGroups[0], rule.Resources[0]}] ||
					len(rule.NonResourceURLs) != 0 || len(rule.Verbs) == 0 {
					return invalid
				}
				for _, verb := range rule.Verbs {
					if verb != "get" && verb != "list" && verb != "watch" {
						return invalid
					}
				}
			}
			roles[role.Name] = true
		}
	}
	for _, obj := range objects {
		if obj.GetKind() != "ClusterRoleBinding" {
			continue
		}
		binding := &rbacv1.ClusterRoleBinding{}
		if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, binding) != nil ||
			binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "ClusterRole" ||
			!roles[binding.RoleRef.Name] || len(binding.Subjects) == 0 {
			return invalid
		}
		for _, subject := range binding.Subjects {
			if subject.Kind != "ServiceAccount" || subject.APIGroup != "" || subject.Namespace != namespace || !accounts[subject.Name] {
				return invalid
			}
		}
	}
	return nil
}
