// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Every credential value in these tests carries this marker so leaks are detectable.
const secretMarker = "SECRET-VALUE"

type recordedEvent struct {
	eventType string
	reason    string
	message   string
}

type fakeCredentialClient struct {
	h        *osHarness
	readyErr error
	putErr   map[objectStorageProvider]error
	onPut    func(provider objectStorageProvider)

	probes int
	aws    []mlmanage.AWSCredentials
	azure  []mlmanage.AzureCredentials
}

func (f *fakeCredentialClient) CheckBootstrapReady(context.Context) error {
	f.probes++
	f.h.log("probe")
	return f.readyErr
}

func (f *fakeCredentialClient) ApplyAWSCredentials(_ context.Context, creds mlmanage.AWSCredentials) error {
	f.h.log("put:aws")
	f.aws = append(f.aws, creds)
	if f.onPut != nil {
		f.onPut(providerAWS)
	}
	return f.putErr[providerAWS]
}

func (f *fakeCredentialClient) ApplyAzureCredentials(_ context.Context, creds mlmanage.AzureCredentials) error {
	f.h.log("put:azure")
	f.azure = append(f.azure, creds)
	if f.onPut != nil {
		f.onPut(providerAzure)
	}
	return f.putErr[providerAzure]
}

type opRecorder struct{ h *osHarness }

func (r opRecorder) Event(_ runtime.Object, eventType, reason, message string) {
	r.h.log("event:" + reason)
	r.h.events = append(r.h.events, recordedEvent{eventType: eventType, reason: reason, message: message})
}

func (r opRecorder) Eventf(obj runtime.Object, eventType, reason, format string, args ...interface{}) {
	r.Event(obj, eventType, reason, fmt.Sprintf(format, args...))
}

func (r opRecorder) AnnotatedEventf(obj runtime.Object, _ map[string]string, eventType, reason, format string, args ...interface{}) {
	r.Event(obj, eventType, reason, fmt.Sprintf(format, args...))
}

type osHarness struct {
	t      *testing.T
	scheme *runtime.Scheme
	c      client.Client
	creds  *fakeCredentialClient

	ops    []string
	events []recordedEvent

	statusWriteErr func() error
	secretGetErr   map[string]error
	createErr      func(obj client.Object) error
}

const (
	osNamespace = "ml-ns"
	osCluster   = "ml"
)

func newOSHarness(t *testing.T, mutate func(*marklogicv1.MarklogicCluster)) *osHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}

	h := &osHarness{t: t, scheme: scheme, secretGetErr: map[string]error{}}
	h.creds = &fakeCredentialClient{h: h, putErr: map[objectStorageProvider]error{}}

	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: osCluster, Namespace: osNamespace, UID: "cluster-uid-1", Generation: 1},
		Spec: marklogicv1.MarklogicClusterSpec{
			ClusterDomain: "cluster.local",
			Image:         "progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6",
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: "dnode", IsBootstrap: true, GroupConfig: &marklogicv1.GroupConfig{Name: "Default"}},
			},
			ObjectStorage: &marklogicv1.ObjectStorageConfig{
				AWS:   &marklogicv1.AWSObjectStorage{AuthType: marklogicv1.ObjectStorageAuthSecret, SecretName: "aws-creds"},
				Azure: &marklogicv1.AzureObjectStorage{AuthType: marklogicv1.ObjectStorageAuthSecret, SecretName: "azure-creds"},
			},
		},
	}
	if mutate != nil {
		mutate(cluster)
	}

	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: osCluster + "-admin", Namespace: osNamespace},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("admin-" + secretMarker)},
	}

	h.c = fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicCluster{}).
		WithObjects(cluster, admin).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if h.statusWriteErr != nil {
					if err := h.statusWriteErr(); err != nil {
						h.log("status-write-failed")
						return err
					}
				}
				h.log("status-write")
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					if err, found := h.secretGetErr[key.Name]; found {
						return err
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if h.createErr != nil {
					if err := h.createErr(obj); err != nil {
						return err
					}
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	t.Cleanup(h.assertNoSecretLeak)
	return h
}

func (h *osHarness) log(op string) { h.ops = append(h.ops, op) }

func (h *osHarness) resetObservations() {
	h.ops = nil
	h.events = nil
	h.creds.aws = nil
	h.creds.azure = nil
	h.creds.probes = 0
}

func (h *osHarness) assertNoSecretLeak() {
	h.t.Helper()
	cluster := h.cluster()
	raw, _ := json.Marshal(cluster.Status)
	if strings.Contains(string(raw), secretMarker) {
		h.t.Errorf("cluster status exposes credential material")
	}
	for _, event := range h.events {
		if strings.Contains(event.message, secretMarker) {
			h.t.Errorf("event exposes credential material")
		}
	}
}

func (h *osHarness) cluster() *marklogicv1.MarklogicCluster {
	h.t.Helper()
	cluster := &marklogicv1.MarklogicCluster{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: osNamespace, Name: osCluster}, cluster); err != nil {
		h.t.Fatalf("get cluster: %v", err)
	}
	return cluster
}

func (h *osHarness) entry(provider objectStorageProvider) *marklogicv1.ObjectStorageProviderStatus {
	h.t.Helper()
	return providerEntry(h.cluster().Status.ObjectStorage, provider)
}

func (h *osHarness) mutateSpec(mutate func(*marklogicv1.MarklogicCluster)) {
	h.t.Helper()
	cluster := h.cluster()
	mutate(cluster)
	cluster.Generation++
	if err := h.c.Update(context.Background(), cluster); err != nil {
		h.t.Fatalf("update cluster: %v", err)
	}
}

func (h *osHarness) setSecret(name string, uid types.UID, data map[string]string) {
	h.t.Helper()
	secret := &corev1.Secret{}
	err := h.c.Get(context.Background(), types.NamespacedName{Namespace: osNamespace, Name: name}, secret)
	bytes := map[string][]byte{}
	for key, value := range data {
		bytes[key] = []byte(value)
	}
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: osNamespace, UID: uid}, Data: bytes}
		if err := h.c.Create(context.Background(), secret); err != nil {
			h.t.Fatalf("create secret: %v", err)
		}
		return
	} else if err != nil {
		h.t.Fatalf("get secret: %v", err)
	}
	secret.Data = bytes
	if err := h.c.Update(context.Background(), secret); err != nil {
		h.t.Fatalf("update secret: %v", err)
	}
}

func (h *osHarness) touchSecret(name string) {
	h.t.Helper()
	secret := &corev1.Secret{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: osNamespace, Name: name}, secret); err != nil {
		h.t.Fatalf("get secret: %v", err)
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations["touched"] = fmt.Sprint(len(secret.Annotations))
	if err := h.c.Update(context.Background(), secret); err != nil {
		h.t.Fatalf("update secret: %v", err)
	}
}

func (h *osHarness) deleteSecret(name string) {
	h.t.Helper()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: osNamespace}}
	if err := h.c.Delete(context.Background(), secret); err != nil {
		h.t.Fatalf("delete secret: %v", err)
	}
}

func (h *osHarness) context() *ClusterContext {
	cluster := h.cluster()
	return &ClusterContext{
		Ctx:                     context.Background(),
		Client:                  h.c,
		Scheme:                  h.scheme,
		MarklogicCluster:        cluster,
		ReqLogger:               logr.Discard(),
		Recorder:                opRecorder{h: h},
		CredentialClientFactory: func(mlmanage.ClientOptions) mlmanage.CredentialClient { return h.creds },
	}
}

func (h *osHarness) reconcile() (reconcile.Result, error) {
	return h.context().ReconcileObjectStorage()
}

func (h *osHarness) mustReconcile() reconcile.Result {
	h.t.Helper()
	res, err := h.reconcile()
	if err != nil {
		h.t.Fatalf("ReconcileObjectStorage returned error: %v", err)
	}
	return res
}

func awsMaterial(suffix string) map[string]string {
	return map[string]string{"accessKey": secretMarker + "-ak-" + suffix, "secretKey": secretMarker + "-sk-" + suffix}
}

func azureMaterial(suffix string) map[string]string {
	return map[string]string{"storageAccount": "acct-" + suffix, "storageKey": secretMarker + "-key-" + suffix}
}

func (h *osHarness) seedBothSecrets() {
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	h.setSecret("azure-creds", "azure-uid-1", azureMaterial("1"))
}

func (h *osHarness) opIndex(op string) int {
	for i, got := range h.ops {
		if got == op {
			return i
		}
	}
	return -1
}

func (h *osHarness) countOps(op string) int {
	count := 0
	for _, got := range h.ops {
		if got == op {
			count++
		}
	}
	return count
}

func (h *osHarness) eventReasons() []string {
	var reasons []string
	for _, event := range h.events {
		reasons = append(reasons, event.reason)
	}
	return reasons
}

func assertPhase(t *testing.T, entry *marklogicv1.ObjectStorageProviderStatus, phase marklogicv1.ObjectStoragePhase, reason marklogicv1.ObjectStorageReason) {
	t.Helper()
	if entry == nil {
		t.Fatalf("expected a status entry with phase %s, got none", phase)
	}
	if entry.Phase != phase || entry.Reason != reason {
		t.Fatalf("phase/reason = %s/%s, want %s/%s", entry.Phase, entry.Reason, phase, reason)
	}
}

func assertEligible(t *testing.T, entry *marklogicv1.ObjectStorageProviderStatus, want bool) {
	t.Helper()
	if isDetachEligible(entry) != want {
		t.Fatalf("detachEligible = %v, want %v", isDetachEligible(entry), want)
	}
}

func appliedBothProviders(t *testing.T) *osHarness {
	t.Helper()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
	assertPhase(t, h.entry(providerAzure), marklogicv1.ObjectStoragePhaseApplied, "")
	h.resetObservations()
	return h
}

var errForbidden = apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", errors.New("denied"))
