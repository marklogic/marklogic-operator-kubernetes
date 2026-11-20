// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	pollInterval        = 2 * time.Second
	statusTimeout       = 3 * time.Minute
	podTimeout          = 12 * time.Minute
	reconcileAnnotation = "marklogic.progress.com/reconcile-request"
)

// Options identifies the installation under test. Restart scenarios require exclusive use of it.
type Options struct {
	Namespace         string
	MarkLogicImage    string
	OperatorNamespace string
	OperatorImage     string
}

type scenario struct {
	ctx             context.Context
	client          client.Client
	kube            kubernetes.Interface
	restConfig      *rest.Config
	opts            Options
	name            string
	clusterUID      types.UID
	group           string
	admin           string
	secrets         []*corev1.Secret
	namespace       *corev1.Namespace
	values          []string
	started         metav1.Time
	operatorSources map[types.UID]int32
	operatorEpochs  []operatorLogEpoch
}

// Run exercises real watches, controller restarts and credential token readback. Cloud
// authorization and request attribution need separate evidence; RunUpgrade covers installations.
func Run(ctx context.Context, t *testing.T, cfg *envconf.Config, opts Options) {
	t.Helper()
	credentials, err := loadFixture(os.Getenv("E2E_OBJECT_STORAGE_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Namespace == "" || opts.MarkLogicImage == "" || opts.OperatorNamespace == "" || opts.OperatorImage == "" {
		t.Fatal("object storage E2E requires explicit namespace, server image and candidate operator image")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{ml.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if add(scheme) != nil {
			t.Fatal("cannot register E2E resource types")
		}
	}
	// Bound individual requests too: mutation and diagnostic calls are outside polling loops.
	restConfig := rest.CopyConfig(cfg.Client().RESTConfig())
	restConfig.Timeout = 30 * time.Second
	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("cannot create E2E resource client")
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal("cannot create Kubernetes client")
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("cannot generate run identity")
	}
	id := hex.EncodeToString(suffix[:])
	s := &scenario{ctx: ctx, client: apiClient, kube: kube, restConfig: restConfig, opts: opts,
		name: "os-" + id, group: "os-node-" + id, admin: "os-admin-" + id, started: metav1.Now()}
	if credentials != nil {
		for _, values := range []material{credentials.AWS.Initial, credentials.AWS.Rotated, credentials.Azure.Initial, credentials.Azure.Rotated} {
			for _, value := range values {
				s.values = append(s.values, value, strings.TrimSpace(value))
			}
		}
	}
	t.Logf("status timeout=%s; pod timeout=%s; polling=%s; run=%s", statusTimeout, podTimeout, pollInterval, s.name)
	s.verifyOperator(t)
	t.Cleanup(func() { s.cleanup(t) })
	defer func() {
		if t.Failed() && s.clusterUID != "" {
			s.checkPublishedSafety(t)
		}
	}()
	s.setup(t)

	if !t.Run("OS-T05_cross_namespace_secret_does_not_resolve", func(t *testing.T) {
		foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.name + "-foreign"}}
		if s.client.Create(s.ctx, foreign) != nil {
			t.Fatal("cannot create owned foreign namespace for Secret isolation test")
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.client.Delete(ctx, foreign, client.Preconditions{UID: &foreign.UID}); err != nil && !apierrors.IsNotFound(err) {
				t.Error("cleanup: foreign Secret isolation namespace may remain")
			}
		})
		// Empty data is deliberate: resolving this existing foreign Secret would
		// produce SecretKeyMissing rather than the required local SecretNotFound.
		foreignSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.name + "-shared", Namespace: foreign.Name}, Type: corev1.SecretTypeOpaque}
		if s.client.Create(s.ctx, foreignSecret) != nil {
			t.Fatal("cannot create foreign same-name Secret")
		}
		// Force an observable evaluation after the foreign object exists; the
		// initial SecretNotFound status alone is not evidence of scope isolation.
		s.updateCluster(t, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage.AWS.Region = "us-east-1" })
		for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
			entry := s.waitPhase(t, provider, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretNotFound)
			if entry.ObservedSecret.UID != "" || entry.ObservedSecret.ResourceVersion != "" {
				t.Fatal("never-observed local Secret acquired a foreign identity or revision")
			}
		}
	}) {
		return
	}

	if !t.Run("OS-T05_missing_secret_and_revision_watch", func(t *testing.T) {
		s.waitPhase(t, testutils.ObjectStorageAWS, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretNotFound)
		s.waitPhase(t, testutils.ObjectStorageAzure, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretNotFound)
		// This contains no usable material: neither provider may reach the Management API.
		s.createSecret(t, s.name+"-shared", map[string][]byte{})
		s.waitPhase(t, testutils.ObjectStorageAWS, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
		s.waitPhase(t, testutils.ObjectStorageAzure, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
		s.updateSecret(t, s.name+"-shared", func(secret *corev1.Secret) { secret.Annotations = map[string]string{"e2e-revision": "1"} })
		s.waitPhase(t, testutils.ObjectStorageAWS, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
		s.waitPhase(t, testutils.ObjectStorageAzure, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
	}) {
		return
	}

	if !t.Run("OS-T11_scale_while_credentials_invalid", func(t *testing.T) {
		s.waitPod(t, 0)
		s.updateCluster(t, func(c *ml.MarklogicCluster) { replicas := int32(2); c.Spec.MarkLogicGroups[0].Replicas = &replicas })
		s.waitPod(t, 1)
		s.waitPhase(t, testutils.ObjectStorageAWS, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
		s.waitPhase(t, testutils.ObjectStorageAzure, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
	}) {
		return
	}

	t.Run("credential_lifecycle", func(t *testing.T) {
		if credentials == nil {
			t.Skip("BLOCKED: E2E_OBJECT_STORAGE_FIXTURE is unset; real credential application/rotation/detachment not exercised")
		}
		s.lifecycle(t, credentials)
	})
	// Do not dump resource bodies or operator logs, even when a preceding assertion failed.
	t.Run("OS-T14_sanitized_status_events_and_operator_logs", func(t *testing.T) { s.checkPublishedSafety(t) })
}

func (s *scenario) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: s.opts.Namespace, Name: name}
}

func (s *scenario) setup(t *testing.T) {
	t.Helper()
	ns := &corev1.Namespace{}
	err := s.client.Get(s.ctx, types.NamespacedName{Name: s.opts.Namespace}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.opts.Namespace}}
		if s.client.Create(s.ctx, ns) != nil {
			t.Fatal("cannot create test namespace")
		}
		s.namespace = ns.DeepCopy()
	} else if err != nil || ns.DeletionTimestamp != nil {
		t.Fatal("test namespace unavailable")
	}
	// A unique group name prevents collisions with any other run in a precreated Helm namespace.
	var passwordBytes [24]byte
	if _, err := rand.Read(passwordBytes[:]); err != nil {
		t.Fatal("cannot generate bootstrap password")
	}
	password := hex.EncodeToString(passwordBytes[:])
	s.values = append(s.values, password)
	s.createSecret(t, s.admin, map[string][]byte{"username": []byte("os-admin"), "password": []byte(password), "wallet-password": []byte(password)})
	replicas := int32(1)
	c := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.opts.Namespace}, Spec: ml.MarklogicClusterSpec{
		Image: s.opts.MarkLogicImage, Auth: &ml.AdminAuth{SecretName: &s.admin},
		ServiceAccountName: s.name + "-workload",
		Persistence:        &ml.Persistence{Enabled: false, Size: "1Gi"},
		MarkLogicGroups:    []*ml.MarklogicGroups{{Name: s.group, Replicas: &replicas, IsBootstrap: true}},
		ObjectStorage:      &ml.ObjectStorageConfig{AWS: &ml.AWSObjectStorage{SecretName: s.name + "-shared"}, Azure: &ml.AzureObjectStorage{SecretName: s.name + "-shared"}},
	}}
	if s.client.Create(s.ctx, c) != nil {
		t.Fatal("cannot create test cluster (details suppressed)")
	}
	s.clusterUID = c.UID
}

func (s *scenario) lifecycle(t *testing.T, f *fixture) {
	awsName, azureName := s.name+"-aws", s.name+"-azure"
	s.createSecret(t, awsName, secretData(f.AWS.Initial))
	s.createSecret(t, azureName, secretData(f.Azure.Initial))
	if !t.Run("OS-T03_initial_apply_separate_secrets", func(t *testing.T) {
		s.updateCluster(t, func(c *ml.MarklogicCluster) {
			c.Spec.ObjectStorage.AWS.SecretName = awsName
			c.Spec.ObjectStorage.Azure.SecretName = azureName
		})
		s.waitReady(t, testutils.ObjectStorageAWS)
		s.waitReady(t, testutils.ObjectStorageAzure)
	}) {
		return
	}
	baselineChildren := s.children(t)

	if !t.Run("OS-T07_restart_applied_and_detached", func(t *testing.T) { s.restartScenarios(t, f) }) {
		return
	}

	if !t.Run("OS-T04_metadata_unrelated_keys_and_region", func(t *testing.T) {
		for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
			before := s.waitReady(t, provider)
			s.updateSecret(t, before.ObservedSecret.Name, func(secret *corev1.Secret) { secret.Annotations = map[string]string{"e2e-revision": "2"} })
			assertSameSuccess(t, before, s.waitReady(t, provider))
			s.updateSecret(t, before.ObservedSecret.Name, func(secret *corev1.Secret) { secret.Data["unrelated"] = []byte("not-a-credential") })
			assertSameSuccess(t, before, s.waitReady(t, provider))
		}
		before := s.waitReady(t, testutils.ObjectStorageAWS)
		s.updateCluster(t, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage.AWS.Region = "us-west-2" })
		assertSameSuccess(t, before, s.waitReady(t, testutils.ObjectStorageAWS))
	}) {
		return
	}

	if !t.Run("OS-T05_manual_reconcile_preserves_children", func(t *testing.T) {
		beforeAWS, beforeAzure := s.waitReady(t, testutils.ObjectStorageAWS), s.waitReady(t, testutils.ObjectStorageAzure)
		before := s.children(t)
		s.updateCluster(t, func(c *ml.MarklogicCluster) {
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[reconcileAnnotation] = "object-storage-e2e"
		})
		// A Secret revision is a visible processing barrier; this does not claim to prove which
		// of the two watch events caused reconciliation (the controller envtest covers mapping).
		s.updateSecret(t, awsName, func(secret *corev1.Secret) { secret.Annotations["e2e-revision"] = "3" })
		assertSameSuccess(t, beforeAWS, s.waitReady(t, testutils.ObjectStorageAWS))
		assertSameSuccess(t, beforeAzure, s.waitReady(t, testutils.ObjectStorageAzure))
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !reflect.DeepEqual(before, s.children(t)) {
				t.Fatal("manual reconcile changed child metadata, pod template, or pod identity")
			}
			select {
			case <-s.ctx.Done():
				t.Fatal("manual reconcile observation canceled")
			case <-time.After(pollInterval):
			}
		}
	}) {
		return
	}

	if !t.Run("OS-T03_independent_rotation", func(t *testing.T) {
		for _, tc := range []struct {
			provider, other testutils.ObjectStorageProvider
			name            string
			values          material
		}{
			{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure, awsName, f.AWS.Rotated},
			{testutils.ObjectStorageAzure, testutils.ObjectStorageAWS, azureName, f.Azure.Rotated},
		} {
			before, other := s.waitReady(t, tc.provider), s.waitReady(t, tc.other)
			s.updateSecret(t, tc.name, func(secret *corev1.Secret) { secret.Data = secretData(tc.values) })
			after := s.waitReady(t, tc.provider)
			if before.AppliedFingerprint == after.AppliedFingerprint || before.AppliedSecret.ResourceVersion == after.AppliedSecret.ResourceVersion {
				t.Fatal("rotation did not record changed material and source revision")
			}
			assertSameSuccess(t, other, s.waitReady(t, tc.other))
		}
	}) {
		return
	}

	if !s.tokenClearingScenarios(t, f) {
		return
	}

	if !t.Run("OS-T07_detach_and_identical_recreation", func(t *testing.T) {
		for _, tc := range []struct {
			provider testutils.ObjectStorageProvider
			name     string
			values   material
		}{
			{testutils.ObjectStorageAWS, awsName, f.AWS.Rotated}, {testutils.ObjectStorageAzure, azureName, f.Azure.Rotated},
		} {
			before := s.waitReady(t, tc.provider)
			s.deleteSecret(t, tc.name)
			detached := s.waitPhase(t, tc.provider, ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply)
			assertSameSuccess(t, before, detached)
			if !reflect.DeepEqual(before.ObservedSecret, detached.ObservedSecret) {
				t.Fatal("detachment did not retain the last observed Secret identity/revision")
			}
			assertDetachedMessage(t, detached.Message)
			s.createSecret(t, tc.name, secretData(tc.values))
			after := s.waitReady(t, tc.provider)
			if before.AppliedSecret.UID == after.AppliedSecret.UID || before.AppliedFingerprint != after.AppliedFingerprint {
				t.Fatal("identical recreation did not establish a fresh source checkpoint")
			}
		}
	}) {
		return
	}

	if !t.Run("OS-T08_shared_secret_independent_eligibility", func(t *testing.T) {
		shared := s.name + "-shared"
		s.updateSecret(t, shared, func(secret *corev1.Secret) { secret.Data = secretData(f.AWS.Rotated, f.Azure.Rotated) })
		s.updateCluster(t, func(c *ml.MarklogicCluster) {
			c.Spec.ObjectStorage.AWS.SecretName = shared
			c.Spec.ObjectStorage.Azure.SecretName = shared
		})
		s.waitReady(t, testutils.ObjectStorageAWS)
		s.waitReady(t, testutils.ObjectStorageAzure)
		// Run the same invalidation/deletion path for both providers against one shared source.
		for _, tc := range []struct {
			provider, other testutils.ObjectStorageProvider
			key             string
		}{
			{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure, "secretKey"},
			{testutils.ObjectStorageAzure, testutils.ObjectStorageAWS, "storageKey"},
		} {
			before, other := s.waitReady(t, tc.provider), s.waitReady(t, tc.other)
			s.updateSecret(t, shared, func(secret *corev1.Secret) { secret.Data[tc.key] = []byte(" \n\t") })
			failed := s.waitPhase(t, tc.provider, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing)
			assertSameSuccess(t, before, failed)
			assertSameSuccess(t, other, s.waitReady(t, tc.other))
			s.deleteSecret(t, shared)
			s.waitPhase(t, tc.provider, ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretNotFound)
			s.waitPhase(t, tc.other, ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply)
			s.createSecret(t, shared, secretData(f.AWS.Rotated, f.Azure.Rotated))
			s.waitReady(t, testutils.ObjectStorageAWS)
			s.waitReady(t, testutils.ObjectStorageAzure)
		}
	}) {
		return
	}

	if !t.Run("OS-T13_incomplete_success_requires_new_checkpoint", func(t *testing.T) {
		providers := []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure}
		before := map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus{}
		for _, provider := range providers {
			before[provider] = s.waitReady(t, provider)
		}
		children := s.children(t)
		operator := s.stopOperator(t)
		// Quiesce every controller before seeding incomplete checkpoints, so recovery
		// can only be performed by the new controller process after startup.
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &ml.MarklogicCluster{}
			if err := s.client.Get(s.ctx, s.key(s.name), c); err != nil {
				return err
			}
			if c.UID != s.clusterUID {
				return fmt.Errorf("test cluster identity changed")
			}
			for _, provider := range providers {
				entry := providerStatus(c, provider)
				if entry == nil {
					return fmt.Errorf("provider status unavailable")
				}
				entry.DetachEligible = nil
				entry.AppliedSecret = nil
				entry.LastAppliedTime = &metav1.Time{Time: time.Unix(1, 0).UTC()}
			}
			return s.client.Status().Update(s.ctx, c)
		})
		if err != nil {
			t.Fatal("cannot seed incomplete success records while operator is stopped")
		}
		// Before changing any Secret revision, isolate completeness from freshness:
		// all observed markers still match, yet this checkpoint must not be ready.
		s.assertCurrentIncompleteCheckpoint(t)
		for _, provider := range providers {
			s.updateSecret(t, before[provider].ObservedSecret.Name, func(secret *corev1.Secret) {
				if secret.Annotations == nil {
					secret.Annotations = map[string]string{}
				}
				secret.Annotations["e2e-recovery-"+string(provider)] = s.name
			})
		}
		incomplete := &ml.MarklogicCluster{}
		if s.client.Get(s.ctx, s.key(s.name), incomplete) != nil || incomplete.UID != s.clusterUID {
			t.Fatal("cannot read persisted incomplete checkpoints")
		}
		for _, provider := range providers {
			entry := providerStatus(incomplete, provider)
			if entry == nil || entry.DetachEligible != nil || entry.AppliedSecret != nil || entry.LastAppliedTime == nil || !entry.LastAppliedTime.Time.Equal(time.Unix(1, 0)) {
				t.Fatal("incomplete checkpoint seed did not persist while operator was stopped")
			}
			secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
			if s.client.Get(s.ctx, s.key(before[provider].ObservedSecret.Name), secret) != nil {
				t.Fatal("cannot read current Secret metadata for recovery readiness check")
			}
			if ready, _ := testutils.ObjectStorageReady(incomplete, provider, testutils.SecretMeta{Name: secret.Name, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion}); ready {
				t.Fatal("incomplete persisted success incorrectly satisfies readiness before restart")
			}
		}
		s.startOperator(t, operator, operator.Image)
		for _, provider := range providers {
			after := s.waitReady(t, provider)
			if after.LastAppliedTime.Time.Equal(time.Unix(1, 0)) || after.AppliedFingerprint != before[provider].AppliedFingerprint || after.AppliedSecret.ResourceVersion == before[provider].AppliedSecret.ResourceVersion {
				t.Fatal("restarted operator did not establish a fresh checkpoint for unchanged material")
			}
		}
		if !reflect.DeepEqual(children, s.children(t)) {
			t.Fatal("incomplete checkpoint recovery changed MarkLogic child metadata, template, or pod identity")
		}
		s.assertChildrenStable(t, children)
	}) {
		return
	}

	t.Run("OS-T12_remove_providers", func(t *testing.T) {
		azure := s.waitReady(t, testutils.ObjectStorageAzure)
		s.updateCluster(t, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage.AWS = nil })
		s.waitCluster(t, "AWS tracking removal", func(ctx context.Context, c *ml.MarklogicCluster) bool {
			return c.Status.ObjectStorage != nil && c.Status.ObjectStorage.AWS == nil
		})
		assertSameSuccess(t, azure, s.waitReady(t, testutils.ObjectStorageAzure))
		// Azure-only and AWS-only are both exercised, as well as the both-provider configuration.
		s.updateCluster(t, func(c *ml.MarklogicCluster) {
			c.Spec.ObjectStorage.AWS = &ml.AWSObjectStorage{SecretName: s.name + "-shared"}
			c.Spec.ObjectStorage.Azure = nil
		})
		s.waitReady(t, testutils.ObjectStorageAWS)
		s.waitCluster(t, "Azure tracking removal", func(ctx context.Context, c *ml.MarklogicCluster) bool {
			return c.Status.ObjectStorage != nil && c.Status.ObjectStorage.Azure == nil
		})
		s.updateCluster(t, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage = nil })
		s.waitCluster(t, "all provider tracking removal", func(ctx context.Context, c *ml.MarklogicCluster) bool { return c.Status.ObjectStorage == nil })
		t.Log("Status removal verified; remote non-revocation and no-GET/no-DELETE remain BLOCKED pending cloud probes and attributed access logs")
	})
	if !t.Failed() {
		s.assertChildrenStable(t, baselineChildren)
	}
}

func providerStatus(c *ml.MarklogicCluster, p testutils.ObjectStorageProvider) *ml.ObjectStorageProviderStatus {
	if c.Status.ObjectStorage == nil {
		return nil
	}
	if p == testutils.ObjectStorageAWS {
		return c.Status.ObjectStorage.AWS
	}
	return c.Status.ObjectStorage.Azure
}

func (s *scenario) waitReady(t *testing.T, p testutils.ObjectStorageProvider) *ml.ObjectStorageProviderStatus {
	t.Helper()
	var result *ml.ObjectStorageProviderStatus
	s.waitCluster(t, string(p)+" fresh Applied", func(ctx context.Context, c *ml.MarklogicCluster) bool {
		var name string
		if c.Spec.ObjectStorage != nil {
			if p == testutils.ObjectStorageAWS && c.Spec.ObjectStorage.AWS != nil {
				name = c.Spec.ObjectStorage.AWS.SecretName
			}
			if p == testutils.ObjectStorageAzure && c.Spec.ObjectStorage.Azure != nil {
				name = c.Spec.ObjectStorage.Azure.SecretName
			}
		}
		secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
		if name == "" || s.client.Get(ctx, s.key(name), secret) != nil {
			return false
		}
		ok, _ := testutils.ObjectStorageReady(c, p, testutils.SecretMeta{Name: secret.Name, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion})
		if ok {
			if providerStatus(c, p).Reason != "" {
				t.Fatal("Applied status contains a failure reason")
			}
			result = providerStatus(c, p).DeepCopy()
		}
		return ok
	})
	return result
}

func (s *scenario) waitPhase(t *testing.T, p testutils.ObjectStorageProvider, phase ml.ObjectStoragePhase, reason ml.ObjectStorageReason) *ml.ObjectStorageProviderStatus {
	t.Helper()
	var result *ml.ObjectStorageProviderStatus
	s.waitCluster(t, string(p)+" "+string(phase)+"/"+string(reason), func(ctx context.Context, c *ml.MarklogicCluster) bool {
		e := providerStatus(c, p)
		if e == nil || e.Phase != phase || e.Reason != reason || e.ObservedGeneration != c.Generation || e.ObservedSecret == nil {
			return false
		}
		name := ""
		if c.Spec.ObjectStorage != nil {
			if p == testutils.ObjectStorageAWS && c.Spec.ObjectStorage.AWS != nil {
				name = c.Spec.ObjectStorage.AWS.SecretName
			}
			if p == testutils.ObjectStorageAzure && c.Spec.ObjectStorage.Azure != nil {
				name = c.Spec.ObjectStorage.Azure.SecretName
			}
		}
		if name == "" || e.ObservedSecret.Name != name {
			return false
		}
		secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
		err := s.client.Get(ctx, s.key(name), secret)
		if reason == ml.ObjectStorageReasonSecretNotFound || phase == ml.ObjectStoragePhaseDetached {
			if !apierrors.IsNotFound(err) {
				return false
			}
		} else if err != nil || e.ObservedSecret.UID != string(secret.UID) || e.ObservedSecret.ResourceVersion != secret.ResourceVersion {
			return false
		}
		eligible := e.DetachEligible != nil && *e.DetachEligible
		if (phase == ml.ObjectStoragePhaseDetached) != eligible {
			return false
		}
		result = e.DeepCopy()
		return true
	})
	return result
}

func (s *scenario) waitCluster(t *testing.T, description string, condition func(context.Context, *ml.MarklogicCluster) bool) {
	t.Helper()
	last := "no status"
	err := wait.PollUntilContextTimeout(s.ctx, pollInterval, statusTimeout, true, func(ctx context.Context) (bool, error) {
		c := &ml.MarklogicCluster{}
		if s.client.Get(ctx, s.key(s.name), c) != nil {
			return false, nil
		}
		if c.UID != s.clusterUID || c.DeletionTimestamp != nil {
			return false, fmt.Errorf("test cluster identity changed or is terminating")
		}
		data, _ := json.Marshal(c.Status)
		if containsMaterial(data, s.values) {
			return false, fmt.Errorf("credential material detected in status (value suppressed)")
		}
		last = ""
		for _, p := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
			if e := providerStatus(c, p); e != nil {
				last += fmt.Sprintf("%s=%s/%s ", p, e.Phase, e.Reason)
			}
		}
		return condition(ctx, c), nil
	})
	if err != nil {
		t.Fatalf("waiting for %s failed; sanitized status: %s (no resource/error bodies emitted)", description, last)
	}
}

func (s *scenario) updateCluster(t *testing.T, mutate func(*ml.MarklogicCluster)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &ml.MarklogicCluster{}
		if err := s.client.Get(s.ctx, s.key(s.name), c); err != nil {
			return err
		}
		if c.UID != s.clusterUID || c.DeletionTimestamp != nil {
			return fmt.Errorf("test cluster identity changed or is terminating")
		}
		mutate(c)
		return s.client.Update(s.ctx, c)
	})
	if err != nil {
		t.Fatal("cluster update failed (details suppressed)")
	}
}

func (s *scenario) createSecret(t *testing.T, name string, data map[string][]byte) {
	t.Helper()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.opts.Namespace}, Type: corev1.SecretTypeOpaque, Data: data}
	if s.client.Create(s.ctx, secret) != nil {
		t.Fatal("test Secret creation failed (details suppressed)")
	}
	s.secrets = append(s.secrets, secret.DeepCopy())
}

func (s *scenario) updateSecret(t *testing.T, name string, mutate func(*corev1.Secret)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		if err := s.client.Get(s.ctx, s.key(name), secret); err != nil {
			return err
		}
		mutate(secret)
		return s.client.Update(s.ctx, secret)
	})
	if err != nil {
		t.Fatal("test Secret update failed (details suppressed)")
	}
}

func (s *scenario) deleteSecret(t *testing.T, name string) {
	t.Helper()
	secret := &corev1.Secret{}
	if s.client.Get(s.ctx, s.key(name), secret) != nil {
		t.Fatal("cannot get test Secret for deletion")
	}
	if s.client.Delete(s.ctx, secret, client.Preconditions{UID: &secret.UID}) != nil {
		t.Fatal("cannot delete test Secret")
	}
	if wait.PollUntilContextTimeout(s.ctx, pollInterval, statusTimeout, true, func(ctx context.Context) (bool, error) {
		e := s.client.Get(ctx, s.key(name), &corev1.Secret{})
		return apierrors.IsNotFound(e), nil
	}) != nil {
		t.Fatal("test Secret deletion timed out")
	}
}

func assertSameSuccess(t *testing.T, a, b *ml.ObjectStorageProviderStatus) {
	t.Helper()
	if a == nil || b == nil || a.AuthType != b.AuthType || a.AppliedFingerprint != b.AppliedFingerprint || !reflect.DeepEqual(a.AppliedSecret, b.AppliedSecret) || !reflect.DeepEqual(a.LastAppliedTime, b.LastAppliedTime) {
		t.Fatal("last-success record changed unexpectedly (values suppressed)")
	}
}

func assertDetachedMessage(t *testing.T, message string) {
	t.Helper()
	for _, fragment := range []string{"remain", "recreat", "verif", "expir"} {
		if !strings.Contains(strings.ToLower(message), fragment) {
			t.Fatal("detached message omits retention, recreation, validity or expiry warning")
		}
	}
}

func (s *scenario) waitPod(t *testing.T, ordinal int) {
	t.Helper()
	name := fmt.Sprintf("%s-%d", s.group, ordinal)
	err := wait.PollUntilContextTimeout(s.ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		pod := &corev1.Pod{}
		if s.client.Get(ctx, s.key(name), pod) != nil {
			return false, nil
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("pod %s readiness timed out (diagnostic bodies suppressed)", name)
	}
	pod := &corev1.Pod{}
	if s.client.Get(s.ctx, s.key(name), pod) != nil {
		t.Fatal("cannot read ready pod identity")
	}
	foundSpec := false
	for _, container := range pod.Spec.Containers {
		if container.Name == "marklogic-server" {
			foundSpec = true
			if container.Image != s.opts.MarkLogicImage {
				t.Fatal("server pod does not use the requested MarkLogic image")
			}
		}
	}
	foundStatus := false
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == "marklogic-server" {
			foundStatus = true
			if container.ImageID == "" || !container.Ready {
				t.Fatal("server image identity missing")
			}
			t.Logf("server pod=%s image=%s imageID=%s", name, container.Image, container.ImageID)
		}
	}
	if !foundSpec || !foundStatus {
		t.Fatal("server container identity missing")
	}
}

func (s *scenario) children(t *testing.T) childSnapshot {
	t.Helper()
	return s.childrenAt(s.ctx, t)
}

func (s *scenario) childrenAt(ctx context.Context, t *testing.T) childSnapshot {
	t.Helper()
	g := &ml.MarklogicGroup{}
	sts := &appsv1.StatefulSet{}
	if s.client.Get(ctx, s.key(s.group), g) != nil || s.client.Get(ctx, s.key(s.group), sts) != nil {
		t.Fatal("cannot read child configuration")
	}
	if _, ok := g.Annotations[reconcileAnnotation]; ok {
		t.Fatal("reconcile annotation propagated to child group")
	}
	if _, ok := sts.Annotations[reconcileAnnotation]; ok {
		t.Fatal("reconcile annotation propagated to StatefulSet")
	}
	pods := []*corev1.Pod{}
	for ordinal := 0; ordinal < 2; ordinal++ {
		pod := &corev1.Pod{}
		if s.client.Get(ctx, s.key(fmt.Sprintf("%s-%d", s.group, ordinal)), pod) != nil {
			t.Fatal("cannot read child pod identity")
		}
		pods = append(pods, pod)
	}
	snapshot, err := snapshotChildren(g, sts, pods)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (s *scenario) verifyOperator(t *testing.T) {
	t.Helper()
	s.beginOperatorLogEpoch(t, nil)
	version, err := s.kube.Discovery().ServerVersion()
	if err != nil {
		t.Fatal("cannot record Kubernetes version")
	}
	t.Logf("Kubernetes version=%s", version.GitVersion)
}

func (s *scenario) checkPublishedSafety(t *testing.T) {
	t.Helper()
	cluster := &ml.MarklogicCluster{}
	if s.client.Get(s.ctx, s.key(s.name), cluster) != nil {
		t.Fatal("cannot inspect published status")
	}
	data, _ := json.Marshal(cluster)
	if containsMaterial(data, s.values) {
		t.Fatal("credential material detected in cluster (value suppressed)")
	}
	events := &corev1.EventList{}
	if s.client.List(s.ctx, events, client.InNamespace(s.opts.Namespace)) != nil {
		t.Fatal("cannot inspect events")
	}
	for _, event := range events.Items {
		if event.InvolvedObject.UID != cluster.UID {
			continue
		}
		data, _ = json.Marshal(event)
		if containsMaterial(data, s.values) {
			t.Fatal("credential material detected in event (value suppressed)")
		}
		if event.Reason == "ObjectStorageDetached" {
			assertDetachedMessage(t, event.Message)
		}
	}
	s.checkOperatorLogSafety(t)
}

func (s *scenario) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if s.clusterUID != "" {
		cluster := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.opts.Namespace}}
		err := s.client.Delete(ctx, cluster, client.Preconditions{UID: &s.clusterUID})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("cleanup: cannot delete owned cluster %s (details suppressed)", s.name)
			return
		}
		err = wait.PollUntilContextTimeout(ctx, pollInterval, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
			if !apierrors.IsNotFound(s.client.Get(ctx, s.key(s.name), &ml.MarklogicCluster{})) {
				return false, nil
			}
			for i := 0; i < 2; i++ {
				if !apierrors.IsNotFound(s.client.Get(ctx, s.key(fmt.Sprintf("%s-%d", s.group, i)), &corev1.Pod{})) {
					return false, nil
				}
			}
			return true, nil
		})
		if err != nil {
			t.Errorf("cleanup: cluster/pods for %s remain; bootstrap Secret retained", s.name)
			return
		}
	}
	for _, secret := range s.secrets {
		err := s.client.Delete(ctx, secret, client.Preconditions{UID: &secret.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleanup: owned Secret %s may remain", secret.Name)
		}
	}
	if s.namespace != nil {
		if err := s.client.Delete(ctx, s.namespace, client.Preconditions{UID: &s.namespace.UID}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("cleanup: owned namespace %s remains", s.namespace.Name)
		}
	}
}
