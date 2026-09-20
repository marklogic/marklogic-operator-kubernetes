// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *Run) preflight(ctx context.Context, t *testing.T, needsMarkLogic bool) error {
	version, err := r.client.Discovery().ServerVersion()
	if err != nil {
		return err
	}
	t.Logf("Kubernetes version=%s", version.GitVersion)
	permissions := []authorizationv1.ResourceAttributes{
		{Resource: "namespaces", Verb: "create"}, {Resource: "namespaces", Verb: "get"}, {Resource: "namespaces", Verb: "delete"},
		{Resource: "pods", Verb: "create"}, {Resource: "pods", Verb: "get"}, {Resource: "pods", Verb: "list"}, {Resource: "pods", Verb: "watch"},
		{Resource: "pods", Subresource: "exec", Verb: "create"}, {Resource: "pods", Subresource: "log", Verb: "get"},
		{Resource: "events", Verb: "list"}, {Resource: "persistentvolumeclaims", Verb: "list"},
	}
	for _, resource := range []struct{ group, name string }{{"", "services"}, {"", "configmaps"}, {"", "secrets"}, {"apps", "deployments"}} {
		for _, verb := range []string{"get", "list", "watch", "create", "patch"} {
			permissions = append(permissions, authorizationv1.ResourceAttributes{Group: resource.group, Resource: resource.name, Verb: verb})
		}
	}
	if needsMarkLogic {
		permissions = append(permissions, authorizationv1.ResourceAttributes{Resource: "persistentvolumes", Verb: "get"})
		for _, verb := range []string{"get", "create", "patch"} {
			permissions = append(permissions, authorizationv1.ResourceAttributes{Group: "marklogic.progress.com", Resource: "marklogicclusters", Verb: verb})
		}
		for _, verb := range []string{"get", "list", "watch"} {
			permissions = append(permissions, authorizationv1.ResourceAttributes{Group: "apps", Resource: "statefulsets", Verb: verb})
		}
	}
	for _, attributes := range permissions {
		review, err := r.client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes}}, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		if !review.Status.Allowed {
			return fmt.Errorf("requires %s %s/%s across test namespaces: %s", attributes.Verb, attributes.Resource, attributes.Subresource, review.Status.Reason)
		}
	}
	if !needsMarkLogic {
		return nil
	}
	resources, err := r.client.Discovery().ServerResourcesForGroupVersion("marklogic.progress.com/v1")
	if err != nil {
		return fmt.Errorf("MarkLogic CRD discovery: %w", err)
	}
	found := false
	for _, resource := range resources.APIResources {
		if resource.Name == "marklogicclusters" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("marklogicclusters CRD is not served")
	}
	operatorNamespace, operatorDeployment := os.Getenv("INTEGRATION_OPERATOR_NAMESPACE"), os.Getenv("INTEGRATION_OPERATOR_DEPLOYMENT")
	if operatorNamespace == "" || operatorDeployment == "" {
		return fmt.Errorf("set INTEGRATION_OPERATOR_NAMESPACE and INTEGRATION_OPERATOR_DEPLOYMENT")
	}
	deployment, err := r.client.AppsV1().Deployments(operatorNamespace).Get(ctx, operatorDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("operator deployment: %w", err)
	}
	if deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.AvailableReplicas < 1 {
		return fmt.Errorf("operator deployment is not available at its current generation")
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		t.Logf("Operator container=%s image=%s", container.Name, container.Image)
	}
	classes, err := r.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	requested := strings.TrimSpace(os.Getenv("INTEGRATION_STORAGE_CLASS"))
	matches := 0
	for _, class := range classes.Items {
		selected := class.Name == requested
		if requested == "" {
			selected = class.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || class.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
		}
		if !selected {
			continue
		}
		matches++
		if class.ReclaimPolicy != nil && *class.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			return fmt.Errorf("storage class %s must use Delete reclaim policy for disposable runs", class.Name)
		}
		r.StorageClass = class.Name
		t.Logf("Storage class=%s provisioner=%s", class.Name, class.Provisioner)
	}
	if matches != 1 {
		return fmt.Errorf("expected exactly one storage class (selected=%q, matches=%d); set INTEGRATION_STORAGE_CLASS", requested, matches)
	}
	return nil
}
