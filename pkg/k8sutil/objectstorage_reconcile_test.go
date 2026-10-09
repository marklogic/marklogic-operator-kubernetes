// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestObjectStorageInitialApplyOrderingAndRecord(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()

	res := h.mustReconcile()
	if res.RequeueAfter != 0 {
		t.Fatalf("no retry expected after success, got %v", res.RequeueAfter)
	}

	want := []string{"probe", "put:aws", "put:azure", "status-write", "event:ObjectStorageApplied", "event:ObjectStorageApplied"}
	if strings.Join(h.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("operation order = %v, want %v (no placeholder checkpoint before the first PUT, events after persistence)", h.ops, want)
	}
	for _, provider := range objectStorageProviders {
		entry := h.entry(provider)
		assertPhase(t, entry, marklogicv1.ObjectStoragePhaseApplied, "")
		assertEligible(t, entry, true)
		if !hasCompleteSuccess(entry) || entry.AuthType != "secret" {
			t.Fatalf("%s: incomplete success record %#v", provider, entry)
		}
		if entry.ObservedGeneration != 1 || entry.ObservedSecret == nil || entry.ObservedSecret.UID == "" || entry.ObservedSecret.ResourceVersion == "" {
			t.Fatalf("%s: observed markers not published: %#v", provider, entry.ObservedSecret)
		}
		if *entry.AppliedSecret != *entry.ObservedSecret {
			t.Fatalf("%s: applied and observed secret should match after first apply", provider)
		}
	}
}

func TestObjectStorageUnchangedReconcileDoesNothing(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)

	res := h.mustReconcile()
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("unexpected requeue %+v", res)
	}
	if len(h.ops) != 0 {
		t.Fatalf("unchanged reconcile must not probe, PUT, write status or emit events: %v", h.ops)
	}
}

func TestObjectStorageMetadataOnlySecretUpdateAcknowledgesRevisionWithoutPut(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	before := h.entry(providerAWS)

	h.touchSecret("aws-creds")
	h.mustReconcile()

	after := h.entry(providerAWS)
	if h.countOps("put:aws") != 0 || h.countOps("probe") != 0 || len(h.events) != 0 {
		t.Fatalf("metadata-only update must not PUT or emit events: ops=%v events=%v", h.ops, h.eventReasons())
	}
	if after.ObservedSecret.ResourceVersion == before.ObservedSecret.ResourceVersion {
		t.Fatalf("observed resourceVersion should advance")
	}
	if *after.AppliedSecret != *before.AppliedSecret || !after.LastAppliedTime.Equal(before.LastAppliedTime) || after.AppliedFingerprint != before.AppliedFingerprint {
		t.Fatalf("success history must not change on a metadata-only update")
	}
	assertEligible(t, after, true)
	assertPhase(t, after, marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestObjectStorageRotationCheckpointsEligibilityBeforePut(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	before := h.entry(providerAWS)
	azureBefore := h.entry(providerAzure)

	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("2"))
	var duringPut *marklogicv1.ObjectStorageProviderStatus
	h.creds.onPut = func(provider objectStorageProvider) {
		if provider == providerAWS {
			duringPut = h.entry(providerAWS)
		}
	}

	h.mustReconcile()

	// Durable state at PUT time: only eligibility changed.
	if duringPut == nil || isDetachEligible(duringPut) {
		t.Fatalf("eligibility must be durably false before the PUT")
	}
	preserved := duringPut.DeepCopy()
	preserved.DetachEligible = before.DetachEligible
	if !strings.EqualFold(string(preserved.Phase), string(before.Phase)) || preserved.ObservedSecret.ResourceVersion != before.ObservedSecret.ResourceVersion ||
		preserved.AppliedFingerprint != before.AppliedFingerprint || preserved.Message != before.Message {
		t.Fatalf("checkpoint must change only detachEligible")
	}
	if h.countOps("put:azure") != 0 {
		t.Fatalf("the unchanged provider must not be rewritten")
	}

	after := h.entry(providerAWS)
	assertEligible(t, after, true)
	if after.AppliedFingerprint == before.AppliedFingerprint {
		t.Fatalf("rotation should change the fingerprint")
	}
	if got := h.entry(providerAzure); got.ObservedSecret.ResourceVersion != azureBefore.ObservedSecret.ResourceVersion || got.AppliedFingerprint != azureBefore.AppliedFingerprint {
		t.Fatalf("azure entry must be untouched by an AWS rotation")
	}
	order := []string{"status-write", "probe", "put:aws", "status-write", "event:ObjectStorageApplied"}
	if got := strings.Join(h.ops, ","); got != strings.Join(order, ",") {
		t.Fatalf("operations = %v, want suffix %v", h.ops, order)
	}
}

func TestObjectStorageCheckpointWriteFailureBlocksPut(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("2"))
	h.statusWriteErr = func() error { return errors.New("status unavailable") }

	_, err := h.reconcile()
	if err == nil {
		t.Fatalf("a failed checkpoint is a retriable error")
	}
	if h.countOps("put:aws") != 0 || h.countOps("probe") != 0 {
		t.Fatalf("no remote work may precede a durable checkpoint: %v", h.ops)
	}
	if !isDetachEligible(h.entry(providerAWS)) {
		t.Fatalf("durable status should be unchanged")
	}

	// An unobserved/unrecorded change does not invalidate an eligible success: deletion detaches.
	h.statusWriteErr = nil
	h.deleteSecret("aws-creds")
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseDetached, marklogicv1.ObjectStorageReasonSecretDeletedAfterApply)
}

func TestObjectStorageOutcomeWriteFailureKeepsEligibilityInvalid(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	originalFingerprint := h.entry(providerAWS).AppliedFingerprint

	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("2"))
	writes := 0
	h.statusWriteErr = func() error {
		writes++
		if writes == 2 { // checkpoint succeeds, outcome write fails
			return errors.New("status unavailable")
		}
		return nil
	}
	if _, err := h.reconcile(); err == nil {
		t.Fatalf("expected the outcome write failure to surface")
	}
	if h.countOps("put:aws") != 1 {
		t.Fatalf("the PUT happened before the failed outcome write")
	}
	if len(h.events) != 0 {
		t.Fatalf("a failed status write must not emit provider events: %v", h.eventReasons())
	}
	entry := h.entry(providerAWS)
	assertEligible(t, entry, false)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseApplied, "")
	if entry.AppliedFingerprint != originalFingerprint {
		t.Fatalf("success history must still describe the previous success")
	}

	// Rolling the material back to the old value still requires a PUT.
	h.statusWriteErr = nil
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	h.resetObservations()
	h.mustReconcile()
	if h.countOps("put:aws") != 1 {
		t.Fatalf("eligibility cannot be restored by an old fingerprint; a PUT is required")
	}
	assertEligible(t, h.entry(providerAWS), true)
}

func TestObjectStorageDeletionAfterRecordedInvalidationFails(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("2"))
	writes := 0
	h.statusWriteErr = func() error {
		writes++
		if writes == 2 {
			return errors.New("status unavailable")
		}
		return nil
	}
	_, _ = h.reconcile()
	h.statusWriteErr = nil

	h.deleteSecret("aws-creds")
	h.resetObservations()
	h.mustReconcile()

	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound)
	assertEligible(t, entry, false)
	if h.countOps("put:aws") != 0 {
		t.Fatalf("no remote call for an absent Secret")
	}
}

func TestObjectStorageLostPutResponseRepeatsPut(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("2"))
	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /manage/v2/credentials/properties"}

	res := h.mustReconcile()
	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonManagementAPIUnreachable)
	assertEligible(t, entry, false)
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("retry = %v, want 30s", res.RequeueAfter)
	}

	h.creds.putErr = map[objectStorageProvider]error{}
	h.resetObservations()
	h.mustReconcile()
	if h.countOps("put:aws") != 1 {
		t.Fatalf("a failed attempt must be retried")
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestObjectStorageDetachedAfterEligibleDeletion(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	before := h.entry(providerAWS)

	h.deleteSecret("aws-creds")
	h.creds.readyErr = errors.New("bootstrap outage")
	res := h.mustReconcile()

	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseDetached, marklogicv1.ObjectStorageReasonSecretDeletedAfterApply)
	assertEligible(t, entry, true)
	if res.RequeueAfter != 0 {
		t.Fatalf("no timed retry solely for eligible absence, got %v", res.RequeueAfter)
	}
	if h.creds.probes != 0 || h.countOps("put:aws") != 0 {
		t.Fatalf("detachment must not touch the Management API")
	}
	for _, phrase := range []string{"remain in effect", "recreated", "cloud validity is not verified", "temporary credentials can expire"} {
		if !strings.Contains(entry.Message, phrase) {
			t.Fatalf("status message missing %q", phrase)
		}
	}
	if len(entry.Message) > 512 {
		t.Fatalf("message exceeds 512 characters")
	}
	if len(h.events) != 1 || h.events[0].reason != "ObjectStorageDetached" || h.events[0].eventType != "Warning" {
		t.Fatalf("expected one Warning ObjectStorageDetached, got %v", h.eventReasons())
	}
	for _, phrase := range []string{"remain in effect", "cloud validity is not verified", "temporary credentials can expire"} {
		if !strings.Contains(h.events[0].message, phrase) {
			t.Fatalf("event text missing %q", phrase)
		}
	}
	// Observed markers come from observedSecret, and history is retained.
	if *entry.ObservedSecret != *before.ObservedSecret || *entry.AppliedSecret != *before.AppliedSecret {
		t.Fatalf("markers/history must be retained")
	}

	// Staying absent across restarts and outages: no writes, no repeated warning.
	h.resetObservations()
	h.mustReconcile()
	if len(h.ops) != 0 {
		t.Fatalf("repeated absence must be silent: %v", h.ops)
	}
}

func TestObjectStorageReadoptionRequiresPutEvenForIdenticalMaterial(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	h.deleteSecret("aws-creds")
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseDetached, marklogicv1.ObjectStorageReasonSecretDeletedAfterApply)
	oldFingerprint := h.entry(providerAWS).AppliedFingerprint

	h.setSecret("aws-creds", "aws-uid-2", awsMaterial("1"))
	h.resetObservations()
	h.mustReconcile()

	if h.countOps("put:aws") != 1 {
		t.Fatalf("recreated Secret requires a new PUT")
	}
	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseApplied, "")
	assertEligible(t, entry, true)
	if entry.AppliedFingerprint != oldFingerprint {
		t.Fatalf("identical material should keep an identical fingerprint")
	}
	if entry.AppliedSecret.UID != "aws-uid-2" {
		t.Fatalf("applied secret identity should be the recreated Secret")
	}
}

func TestObjectStorageAbsentBeforeSuccessFailsAndDeduplicatesEvents(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)

	res := h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound)
	assertEligible(t, h.entry(providerAWS), false)
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("retry = %v, want 30s", res.RequeueAfter)
	}
	if h.entry(providerAWS).ObservedSecret.UID != "" || h.entry(providerAWS).ObservedSecret.ResourceVersion != "" {
		t.Fatalf("a never-observed Secret has no identity or revision")
	}
	if got := strings.Join(h.eventReasons(), ","); got != "ObjectStorageApplyFailed,ObjectStorageApplyFailed" {
		t.Fatalf("expected one failure event per provider, got %s", got)
	}

	// Timed retry and restart with identical persisted failure: silent and no write.
	h.resetObservations()
	h.mustReconcile()
	if len(h.ops) != 0 {
		t.Fatalf("an unchanged failure must not write or emit again: %v", h.ops)
	}

	// An unrelated spec edit changes observedGeneration, so one new warning is expected.
	h.mutateSpec(func(c *marklogicv1.MarklogicCluster) { c.Spec.ImagePullPolicy = "Always" })
	h.resetObservations()
	h.mustReconcile()
	if len(h.events) != 2 {
		t.Fatalf("a changed observedGeneration yields a new failure event, got %v", h.eventReasons())
	}
}

func TestObjectStorageSecretKeyMissingAndNormalization(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.setSecret("aws-creds", "aws-uid-1", map[string]string{"accessKey": "  " + secretMarker + "-ak  ", "secretKey": " \n\t"})
	h.setSecret("azure-creds", "azure-uid-1", map[string]string{
		"storageAccount": "  acct  \n", "storageKey": "\t" + secretMarker + "-key ", "unrelated": "ignored",
	})

	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretKeyMissing)
	if !strings.Contains(h.entry(providerAWS).Message, "secretKey") {
		t.Fatalf("message should name the missing key")
	}
	if len(h.creds.aws) != 0 {
		t.Fatalf("invalid material must never be applied")
	}
	// The failing provider does not block its sibling.
	assertPhase(t, h.entry(providerAzure), marklogicv1.ObjectStoragePhaseApplied, "")
	if len(h.creds.azure) != 1 || h.creds.azure[0].StorageAccount != "acct" || h.creds.azure[0].StorageKey != secretMarker+"-key" {
		t.Fatalf("applied Azure values must be trimmed")
	}
}

func TestObjectStorageSessionTokenClearedByRotationToStatic(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	temporary := awsMaterial("1")
	temporary["sessionToken"] = " " + secretMarker + "-token "
	h.setSecret("aws-creds", "aws-uid-1", temporary)
	h.setSecret("azure-creds", "azure-uid-1", azureMaterial("1"))
	h.mustReconcile()
	if len(h.creds.aws) != 1 || h.creds.aws[0].SessionToken != secretMarker+"-token" {
		t.Fatalf("a non-empty token must be applied trimmed")
	}
	withToken := h.entry(providerAWS).AppliedFingerprint

	static := awsMaterial("2")
	static["sessionToken"] = "   "
	h.setSecret("aws-creds", "aws-uid-1", static)
	h.mustReconcile()
	if len(h.creds.aws) != 2 || h.creds.aws[1].SessionToken != "" {
		t.Fatalf("rotation to static credentials must PUT without a session token")
	}
	if h.entry(providerAWS).AppliedFingerprint == withToken {
		t.Fatalf("fingerprint should change")
	}
}

func TestObjectStorageEmptyTokenEqualsAbsentToken(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	absent := awsMaterial("1")
	h.setSecret("aws-creds", "aws-uid-1", absent)
	h.setSecret("azure-creds", "azure-uid-1", azureMaterial("1"))
	h.mustReconcile()

	empty := awsMaterial("1")
	empty["sessionToken"] = ""
	h.setSecret("aws-creds", "aws-uid-1", empty)
	h.resetObservations()
	h.mustReconcile()
	if h.countOps("put:aws") != 0 {
		t.Fatalf("an empty token normalizes to absent and must not trigger a PUT")
	}
}

func TestObjectStorageSecretReadFailurePreservesHistoryAndEligibility(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	before := h.entry(providerAWS)

	h.secretGetErr["aws-creds"] = errForbidden
	res := h.mustReconcile()

	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretReadFailed)
	assertEligible(t, entry, true)
	if entry.AppliedFingerprint != before.AppliedFingerprint {
		t.Fatalf("history must be preserved")
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("retry = %v, want 30s", res.RequeueAfter)
	}
	if strings.Contains(entry.Message, "denied") {
		t.Fatalf("message must not include raw Kubernetes error text")
	}
	if h.countOps("put:aws") != 0 {
		t.Fatalf("a read failure must not be treated as deletion or trigger a PUT")
	}
}

func TestObjectStorageBindingChangeToMissingSecretInvalidates(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)

	h.mutateSpec(func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage.AWS.SecretName = "other-creds" })
	h.mustReconcile()

	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound)
	assertEligible(t, entry, false)
	if entry.ObservedSecret.Name != "other-creds" || entry.ObservedSecret.UID != "" || entry.ObservedSecret.ResourceVersion != "" {
		t.Fatalf("a changed binding publishes the new name and clears stale identity: %#v", entry.ObservedSecret)
	}
	if entry.AppliedSecret.Name != "aws-creds" {
		t.Fatalf("last-success history must be kept")
	}
}

func TestObjectStorageBootstrapNotReadyIsPendingAndIndependent(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	// Azure has no Secret: resolved locally without bootstrap access.
	h.creds.readyErr = &mlmanage.CredentialError{Operation: "GET /manage/v2/hosts", StatusCode: 401}

	res := h.mustReconcile()
	aws := h.entry(providerAWS)
	assertPhase(t, aws, marklogicv1.ObjectStoragePhasePending, marklogicv1.ObjectStorageReasonBootstrapNotReady)
	assertEligible(t, aws, false)
	if !strings.Contains(aws.Message, "401") || !strings.Contains(strings.ToLower(aws.Message), "authentication") {
		t.Fatalf("probe 401 should be reported as bootstrap authentication failure: %q", aws.Message)
	}
	assertPhase(t, h.entry(providerAzure), marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound)
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("earliest retry = %v, want 10s", res.RequeueAfter)
	}
	if h.countOps("put:aws") != 0 {
		t.Fatalf("no PUT while prerequisites fail")
	}
	if got := strings.Join(h.eventReasons(), ","); got != "ObjectStorageApplyFailed" {
		t.Fatalf("Pending emits no failure event; only the Azure failure is expected, got %s", got)
	}

	// Repeating the pending state writes nothing.
	h.resetObservations()
	h.mustReconcile()
	if h.countOps("status-write") != 0 || len(h.events) != 0 {
		t.Fatalf("an unchanged Pending result must not write or emit: %v", h.ops)
	}

	h.creds.readyErr = nil
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestObjectStorageProbesTheBootstrapHost(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()
	h.mustReconcile()
	if len(h.creds.probedHosts) != 1 || h.creds.probedHosts[0] != "dnode-0.dnode.ml-ns.svc.cluster.local" {
		t.Fatalf("the readiness probe must name the bootstrap host, got %v", h.creds.probedHosts)
	}
}

func TestObjectStorageOfflineBootstrapHostIsPendingNotApplied(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()
	h.creds.readyErr = &mlmanage.BootstrapNotReadyError{Reason: "the bootstrap host is not online"}

	res := h.mustReconcile()
	for _, provider := range objectStorageProviders {
		entry := h.entry(provider)
		assertPhase(t, entry, marklogicv1.ObjectStoragePhasePending, marklogicv1.ObjectStorageReasonBootstrapNotReady)
		assertEligible(t, entry, false)
		if !strings.Contains(entry.Message, "not online") {
			t.Fatalf("message should identify the failed prerequisite: %q", entry.Message)
		}
	}
	if len(h.creds.aws)+len(h.creds.azure) != 0 {
		t.Fatalf("no PUT while the bootstrap host is not online")
	}
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("retry = %v, want 10s", res.RequeueAfter)
	}
}

func TestObjectStorageUnconfirmedPutResponseIsNotApplied(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)
	before := h.entry(providerAWS)
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("2"))
	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /manage/v2/credentials/properties", StatusCode: 204, ResponseIncomplete: true}

	res := h.mustReconcile()
	entry := h.entry(providerAWS)
	assertPhase(t, entry, marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonManagementAPIError)
	assertEligible(t, entry, false)
	if entry.AppliedFingerprint != before.AppliedFingerprint {
		t.Fatalf("an unconfirmed PUT must not replace the success record")
	}
	if !strings.Contains(entry.Message, "204") || !strings.Contains(entry.Message, "unconfirmed") {
		t.Fatalf("message should carry the status code and say the result is unconfirmed: %q", entry.Message)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("retry = %v, want 30s", res.RequeueAfter)
	}
	if got := strings.Join(h.eventReasons(), ","); got != "ObjectStorageApplyFailed" {
		t.Fatalf("expected a failure event and no Applied event, got %s", got)
	}
}

func TestObjectStorageBootstrapFailureMessagesAreSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"transport", &mlmanage.CredentialError{Operation: "GET /manage/v2/hosts"}, "unreachable"},
		{"not ready", &mlmanage.CredentialError{Operation: "GET /manage/v2/hosts", StatusCode: 503}, "HTTP 503"},
		{"offline host", &mlmanage.BootstrapNotReadyError{Reason: "the bootstrap host is not online"}, "not online"},
		{"unreadable response", &mlmanage.CredentialError{Operation: "GET /manage/v2/hosts", StatusCode: 200, ResponseIncomplete: true}, "could not be read completely"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			message := bootstrapProbeMessage(test.err)
			if !strings.Contains(message, test.want) {
				t.Fatalf("message %q should contain %q", message, test.want)
			}
		})
	}
}

func TestObjectStorageBootstrapAdminSecretMissingIsPending(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()
	h.secretGetErr[osCluster+"-admin"] = apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, osCluster+"-admin")

	h.mustReconcile()
	pending := h.entry(providerAWS)
	assertPhase(t, pending, marklogicv1.ObjectStoragePhasePending, marklogicv1.ObjectStorageReasonBootstrapNotReady)
	if !strings.Contains(pending.Message, "admin Secret") {
		t.Fatalf("message should identify the failed prerequisite: %q", pending.Message)
	}
	if h.creds.probes != 0 {
		t.Fatalf("no probe without credentials")
	}
}

func TestObjectStoragePutFailureClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		reason marklogicv1.ObjectStorageReason
		retry  time.Duration
		code   string
	}{
		{"400", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 400}, marklogicv1.ObjectStorageReasonInvalidPayload, 0, "400"},
		{"401", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 401}, marklogicv1.ObjectStorageReasonAuthenticationFailed, 30 * time.Second, "401"},
		{"403", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 403}, marklogicv1.ObjectStorageReasonInsufficientPrivilege, 0, "403"},
		{"404", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 404}, marklogicv1.ObjectStorageReasonManagementAPIError, 30 * time.Second, "404"},
		{"405", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 405}, marklogicv1.ObjectStorageReasonManagementAPIError, 30 * time.Second, "405"},
		{"500", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 500}, marklogicv1.ObjectStorageReasonManagementAPIUnreachable, 30 * time.Second, "500"},
		{"418", &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 418}, marklogicv1.ObjectStorageReasonManagementAPIError, 30 * time.Second, "418"},
		{"transport", &mlmanage.CredentialError{Operation: "PUT /x"}, marklogicv1.ObjectStorageReasonManagementAPIUnreachable, 30 * time.Second, ""},
		{"foreign error", errors.New("dial tcp 10.1.1.1 " + secretMarker), marklogicv1.ObjectStorageReasonManagementAPIUnreachable, 30 * time.Second, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage.Azure = nil })
			h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
			h.creds.putErr[providerAWS] = test.err

			res := h.mustReconcile()
			entry := h.entry(providerAWS)
			assertPhase(t, entry, marklogicv1.ObjectStoragePhaseFailed, test.reason)
			assertEligible(t, entry, false)
			if res.RequeueAfter != test.retry {
				t.Fatalf("retry = %v, want %v", res.RequeueAfter, test.retry)
			}
			if test.code != "" && !strings.Contains(entry.Message, "HTTP "+test.code) {
				t.Fatalf("message %q should contain the numeric status code", entry.Message)
			}
			if test.code == "" && regexp.MustCompile(`HTTP \d`).MatchString(entry.Message) {
				t.Fatalf("a transport failure must not invent an HTTP status: %q", entry.Message)
			}
			if strings.Contains(entry.Message, "10.1.1.1") || strings.Contains(entry.Message, secretMarker) {
				t.Fatalf("message must not include raw error text")
			}
			if test.code == "404" && !strings.Contains(entry.Message, "/manage/v2/credentials/properties") {
				t.Fatalf("unexpected-response messages must name the credential endpoint")
			}
		})
	}
}

func TestObjectStorageFailureEventDeduplicationKey(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage.Azure = nil })
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 403}

	h.mustReconcile()
	if len(h.events) != 1 || h.events[0].eventType != "Warning" || h.events[0].reason != "ObjectStorageApplyFailed" {
		t.Fatalf("expected one failure warning, got %v", h.eventReasons())
	}
	if !strings.Contains(h.events[0].message, "AWS") || !strings.Contains(h.events[0].message, "InsufficientPrivilege") {
		t.Fatalf("event should name the provider and reason: %q", h.events[0].message)
	}

	// Same key: silent, even across a restart (new context) and with a message-only change.
	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 403}
	h.resetObservations()
	h.mustReconcile()
	if len(h.events) != 0 {
		t.Fatalf("an unchanged key must stay silent")
	}

	// A metadata-only Secret revision is a new input revision: one new warning.
	h.touchSecret("aws-creds")
	h.resetObservations()
	h.mustReconcile()
	if len(h.events) != 1 {
		t.Fatalf("a new input revision with the same failure warns once, got %v", h.eventReasons())
	}

	// A changed reason warns again.
	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 400}
	h.resetObservations()
	h.mustReconcile()
	if len(h.events) != 1 {
		t.Fatalf("a changed reason warns, got %v", h.eventReasons())
	}

	// Manual retry (no key change) that produces the same failure is silent.
	h.resetObservations()
	h.mustReconcile()
	if len(h.events) != 0 {
		t.Fatalf("a retry producing the same failure is silent")
	}
}

func TestObjectStorageMessageOnlyChangeUpdatesStatusWithoutEvent(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage.Azure = nil })
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 502}
	h.mustReconcile()

	h.creds.putErr[providerAWS] = &mlmanage.CredentialError{Operation: "PUT /x", StatusCode: 503}
	h.resetObservations()
	h.mustReconcile()
	if len(h.events) != 0 {
		t.Fatalf("a different HTTP code under the same reason must not warn")
	}
	if !strings.Contains(h.entry(providerAWS).Message, "503") {
		t.Fatalf("status message should be updated")
	}
}

func TestObjectStorageProviderRemovalClearsOnlyThatEntry(t *testing.T) {
	t.Parallel()
	h := appliedBothProviders(t)

	h.mutateSpec(func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage.Azure = nil })
	h.mustReconcile()
	if h.entry(providerAzure) != nil || h.entry(providerAWS) == nil {
		t.Fatalf("only the Azure entry should be removed")
	}
	assertEligible(t, h.entry(providerAWS), true)
	if h.countOps("put:aws") != 0 || h.countOps("probe") != 0 {
		t.Fatalf("removal must not revoke or reapply credentials")
	}

	h.mutateSpec(func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage = nil })
	h.mustReconcile()
	if h.cluster().Status.ObjectStorage != nil {
		t.Fatalf("with no provider declared the status block must be removed")
	}
}

func TestObjectStorageSharedSecretProvidersAreIndependent(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) {
		c.Spec.ObjectStorage.Azure.SecretName = "shared-creds"
		c.Spec.ObjectStorage.AWS.SecretName = "shared-creds"
	})
	shared := awsMaterial("1")
	for key, value := range azureMaterial("1") {
		shared[key] = value
	}
	h.setSecret("shared-creds", "shared-uid-1", shared)
	h.mustReconcile()
	if len(h.creds.aws) != 1 || len(h.creds.azure) != 1 {
		t.Fatalf("each provider applies only its own keys")
	}

	h.deleteSecret("shared-creds")
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseDetached, marklogicv1.ObjectStorageReasonSecretDeletedAfterApply)
	assertPhase(t, h.entry(providerAzure), marklogicv1.ObjectStoragePhaseDetached, marklogicv1.ObjectStorageReasonSecretDeletedAfterApply)

	// Recreate with only the AWS keys: AWS re-adopts, Azure fails independently.
	h.setSecret("shared-creds", "shared-uid-2", awsMaterial("1"))
	h.resetObservations()
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
	assertPhase(t, h.entry(providerAzure), marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretKeyMissing)
	assertEligible(t, h.entry(providerAzure), false)
}

func TestObjectStorageLegacyStatusRequiresFreshPut(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) {
		c.Spec.ObjectStorage.Azure = nil
		c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: &marklogicv1.ObjectStorageProviderStatus{
			Phase:              marklogicv1.ObjectStoragePhaseApplied,
			ObservedGeneration: 1,
			ObservedSecret:     &marklogicv1.ObjectStorageSecretRef{Name: "aws-creds"},
		}}
	})
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))

	h.mustReconcile()
	if h.countOps("put:aws") != 1 {
		t.Fatalf("a legacy Applied entry without a complete success record requires a PUT")
	}
	entry := h.entry(providerAWS)
	assertEligible(t, entry, true)
	if !hasCompleteSuccess(entry) {
		t.Fatalf("success record should now be complete")
	}
	h.resetObservations()
	h.mustReconcile()
	if len(h.ops) != 0 {
		t.Fatalf("subsequent unchanged reconciles skip: %v", h.ops)
	}
}

func TestObjectStorageStaleClusterGenerationDiscardsOutcome(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage.Azure = nil })
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	h.creds.onPut = func(objectStorageProvider) {
		h.mutateSpec(func(c *marklogicv1.MarklogicCluster) { c.Spec.ImagePullPolicy = "Always" })
	}

	res, err := h.reconcile()
	if err != nil || !res.Requeue {
		t.Fatalf("a changed generation requests an immediate requeue without error, got %+v, %v", res, err)
	}
	if h.entry(providerAWS) != nil || len(h.events) != 0 {
		t.Fatalf("an outcome computed for an older generation must not be published")
	}
}

func TestObjectStorageNoStatusAndNoDeclarationIsNoop(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage = nil })
	h.mustReconcile()
	if len(h.ops) != 0 {
		t.Fatalf("nothing declared and nothing tracked must be a no-op: %v", h.ops)
	}
}

func TestObjectStorageSkipsDeletingCluster(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) {
		now := metav1.Now()
		c.DeletionTimestamp = &now
		c.Finalizers = []string{"test/hold"}
	})
	h.seedBothSecrets()
	h.mustReconcile()
	if len(h.ops) != 0 {
		t.Fatalf("teardown must not run credential operations: %v", h.ops)
	}
}

func TestObjectStorageRefusesToFingerprintWithoutClusterUID(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) { c.UID = "" })
	h.seedBothSecrets()
	if _, err := h.reconcile(); err == nil {
		t.Fatalf("fingerprinting must fail closed without a cluster UID")
	}
	if len(h.creds.aws) != 0 {
		t.Fatalf("no application without a fingerprint key")
	}
}

func TestObjectStorageStatusWriteFailureIsRetriableAndEmitsNoEvent(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()
	h.statusWriteErr = func() error { return errors.New("status unavailable") }

	if _, err := h.reconcile(); err == nil {
		t.Fatalf("expected a retriable error")
	}
	if len(h.events) != 0 {
		t.Fatalf("failed writes emit no provider events: %v", h.eventReasons())
	}
	h.statusWriteErr = nil
	h.resetObservations()
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
	if len(h.events) != 2 {
		t.Fatalf("events follow the persisted outcome on retry: %v", h.eventReasons())
	}
}

func TestObjectStorageBothProvidersShareOneBootstrapProbe(t *testing.T) {
	t.Parallel()
	h := newOSHarness(t, nil)
	h.seedBothSecrets()
	h.mustReconcile()
	if h.creds.probes != 1 {
		t.Fatalf("bootstrap readiness is checked once per reconcile, got %d", h.creds.probes)
	}
}

func TestObjectStorageNeverReadsOrDeletesCredentials(t *testing.T) {
	t.Parallel()
	// The credential client interface exposes no read or delete operation.
	var _ mlmanage.CredentialClient = (*fakeCredentialClient)(nil)
	h := appliedBothProviders(t)
	h.deleteSecret("aws-creds")
	h.mutateSpec(func(c *marklogicv1.MarklogicCluster) { c.Spec.ObjectStorage = nil })
	h.mustReconcile()
	for _, op := range h.ops {
		if strings.Contains(op, "get") || strings.Contains(op, "delete") {
			t.Fatalf("unexpected credential operation %q", op)
		}
	}
}

func TestBootstrapManagementEndpoint(t *testing.T) {
	t.Parallel()
	enabled := &marklogicv1.Tls{EnableOnDefaultAppServers: true}
	tests := []struct {
		name    string
		mutate  func(*marklogicv1.MarklogicCluster)
		host    string
		useTLS  bool
		wantErr bool
	}{
		{"defaults", func(c *marklogicv1.MarklogicCluster) {}, "dnode-0.dnode.ml-ns.svc.cluster.local", false, false},
		{"empty domain falls back", func(c *marklogicv1.MarklogicCluster) { c.Spec.ClusterDomain = "" }, "dnode-0.dnode.ml-ns.svc.cluster.local", false, false},
		{"custom domain", func(c *marklogicv1.MarklogicCluster) { c.Spec.ClusterDomain = "corp.example" }, "dnode-0.dnode.ml-ns.svc.corp.example", false, false},
		{"cluster tls", func(c *marklogicv1.MarklogicCluster) { c.Spec.Tls = enabled }, "dnode-0.dnode.ml-ns.svc.cluster.local", true, false},
		{"group tls overrides cluster", func(c *marklogicv1.MarklogicCluster) {
			c.Spec.Tls = enabled
			c.Spec.MarkLogicGroups[0].Tls = &marklogicv1.Tls{}
		}, "dnode-0.dnode.ml-ns.svc.cluster.local", false, false},
		{"no bootstrap group", func(c *marklogicv1.MarklogicCluster) { c.Spec.MarkLogicGroups[0].IsBootstrap = false }, "", false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newOSHarness(t, test.mutate)
			host, useTLS, err := bootstrapManagementEndpoint(h.cluster())
			if (err != nil) != test.wantErr || host != test.host || useTLS != test.useTLS {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, wantErr=%v)", host, useTLS, err, test.host, test.useTLS, test.wantErr)
			}
		})
	}
}

func TestObjectStorageAdminSecretFromAuthSecretName(t *testing.T) {
	t.Parallel()
	custom := "custom-admin"
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) {
		c.Spec.Auth = &marklogicv1.AdminAuth{SecretName: &custom}
		c.Spec.ObjectStorage.Azure = nil
	})
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhasePending, marklogicv1.ObjectStorageReasonBootstrapNotReady)

	h.setSecret(custom, "custom-uid", map[string]string{"username": "admin", "password": secretMarker})
	h.mustReconcile()
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}
