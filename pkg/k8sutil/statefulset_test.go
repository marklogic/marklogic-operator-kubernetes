// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"reflect"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestStatefulSetRejectsUnsupportedClaimTemplateDrift(t *testing.T) {
	for _, test := range []struct {
		name       string
		additional bool
		changeVAC  bool
		noScale    bool
	}{
		{name: "primary StorageClass with unchanged VAC"},
		{name: "additional StorageClass with unchanged VAC", additional: true},
		{name: "additional StorageClass with changed VAC", additional: true, changeVAC: true},
		{name: "additional StorageClass without scaling", additional: true, noScale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, statefulSet, _, _ := liveVolumeAttributesClassFixture(t)
			group := operator.MarklogicGroup
			group.Spec.Image = "marklogic:12.0.3"
			group.Spec.GroupConfig = &marklogicv1.GroupConfig{Name: "Default"}
			group.Spec.HugePages = &marklogicv1.HugePages{}
			group.Spec.LogCollection = &marklogicv1.LogCollection{}
			if test.additional {
				additional := generatePVCTemplate(group.Spec.Persistence)
				additional.Name = "logs"
				group.Spec.AdditionalVolumeClaimTemplates = &[]corev1.PersistentVolumeClaim{additional}
			}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(statefulSet), statefulSet); err != nil {
				t.Fatal(err)
			}
			current := generateStatefulSetsDef(
				generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabels(group.Spec.Name), nil),
				generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
			)
			current.UID, current.ResourceVersion = statefulSet.UID, statefulSet.ResourceVersion
			if err := operator.Client.Update(operator.Ctx, current); err != nil {
				t.Fatal(err)
			}
			desiredReplicas := int32(2)
			if test.noScale {
				desiredReplicas = 1
			}
			group.Spec.Replicas = &desiredReplicas
			group.Spec.Image = "marklogic:12.0.4"
			if test.additional {
				desired := &(*group.Spec.AdditionalVolumeClaimTemplates)[0]
				desired.Spec.StorageClassName = pointerToVACStorageClass("other-storage-class")
				if test.changeVAC {
					desired.Spec.VolumeAttributesClassName = pointerToVACStorageClass("other-profile")
				}
			} else {
				group.Spec.Persistence.StorageClassName = "other-storage-class"
			}
			if err := operator.Client.Update(operator.Ctx, group); err != nil {
				t.Fatal(err)
			}
			operator.VACValidator = vacValidatorForGroup(t, group)
			if _, err := operator.ReconcileStatefulset(); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("unsupported claim-template drift must be reported before scale-up, got %v", err)
			}
			recorder := operator.Recorder.(*record.FakeRecorder)
			warned := false
			for len(recorder.Events) > 0 {
				warned = strings.Contains(<-recorder.Events, "UnsupportedClaimTemplateChange") || warned
			}
			if !warned {
				t.Fatal("unsupported claim-template drift must emit a warning diagnostic")
			}
			actual := &appsv1.StatefulSet{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.Spec, current.Spec) {
				t.Fatal("unsupported claim-template drift must not patch or scale the StatefulSet")
			}
			claims := &corev1.PersistentVolumeClaimList{}
			if err := operator.Client.List(operator.Ctx, claims, client.InNamespace(group.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(claims.Items) != 1 {
				t.Fatal("unsupported claim-template drift must be detected before PVC precreation")
			}
		})
	}
}

func TestClaimTemplateChanges(t *testing.T) {
	source := corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: "restore"}
	for _, test := range []struct {
		name      string
		mutate    func(current, desired *corev1.PersistentVolumeClaim)
		wantError bool
	}{
		{name: "unchanged"},
		{name: "VAC change", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.VolumeAttributesClassName = pointerToVACStorageClass("performance")
		}},
		{name: "VAC opt out", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.VolumeAttributesClassName = nil
		}},
		{name: "size handled by resize workflow", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("30Gi")
		}},
		{name: "API defaults and admission metadata", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			mode := corev1.PersistentVolumeFilesystem
			current.Spec.VolumeMode = &mode
			current.Spec.Resources.Limits = corev1.ResourceList{}
			current.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
			current.Status.Phase = corev1.ClaimPending
			current.Labels = map[string]string{"admission.example.com/managed": "true"}
			current.Annotations = map[string]string{"admission.example.com/profile": "retained"}
		}},
		{name: "access modes reordered", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			current.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadOnlyMany}
			desired.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany, corev1.ReadWriteOnce}
		}},
		{name: "equivalent resource quantities", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			current.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}
			desired.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20480Mi")}
		}},
		{name: "mirrored data source default", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			current.Spec.DataSource = source.DeepCopy()
			current.Spec.DataSourceRef = &corev1.TypedObjectReference{Kind: source.Kind, Name: source.Name}
			desired.Spec.DataSource = source.DeepCopy()
		}},
		{name: "local source reference and empty core defaults", mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			current.Spec.DataSourceRef = &corev1.TypedObjectReference{APIGroup: pointerToVACStorageClass(""), Kind: source.Kind, Name: source.Name, Namespace: pointerToVACStorageClass("")}
			desired.Spec.DataSource = source.DeepCopy()
		}},
		{name: "StorageClass change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.StorageClassName = pointerToVACStorageClass("other-storage-class")
		}},
		{name: "explicit no class is not default class", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			current.Spec.StorageClassName = nil
			desired.Spec.StorageClassName = pointerToVACStorageClass("")
		}},
		{name: "access mode change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		}},
		{name: "block volume is not filesystem default", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			mode := corev1.PersistentVolumeBlock
			desired.Spec.VolumeMode = &mode
		}},
		{name: "selector change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"volume": "other"}}
		}},
		{name: "data source change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.DataSource = source.DeepCopy()
		}},
		{name: "data source namespace change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			current.Spec.DataSourceRef = &corev1.TypedObjectReference{Kind: source.Kind, Name: source.Name}
			desired.Spec.DataSourceRef = current.Spec.DataSourceRef.DeepCopy()
			desired.Spec.DataSourceRef.Namespace = pointerToVACStorageClass("other-namespace")
		}},
		{name: "resource limit change is not a resize request", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("30Gi")}
		}},
		{name: "storage request removal cannot be resized", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			delete(desired.Spec.Resources.Requests, corev1.ResourceStorage)
		}},
		{name: "zero storage request cannot be resized", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("0")
		}},
		{name: "requested template label change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Labels = map[string]string{"workload": "new"}
		}},
		{name: "requested template annotation change", wantError: true, mutate: func(current, desired *corev1.PersistentVolumeClaim) {
			desired.Annotations = map[string]string{"storage-profile": "new"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			template := generatePVCTemplate(storageWithClass("standard"))
			current, desired := template.DeepCopy(), template.DeepCopy()
			if test.mutate != nil {
				test.mutate(current, desired)
			}
			beforeCurrent, beforeDesired := current.DeepCopy(), desired.DeepCopy()
			if err := validateClaimTemplateChanges([]corev1.PersistentVolumeClaim{*current}, []corev1.PersistentVolumeClaim{*desired}); (err != nil) != test.wantError {
				t.Fatalf("unsupported drift error = %v, want error = %t", err, test.wantError)
			}
			if !reflect.DeepEqual(current, beforeCurrent) || !reflect.DeepEqual(desired, beforeDesired) {
				t.Fatal("comparison must not mutate historical or desired claim templates")
			}
		})
	}
}

func TestClaimTemplateChangesTopology(t *testing.T) {
	primary := generatePVCTemplate(storageWithClass("standard"))
	additional := *primary.DeepCopy()
	additional.Name = "logs"
	for _, test := range []struct {
		name      string
		desired   []corev1.PersistentVolumeClaim
		wantError bool
	}{
		{name: "reordered templates", desired: []corev1.PersistentVolumeClaim{additional, primary}},
		{name: "removed template", desired: []corev1.PersistentVolumeClaim{primary}, wantError: true},
		{name: "renamed template", desired: []corev1.PersistentVolumeClaim{primary, {ObjectMeta: metav1.ObjectMeta{Name: "new-logs"}}}, wantError: true},
		{name: "duplicate template", desired: []corev1.PersistentVolumeClaim{primary, primary}, wantError: true},
		{name: "added template", desired: []corev1.PersistentVolumeClaim{primary, additional, {ObjectMeta: metav1.ObjectMeta{Name: "backup"}}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateClaimTemplateChanges([]corev1.PersistentVolumeClaim{primary, additional}, test.desired); (err != nil) != test.wantError {
				t.Fatalf("unsupported topology error = %v, want error = %t", err, test.wantError)
			}
		})
	}
}
