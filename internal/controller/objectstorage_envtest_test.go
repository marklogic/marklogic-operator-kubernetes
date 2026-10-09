/*
Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

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

package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
)

// envtestCredentialClient stands in for MarkLogic in envtest; it records requests and never sees real credentials.
type envtestCredentialClient struct {
	mu       sync.Mutex
	readyErr error
	putErr   error
	puts     map[string]int
}

func newEnvtestCredentialClient() *envtestCredentialClient {
	return &envtestCredentialClient{puts: map[string]int{}}
}

func (c *envtestCredentialClient) CheckBootstrapReady(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readyErr
}

func (c *envtestCredentialClient) ApplyAWSCredentials(context.Context, mlmanage.AWSCredentials) error {
	return c.record("aws")
}

func (c *envtestCredentialClient) ApplyAzureCredentials(context.Context, mlmanage.AzureCredentials) error {
	return c.record("azure")
}

func (c *envtestCredentialClient) record(provider string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts[provider]++
	return c.putErr
}

func (c *envtestCredentialClient) count(provider string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts[provider]
}

func (c *envtestCredentialClient) setPutErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putErr = err
}

var objectStorageStub = newEnvtestCredentialClient()

func objectStorageTestCluster(namespace, name string, mutate func(*marklogicv1.MarklogicCluster)) *marklogicv1.MarklogicCluster {
	replicas := int32(1)
	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{Kind: "MarklogicCluster", APIVersion: "marklogic.progress.com/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: "progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6",
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{{
				Name:        "dnode",
				IsBootstrap: true,
				Replicas:    &replicas,
				GroupConfig: &marklogicv1.GroupConfig{Name: "dnode", EnableXdqpSsl: true},
				Service:     marklogicv1.Service{Type: corev1.ServiceTypeClusterIP},
			}},
		},
	}
	if mutate != nil {
		mutate(cluster)
	}
	return cluster
}

func createNamespace(ctx context.Context, name string) {
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
}

var _ = Describe("MarklogicCluster object storage schema", func() {
	ctx := context.Background()

	It("defaults authType and round-trips every status field and phase", func() {
		const ns = "os-schema-defaults"
		createNamespace(ctx, ns)
		cluster := objectStorageTestCluster(ns, "defaults", func(c *marklogicv1.MarklogicCluster) {
			c.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{
				AWS:   &marklogicv1.AWSObjectStorage{SecretName: "aws-creds", Region: "us-east-1"},
				Azure: &marklogicv1.AzureObjectStorage{SecretName: "azure-creds"},
			}
		})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stored := &marklogicv1.MarklogicCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
		Expect(stored.Spec.ObjectStorage.AWS.AuthType).To(Equal(marklogicv1.ObjectStorageAuthSecret))
		Expect(stored.Spec.ObjectStorage.Azure.AuthType).To(Equal(marklogicv1.ObjectStorageAuthSecret))
		Expect(stored.Spec.ObjectStorage.AWS.Region).To(Equal("us-east-1"))

		now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
		eligible := true
		full := func(phase marklogicv1.ObjectStoragePhase, reason marklogicv1.ObjectStorageReason) *marklogicv1.ObjectStorageProviderStatus {
			return &marklogicv1.ObjectStorageProviderStatus{
				Phase: phase, Reason: reason, Message: "message",
				AuthType: "secret", AppliedFingerprint: strings.Repeat("a", 64), LastAppliedTime: &now,
				AppliedSecret:      &marklogicv1.ObjectStorageSecretRef{Name: "n", UID: "u", ResourceVersion: "1"},
				DetachEligible:     &eligible,
				ObservedGeneration: 1,
				ObservedSecret:     &marklogicv1.ObjectStorageSecretRef{Name: "n", UID: "u2", ResourceVersion: "2"},
			}
		}
		combos := []struct {
			phase  marklogicv1.ObjectStoragePhase
			reason marklogicv1.ObjectStorageReason
		}{
			{marklogicv1.ObjectStoragePhasePending, marklogicv1.ObjectStorageReasonBootstrapNotReady},
			{marklogicv1.ObjectStoragePhaseApplied, ""},
			{marklogicv1.ObjectStoragePhaseDetached, marklogicv1.ObjectStorageReasonSecretDeletedAfterApply},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretReadFailed},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretKeyMissing},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonInvalidPayload},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonAuthenticationFailed},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonInsufficientPrivilege},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonManagementAPIUnreachable},
			{marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonManagementAPIError},
		}
		// The running controller also writes this status, so each write rereads and retries on conflict;
		// assertions use the object the API server returned from the write.
		writeStatus := func(mutate func(*marklogicv1.MarklogicCluster)) (*marklogicv1.MarklogicCluster, error) {
			var written *marklogicv1.MarklogicCluster
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &marklogicv1.MarklogicCluster{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), current); err != nil {
					return err
				}
				mutate(current)
				written = current
				return k8sClient.Status().Update(ctx, current)
			})
			return written, err
		}
		for _, combo := range combos {
			combo := combo
			got, err := writeStatus(func(c *marklogicv1.MarklogicCluster) {
				c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: full(combo.phase, combo.reason), Azure: full(combo.phase, combo.reason)}
			})
			Expect(err).To(Succeed(), "phase %s reason %s", combo.phase, combo.reason)
			entry := got.Status.ObjectStorage.AWS
			Expect(entry.Phase).To(Equal(combo.phase))
			Expect(entry.Reason).To(Equal(combo.reason))
			Expect(entry.AppliedFingerprint).To(Equal(strings.Repeat("a", 64)))
			Expect(entry.LastAppliedTime.UTC().Unix()).To(Equal(now.UTC().Unix()))
			Expect(*entry.AppliedSecret).To(Equal(*full(combo.phase, combo.reason).AppliedSecret))
			Expect(*entry.ObservedSecret).To(Equal(*full(combo.phase, combo.reason).ObservedSecret))
			Expect(entry.DetachEligible).NotTo(BeNil())
			Expect(*entry.DetachEligible).To(BeTrue())
			Expect(entry.ObservedGeneration).To(Equal(int64(1)))
		}

		// An explicit false must survive, distinct from absence.
		notEligible := false
		got, err := writeStatus(func(c *marklogicv1.MarklogicCluster) {
			c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: full(marklogicv1.ObjectStoragePhaseApplied, "")}
			c.Status.ObjectStorage.AWS.DetachEligible = &notEligible
		})
		Expect(err).To(Succeed())
		Expect(got.Status.ObjectStorage.AWS.DetachEligible).NotTo(BeNil())
		Expect(*got.Status.ObjectStorage.AWS.DetachEligible).To(BeFalse())

		// Out-of-contract values are rejected.
		_, err = writeStatus(func(c *marklogicv1.MarklogicCluster) {
			c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: full("Bogus", "")}
		})
		Expect(err).To(HaveOccurred())
		_, err = writeStatus(func(c *marklogicv1.MarklogicCluster) {
			c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: full(marklogicv1.ObjectStoragePhaseApplied, "")}
			c.Status.ObjectStorage.AWS.Message = strings.Repeat("m", 513)
		})
		Expect(err).To(HaveOccurred())
		_, err = writeStatus(func(c *marklogicv1.MarklogicCluster) {
			c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: full(marklogicv1.ObjectStoragePhaseApplied, "")}
			c.Status.ObjectStorage.AWS.AppliedFingerprint = strings.Repeat("f", 129)
		})
		Expect(err).To(HaveOccurred())
	})

	It("rejects unsupported auth modes and invalid secret names", func() {
		const ns = "os-schema-invalid"
		createNamespace(ctx, ns)
		create := func(name string, mutate func(*marklogicv1.ObjectStorageConfig)) error {
			return k8sClient.Create(ctx, objectStorageTestCluster(ns, name, func(c *marklogicv1.MarklogicCluster) {
				c.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{}
				mutate(c.Spec.ObjectStorage)
			}))
		}

		err := create("irsa", func(o *marklogicv1.ObjectStorageConfig) {
			o.AWS = &marklogicv1.AWSObjectStorage{AuthType: marklogicv1.ObjectStorageAuthInstanceRole, SecretName: "s"}
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("authType must be 'secret'"))

		err = create("managed-identity", func(o *marklogicv1.ObjectStorageConfig) {
			o.Azure = &marklogicv1.AzureObjectStorage{AuthType: marklogicv1.ObjectStorageAuthManagedIdentity, SecretName: "s"}
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("authType must be 'secret'"))

		Expect(create("unknown-mode", func(o *marklogicv1.ObjectStorageConfig) {
			o.AWS = &marklogicv1.AWSObjectStorage{AuthType: "token", SecretName: "s"}
		})).To(HaveOccurred())
		Expect(create("empty-aws-secret", func(o *marklogicv1.ObjectStorageConfig) { o.AWS = &marklogicv1.AWSObjectStorage{} })).To(HaveOccurred())
		Expect(create("empty-azure-secret", func(o *marklogicv1.ObjectStorageConfig) { o.Azure = &marklogicv1.AzureObjectStorage{} })).To(HaveOccurred())
		Expect(create("long-secret-name", func(o *marklogicv1.ObjectStorageConfig) {
			o.AWS = &marklogicv1.AWSObjectStorage{SecretName: strings.Repeat("a", 254)}
		})).To(HaveOccurred())
	})

	It("does not accept unknown inline credential fields", func() {
		const ns = "os-schema-inline"
		createNamespace(ctx, ns)
		cluster := objectStorageTestCluster(ns, "inline", func(c *marklogicv1.MarklogicCluster) {
			c.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{AWS: &marklogicv1.AWSObjectStorage{SecretName: "aws-creds"}}
		})
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
		Expect(err).NotTo(HaveOccurred())
		withInline := func(value string) *unstructured.Unstructured {
			obj := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(content)}
			Expect(unstructured.SetNestedField(obj.Object, value, "spec", "objectStorage", "aws", "accessKey")).To(Succeed())
			return obj
		}

		// Strict validation rejects the create and leaves no object.
		Expect(k8sClient.Create(ctx, withInline("inline-value"), client.FieldValidation("Strict"))).NotTo(Succeed())
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "inline"}, &marklogicv1.MarklogicCluster{})
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		Expect(err).To(HaveOccurred())

		// Non-strict validation prunes the field from the stored spec.
		Expect(k8sClient.Create(ctx, withInline("inline-value"), client.FieldValidation("Ignore"))).To(Succeed())
		stored := &unstructured.Unstructured{}
		stored.SetGroupVersionKind(cluster.GroupVersionKind())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "inline"}, stored)).To(Succeed())
		aws, found, err := unstructured.NestedMap(stored.Object, "spec", "objectStorage", "aws")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(aws).NotTo(HaveKey("accessKey"))
		Expect(aws).To(HaveKeyWithValue("secretName", "aws-creds"))

		// A strict update with an inline field is rejected and the stored spec is unchanged.
		update := stored.DeepCopy()
		Expect(unstructured.SetNestedField(update.Object, "inline-value", "spec", "objectStorage", "aws", "secretKey")).To(Succeed())
		Expect(k8sClient.Update(ctx, update, client.FieldValidation("Strict"))).NotTo(Succeed())
		unchanged := &unstructured.Unstructured{}
		unchanged.SetGroupVersionKind(cluster.GroupVersionKind())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "inline"}, unchanged)).To(Succeed())
		aws, _, _ = unstructured.NestedMap(unchanged.Object, "spec", "objectStorage", "aws")
		Expect(aws).NotTo(HaveKey("secretKey"))
	})
})

var _ = Describe("MarklogicCluster object storage reconciliation", func() {
	ctx := context.Background()

	It("reconciles through real Secret and cluster watches", func() {
		const ns = "os-watch"
		createNamespace(ctx, ns)
		cluster := objectStorageTestCluster(ns, "watch", func(c *marklogicv1.MarklogicCluster) {
			c.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{AWS: &marklogicv1.AWSObjectStorage{SecretName: "os-aws"}}
		})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		key := client.ObjectKeyFromObject(cluster)
		secretKey := types.NamespacedName{Namespace: ns, Name: "os-aws"}

		awsEntry := func() *marklogicv1.ObjectStorageProviderStatus {
			current := &marklogicv1.MarklogicCluster{}
			if err := k8sClient.Get(ctx, key, current); err != nil || current.Status.ObjectStorage == nil {
				return nil
			}
			return current.Status.ObjectStorage.AWS
		}
		// Every poll fetches the current cluster and Secret metadata.
		ready := func() bool {
			current := &marklogicv1.MarklogicCluster{}
			secret := &corev1.Secret{}
			if k8sClient.Get(ctx, key, current) != nil || k8sClient.Get(ctx, secretKey, secret) != nil {
				return false
			}
			ok, _ := testutils.ObjectStorageReady(current, testutils.ObjectStorageAWS, testutils.SecretMeta{Name: secret.Name, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion})
			return ok
		}
		const timeout, poll = 45 * time.Second, 250 * time.Millisecond

		By("reporting a missing Secret")
		Eventually(awsEntry, timeout, poll).Should(And(Not(BeNil()), HaveField("Reason", marklogicv1.ObjectStorageReasonSecretNotFound)))

		By("applying when the Secret is created")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "os-aws"},
			StringData: map[string]string{"accessKey": "SECRET-VALUE-ak-1", "secretKey": "SECRET-VALUE-sk-1"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		Eventually(ready, timeout, poll).Should(BeTrue())
		Expect(objectStorageStub.count("aws")).To(Equal(1))
		applied := awsEntry().DeepCopy()

		By("acknowledging a metadata-only update without another PUT")
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Annotations = map[string]string{"touched": "1"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(ready, timeout, poll).Should(BeTrue())
		Expect(objectStorageStub.count("aws")).To(Equal(1))
		Expect(awsEntry().LastAppliedTime.Equal(applied.LastAppliedTime)).To(BeTrue())
		Expect(*awsEntry().AppliedSecret).To(Equal(*applied.AppliedSecret))

		By("rotating changed material")
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data = map[string][]byte{"accessKey": []byte("SECRET-VALUE-ak-2"), "secretKey": []byte("SECRET-VALUE-sk-2")}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(func() int { return objectStorageStub.count("aws") }, timeout, poll).Should(Equal(2))
		Eventually(ready, timeout, poll).Should(BeTrue())
		Expect(awsEntry().AppliedFingerprint).NotTo(Equal(applied.AppliedFingerprint))

		By("detaching after an eligible deletion")
		Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		Eventually(awsEntry, timeout, poll).Should(And(Not(BeNil()), HaveField("Phase", marklogicv1.ObjectStoragePhaseDetached), HaveField("Reason", marklogicv1.ObjectStorageReasonSecretDeletedAfterApply)))
		Expect(objectStorageStub.count("aws")).To(Equal(2))

		By("re-adopting a recreated Secret with identical material")
		recreated := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "os-aws"},
			StringData: map[string]string{"accessKey": "SECRET-VALUE-ak-2", "secretKey": "SECRET-VALUE-sk-2"},
		}
		Expect(k8sClient.Create(ctx, recreated)).To(Succeed())
		Eventually(ready, timeout, poll).Should(BeTrue())
		Expect(objectStorageStub.count("aws")).To(Equal(3))

		By("retrying a failed provider through the reconcile-request annotation without rolling pods")
		objectStorageStub.setPutErr(&mlmanage.CredentialError{Operation: "PUT /manage/v2/credentials/properties", StatusCode: 403})
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data = map[string][]byte{"accessKey": []byte("SECRET-VALUE-ak-3"), "secretKey": []byte("SECRET-VALUE-sk-3")}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(awsEntry, timeout, poll).Should(And(Not(BeNil()), HaveField("Reason", marklogicv1.ObjectStorageReasonInsufficientPrivilege)))

		objectStorageStub.setPutErr(nil)
		Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
		cluster.Annotations = map[string]string{"marklogic.progress.com/reconcile-request": "1"}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		Eventually(ready, timeout, poll).Should(BeTrue())

		group := &marklogicv1.MarklogicGroup{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "dnode"}, group)).To(Succeed())
		Expect(group.Annotations).NotTo(HaveKey("marklogic.progress.com/reconcile-request"))

		By("removing the provider clears its tracking")
		Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
		cluster.Spec.ObjectStorage = nil
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		Eventually(func() bool {
			current := &marklogicv1.MarklogicCluster{}
			return k8sClient.Get(ctx, key, current) == nil && current.Status.ObjectStorage == nil
		}, timeout, poll).Should(BeTrue())
	})
})
