// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	objectStorageNamespace = "ml"
	objectStorageCluster   = "test-cluster"
	objectStorageUID       = "9b7c1f0e-2a3d-4c5b-8e6f-1d2c3b4a5968"
	bootstrapFQDN          = "dnode-0.dnode.ml.svc.cluster.local"
)

// stubObjectStorageClient records what the reconcile applied.
type stubObjectStorageClient struct {
	mlmanage.Client
	hosts      []mlmanage.HostStatus
	hostsErr   error
	awsCalls   []mlmanage.AWSCredentials
	azureCalls []mlmanage.AzureCredentials
	awsErr     error
	azureErr   error
}

func (s *stubObjectStorageClient) ListHostsStatus(context.Context) ([]mlmanage.HostStatus, error) {
	if s.hostsErr != nil {
		return nil, s.hostsErr
	}
	return s.hosts, nil
}

func (s *stubObjectStorageClient) EnsureAWSCredentials(_ context.Context, config mlmanage.AWSCredentials) error {
	s.awsCalls = append(s.awsCalls, config)
	return s.awsErr
}

func (s *stubObjectStorageClient) EnsureAzureCredentials(_ context.Context, config mlmanage.AzureCredentials) error {
	s.azureCalls = append(s.azureCalls, config)
	return s.azureErr
}

func objectStorageScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("marklogic scheme: %v", err)
	}
	return scheme
}

func objectStorageTestCluster(config *marklogicv1.ObjectStorageConfig) *marklogicv1.MarklogicCluster {
	secretName := "ml-admin"
	return &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectStorageCluster,
			Namespace: objectStorageNamespace,
			UID:       objectStorageUID,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			ClusterDomain: "cluster.local",
			Auth:          &marklogicv1.AdminAuth{SecretName: &secretName},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: "dnode", IsBootstrap: true},
			},
			ObjectStorage: config,
		},
	}
}

func adminSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-admin", Namespace: objectStorageNamespace},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("admin-password"),
		},
	}
}

func awsSecret(accessKey, secretKey string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-s3", Namespace: objectStorageNamespace},
		Data: map[string][]byte{
			"accessKey": []byte(accessKey),
			"secretKey": []byte(secretKey),
		},
	}
}

func awsSecretWithSessionToken(accessKey, secretKey, sessionToken string) *corev1.Secret {
	secret := awsSecret(accessKey, secretKey)
	secret.Data["sessionToken"] = []byte(sessionToken)
	return secret
}

func newObjectStorageContext(t *testing.T, stub *stubObjectStorageClient, objects ...runtime.Object) *ClusterContext {
	t.Helper()

	scheme := objectStorageScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&marklogicv1.MarklogicCluster{})
	for _, object := range objects {
		builder = builder.WithRuntimeObjects(object)
	}

	original := NewObjectStorageManagementClient
	NewObjectStorageManagementClient = func(mlmanage.ClientOptions) mlmanage.Client { return stub }
	t.Cleanup(func() { NewObjectStorageManagementClient = original })

	var cluster *marklogicv1.MarklogicCluster
	for _, object := range objects {
		if candidate, ok := object.(*marklogicv1.MarklogicCluster); ok {
			cluster = candidate
		}
	}

	return &ClusterContext{
		Ctx:              context.Background(),
		Client:           builder.Build(),
		MarklogicCluster: cluster,
		ReqLogger:        logr.Discard(),
		Recorder:         record.NewFakeRecorder(20),
	}
}

func onlineBootstrap() *stubObjectStorageClient {
	return &stubObjectStorageClient{
		hosts: []mlmanage.HostStatus{{Name: bootstrapFQDN, Online: true, Version: "12.0.3"}},
	}
}

func TestReconcileObjectStorageAppliesBothProviders(t *testing.T) {
	stub := onlineBootstrap()
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS:   &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
		Azure: &marklogicv1.AzureObjectStorage{SecretName: "ml-azure"},
	})
	azure := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-azure", Namespace: objectStorageNamespace},
		Data:       map[string][]byte{"storageAccount": []byte("acct"), "storageKey": []byte("a2V5")},
	}

	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), awsSecret("AKIA123", "s3cret"), azure)

	if res := cc.ReconcileObjectStorage(); res.Completed() {
		t.Fatalf("expected reconcile to continue, got %+v", res)
	}

	if len(stub.awsCalls) != 1 || stub.awsCalls[0].AccessKey != "AKIA123" {
		t.Fatalf("unexpected AWS calls: %+v", stub.awsCalls)
	}
	if len(stub.azureCalls) != 1 || stub.azureCalls[0].StorageAccount != "acct" {
		t.Fatalf("unexpected Azure calls: %+v", stub.azureCalls)
	}

	status := cc.MarklogicCluster.Status.ObjectStorage
	if status == nil || status.AWS.Phase != marklogicv1.ObjectStoragePhaseApplied || status.Azure.Phase != marklogicv1.ObjectStoragePhaseApplied {
		t.Fatalf("expected both providers Applied, got %+v", status)
	}
	if status.AWS.AppliedFingerprint == "" || status.AWS.AppliedFingerprint == status.Azure.AppliedFingerprint {
		t.Fatal("expected distinct, non-empty fingerprints per provider")
	}
}

// sessionToken is optional: when present in the Secret it is passed through
// as-is; when absent, behavior is unchanged from long-lived IAM user keys.
func TestReconcileObjectStorageAWSSessionTokenIsOptionalPassThrough(t *testing.T) {
	stub := onlineBootstrap()
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS: &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
	})

	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), awsSecretWithSessionToken("AKIA123", "s3cret", "testSessionToken123"))

	if res := cc.ReconcileObjectStorage(); res.Completed() {
		t.Fatalf("expected reconcile to continue, got %+v", res)
	}

	if len(stub.awsCalls) != 1 || stub.awsCalls[0].SessionToken != "testSessionToken123" {
		t.Fatalf("expected sessionToken to be passed through, got %+v", stub.awsCalls)
	}
}

// An unchanged Secret must not produce a repeated write, otherwise every resync
// emits a spurious rotation event.
func TestReconcileObjectStorageSkipsUnchangedMaterial(t *testing.T) {
	stub := onlineBootstrap()
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS: &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
	})
	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), awsSecret("AKIA123", "s3cret"))

	cc.ReconcileObjectStorage()
	cc.ReconcileObjectStorage()

	if len(stub.awsCalls) != 1 {
		t.Fatalf("expected exactly 1 apply for unchanged material, got %d", len(stub.awsCalls))
	}
}

func TestReconcileObjectStorageReappliesRotatedMaterial(t *testing.T) {
	stub := onlineBootstrap()
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS: &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
	})
	secret := awsSecret("AKIA123", "s3cret")
	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), secret)

	cc.ReconcileObjectStorage()
	firstFingerprint := cc.MarklogicCluster.Status.ObjectStorage.AWS.AppliedFingerprint

	rotated := secret.DeepCopy()
	rotated.Data["secretKey"] = []byte("rotated-secret")
	if err := cc.Client.Update(cc.Ctx, rotated); err != nil {
		t.Fatalf("failed to rotate secret: %v", err)
	}

	cc.ReconcileObjectStorage()

	if len(stub.awsCalls) != 2 {
		t.Fatalf("expected re-apply after rotation, got %d calls", len(stub.awsCalls))
	}
	if cc.MarklogicCluster.Status.ObjectStorage.AWS.AppliedFingerprint == firstFingerprint {
		t.Fatal("expected the fingerprint to change after rotation")
	}
}

// A failure in one provider must not prevent the other from being configured.
func TestReconcileObjectStorageProvidersAreIndependent(t *testing.T) {
	stub := onlineBootstrap()
	stub.azureErr = &mlmanage.CredentialsError{StatusCode: http.StatusForbidden}
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS:   &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
		Azure: &marklogicv1.AzureObjectStorage{SecretName: "ml-azure"},
	})
	azure := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-azure", Namespace: objectStorageNamespace},
		Data:       map[string][]byte{"storageAccount": []byte("acct"), "storageKey": []byte("a2V5")},
	}

	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), awsSecret("AKIA123", "s3cret"), azure)
	cc.ReconcileObjectStorage()

	status := cc.MarklogicCluster.Status.ObjectStorage
	if status.AWS.Phase != marklogicv1.ObjectStoragePhaseApplied {
		t.Fatalf("expected AWS to succeed despite the Azure failure, got %+v", status.AWS)
	}
	if status.Azure.Phase != marklogicv1.ObjectStoragePhaseFailed ||
		status.Azure.Reason != marklogicv1.ObjectStorageReasonInsufficientPrivilege {
		t.Fatalf("expected Azure InsufficientPrivilege, got %+v", status.Azure)
	}
}

func TestReconcileObjectStorageSecretProblems(t *testing.T) {
	tests := map[string]struct {
		secret     *corev1.Secret
		wantReason marklogicv1.ObjectStorageFailureReason
	}{
		"secret missing": {
			secret:     nil,
			wantReason: marklogicv1.ObjectStorageReasonSecretNotFound,
		},
		"key missing": {
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ml-s3", Namespace: objectStorageNamespace},
				Data:       map[string][]byte{"accessKey": []byte("AKIA123")},
			},
			wantReason: marklogicv1.ObjectStorageReasonSecretKeyMissing,
		},
		"key present but empty": {
			secret:     awsSecret("AKIA123", "   "),
			wantReason: marklogicv1.ObjectStorageReasonSecretKeyMissing,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := onlineBootstrap()
			cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
				AWS: &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
			})

			objects := []runtime.Object{cluster, adminSecret()}
			if test.secret != nil {
				objects = append(objects, test.secret)
			}
			cc := newObjectStorageContext(t, stub, objects...)
			cc.ReconcileObjectStorage()

			status := cc.MarklogicCluster.Status.ObjectStorage.AWS
			if status.Phase != marklogicv1.ObjectStoragePhaseFailed || status.Reason != test.wantReason {
				t.Fatalf("expected %s, got %+v", test.wantReason, status)
			}
			if len(stub.awsCalls) != 0 {
				t.Fatal("no credential write should be attempted when the Secret is unusable")
			}
		})
	}
}

func TestReconcileObjectStorageWaitsForBootstrap(t *testing.T) {
	stub := &stubObjectStorageClient{hostsErr: errors.New("connection refused")}
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS: &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
	})
	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), awsSecret("AKIA123", "s3cret"))

	res := cc.ReconcileObjectStorage()
	if !res.Completed() {
		t.Fatal("expected the reconcile to stop and requeue while the bootstrap is unreachable")
	}

	status := cc.MarklogicCluster.Status.ObjectStorage.AWS
	if status.Phase != marklogicv1.ObjectStoragePhasePending ||
		status.Reason != marklogicv1.ObjectStorageReasonBootstrapNotReady {
		t.Fatalf("expected Pending/BootstrapNotReady, got %+v", status)
	}
	if len(stub.awsCalls) != 0 {
		t.Fatal("credentials must not be applied before the bootstrap host is ready")
	}
}

func TestReconcileObjectStorageNotDeclared(t *testing.T) {
	stub := onlineBootstrap()
	cluster := objectStorageTestCluster(nil)
	cc := newObjectStorageContext(t, stub, cluster, adminSecret())

	if res := cc.ReconcileObjectStorage(); res.Completed() {
		t.Fatalf("expected reconcile to continue, got %+v", res)
	}
	if cc.MarklogicCluster.Status.ObjectStorage != nil {
		t.Fatal("expected no object storage status when nothing is declared")
	}
	if len(stub.awsCalls) != 0 || len(stub.azureCalls) != 0 {
		t.Fatal("expected no Management API calls")
	}
}

// Status is world-readable to anyone with get on the resource.
func TestReconcileObjectStorageStatusNeverContainsMaterial(t *testing.T) {
	stub := onlineBootstrap()
	stub.awsErr = &mlmanage.CredentialsError{
		StatusCode:  http.StatusBadRequest,
		MessageCode: "MANAGE-INVALIDPAYLOAD",
		Message:     "Payload has errors in structure, content-type or values.",
	}
	cluster := objectStorageTestCluster(&marklogicv1.ObjectStorageConfig{
		AWS: &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
	})
	cc := newObjectStorageContext(t, stub, cluster, adminSecret(), awsSecret("AKIASECRETID", "super-secret-value"))
	cc.ReconcileObjectStorage()

	status := cc.MarklogicCluster.Status.ObjectStorage.AWS
	for _, material := range []string{"AKIASECRETID", "super-secret-value"} {
		if strings.Contains(status.Message, material) || strings.Contains(status.AppliedFingerprint, material) {
			t.Fatalf("status leaked credential material: %+v", status)
		}
	}
	if status.Reason != marklogicv1.ObjectStorageReasonInvalidPayload {
		t.Fatalf("expected InvalidPayload, got %s", status.Reason)
	}
}

func TestObjectStorageFailureReasonMapping(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want marklogicv1.ObjectStorageFailureReason
	}{
		"transport failure":    {&mlmanage.CredentialsError{StatusCode: 0}, marklogicv1.ObjectStorageReasonManagementAPIUnreachable},
		"bad request":          {&mlmanage.CredentialsError{StatusCode: http.StatusBadRequest}, marklogicv1.ObjectStorageReasonInvalidPayload},
		"unauthorized":         {&mlmanage.CredentialsError{StatusCode: http.StatusUnauthorized}, marklogicv1.ObjectStorageReasonAuthenticationFailed},
		"forbidden":            {&mlmanage.CredentialsError{StatusCode: http.StatusForbidden}, marklogicv1.ObjectStorageReasonInsufficientPrivilege},
		"server error":         {&mlmanage.CredentialsError{StatusCode: http.StatusBadGateway}, marklogicv1.ObjectStorageReasonManagementAPIUnreachable},
		"unexpected 4xx":       {&mlmanage.CredentialsError{StatusCode: http.StatusConflict}, marklogicv1.ObjectStorageReasonManagementAPIError},
		"non-credential error": {errors.New("boom"), marklogicv1.ObjectStorageReasonManagementAPIError},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := objectStorageFailureReason(test.err); got != test.want {
				t.Fatalf("expected %s, got %s", test.want, got)
			}
		})
	}
}

// Misconfigurations that cannot resolve on their own must not be retried in a loop.
func TestObjectStorageRequeuePolicy(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status *marklogicv1.ObjectStorageProviderStatus
		want   bool
	}{
		"applied":                {&marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseApplied}, false},
		"disabled":               {&marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseDisabled}, false},
		"invalid payload":        {&marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseFailed, Reason: marklogicv1.ObjectStorageReasonInvalidPayload}, false},
		"insufficient privilege": {&marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseFailed, Reason: marklogicv1.ObjectStorageReasonInsufficientPrivilege}, false},
		"secret not found":       {&marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseFailed, Reason: marklogicv1.ObjectStorageReasonSecretNotFound}, true},
		"unreachable":            {&marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseFailed, Reason: marklogicv1.ObjectStorageReasonManagementAPIUnreachable}, true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := objectStorageNeedsRequeue(test.status); got != test.want {
				t.Fatalf("expected %v, got %v", test.want, got)
			}
		})
	}
}
