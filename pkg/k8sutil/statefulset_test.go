// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

func TestGeneratePVCTemplateVolumeAttributesClassName(t *testing.T) {
	volumeAttributesClassName := "gold-iops"
	persistence := &marklogicv1.Persistence{
		Size:                      "20Gi",
		VolumeAttributesClassName: &volumeAttributesClassName,
	}

	pvcTemplate := generatePVCTemplate(persistence)

	if pvcTemplate.Spec.VolumeAttributesClassName == nil {
		t.Fatal("expected volumeAttributesClassName to be set")
	}
	if got, want := *pvcTemplate.Spec.VolumeAttributesClassName, "gold-iops"; got != want {
		t.Fatalf("volumeAttributesClassName = %q, want %q", got, want)
	}
}

func TestGeneratePVCTemplateLeavesVolumeAttributesClassNameUnset(t *testing.T) {
	pvcTemplate := generatePVCTemplate(&marklogicv1.Persistence{Size: "20Gi"})

	if pvcTemplate.Spec.VolumeAttributesClassName != nil {
		t.Fatalf("volumeAttributesClassName = %q, want nil", *pvcTemplate.Spec.VolumeAttributesClassName)
	}
}
