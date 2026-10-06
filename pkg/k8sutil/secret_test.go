// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileSecretCreatesAndReusesOperatorCredentials(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "search",
			Namespace: "database",
			UID:       types.UID("cluster-uid"),
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Auth: &marklogicv1.AdminAuth{SecretName: stringPointer("bootstrap")},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	ctx := &ClusterContext{
		Ctx:              context.Background(),
		Client:           fakeClient,
		Scheme:           scheme,
		MarklogicCluster: cluster,
	}

	if result := ctx.ReconcileSecret(); result.Completed() {
		t.Fatalf("first reconcile unexpectedly completed: %v", result)
	}

	secretKey := types.NamespacedName{Name: "search-operator", Namespace: "database"}
	secret := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(), secretKey, secret); err != nil {
		t.Fatalf("get generated operator Secret: %v", err)
	}
	if string(secret.Data["username"]) != operatorUsername {
		t.Fatalf("operator username = %q, want %q", secret.Data["username"], operatorUsername)
	}
	password := string(secret.Data["password"])
	if len(password) != operatorPasswordLength {
		t.Fatalf("operator password length = %d, want %d", len(password), operatorPasswordLength)
	}
	for _, char := range password {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", char) {
			t.Fatalf("operator password contains unsupported character %q", char)
		}
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != cluster.Name || secret.OwnerReferences[0].Kind != "MarklogicCluster" {
		t.Fatalf("operator Secret owner references = %+v, want the owning MarklogicCluster", secret.OwnerReferences)
	}

	if result := ctx.ReconcileSecret(); result.Completed() {
		t.Fatalf("second reconcile unexpectedly completed: %v", result)
	}
	if err := fakeClient.Get(context.Background(), secretKey, secret); err != nil {
		t.Fatalf("get reused operator Secret: %v", err)
	}
	if string(secret.Data["password"]) != password {
		t.Fatal("operator password changed on a subsequent reconcile")
	}

	secret.Data["password"] = []byte("rotated-by-user")
	if err := fakeClient.Update(context.Background(), secret); err != nil {
		t.Fatalf("update operator Secret for rotation: %v", err)
	}
	if result := ctx.ReconcileSecret(); result.Completed() {
		t.Fatalf("rotation reconcile unexpectedly completed: %v", result)
	}
	if err := fakeClient.Get(context.Background(), secretKey, secret); err != nil {
		t.Fatalf("get rotated operator Secret: %v", err)
	}
	if string(secret.Data["password"]) != "rotated-by-user" {
		t.Fatal("operator Secret reconciliation overwrote the rotated password")
	}
}

func TestReconcileSecretDoesNotCreateGeneratedSecretWhenUserSecretIsConfigured(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	cluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "database"},
		Spec: marklogicv1.MarklogicClusterSpec{
			Auth: &marklogicv1.AdminAuth{
				SecretName:         stringPointer("bootstrap"),
				OperatorSecretName: stringPointer("custom-operator-auth"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	ctx := &ClusterContext{Ctx: context.Background(), Client: fakeClient, Scheme: scheme, MarklogicCluster: cluster}

	if result := ctx.ReconcileSecret(); result.Completed() {
		t.Fatalf("reconcile unexpectedly completed: %v", result)
	}
	secret := &corev1.Secret{}
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "search-operator", Namespace: "database"}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected generated operator Secret to be skipped, got err=%v", err)
	}
}

func TestReconcileOperatorUserUsesConfiguredSecretAndBootstrapCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	const bootstrapHost = "bootstrap-0.bootstrap.database.svc.cluster.local"
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap",
			Namespace: "database",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "marklogic.progress.com/v1",
				Kind:       "MarklogicCluster",
				Name:       "search",
			}},
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:          "bootstrap",
			SecretName:    "bootstrap-admin",
			BootstrapHost: bootstrapHost,
			Auth: &marklogicv1.AdminAuth{
				OperatorSecretName: stringPointer("custom-operator-auth"),
			},
		},
	}
	adminSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-admin", Namespace: "database"}, Data: map[string][]byte{
		"username": []byte("bootstrap-admin"),
		"password": []byte("bootstrap-password"),
	}}
	operatorSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "custom-operator-auth", Namespace: "database"}, Data: map[string][]byte{
		"password": []byte("user-provided-password"),
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&marklogicv1.MarklogicGroup{}).WithObjects(group, adminSecret, operatorSecret).Build()
	listCalls := 0
	roleCalls := 0
	operatorUserCalls := 0
	stub := &stubDynamicManagementClient{
		hostStatuses: []mlmanage.HostStatus{{Name: bootstrapHost, Online: true}},
		listHostStatusesFn: func() ([]mlmanage.HostStatus, error) {
			listCalls++
			if listCalls == 1 {
				return nil, fmt.Errorf("management api GET /manage/v2/hosts returned status 401")
			}
			return []mlmanage.HostStatus{{Name: bootstrapHost, Online: true}}, nil
		},
		ensureOperatorRoleFn: func() error {
			roleCalls++
			return nil
		},
		ensureOperatorUser: func(username, password string) error {
			operatorUserCalls++
			if username != operatorUsername || password != "user-provided-password" {
				t.Fatalf("EnsureOperatorUser got (%q, %q)", username, password)
			}
			return nil
		},
	}
	originalFactory := NewDynamicManagementClient
	factoryCalls := 0
	NewDynamicManagementClient = func(options mlmanage.ClientOptions) mlmanage.Client {
		factoryCalls++
		if options.Host != bootstrapHost {
			t.Fatalf("unexpected management host: %+v", options)
		}
		if factoryCalls == 1 {
			if options.Username != operatorUsername || options.Password != "user-provided-password" {
				t.Fatalf("first client should use operator credentials: %+v", options)
			}
		} else if options.Username != "bootstrap-admin" || options.Password != "bootstrap-password" {
			t.Fatalf("recovery client should use bootstrap credentials: %+v", options)
		}
		return stub
	}
	defer func() { NewDynamicManagementClient = originalFactory }()

	recorder := record.NewFakeRecorder(1)
	oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group, Recorder: recorder}
	if result := oc.ReconcileOperatorUser(); !result.Completed() {
		t.Fatal("expected bootstrap to requeue for a follow-up reconciliation using operator credentials")
	}
	if roleCalls != 1 || operatorUserCalls != 1 {
		t.Fatalf("operator role/user calls = %d/%d, want 1/1", roleCalls, operatorUserCalls)
	}
	if factoryCalls != 2 {
		t.Fatalf("management clients created = %d, want operator then bootstrap admin", factoryCalls)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Normal OperatorIdentityReconciled") {
			t.Fatalf("credential recovery event = %q, want Normal OperatorIdentityReconciled", event)
		}
	case <-time.After(time.Second):
		t.Fatal("reconciling the operator identity did not emit a Normal event")
	}
}

func TestReconcileOperatorUserReportsMissingSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap",
			Namespace: "database",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "marklogic.progress.com/v1",
				Kind:       "MarklogicCluster",
				Name:       "search",
			}},
		},
		Spec: marklogicv1.MarklogicGroupSpec{Name: "bootstrap"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
		WithObjects(group).
		Build()
	recorder := record.NewFakeRecorder(1)
	oc := &OperatorContext{
		Ctx:            context.Background(),
		Client:         fakeClient,
		MarklogicGroup: group,
		Recorder:       recorder,
	}

	reconcileResult := oc.ReconcileOperatorUser()
	output, err := reconcileResult.Output()
	if err != nil {
		t.Fatalf("reconcile operator credentials: %v", err)
	}
	if !output.Requeue || output.RequeueAfter <= 0 {
		t.Fatalf("missing operator Secret should requeue, got %+v", output)
	}

	updated := &marklogicv1.MarklogicGroup{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: group.Namespace, Name: group.Name}, updated); err != nil {
		t.Fatalf("get updated MarklogicGroup: %v", err)
	}
	var condition *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == operatorCredentialsReadyCondition {
			condition = &updated.Status.Conditions[i]
			break
		}
	}
	if condition == nil {
		t.Fatal("OperatorCredentialsReady condition was not set")
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != "OperatorSecretUnavailable" {
		t.Fatalf("credential condition = %s/%s, want False/OperatorSecretUnavailable", condition.Status, condition.Reason)
	}
	if !strings.Contains(condition.Message, "search-operator") || !strings.Contains(condition.Message, "database") {
		t.Fatalf("credential condition is not actionable: %q", condition.Message)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning OperatorSecretUnavailable") {
			t.Fatalf("credential event = %q, want Warning OperatorSecretUnavailable", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing operator Secret did not emit a Kubernetes Warning event")
	}
}

func TestReconcileOperatorUserRepairsRoleDriftWhenOperatorCredentialsWork(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	const bootstrapHost = "bootstrap-0.bootstrap.database.svc.cluster.local"
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bootstrap", Namespace: "database",
			OwnerReferences: []metav1.OwnerReference{{Kind: "MarklogicCluster", Name: "search"}},
		},
		Spec: marklogicv1.MarklogicGroupSpec{Name: "bootstrap", BootstrapHost: bootstrapHost},
	}
	operatorSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "search-operator", Namespace: "database"},
		Data:       map[string][]byte{"password": []byte("generated-operator-password")},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-admin", Namespace: "database"},
		Data:       map[string][]byte{"username": []byte("bootstrap-admin"), "password": []byte("bootstrap-password")},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&marklogicv1.MarklogicGroup{}).WithObjects(group, operatorSecret, adminSecret).Build()
	roleCalls := 0
	userCalls := 0
	stub := &stubDynamicManagementClient{
		hostStatuses:         []mlmanage.HostStatus{{Name: bootstrapHost, Online: true}},
		ensureOperatorRoleFn: func() error { roleCalls++; return nil },
		ensureOperatorUser:   func(username, password string) error { userCalls++; return nil },
	}
	originalFactory := NewDynamicManagementClient
	factoryCalls := 0
	NewDynamicManagementClient = func(options mlmanage.ClientOptions) mlmanage.Client {
		factoryCalls++
		if factoryCalls == 1 {
			if options.Username != operatorUsername || options.Password != "generated-operator-password" {
				t.Fatalf("expected operator credentials, got %+v", options)
			}
		} else if options.Username != "bootstrap-admin" || options.Password != "bootstrap-password" {
			t.Fatalf("expected retained bootstrap admin credentials, got %+v", options)
		}
		return stub
	}
	defer func() { NewDynamicManagementClient = originalFactory }()

	recorder := record.NewFakeRecorder(1)
	oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group, Recorder: recorder}
	reconcileResult := oc.ReconcileOperatorUser()
	output, err := reconcileResult.Output()
	if err != nil {
		t.Fatalf("reconcile operator credentials: %v", err)
	}
	if !output.Requeue || output.RequeueAfter <= 0 {
		t.Fatalf("healthy operator reconciliation should persist handoff and requeue, got %+v", output)
	}
	if factoryCalls != 2 || roleCalls != 1 || userCalls != 1 {
		t.Fatalf("management clients/role/user calls = %d/%d/%d, want 2/1/1", factoryCalls, roleCalls, userCalls)
	}
	updated := &marklogicv1.MarklogicGroup{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: group.Namespace, Name: group.Name}, updated); err != nil {
		t.Fatalf("get updated MarklogicGroup: %v", err)
	}
	if updated.Status.CredentialSecretName != "search-operator" {
		t.Fatalf("active credential Secret = %q, want search-operator", updated.Status.CredentialSecretName)
	}
	var condition *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == operatorCredentialsReadyCondition {
			condition = &updated.Status.Conditions[i]
			break
		}
	}
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "OperatorCredentialsActive" {
		t.Fatalf("credential condition = %+v, want True/OperatorCredentialsActive", condition)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Normal OperatorCredentialsActive") {
			t.Fatalf("credential activation event = %q, want Normal OperatorCredentialsActive", event)
		}
	case <-time.After(time.Second):
		t.Fatal("activating operator credentials did not emit a Normal event")
	}
}

func stringPointer(value string) *string {
	return &value
}
