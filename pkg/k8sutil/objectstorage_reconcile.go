// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	eventReasonObjectStorageApplied     = "ObjectStorageApplied"
	eventReasonObjectStorageApplyFailed = "ObjectStorageApplyFailed"
	eventReasonObjectStorageDetached    = "ObjectStorageDetached"
)

// errObjectStorageStale means the cluster changed identity or generation while reconciling.
var errObjectStorageStale = errors.New("cluster changed during object storage reconciliation")

// defaultCredentialClientFactory builds the Management API credential client.
func defaultCredentialClientFactory(opts mlmanage.ClientOptions) mlmanage.CredentialClient {
	return mlmanage.NewCredentialClient(opts)
}

func (cc *ClusterContext) objectStorageReader() client.Reader {
	if cc.APIReader != nil {
		return cc.APIReader
	}
	return cc.Client
}

type providerWork struct {
	provider objectStorageProvider
	binding  providerBinding
	snapshot *marklogicv1.ObjectStorageProviderStatus
	eval     providerEvaluation
	result   *providerResult
}

// ReconcileObjectStorage applies declared object storage credentials and publishes per-provider status.
func (cc *ClusterContext) ReconcileObjectStorage() (reconcile.Result, error) {
	ctx := cc.Ctx
	logger := cc.ReqLogger

	cluster := &marklogicv1.MarklogicCluster{}
	key := types.NamespacedName{Name: cc.MarklogicCluster.Name, Namespace: cc.MarklogicCluster.Namespace}
	if err := cc.objectStorageReader().Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if cluster.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}

	declared := declaredObjectStorageProviders(cluster)
	snapshot := cluster.Status.ObjectStorage.DeepCopy()
	if len(declared) == 0 && snapshot == nil {
		return reconcile.Result{}, nil
	}
	if len(declared) > 0 && cluster.UID == "" {
		return reconcile.Result{}, errors.New("cluster UID is unavailable; refusing to fingerprint object storage credentials")
	}

	var works []*providerWork
	for _, provider := range objectStorageProviders {
		binding, ok := declared[provider]
		if !ok {
			continue
		}
		work := &providerWork{provider: provider, binding: binding, snapshot: providerEntry(snapshot, provider)}
		secret := &corev1.Secret{}
		readErr := cc.Client.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: binding.secretName}, secret)
		work.eval = evaluateProvider(provider, binding, work.snapshot, secret, readErr, cluster.UID)
		work.result = work.eval.result
		works = append(works, work)
	}

	var errs []error
	var puts []*providerWork
	for _, work := range works {
		if work.eval.put != nil {
			puts = append(puts, work)
		}
	}

	if len(puts) > 0 {
		if err := cc.persistObjectStorageCheckpoints(cluster, puts); err != nil {
			errs = append(errs, err)
		} else {
			cc.applyPendingPuts(ctx, cluster, puts)
		}
	}

	results := map[objectStorageProvider]*providerResult{}
	for _, work := range works {
		if work.result != nil {
			results[work.provider] = work.result
		}
	}
	if err := cc.persistObjectStorageResults(cluster, declared, results); err != nil {
		errs = append(errs, err)
	} else {
		cc.publishObjectStorageEvents(cluster, works)
	}

	stale := false
	filtered := errs[:0]
	for _, err := range errs {
		if errors.Is(err, errObjectStorageStale) {
			stale = true
			continue
		}
		filtered = append(filtered, err)
	}
	if len(filtered) > 0 {
		return reconcile.Result{}, errors.Join(filtered...)
	}
	if stale {
		return reconcile.Result{Requeue: true}, nil
	}

	var retryAfter time.Duration
	for _, work := range works {
		if work.result == nil || work.result.retryAfter <= 0 {
			continue
		}
		if retryAfter == 0 || work.result.retryAfter < retryAfter {
			retryAfter = work.result.retryAfter
		}
	}
	if retryAfter > 0 {
		logger.Info("object storage reconcile requested a retry", "after", retryAfter.String())
	}
	return reconcile.Result{RequeueAfter: retryAfter}, nil
}

// applyPendingPuts checks bootstrap readiness once, then applies each provider independently.
func (cc *ClusterContext) applyPendingPuts(ctx context.Context, cluster *marklogicv1.MarklogicCluster, puts []*providerWork) {
	credentialClient, prerequisiteErr := cc.newBootstrapCredentialClient(ctx, cluster)
	for _, work := range puts {
		if prerequisiteErr != nil {
			work.result = bootstrapPendingResult(work.eval.put, bootstrapProbeMessage(prerequisiteErr))
			continue
		}
		applyErr := work.eval.put.creds.apply(ctx, credentialClient)
		work.result = putOutcome(work.binding, work.eval.put, applyErr, metav1.Now())
	}
}

func (cc *ClusterContext) newBootstrapCredentialClient(ctx context.Context, cluster *marklogicv1.MarklogicCluster) (mlmanage.CredentialClient, error) {
	host, useTLS, err := bootstrapManagementEndpoint(cluster)
	if err != nil {
		return nil, err
	}
	username, password, err := cc.readBootstrapAdminCredentials(ctx, cluster)
	if err != nil {
		return nil, err
	}
	factory := cc.CredentialClientFactory
	if factory == nil {
		factory = defaultCredentialClientFactory
	}
	credentialClient := factory(mlmanage.ClientOptions{
		Host:     host,
		Username: username,
		Password: password,
		UseTLS:   useTLS,
		// Follows the existing Management API transport behavior for operator-managed certificates.
		InsecureSkipVerify: useTLS,
	})
	if err := credentialClient.CheckBootstrapReady(ctx); err != nil {
		return nil, err
	}
	return credentialClient, nil
}

func bootstrapManagementEndpoint(cluster *marklogicv1.MarklogicCluster) (string, bool, error) {
	for _, group := range cluster.Spec.MarkLogicGroups {
		if group == nil || !group.IsBootstrap {
			continue
		}
		name := strings.TrimSpace(group.Name)
		domain := strings.TrimSpace(cluster.Spec.ClusterDomain)
		if domain == "" {
			domain = "cluster.local"
		}
		tls := cluster.Spec.Tls
		if group.Tls != nil {
			tls = group.Tls
		}
		useTLS := tls != nil && tls.EnableOnDefaultAppServers
		return fmt.Sprintf("%s-0.%s.%s.svc.%s", name, name, cluster.Namespace, domain), useTLS, nil
	}
	return "", false, &bootstrapPrerequisiteError{message: "the bootstrap group is not defined."}
}

func (cc *ClusterContext) readBootstrapAdminCredentials(ctx context.Context, cluster *marklogicv1.MarklogicCluster) (string, string, error) {
	secretName := cluster.Name + "-admin"
	if cluster.Spec.Auth != nil && cluster.Spec.Auth.SecretName != nil && strings.TrimSpace(*cluster.Spec.Auth.SecretName) != "" {
		secretName = strings.TrimSpace(*cluster.Spec.Auth.SecretName)
	}
	secret := &corev1.Secret{}
	if err := cc.Client.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secretName}, secret); err != nil {
		return "", "", &bootstrapPrerequisiteError{message: "the bootstrap admin Secret could not be read."}
	}
	username, password := secret.Data["username"], secret.Data["password"]
	if len(username) == 0 || len(password) == 0 {
		return "", "", &bootstrapPrerequisiteError{message: "the bootstrap admin Secret is missing username or password."}
	}
	return string(username), string(password), nil
}

// persistObjectStorageCheckpoints durably clears eligibility, and nothing else, before a required PUT.
func (cc *ClusterContext) persistObjectStorageCheckpoints(cluster *marklogicv1.MarklogicCluster, puts []*providerWork) error {
	return cc.updateObjectStorageStatus(cluster, func(current *marklogicv1.ObjectStorageStatus) *marklogicv1.ObjectStorageStatus {
		for _, work := range puts {
			entry := providerEntry(current, work.provider)
			if isDetachEligible(entry) {
				value := false
				entry.DetachEligible = &value
			}
		}
		return current
	})
}

// persistObjectStorageResults publishes outcomes and removes entries for undeclared providers.
func (cc *ClusterContext) persistObjectStorageResults(cluster *marklogicv1.MarklogicCluster, declared map[objectStorageProvider]providerBinding, results map[objectStorageProvider]*providerResult) error {
	return cc.updateObjectStorageStatus(cluster, func(current *marklogicv1.ObjectStorageStatus) *marklogicv1.ObjectStorageStatus {
		next := current
		if next == nil {
			next = &marklogicv1.ObjectStorageStatus{}
		}
		for _, provider := range objectStorageProviders {
			if _, ok := declared[provider]; !ok {
				setProviderEntry(next, provider, nil)
				continue
			}
			if result := results[provider]; result != nil {
				setProviderEntry(next, provider, applyProviderResult(providerEntry(next, provider), *result, cluster.Generation))
			}
		}
		if next.AWS == nil && next.Azure == nil {
			return nil
		}
		return next
	})
}

// updateObjectStorageStatus rereads the cluster uncached and writes only when the status changes.
func (cc *ClusterContext) updateObjectStorageStatus(cluster *marklogicv1.MarklogicCluster, mutate func(*marklogicv1.ObjectStorageStatus) *marklogicv1.ObjectStorageStatus) error {
	key := types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &marklogicv1.MarklogicCluster{}
		if err := cc.objectStorageReader().Get(cc.Ctx, key, current); err != nil {
			return err
		}
		if current.UID != cluster.UID || current.DeletionTimestamp != nil || current.Generation != cluster.Generation {
			return errObjectStorageStale
		}
		next := mutate(current.Status.ObjectStorage.DeepCopy())
		if equality.Semantic.DeepEqual(current.Status.ObjectStorage, next) {
			return nil
		}
		current.Status.ObjectStorage = next
		return cc.Client.Status().Update(cc.Ctx, current)
	})
}

// publishObjectStorageEvents emits best-effort events after outcomes are persisted.
func (cc *ClusterContext) publishObjectStorageEvents(cluster *marklogicv1.MarklogicCluster, works []*providerWork) {
	for _, work := range works {
		result := work.result
		if result == nil {
			continue
		}
		name := work.provider.displayName()
		switch result.phase {
		case marklogicv1.ObjectStoragePhaseApplied:
			if result.applied != nil {
				cc.ReqLogger.Info("object storage credentials applied", "provider", string(work.provider))
				cc.recordObjectStorageEvent(cluster, corev1.EventTypeNormal, eventReasonObjectStorageApplied, name+" object storage credentials applied.")
			}
		case marklogicv1.ObjectStoragePhaseFailed:
			if work.snapshot == nil || failureKeyOfEntry(work.snapshot) != failureKeyOfResult(cluster.Generation, *result) {
				cc.ReqLogger.Info("object storage credentials failed", "provider", string(work.provider), "reason", string(result.reason))
				cc.recordObjectStorageEvent(cluster, corev1.EventTypeWarning, eventReasonObjectStorageApplyFailed,
					fmt.Sprintf("%s object storage credentials failed (%s): %s", name, result.reason, result.message))
			}
		case marklogicv1.ObjectStoragePhaseDetached:
			if work.snapshot == nil || work.snapshot.Phase != marklogicv1.ObjectStoragePhaseDetached {
				cc.recordObjectStorageEvent(cluster, corev1.EventTypeWarning, eventReasonObjectStorageDetached, name+" object storage Secret detached. "+result.message)
			}
		}
	}
}

func (cc *ClusterContext) recordObjectStorageEvent(cluster *marklogicv1.MarklogicCluster, eventType, reason, message string) {
	if cc.Recorder == nil {
		return
	}
	cc.Recorder.Event(cluster, eventType, reason, message)
}
