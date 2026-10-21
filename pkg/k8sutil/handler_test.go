// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestGroupHandlerContinuesAfterScaleUpPVCPreparationFailure(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprintf("dynamic-%t", dynamic), func(t *testing.T) {
			operator, statefulSet, _, claim := liveVolumeAttributesClassFixture(t)
			group := operator.MarklogicGroup
			group.Spec.IsDynamic = dynamic
			group.Spec.Image = testMarkLogicImage
			group.Spec.GroupConfig = &marklogicv1.GroupConfig{Name: "Default"}
			group.Spec.HugePages = &marklogicv1.HugePages{}
			group.Spec.LogCollection = &marklogicv1.LogCollection{}
			group.Finalizers = []string{dynamicGroupCleanupFinalizer}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(statefulSet), statefulSet); err != nil {
				t.Fatal(err)
			}
			current := generateStatefulSetsDef(
				generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabelsByComponent(group.Spec.Name, dynamic), nil),
				generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
			)
			current.UID, current.ResourceVersion = statefulSet.UID, statefulSet.ResourceVersion
			current.Spec.VolumeClaimTemplates[0].Spec.VolumeAttributesClassName = pointerToVACStorageClass("standard")
			if err := operator.Client.Update(operator.Ctx, current); err != nil {
				t.Fatal(err)
			}
			desiredReplicas := int32(2)
			group.Spec.Replicas = &desiredReplicas
			group.Spec.Image = testUpdatedMarkLogicImage
			group.Spec.NodeSelector = map[string]string{"nodepool": "updated"}
			if dynamic {
				group.Spec.Persistence.VolumeAttributesClassName = nil
			}
			if err := operator.Client.Update(operator.Ctx, group); err != nil {
				t.Fatal(err)
			}
			operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, pvc := object.(*corev1.PersistentVolumeClaim); pvc {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName(), fmt.Errorf("PVC quota exceeded"))
					}
					return underlying.Create(ctx, object, options...)
				},
			})
			outcome, err := operator.ReconsileMarklogicGroupHandler()
			if err != nil || outcome.RequeueAfter != 30*time.Second {
				t.Fatalf("preparation failure must not stop the handler or lose its retry, got %+v, %v", outcome, err)
			}
			actual := &appsv1.StatefulSet{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
				t.Fatal(err)
			}
			if *actual.Spec.Replicas != 1 || actual.Spec.Template.Spec.Containers[0].Image != group.Spec.Image || actual.Spec.Template.Spec.NodeSelector["nodepool"] != "updated" {
				t.Fatal("scale-up must stay frozen while image and scheduling updates continue")
			}
			if dynamic {
				saved := &marklogicv1.MarklogicGroup{}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(group), saved); err != nil {
					t.Fatal(err)
				}
				if saved.Status.Dynamic == nil || saved.Status.Dynamic.Reason != dynamicReasonInvalidConfig {
					t.Fatal("dynamic-group reconciliation must still evaluate the missing configuration")
				}
			} else {
				updated := &corev1.PersistentVolumeClaim{}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), updated); err != nil {
					t.Fatal(err)
				}
				if updated.Spec.VolumeAttributesClassName == nil || *updated.Spec.VolumeAttributesClassName != "premium" {
					t.Fatal("VAC reconciliation must still update the existing active PVC")
				}
			}
		})
	}
}

func TestGroupHandlerContinuesWhileResizeWaitsForVAC(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		for _, dynamic := range []bool{false, true} {
			t.Run(fmt.Sprintf("guarded-%t/dynamic-%t", guarded, dynamic), func(t *testing.T) {
				operator, statefulSet, _, claim := liveVolumeAttributesClassFixture(t)
				storageClass := &storagev1.StorageClass{}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: *claim.Spec.StorageClassName}, storageClass); err != nil {
					t.Fatal(err)
				}
				expandable := true
				storageClass.AllowVolumeExpansion = &expandable
				if err := operator.Client.Update(operator.Ctx, storageClass); err != nil {
					t.Fatal(err)
				}
				group := operator.MarklogicGroup
				group.Spec.IsDynamic = dynamic
				group.Spec.Image = testMarkLogicImage
				group.Spec.GroupConfig = &marklogicv1.GroupConfig{Name: "Default"}
				group.Spec.HugePages = &marklogicv1.HugePages{}
				group.Spec.LogCollection = &marklogicv1.LogCollection{}
				group.Spec.UpdateStrategy = appsv1.OnDeleteStatefulSetStrategyType
				group.Finalizers = []string{dynamicGroupCleanupFinalizer}
				current := generateStatefulSetsDef(
					generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabelsByComponent(group.Spec.Name, dynamic), nil),
					generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
				)
				current.UID, current.ResourceVersion = statefulSet.UID, statefulSet.ResourceVersion
				current.Status.ReadyReplicas = 1
				if err := operator.Client.Update(operator.Ctx, current); err != nil {
					t.Fatal(err)
				}
				group.Spec.Persistence.Size = "40Gi"
				group.Spec.Persistence.VolumeAttributesClassName = nil
				group.Spec.Image = testUpdatedMarkLogicImage
				group.Spec.NodeSelector = map[string]string{"nodepool": "updated"}
				if err := operator.Client.Update(operator.Ctx, group); err != nil {
					t.Fatal(err)
				}
				claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
				if err := operator.Client.Update(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
				if guarded {
					if _, operation, err := operator.claimVACStorageOperation(group, claim, nil); err != nil || operation == nil {
						t.Fatalf("claim VAC guard: operation=%+v, err=%v", operation, err)
					}
				}
				before := claim.DeepCopy()
				outcome, err := operator.ReconsileMarklogicGroupHandler()
				if err != nil || outcome.RequeueAfter != 30*time.Second {
					t.Fatalf("VAC wait must retain its poll without stopping unrelated work: %+v, %v", outcome, err)
				}
				actual := &appsv1.StatefulSet{}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
					t.Fatal(err)
				}
				if actual.UID != current.UID || actual.Spec.Template.Spec.Containers[0].Image != group.Spec.Image || actual.Spec.Template.Spec.NodeSelector["nodepool"] != "updated" {
					t.Fatal("VAC wait must allow safe image and scheduling updates without replacing the workload")
				}
				saved := &marklogicv1.MarklogicGroup{}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(group), saved); err != nil {
					t.Fatal(err)
				}
				if saved.Status.VolumeResizeStatus != nil {
					t.Fatal("pending VAC work must not claim or dispatch resize")
				}
				if dynamic && (saved.Status.Dynamic == nil || saved.Status.Dynamic.Reason != dynamicReasonInvalidConfig) {
					t.Fatal("VAC wait must not skip dynamic-group configuration reconciliation")
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, claim) {
					t.Fatal("waiting for VAC must leave the PVC unchanged")
				}
				claim.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("premium")
				if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
				saved.Spec.Image = testUpdatedMarkLogicImage + "-next"
				if err := operator.Client.Update(operator.Ctx, saved); err != nil {
					t.Fatal(err)
				}
				operator.MarklogicGroup = saved
				outcome, err = operator.ReconsileMarklogicGroupHandler()
				if err != nil || outcome.RequeueAfter != time.Second {
					t.Fatalf("settled VAC must allow resize to start with its shorter retry: %+v, %v", outcome, err)
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(group), saved); err != nil {
					t.Fatal(err)
				}
				if saved.Status.VolumeResizeStatus == nil || saved.Status.VolumeResizeStatus.Phase != marklogicv1.VolumeResizePhaseResizingPVCs {
					t.Fatal("resize must resume after native VAC work settles")
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
					t.Fatal(err)
				}
				if actual.Spec.Template.Spec.Containers[0].Image != testUpdatedMarkLogicImage {
					t.Fatal("starting a real resize must retain the normal handler stop boundary")
				}
			})
		}
	}
}

func TestReconsileMarklogicGroupHandlerSkipsServiceCreateWhenDynamicGroupDeleting(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}

	now := metav1.Now()
	group := &marklogicv1.MarklogicGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicGroup"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "dynamic",
			Namespace:         "ml-dynamic-host",
			DeletionTimestamp: &now,
			Finalizers:        []string{dynamicGroupCleanupFinalizer},
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:      "dynamic",
			IsDynamic: true,
			Service: marklogicv1.Service{
				Type: corev1.ServiceTypeClusterIP,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
		WithObjects(group).
		Build()

	oc := &OperatorContext{
		Ctx:            context.Background(),
		Client:         fakeClient,
		Scheme:         scheme,
		MarklogicGroup: group,
		Recorder:       record.NewFakeRecorder(10),
	}

	if _, err := oc.ReconsileMarklogicGroupHandler(); err != nil {
		t.Fatalf("ReconsileMarklogicGroupHandler returned error: %v", err)
	}

	current := &marklogicv1.MarklogicGroup{}
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "dynamic", Namespace: "ml-dynamic-host"}, current)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("failed to fetch MarklogicGroup: %v", err)
		}
	} else {
		for _, finalizer := range current.Finalizers {
			if finalizer == dynamicGroupCleanupFinalizer {
				t.Fatalf("expected dynamic cleanup finalizer to be removed during delete reconcile")
			}
		}
	}

	svc := &corev1.Service{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "dynamic", Namespace: "ml-dynamic-host"}, svc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no service to be created for deleting dynamic group, got err=%v", err)
	}
}
