// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"errors"
	"reflect"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

func TestDynamicCredentialRotationCleanupReleasesTerminatingPodFinalizer(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}

	replicas := int32(2)
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "database"},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:      "dynamic",
			Replicas:  &replicas,
			IsDynamic: true,
		},
	}
	labels := getSelectorLabelsByComponent("dynamic", true)
	terminating := credentialRevisionPod("dynamic-0", labels, "old")
	terminating.Namespace = group.Namespace
	terminating.Finalizers = []string{dynamicHostCleanupFinalizer}
	deletionTimestamp := metav1.Now()
	terminating.DeletionTimestamp = &deletionTimestamp
	stable := credentialRevisionPod("dynamic-1", labels, "old")
	stable.Namespace = group.Namespace
	stable.Finalizers = []string{dynamicHostCleanupFinalizer}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminating, stable).Build()
	oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group}

	for reconcileCount := 0; reconcileCount < 5; reconcileCount++ {
		cleanupResult := oc.reconcileDynamicCredentialRotationCleanup()
		if cleanupResult.Completed() {
			_, err := cleanupResult.Output()
			if err != nil {
				t.Fatalf("cleanup reconcile %d returned error: %v", reconcileCount+1, err)
			}
		}
	}

	current := &corev1.Pod{}
	err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(terminating), current)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get terminating pod: %v", err)
	}
	if err == nil {
		for _, finalizer := range current.Finalizers {
			if finalizer == dynamicHostCleanupFinalizer {
				t.Fatal("expected rotation cleanup to release the terminating pod finalizer")
			}
		}
	}

	currentStable := &corev1.Pod{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stable), currentStable); err != nil {
		t.Fatalf("get stable pod: %v", err)
	}
	if !containsString(currentStable.Finalizers, dynamicHostCleanupFinalizer) {
		t.Fatal("expected cleanup finalizer to remain on the non-terminating pod")
	}
}

func TestHandlerRestoresBootstrapPodWhileCredentialValidationIsPending(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}

	desiredReplicas := int32(1)
	currentReplicas := int32(0)
	desiredOperatorSecret := "search-operator-next"
	group := &marklogicv1.MarklogicGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicGroup"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "bootstrap", Namespace: "database", UID: types.UID("bootstrap-group"),
			OwnerReferences: []metav1.OwnerReference{{Kind: "MarklogicCluster", Name: "search"}},
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:          "bootstrap",
			Replicas:      &desiredReplicas,
			SecretName:    "search-admin",
			Auth:          &marklogicv1.AdminAuth{OperatorSecretName: &desiredOperatorSecret},
			HugePages:     &marklogicv1.HugePages{},
			LogCollection: &marklogicv1.LogCollection{},
		},
		Status: marklogicv1.MarklogicGroupStatus{CredentialSecretName: "search-operator-active"},
	}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "database"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &currentReplicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{credentialRevisionAnnotation: "last-validated-revision"}},
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
					Name: "mloperator-secrets",
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: "search-operator-active",
					}},
				}}},
			},
		},
	}
	originalTemplate := statefulSet.Spec.Template.DeepCopy()
	activeOperatorSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "search-operator-active", Namespace: "database"},
		Data:       map[string][]byte{"password": []byte("last-validated-password")},
	}
	desiredOperatorSecretObject := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: desiredOperatorSecret, Namespace: "database"},
		Data:       map[string][]byte{"password": []byte("unverified-password")},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
		WithObjects(group, statefulSet, activeOperatorSecret, desiredOperatorSecretObject).
		Build()
	originalFactory := NewDynamicManagementClient
	NewDynamicManagementClient = func(options mlmanage.ClientOptions) mlmanage.Client {
		if options.Password != "unverified-password" {
			t.Fatalf("validation password = %q, want desired unverified credential", options.Password)
		}
		return &stubDynamicManagementClient{listHostStatusesFn: func() ([]mlmanage.HostStatus, error) {
			return nil, errors.New("bootstrap API unavailable")
		}}
	}
	defer func() { NewDynamicManagementClient = originalFactory }()

	oc := &OperatorContext{
		Ctx: context.Background(), Client: fakeClient, Scheme: scheme,
		MarklogicGroup: group, Recorder: record.NewFakeRecorder(10),
	}
	reconcileResult, err := oc.ReconsileMarklogicGroupHandler()
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !reconcileResult.Requeue || reconcileResult.RequeueAfter <= 0 {
		t.Fatalf("handler result = %+v, want pending credential validation requeue", reconcileResult)
	}

	currentStatefulSet := &appsv1.StatefulSet{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(statefulSet), currentStatefulSet); err != nil {
		t.Fatalf("get recovered StatefulSet: %v", err)
	}
	if currentStatefulSet.Spec.Replicas == nil || *currentStatefulSet.Spec.Replicas != 1 {
		t.Fatalf("recovered replicas = %v, want 1", currentStatefulSet.Spec.Replicas)
	}
	if !reflect.DeepEqual(&currentStatefulSet.Spec.Template, originalTemplate) {
		t.Fatal("bootstrap recovery changed the last validated Pod template")
	}
	params := generateContainerParams(group)
	if params.OperatorSecretName != "search-operator-active" {
		t.Fatalf("selected operator Secret = %q, want last validated Secret", params.OperatorSecretName)
	}
}

func TestCredentialValidationRecoveryOnlyRestoresBootstrapFromZero(t *testing.T) {
	tests := []struct {
		name          string
		bootstrapHost string
		current       int32
		desired       int32
		want          int32
	}{
		{name: "non-bootstrap group", bootstrapHost: "bootstrap-0.bootstrap.database.svc.cluster.local", current: 0, desired: 1, want: 0},
		{name: "scale down", current: 1, desired: 0, want: 1},
		{name: "additional scale up", current: 1, desired: 3, want: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add apps scheme: %v", err)
			}
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "database"},
				Spec: marklogicv1.MarklogicGroupSpec{
					Name: "bootstrap", BootstrapHost: test.bootstrapHost, Replicas: &test.desired,
				},
			}
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "database"},
				Spec:       appsv1.StatefulSetSpec{Replicas: &test.current},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(statefulSet).Build()
			oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group}

			if err := oc.reconcileBootstrapStatefulSetRecovery(statefulSet); err != nil {
				t.Fatalf("reconcile recovery: %v", err)
			}
			currentStatefulSet := &appsv1.StatefulSet{}
			if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(statefulSet), currentStatefulSet); err != nil {
				t.Fatalf("get StatefulSet: %v", err)
			}
			if currentStatefulSet.Spec.Replicas == nil || *currentStatefulSet.Spec.Replicas != test.want {
				t.Fatalf("replicas = %v, want %d", currentStatefulSet.Spec.Replicas, test.want)
			}
		})
	}
}
