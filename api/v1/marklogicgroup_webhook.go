/*
Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var marklogicgrouplog = logf.Log.WithName("marklogicgroup-resource")

func (r *MarklogicGroup) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// +kubebuilder:webhook:path=/validate-marklogic-progress-com-v1-marklogicgroup,mutating=false,failurePolicy=fail,sideEffects=None,groups=marklogic.progress.com,resources=marklogicgroups,verbs=create;update,versions=v1,name=vmarklogicgroup.marklogic.progress.com,admissionReviewVersions=v1

var _ webhook.CustomValidator = &MarklogicGroup{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type
func (r *MarklogicGroup) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	marklogicgrouplog.Info("validate create", "name", r.Name)

	// No size validation needed for create - any size is allowed initially
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type
func (r *MarklogicGroup) ValidateUpdate(ctx context.Context, oldObj runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	marklogicgrouplog.Info("validate update", "name", r.Name)

	oldMarklogicGroup := oldObj.(*MarklogicGroup)

	// If persistence is not configured, nothing to validate
	if r.Spec.Persistence == nil || oldMarklogicGroup.Spec.Persistence == nil {
		return nil, nil
	}

	// If size fields are empty, nothing to validate
	if r.Spec.Persistence.Size == "" || oldMarklogicGroup.Spec.Persistence.Size == "" {
		return nil, nil
	}

	// Parse old and new sizes as resource.Quantity for accurate unit conversion
	oldQuantity, err := resource.ParseQuantity(oldMarklogicGroup.Spec.Persistence.Size)
	if err != nil {
		marklogicgrouplog.Error(err, "failed to parse old size", "oldSize", oldMarklogicGroup.Spec.Persistence.Size)
		return nil, fmt.Errorf("invalid old volume size: %w", err)
	}

	newQuantity, err := resource.ParseQuantity(r.Spec.Persistence.Size)
	if err != nil {
		marklogicgrouplog.Error(err, "failed to parse new size", "newSize", r.Spec.Persistence.Size)
		return nil, fmt.Errorf("invalid new volume size: %w", err)
	}

	// Reject if new size is less than old size (shrinking)
	// Cmp() returns: -1 if newQuantity < oldQuantity, 0 if equal, 1 if greater
	if newQuantity.Cmp(oldQuantity) < 0 {
		marklogicgrouplog.Info("rejecting volume shrink request",
			"name", r.Name,
			"oldSize", oldMarklogicGroup.Spec.Persistence.Size,
			"newSize", r.Spec.Persistence.Size)

		return nil, fmt.Errorf(
			"volume shrinking is not supported: cannot shrink from %s to %s. "+
				"Only volume expansion is allowed",
			oldMarklogicGroup.Spec.Persistence.Size,
			r.Spec.Persistence.Size,
		)
	}

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type
func (r *MarklogicGroup) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	marklogicgrouplog.Info("validate delete", "name", r.Name)

	// No validation needed for delete
	return nil, nil
}
