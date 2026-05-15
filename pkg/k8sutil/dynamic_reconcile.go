// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var NewDynamicManagementClient = func(opts mlmanage.ClientOptions) mlmanage.Client {
	return mlmanage.NewClient(opts)
}

const (
	dynamicPhasePending    = "pending"
	dynamicPhaseReconciling = "reconciling"
	dynamicPhaseDeleting   = "deleting"
	dynamicPhaseDegraded   = "degraded"
	dynamicPhaseFailed     = "failed"
	dynamicPhaseConfigured = "configured"

	dynamicReasonBootstrapNotReady   = "BootstrapNotReady"
	dynamicReasonInvalidConfig       = "InvalidConfiguration"
	dynamicReasonGroupConfigFailed   = "GroupConfigFailed"
	dynamicReasonJoinFailed          = "JoinFailed"
	dynamicReasonRemoveFailed        = "RemoveFailed"
	dynamicReasonRetryBudgetExceeded = "RetryBudgetExhausted"

	dynamicHostStatePending = "pending"
	dynamicHostStateJoining = "joining"
	dynamicHostStateJoined  = "joined"
	dynamicHostStateRetained = "retained"
	dynamicHostStateRemoving = "removing"
	dynamicHostStateRemoved  = "removed"
	dynamicHostStateFailed  = "failed"

	dynamicHostCleanupFinalizer  = "marklogic.progress.com/dynamic-host-cleanup"
	dynamicGroupCleanupFinalizer = "marklogic.progress.com/dynamic-group-cleanup"

	minimumSupportedMarkLogicVersion = 12
	dynamicJoinRetryBudget           = int32(3)
	dynamicRemoveRetryBudget         = int32(3)
	dynamicJoinRequeueSeconds        = 2
)

var statusCodeRegex = regexp.MustCompile(`status\s+(\d{3})`)

func (oc *OperatorContext) ReconcileDynamicGroupConfig() result.ReconcileResult {
	if !oc.MarklogicGroup.Spec.IsDynamic {
		return result.Continue()
	}

	if oc.MarklogicGroup.DeletionTimestamp == nil {
		added, err := oc.ensureDynamicGroupFinalizer()
		if err != nil {
			return result.Error(err)
		}
		if added {
			return result.RequeueSoon(dynamicJoinRequeueSeconds)
		}
	}

	if oc.MarklogicGroup.Status.Dynamic == nil {
		phase := dynamicPhasePending
		message := "waiting for bootstrap readiness"
		if oc.MarklogicGroup.DeletionTimestamp != nil {
			phase = dynamicPhaseDeleting
			message = "dynamic group deletion cleanup is pending"
		}
		if err := oc.setDynamicStatus(phase, "", message, false, false, false); err != nil {
			return result.Error(err)
		}
	}

	clusterName, err := oc.getOwningClusterName()
	if err != nil {
		if oc.MarklogicGroup.DeletionTimestamp != nil {
			clusterName = ""
		} else {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, err.Error(), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
		}
	}

	if oc.MarklogicGroup.DeletionTimestamp != nil && oc.isOwningClusterDeletingOrGone() {
		return oc.releaseDynamicFinalizersWithoutBootstrap()
	}

	bootstrapHost := strings.TrimSpace(oc.MarklogicGroup.Spec.BootstrapHost)
	if bootstrapHost == "" {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, "bootstrap host is required for dynamic groups", false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	adminSecretName := strings.TrimSpace(oc.MarklogicGroup.Spec.SecretName)
	if adminSecretName == "" {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, "admin credential secret is missing", false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	adminUser, adminPass, err := oc.readCredentialSecret(adminSecretName)
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, fmt.Sprintf("failed to read admin credentials: %v", err), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	useTLS := oc.MarklogicGroup.Spec.Tls != nil && oc.MarklogicGroup.Spec.Tls.EnableOnDefaultAppServers
	adminClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host:               bootstrapHost,
		Username:           adminUser,
		Password:           adminPass,
		UseTLS:             useTLS,
		InsecureSkipVerify: useTLS,
	})

	hosts, err := adminClient.ListHostsStatus(oc.Ctx)
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, fmt.Sprintf("bootstrap readiness check failed: %v", err), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	version := ""
	for _, host := range hosts {
		if !host.Online {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, "bootstrap hosts are not yet online", false, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if version == "" && host.Version != "" {
			version = host.Version
		}
	}

	if !isSupportedMarkLogicVersion(version) {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, fmt.Sprintf("unsupported bootstrap MarkLogic version: %s", version), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	manageSecret, err := oc.ensureDynamicCredentialSecret(clusterName)
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to reconcile dynamic credential secret: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}
	manageUser := string(manageSecret.Data["username"])
	managePass := string(manageSecret.Data["password"])

	if err := adminClient.EnsureManageAdminUser(oc.Ctx, manageUser, managePass); err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("manage-admin user reconcile is pending: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("manage-admin user reconcile failed: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	groupClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host:               bootstrapHost,
		Username:           manageUser,
		Password:           managePass,
		UseTLS:             useTLS,
		InsecureSkipVerify: useTLS,
	})

	groupName := resolvedMarkLogicGroupName(oc.MarklogicGroup)
	groupInfo, err := groupClient.GetGroup(oc.Ctx, groupName)
	if err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to inspect MarkLogic group: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to inspect MarkLogic group: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if groupInfo.Exists && groupInfo.ForestCount > 0 {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, "dynamic group is mapped to a MarkLogic group that already has forests", true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if !groupInfo.Exists {
		if err := groupClient.CreateGroup(oc.Ctx, groupName); err != nil {
			if isTransientManagementError(err) {
				if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to create MarkLogic group: %v", err), true, false, false); err != nil {
					return result.Error(err)
				}
				return result.RequeueSoon(5)
			}
			if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to create MarkLogic group: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.Done()
		}
	}

	if err := groupClient.EnableDynamicHosts(oc.Ctx, groupName); err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable allow-dynamic-hosts: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable allow-dynamic-hosts: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if err := groupClient.EnableAdminAPITokenAuthentication(oc.Ctx, groupName); err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable API-token-authentication: %v", err), true, false, true); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable API-token-authentication: %v", err), true, false, true); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	desiredReplicas := desiredDynamicReplicas(oc.MarklogicGroup)
	tokenDuration := dynamicTokenDuration(oc.MarklogicGroup)
	return oc.reconcileDynamicLifecycle(groupClient, clusterName, groupName, tokenDuration, desiredReplicas)
}

func (oc *OperatorContext) reconcileDynamicLifecycle(groupClient mlmanage.Client, clusterName, groupName, tokenDuration string, desiredReplicas int32) result.ReconcileResult {
	pods, err := oc.listDynamicPods()
	if err != nil {
		if statusErr := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonJoinFailed, fmt.Sprintf("failed to list dynamic pods: %v", err), true, true, true); statusErr != nil {
			return result.Error(statusErr)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	if oc.MarklogicGroup.DeletionTimestamp != nil {
		return oc.reconcileDynamicDeletionLifecycle(groupClient, clusterName, groupName, desiredReplicas, pods)
	}

	if err := oc.ensureDynamicPodFinalizers(pods); err != nil {
		if statusErr := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonJoinFailed, fmt.Sprintf("failed to ensure pod cleanup finalizers: %v", err), true, true, true); statusErr != nil {
			return result.Error(statusErr)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	members, err := groupClient.ListGroupHosts(oc.Ctx, groupName)
	if err != nil {
		localReadyReplicas := countLocalReadyPods(pods)
		currentReadyReplicas := int32(0)
		hosts := []marklogicv1.DynamicHostStatus{}
		if oc.MarklogicGroup.Status.Dynamic != nil {
			currentReadyReplicas = oc.MarklogicGroup.Status.Dynamic.ReadyReplicas
			hosts = oc.MarklogicGroup.Status.Dynamic.Hosts
		}
		phase := dynamicPhaseDegraded
		reason := dynamicReasonBootstrapNotReady
		if isPermanentAuthError(err) {
			phase = dynamicPhaseFailed
		}
		if statusErr := oc.setDynamicStatusDetailed(phase, reason, fmt.Sprintf("failed to query dynamic group membership: %v", err), true, true, true, desiredReplicas, localReadyReplicas, currentReadyReplicas, hosts); statusErr != nil {
			return result.Error(statusErr)
		}
		if phase == dynamicPhaseFailed {
			return result.Done()
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	previousHosts := []marklogicv1.DynamicHostStatus{}
	if oc.MarklogicGroup.Status.Dynamic != nil {
		previousHosts = oc.MarklogicGroup.Status.Dynamic.Hosts
	}
	hostStatuses, localReadyReplicas, readyReplicas, joinCandidates := oc.buildDynamicHostStatuses(pods, members, previousHosts)
	hostStatuses = pruneRemovedHostStatuses(hostStatuses, pods, members)

	if desiredReplicas < int32(len(members)) || hasPodsAboveDesiredOrdinal(pods, desiredReplicas) {
		return oc.reconcileDynamicScaleDown(groupClient, clusterName, groupName, desiredReplicas, false, pods, members, hostStatuses, localReadyReplicas, readyReplicas)
	}

	if desiredReplicas <= readyReplicas {
		phase := dynamicPhaseConfigured
		reason := ""
		message := "dynamic hosts are configured and at desired joined replicas"
		if hasFailedDynamicHost(hostStatuses) {
			phase = dynamicPhaseDegraded
			reason = dynamicReasonJoinFailed
			message = "one or more dynamic hosts failed and require intervention"
		}
		if err := oc.setDynamicStatusDetailed(phase, reason, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	}

	if len(joinCandidates) == 0 {
		phase := dynamicPhaseReconciling
		reason := ""
		message := fmt.Sprintf("waiting for local-ready pods to join MarkLogic (%d/%d)", readyReplicas, desiredReplicas)
		if hasFailedDynamicHost(hostStatuses) {
			phase = dynamicPhaseDegraded
			reason = dynamicReasonJoinFailed
			message = fmt.Sprintf("one or more hosts failed to join MarkLogic (%d/%d)", readyReplicas, desiredReplicas)
		}
		if err := oc.setDynamicStatusDetailed(phase, reason, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	pod := joinCandidates[0]
	hostFQDN := dynamicPodFQDN(oc.MarklogicGroup, pod.Name)
	currentAttempts := incrementDynamicHostAttempts(hostStatuses, pod.Name)
	hostStatuses = setDynamicHostStatus(hostStatuses, pod.Name, hostFQDN, dynamicHostStateJoining, "joining host with dynamic token", "", currentAttempts)
	if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("joining %s into dynamic group", pod.Name), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}

	joinedHost, err := oc.joinDynamicPod(groupClient, clusterName, groupName, hostFQDN, tokenDuration)
	if err != nil {
		return oc.handleDynamicJoinFailure(hostStatuses, pod.Name, hostFQDN, desiredReplicas, localReadyReplicas, readyReplicas, err)
	}

	members, err = groupClient.ListGroupHosts(oc.Ctx, groupName)
	if err != nil {
		return oc.handleDynamicJoinFailure(hostStatuses, pod.Name, hostFQDN, desiredReplicas, localReadyReplicas, readyReplicas, err)
	}

	member, found := findGroupHostForPod(pod.Name, hostFQDN, members)
	if !found {
		if joinedHost.Name != "" {
			member = joinedHost
			found = true
		} else {
			return oc.handleDynamicJoinFailure(hostStatuses, pod.Name, hostFQDN, desiredReplicas, localReadyReplicas, readyReplicas, fmt.Errorf("host %s is not yet registered in MarkLogic", hostFQDN))
		}
	}

	hostStatuses, localReadyReplicas, readyReplicas, joinCandidates = oc.buildDynamicHostStatuses(pods, members, hostStatuses)
	hostStatuses = setDynamicHostStatus(hostStatuses, pod.Name, hostFQDN, dynamicHostStateJoined, "", member.HostID, 0)

	if desiredReplicas <= readyReplicas {
		if err := oc.setDynamicStatusDetailed(dynamicPhaseConfigured, "", "dynamic hosts are configured and at desired joined replicas", true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	}

	if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("joined %s, continuing scale-up (%d/%d)", pod.Name, readyReplicas, desiredReplicas), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}
	return result.RequeueSoon(dynamicJoinRequeueSeconds)
}

func (oc *OperatorContext) reconcileDynamicDeletionLifecycle(groupClient mlmanage.Client, clusterName, groupName string, desiredReplicas int32, pods []corev1.Pod) result.ReconcileResult {
	if oc.isOwningClusterDeletingOrGone() {
		return oc.releaseDynamicFinalizersWithoutBootstrap()
	}

	members, err := groupClient.ListGroupHosts(oc.Ctx, groupName)
	if err != nil {
		if statusErr := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, fmt.Sprintf("failed to query dynamic group membership during deletion cleanup: %v", err), true, true, true); statusErr != nil {
			return result.Error(statusErr)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	previousHosts := []marklogicv1.DynamicHostStatus{}
	if oc.MarklogicGroup.Status.Dynamic != nil {
		previousHosts = oc.MarklogicGroup.Status.Dynamic.Hosts
	}
	hostStatuses, localReadyReplicas, readyReplicas, _ := oc.buildDynamicHostStatuses(pods, members, previousHosts)
	hostStatuses = pruneRemovedHostStatuses(hostStatuses, pods, members)

	if len(members) > 0 {
		return oc.reconcileDynamicScaleDown(groupClient, clusterName, groupName, desiredReplicas, true, pods, members, hostStatuses, localReadyReplicas, readyReplicas)
	}

	if err := oc.releaseDynamicPodFinalizers(pods); err != nil {
		if statusErr := oc.setDynamicStatusDetailed(dynamicPhaseDegraded, dynamicReasonRemoveFailed, fmt.Sprintf("failed to release dynamic pod finalizers: %v", err), true, true, true, 0, localReadyReplicas, readyReplicas, hostStatuses); statusErr != nil {
			return result.Error(statusErr)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	if err := oc.setDynamicStatusDetailed(dynamicPhaseDeleting, "", "dynamic cleanup completed; releasing group finalizer", true, true, true, 0, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}

	removed, err := oc.removeDynamicGroupFinalizer()
	if err != nil {
		return result.Error(err)
	}
	if removed {
		return result.Done()
	}
	return result.Done()
}

func (oc *OperatorContext) reconcileDynamicScaleDown(groupClient mlmanage.Client, clusterName, groupName string, desiredReplicas int32, deleting bool, pods []corev1.Pod, members []mlmanage.GroupHost, hostStatuses []marklogicv1.DynamicHostStatus, localReadyReplicas, readyReplicas int32) result.ReconcileResult {
	storageRequiresRemove := deleting || !isDynamicPVCBacked(oc.MarklogicGroup)
	candidates := hostsAboveDesiredOrdinal(hostStatuses, desiredReplicas)

	if len(candidates) == 0 {
		phase := dynamicPhaseConfigured
		reason := ""
		message := "dynamic scale-down cleanup is complete"
		if deleting {
			phase = dynamicPhaseDeleting
			message = "deletion cleanup in progress"
		}
		if hasFailedDynamicHost(hostStatuses) {
			phase = dynamicPhaseDegraded
			reason = dynamicReasonRemoveFailed
			message = "one or more dynamic hosts failed cleanup"
		}
		if err := oc.setDynamicStatusDetailed(phase, reason, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	}

	for _, candidate := range candidates {
		pod := findDynamicPodByName(pods, candidate.PodName)
		member, memberFound := findGroupHostForPod(candidate.PodName, candidate.Hostname, members)
		hostID := candidate.HostID
		if hostID == "" && memberFound {
			hostID = member.HostID
		}

		if storageRequiresRemove {
			if memberFound {
				hostStatuses = setDynamicHostStatus(hostStatuses, candidate.PodName, candidate.Hostname, dynamicHostStateRemoving, "removing dynamic host from MarkLogic", hostID, incrementDynamicHostAttempts(hostStatuses, candidate.PodName))
				if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("removing dynamic host %s", candidate.PodName), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
					return result.Error(err)
				}

				if removeErr := groupClient.RemoveDynamicHost(oc.Ctx, clusterName, hostID); removeErr != nil {
					return oc.handleDynamicRemoveFailure(hostStatuses, candidate.PodName, candidate.Hostname, hostID, desiredReplicas, localReadyReplicas, readyReplicas, removeErr, deleting)
				}
				hostStatuses = setDynamicHostStatus(hostStatuses, candidate.PodName, candidate.Hostname, dynamicHostStateRemoved, "host removed from MarkLogic; waiting for pod deletion", hostID, 0)
			}

			if pod != nil {
				if err := oc.releaseDynamicPodFinalizer(pod); err != nil {
					if statusErr := oc.setDynamicStatusDetailed(dynamicPhaseDegraded, dynamicReasonRemoveFailed, fmt.Sprintf("failed to release pod finalizer for %s: %v", candidate.PodName, err), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); statusErr != nil {
						return result.Error(statusErr)
					}
					return result.RequeueSoon(dynamicJoinRequeueSeconds)
				}

				phase := dynamicPhaseReconciling
				if deleting {
					phase = dynamicPhaseDeleting
				}
				if err := oc.setDynamicStatusDetailed(phase, "", fmt.Sprintf("waiting for pod %s deletion after cleanup", candidate.PodName), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
					return result.Error(err)
				}
				return result.RequeueSoon(dynamicJoinRequeueSeconds)
			}

			if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("cleanup complete for host %s", candidate.PodName), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(dynamicJoinRequeueSeconds)
		}

		hostStatuses = setDynamicHostStatus(hostStatuses, candidate.PodName, candidate.Hostname, dynamicHostStateRetained, "retaining host membership for pvc-backed scale-down", hostID, 0)
		if pod != nil {
			if err := oc.releaseDynamicPodFinalizer(pod); err != nil {
				if statusErr := oc.setDynamicStatusDetailed(dynamicPhaseDegraded, dynamicReasonRemoveFailed, fmt.Sprintf("failed to release pod finalizer for %s: %v", candidate.PodName, err), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); statusErr != nil {
					return result.Error(statusErr)
				}
				return result.RequeueSoon(dynamicJoinRequeueSeconds)
			}

			if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("retaining pvc-backed host %s while waiting for pod deletion", candidate.PodName), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(dynamicJoinRequeueSeconds)
		}

		if err := oc.setDynamicStatusDetailed(dynamicPhaseConfigured, "", "pvc-backed dynamic hosts retained after scale-down", true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	}

	return result.Continue()
}

func (oc *OperatorContext) setDynamicStatus(phase, reason, message string, bootstrapReady, configured, dynamicHostsEnabled bool) error {
	current := oc.MarklogicGroup.Status.Dynamic
	desiredReplicas := int32(0)
	localReadyReplicas := int32(0)
	readyReplicas := int32(0)
	var hosts []marklogicv1.DynamicHostStatus
	if current != nil {
		desiredReplicas = current.DesiredReplicas
		localReadyReplicas = current.LocalReadyReplicas
		readyReplicas = current.ReadyReplicas
		hosts = current.Hosts
	}
	return oc.setDynamicStatusDetailed(phase, reason, message, bootstrapReady, configured, dynamicHostsEnabled, desiredReplicas, localReadyReplicas, readyReplicas, hosts)
}

func (oc *OperatorContext) setDynamicStatusDetailed(phase, reason, message string, bootstrapReady, configured, dynamicHostsEnabled bool, desiredReplicas, localReadyReplicas, readyReplicas int32, hosts []marklogicv1.DynamicHostStatus) error {
	current := oc.MarklogicGroup.Status.Dynamic
	next := &marklogicv1.DynamicGroupStatus{
		Phase:               phase,
		Reason:              reason,
		Message:             message,
		BootstrapReady:      bootstrapReady,
		Configured:          configured,
		DynamicHostsEnabled: dynamicHostsEnabled,
		DesiredReplicas:     desiredReplicas,
		LocalReadyReplicas:  localReadyReplicas,
		ReadyReplicas:       readyReplicas,
		Hosts:               hosts,
	}
	if current != nil && current.Phase == next.Phase && current.Reason == next.Reason && current.Message == next.Message && current.BootstrapReady == next.BootstrapReady && current.Configured == next.Configured && current.DynamicHostsEnabled == next.DynamicHostsEnabled && current.DesiredReplicas == next.DesiredReplicas && current.LocalReadyReplicas == next.LocalReadyReplicas && current.ReadyReplicas == next.ReadyReplicas && reflect.DeepEqual(current.Hosts, next.Hosts) {
		return nil
	}

	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	oc.MarklogicGroup.Status.Dynamic = next
	return oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch)
}

func (oc *OperatorContext) getOwningClusterName() (string, error) {
	for _, ownerRef := range oc.MarklogicGroup.OwnerReferences {
		if ownerRef.Kind == "MarklogicCluster" {
			return ownerRef.Name, nil
		}
	}
	if strings.HasSuffix(oc.MarklogicGroup.Spec.SecretName, "-admin") {
		return strings.TrimSuffix(oc.MarklogicGroup.Spec.SecretName, "-admin"), nil
	}
	return "", fmt.Errorf("unable to resolve owning MarklogicCluster for dynamic group")
}

func (oc *OperatorContext) ensureDynamicCredentialSecret(clusterName string) (*corev1.Secret, error) {
	secretName := dynamicCredentialSecretName(clusterName)
	nsName := types.NamespacedName{Name: secretName, Namespace: oc.MarklogicGroup.Namespace}
	secret := &corev1.Secret{}
	if err := oc.Client.Get(oc.Ctx, nsName, secret); err == nil {
		return secret, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	labels := getSelectorLabels(clusterName)
	annotations := oc.GetOperatorAnnotations()
	secretMeta := generateObjectMeta(secretName, oc.MarklogicGroup.Namespace, labels, annotations)
	secretData := generateDynamicSecretData(clusterName)
	secretDef := generateSecretDef(secretMeta, marklogicServerAsOwner(oc.MarklogicGroup), secretData)

	if err := oc.Client.Create(oc.Ctx, secretDef); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}

	if err := oc.Client.Get(oc.Ctx, nsName, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (oc *OperatorContext) readCredentialSecret(secretName string) (string, string, error) {
	secret := &corev1.Secret{}
	nsName := types.NamespacedName{Name: secretName, Namespace: oc.MarklogicGroup.Namespace}
	if err := oc.Client.Get(oc.Ctx, nsName, secret); err != nil {
		return "", "", err
	}
	username, hasUser := secret.Data["username"]
	password, hasPass := secret.Data["password"]
	if !hasUser || !hasPass {
		return "", "", fmt.Errorf("secret %s missing username/password", secretName)
	}
	return string(username), string(password), nil
}

func resolvedMarkLogicGroupName(group *marklogicv1.MarklogicGroup) string {
	if group.Spec.GroupConfig != nil && strings.TrimSpace(group.Spec.GroupConfig.Name) != "" {
		return group.Spec.GroupConfig.Name
	}
	if strings.TrimSpace(group.Spec.Name) != "" {
		return group.Spec.Name
	}
	return group.Name
}

func isSupportedMarkLogicVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	parts := strings.SplitN(version, ".", 2)
	majorPart := parts[0]
	majorPart = strings.TrimLeftFunc(majorPart, func(r rune) bool { return r < '0' || r > '9' })
	if majorPart == "" {
		return false
	}
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return false
	}
	return major >= minimumSupportedMarkLogicVersion
}

func isTransientManagementError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "timeout") || strings.Contains(message, "connection refused") || strings.Contains(message, "no such host") || strings.Contains(message, "temporary") {
		return true
	}
	statusMatch := statusCodeRegex.FindStringSubmatch(message)
	if len(statusMatch) != 2 {
		return false
	}
	statusCode, convErr := strconv.Atoi(statusMatch[1])
	if convErr != nil {
		return false
	}
	return statusCode == 429 || statusCode >= 500
}

func isPermanentAuthError(err error) bool {
	statusCode, ok := statusCodeFromError(err)
	if !ok {
		return false
	}
	return statusCode == 401 || statusCode == 403
}

func isTokenExpiredError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "token") && strings.Contains(message, "expired")
}

func statusCodeFromError(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	statusMatch := statusCodeRegex.FindStringSubmatch(strings.ToLower(err.Error()))
	if len(statusMatch) != 2 {
		return 0, false
	}
	statusCode, convErr := strconv.Atoi(statusMatch[1])
	if convErr != nil {
		return 0, false
	}
	return statusCode, true
}

func desiredDynamicReplicas(group *marklogicv1.MarklogicGroup) int32 {
	if group.Spec.Replicas != nil {
		return *group.Spec.Replicas
	}
	return 1
}

func dynamicTokenDuration(group *marklogicv1.MarklogicGroup) string {
	if group.Spec.Dynamic != nil && strings.TrimSpace(group.Spec.Dynamic.TokenDuration) != "" {
		return strings.TrimSpace(group.Spec.Dynamic.TokenDuration)
	}
	return "PT15M"
}

func (oc *OperatorContext) reconcileDynamicScaleUp(groupClient mlmanage.Client, clusterName, groupName, tokenDuration string, desiredReplicas int32) result.ReconcileResult {
	pods, err := oc.listDynamicPods()
	if err != nil {
		if statusErr := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonJoinFailed, fmt.Sprintf("failed to list dynamic pods: %v", err), true, true, true); statusErr != nil {
			return result.Error(statusErr)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	members, err := groupClient.ListGroupHosts(oc.Ctx, groupName)
	if err != nil {
		localReadyReplicas := countLocalReadyPods(pods)
		currentReadyReplicas := int32(0)
		hosts := []marklogicv1.DynamicHostStatus{}
		if oc.MarklogicGroup.Status.Dynamic != nil {
			currentReadyReplicas = oc.MarklogicGroup.Status.Dynamic.ReadyReplicas
			hosts = oc.MarklogicGroup.Status.Dynamic.Hosts
		}
		phase := dynamicPhaseDegraded
		reason := dynamicReasonJoinFailed
		if isPermanentAuthError(err) {
			phase = dynamicPhaseFailed
		}
		if statusErr := oc.setDynamicStatusDetailed(phase, reason, fmt.Sprintf("failed to query dynamic group membership: %v", err), true, true, true, desiredReplicas, localReadyReplicas, currentReadyReplicas, hosts); statusErr != nil {
			return result.Error(statusErr)
		}
		if phase == dynamicPhaseFailed {
			return result.Done()
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	previousHosts := []marklogicv1.DynamicHostStatus{}
	if oc.MarklogicGroup.Status.Dynamic != nil {
		previousHosts = oc.MarklogicGroup.Status.Dynamic.Hosts
	}
	hostStatuses, localReadyReplicas, readyReplicas, joinCandidates := oc.buildDynamicHostStatuses(pods, members, previousHosts)

	if desiredReplicas <= readyReplicas {
		if err := oc.setDynamicStatusDetailed(dynamicPhaseConfigured, "", "dynamic hosts are configured and at desired joined replicas", true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	}

	if len(joinCandidates) == 0 {
		phase := dynamicPhaseReconciling
		reason := ""
		message := fmt.Sprintf("waiting for local-ready pods to join MarkLogic (%d/%d)", readyReplicas, desiredReplicas)
		if hasFailedDynamicHost(hostStatuses) {
			phase = dynamicPhaseDegraded
			reason = dynamicReasonJoinFailed
			message = fmt.Sprintf("one or more hosts failed to join MarkLogic (%d/%d)", readyReplicas, desiredReplicas)
		}
		if err := oc.setDynamicStatusDetailed(phase, reason, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	pod := joinCandidates[0]
	hostFQDN := dynamicPodFQDN(oc.MarklogicGroup, pod.Name)
	currentAttempts := incrementDynamicHostAttempts(hostStatuses, pod.Name)
	hostStatuses = setDynamicHostStatus(hostStatuses, pod.Name, hostFQDN, dynamicHostStateJoining, "joining host with dynamic token", "", currentAttempts)
	if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("joining %s into dynamic group", pod.Name), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}

	joinedHost, err := oc.joinDynamicPod(groupClient, clusterName, groupName, hostFQDN, tokenDuration)
	if err != nil {
		return oc.handleDynamicJoinFailure(hostStatuses, pod.Name, hostFQDN, desiredReplicas, localReadyReplicas, readyReplicas, err)
	}

	members, err = groupClient.ListGroupHosts(oc.Ctx, groupName)
	if err != nil {
		return oc.handleDynamicJoinFailure(hostStatuses, pod.Name, hostFQDN, desiredReplicas, localReadyReplicas, readyReplicas, err)
	}

	member, found := findGroupHostForPod(pod.Name, hostFQDN, members)
	if !found {
		if joinedHost.Name != "" {
			member = joinedHost
			found = true
		} else {
			return oc.handleDynamicJoinFailure(hostStatuses, pod.Name, hostFQDN, desiredReplicas, localReadyReplicas, readyReplicas, fmt.Errorf("host %s is not yet registered in MarkLogic", hostFQDN))
		}
	}

	hostStatuses, localReadyReplicas, readyReplicas, joinCandidates = oc.buildDynamicHostStatuses(pods, members, hostStatuses)
	hostStatuses = setDynamicHostStatus(hostStatuses, pod.Name, hostFQDN, dynamicHostStateJoined, "", member.HostID, 0)

	if desiredReplicas <= readyReplicas {
		if err := oc.setDynamicStatusDetailed(dynamicPhaseConfigured, "", "dynamic hosts are configured and at desired joined replicas", true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	}

	if err := oc.setDynamicStatusDetailed(dynamicPhaseReconciling, "", fmt.Sprintf("joined %s, continuing scale-up (%d/%d)", pod.Name, readyReplicas, desiredReplicas), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}
	return result.RequeueSoon(dynamicJoinRequeueSeconds)
}

func (oc *OperatorContext) handleDynamicJoinFailure(hostStatuses []marklogicv1.DynamicHostStatus, podName, hostFQDN string, desiredReplicas, localReadyReplicas, readyReplicas int32, joinErr error) result.ReconcileResult {
	attempts := incrementDynamicHostAttempts(hostStatuses, podName) + 1
	message := fmt.Sprintf("join failed for %s: %v", podName, joinErr)

	if isPermanentAuthError(joinErr) {
		hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateFailed, message, "", attempts)
		if err := oc.setDynamicStatusDetailed(dynamicPhaseFailed, dynamicReasonJoinFailed, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if isTransientManagementError(joinErr) || isTokenExpiredError(joinErr) {
		if attempts >= dynamicJoinRetryBudget {
			hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateFailed, fmt.Sprintf("retry budget exhausted for %s: %v", podName, joinErr), "", attempts)
			if err := oc.setDynamicStatusDetailed(dynamicPhaseDegraded, dynamicReasonRetryBudgetExceeded, fmt.Sprintf("retry budget exhausted while joining %s", podName), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(dynamicJoinRequeueSeconds)
		}

		hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStatePending, message, "", attempts)
		if err := oc.setDynamicStatusDetailed(dynamicPhaseDegraded, dynamicReasonJoinFailed, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateFailed, message, "", attempts)
	if err := oc.setDynamicStatusDetailed(dynamicPhaseFailed, dynamicReasonJoinFailed, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}
	return result.Done()
}

func (oc *OperatorContext) handleDynamicRemoveFailure(hostStatuses []marklogicv1.DynamicHostStatus, podName, hostFQDN, hostID string, desiredReplicas, localReadyReplicas, readyReplicas int32, removeErr error, deleting bool) result.ReconcileResult {
	attempts := incrementDynamicHostAttempts(hostStatuses, podName) + 1
	message := fmt.Sprintf("remove failed for %s: %v", podName, removeErr)

	phase := dynamicPhaseDegraded
	if deleting {
		phase = dynamicPhaseDeleting
	}

	if isPermanentAuthError(removeErr) {
		hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateFailed, message, hostID, attempts)
		if err := oc.setDynamicStatusDetailed(dynamicPhaseFailed, dynamicReasonRemoveFailed, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if isTransientManagementError(removeErr) {
		if attempts >= dynamicRemoveRetryBudget {
			hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateFailed, fmt.Sprintf("retry budget exhausted for %s: %v", podName, removeErr), hostID, attempts)
			if err := oc.setDynamicStatusDetailed(dynamicPhaseDegraded, dynamicReasonRetryBudgetExceeded, fmt.Sprintf("retry budget exhausted while removing %s", podName), true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(dynamicJoinRequeueSeconds)
		}

		hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateRemoving, message, hostID, attempts)
		if err := oc.setDynamicStatusDetailed(phase, dynamicReasonRemoveFailed, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(dynamicJoinRequeueSeconds)
	}

	hostStatuses = setDynamicHostStatus(hostStatuses, podName, hostFQDN, dynamicHostStateFailed, message, hostID, attempts)
	if err := oc.setDynamicStatusDetailed(dynamicPhaseFailed, dynamicReasonRemoveFailed, message, true, true, true, desiredReplicas, localReadyReplicas, readyReplicas, hostStatuses); err != nil {
		return result.Error(err)
	}
	return result.Done()
}

func (oc *OperatorContext) joinDynamicPod(groupClient mlmanage.Client, clusterName, groupName, hostFQDN, tokenDuration string) (mlmanage.GroupHost, error) {
	token, err := groupClient.RequestDynamicHostToken(oc.Ctx, clusterName, groupName, hostFQDN, tokenDuration)
	if err != nil {
		return mlmanage.GroupHost{}, err
	}

	err = groupClient.JoinDynamicHost(oc.Ctx, hostFQDN, token)
	if err != nil && isTokenExpiredError(err) {
		token, tokenErr := groupClient.RequestDynamicHostToken(oc.Ctx, clusterName, groupName, hostFQDN, tokenDuration)
		if tokenErr != nil {
			return mlmanage.GroupHost{}, tokenErr
		}
		err = groupClient.JoinDynamicHost(oc.Ctx, hostFQDN, token)
	}
	if err != nil {
		return mlmanage.GroupHost{}, err
	}

	members, err := groupClient.ListGroupHosts(oc.Ctx, groupName)
	if err != nil {
		return mlmanage.GroupHost{}, err
	}
	member, found := findGroupHostForPod(hostnameToPodName(hostFQDN), hostFQDN, members)
	if !found {
		return mlmanage.GroupHost{}, fmt.Errorf("joined host %s not yet visible in group membership", hostFQDN)
	}
	return member, nil
}

func (oc *OperatorContext) listDynamicPods() ([]corev1.Pod, error) {
	labels := getSelectorLabelsByComponent(oc.MarklogicGroup.Spec.Name, true)
	podList := &corev1.PodList{}
	if err := oc.Client.List(oc.Ctx, podList, client.InNamespace(oc.MarklogicGroup.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	pods := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		pods = append(pods, pod)
	}
	sort.SliceStable(pods, func(i, j int) bool {
		leftOrdinal := podOrdinal(pods[i].Name)
		rightOrdinal := podOrdinal(pods[j].Name)
		if leftOrdinal == rightOrdinal {
			return pods[i].Name < pods[j].Name
		}
		return leftOrdinal < rightOrdinal
	})
	return pods, nil
}

func (oc *OperatorContext) buildDynamicHostStatuses(pods []corev1.Pod, members []mlmanage.GroupHost, previous []marklogicv1.DynamicHostStatus) ([]marklogicv1.DynamicHostStatus, int32, int32, []corev1.Pod) {
	statusByPod := map[string]marklogicv1.DynamicHostStatus{}
	for _, host := range previous {
		key := host.PodName
		if key == "" {
			key = hostnameToPodName(host.Hostname)
		}
		if key != "" {
			statusByPod[key] = host
		}
	}

	statuses := make([]marklogicv1.DynamicHostStatus, 0, len(pods)+len(members))
	statusSeen := map[string]bool{}
	joinCandidates := []corev1.Pod{}
	localReadyReplicas := int32(0)
	readyReplicas := int32(0)

	for _, pod := range pods {
		fqdn := dynamicPodFQDN(oc.MarklogicGroup, pod.Name)
		previousStatus, hasPrevious := statusByPod[pod.Name]
		member, memberFound := findGroupHostForPod(pod.Name, fqdn, members)
		locallyReady := isPodLocallyReady(&pod)
		if locallyReady {
			localReadyReplicas++
		}

		hostStatus := marklogicv1.DynamicHostStatus{
			PodName:  pod.Name,
			Hostname: fqdn,
			State:    dynamicHostStatePending,
		}
		if hasPrevious {
			hostStatus.Attempts = previousStatus.Attempts
			hostStatus.Message = previousStatus.Message
			hostStatus.HostID = previousStatus.HostID
		}

		if memberFound {
			hostStatus.State = dynamicHostStateJoined
			hostStatus.HostID = member.HostID
			hostStatus.Attempts = 0
			hostStatus.Message = ""
			if hasPrevious {
				if hostStatus.HostID == "" {
					hostStatus.HostID = previousStatus.HostID
				}
				switch previousStatus.State {
				case dynamicHostStateRetained, dynamicHostStateRemoving, dynamicHostStateFailed, dynamicHostStateRemoved:
					hostStatus.State = previousStatus.State
					hostStatus.Attempts = previousStatus.Attempts
					hostStatus.Message = previousStatus.Message
				}
			}
			if locallyReady {
				readyReplicas++
			}
		} else if hasPrevious && previousStatus.State == dynamicHostStateFailed && previousStatus.Attempts >= dynamicJoinRetryBudget {
			hostStatus.State = dynamicHostStateFailed
			if hostStatus.Message == "" {
				hostStatus.Message = "retry budget exhausted"
			}
		} else if !locallyReady {
			hostStatus.State = dynamicHostStatePending
			if hostStatus.Message == "" {
				hostStatus.Message = "waiting for pod local readiness"
			}
		} else if pod.DeletionTimestamp == nil {
			hostStatus.State = dynamicHostStatePending
			hostStatus.Message = ""
			joinCandidates = append(joinCandidates, pod)
		} else {
			hostStatus.State = dynamicHostStatePending
			if hostStatus.Message == "" {
				hostStatus.Message = "waiting for pod deletion to complete"
			}
		}

		statuses = append(statuses, hostStatus)
		statusSeen[pod.Name] = true
	}

	for _, member := range members {
		podName := hostnameToPodName(member.Name)
		if podName == "" || statusSeen[podName] {
			continue
		}
		if previousStatus, hasPrevious := statusByPod[podName]; hasPrevious && previousStatus.State == dynamicHostStateRetained {
			statuses = append(statuses, marklogicv1.DynamicHostStatus{PodName: podName, Hostname: member.Name, HostID: member.HostID, State: dynamicHostStateRetained, Message: previousStatus.Message, Attempts: previousStatus.Attempts})
		} else {
			statuses = append(statuses, marklogicv1.DynamicHostStatus{PodName: podName, Hostname: member.Name, HostID: member.HostID, State: dynamicHostStateJoined})
		}
		statusSeen[podName] = true
	}

	for _, previousStatus := range previous {
		podName := previousStatus.PodName
		if podName == "" {
			podName = hostnameToPodName(previousStatus.Hostname)
		}
		if podName == "" || statusSeen[podName] {
			continue
		}
		if previousStatus.State == dynamicHostStateRetained || previousStatus.State == dynamicHostStateFailed || previousStatus.State == dynamicHostStateRemoving || previousStatus.State == dynamicHostStateRemoved {
			statuses = append(statuses, previousStatus)
			statusSeen[podName] = true
		}
	}

	return statuses, localReadyReplicas, readyReplicas, joinCandidates
}

func pruneRemovedHostStatuses(hosts []marklogicv1.DynamicHostStatus, pods []corev1.Pod, members []mlmanage.GroupHost) []marklogicv1.DynamicHostStatus {
	memberByPod := map[string]bool{}
	for _, member := range members {
		memberByPod[hostnameToPodName(member.Name)] = true
	}
	podByName := map[string]bool{}
	for _, pod := range pods {
		podByName[pod.Name] = true
	}

	filtered := make([]marklogicv1.DynamicHostStatus, 0, len(hosts))
	for _, host := range hosts {
		if host.State == dynamicHostStateRemoved && !podByName[host.PodName] && !memberByPod[host.PodName] {
			continue
		}
		filtered = append(filtered, host)
	}
	return filtered
}

func hostsAboveDesiredOrdinal(hosts []marklogicv1.DynamicHostStatus, desiredReplicas int32) []marklogicv1.DynamicHostStatus {
	candidates := make([]marklogicv1.DynamicHostStatus, 0, len(hosts))
	for _, host := range hosts {
		if int32(podOrdinal(host.PodName)) >= desiredReplicas {
			candidates = append(candidates, host)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left := podOrdinal(candidates[i].PodName)
		right := podOrdinal(candidates[j].PodName)
		if left == right {
			return candidates[i].PodName > candidates[j].PodName
		}
		return left > right
	})
	return candidates
}

func hasPodsAboveDesiredOrdinal(pods []corev1.Pod, desiredReplicas int32) bool {
	for _, pod := range pods {
		if int32(podOrdinal(pod.Name)) >= desiredReplicas {
			return true
		}
	}
	return false
}

func findDynamicPodByName(pods []corev1.Pod, podName string) *corev1.Pod {
	for i := range pods {
		if pods[i].Name == podName {
			return &pods[i]
		}
	}
	return nil
}

func isDynamicPVCBacked(group *marklogicv1.MarklogicGroup) bool {
	if group == nil || group.Spec.Persistence == nil {
		return false
	}
	return group.Spec.Persistence.Enabled
}

func (oc *OperatorContext) ensureDynamicGroupFinalizer() (bool, error) {
	if controllerutil.ContainsFinalizer(oc.MarklogicGroup, dynamicGroupCleanupFinalizer) {
		return false, nil
	}
	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	controllerutil.AddFinalizer(oc.MarklogicGroup, dynamicGroupCleanupFinalizer)
	if err := oc.Client.Patch(oc.Ctx, oc.MarklogicGroup, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (oc *OperatorContext) removeDynamicGroupFinalizer() (bool, error) {
	if !controllerutil.ContainsFinalizer(oc.MarklogicGroup, dynamicGroupCleanupFinalizer) {
		return false, nil
	}
	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	controllerutil.RemoveFinalizer(oc.MarklogicGroup, dynamicGroupCleanupFinalizer)
	if err := oc.Client.Patch(oc.Ctx, oc.MarklogicGroup, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (oc *OperatorContext) ensureDynamicPodFinalizers(pods []corev1.Pod) error {
	for i := range pods {
		pod := pods[i].DeepCopy()
		if pod.DeletionTimestamp != nil || controllerutil.ContainsFinalizer(pod, dynamicHostCleanupFinalizer) {
			continue
		}
		patch := client.MergeFrom(pod.DeepCopy())
		controllerutil.AddFinalizer(pod, dynamicHostCleanupFinalizer)
		if err := oc.Client.Patch(oc.Ctx, pod, patch); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (oc *OperatorContext) releaseDynamicPodFinalizer(pod *corev1.Pod) error {
	if pod == nil || !controllerutil.ContainsFinalizer(pod, dynamicHostCleanupFinalizer) {
		return nil
	}
	mutablePod := pod.DeepCopy()
	patch := client.MergeFrom(mutablePod.DeepCopy())
	controllerutil.RemoveFinalizer(mutablePod, dynamicHostCleanupFinalizer)
	if err := oc.Client.Patch(oc.Ctx, mutablePod, patch); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (oc *OperatorContext) releaseDynamicPodFinalizers(pods []corev1.Pod) error {
	for i := range pods {
		if err := oc.releaseDynamicPodFinalizer(&pods[i]); err != nil {
			return err
		}
	}
	return nil
}

func (oc *OperatorContext) isOwningClusterDeletingOrGone() bool {
	for _, ownerRef := range oc.MarklogicGroup.OwnerReferences {
		if ownerRef.Kind != "MarklogicCluster" {
			continue
		}
		cluster := &marklogicv1.MarklogicCluster{}
		err := oc.Client.Get(oc.Ctx, types.NamespacedName{Name: ownerRef.Name, Namespace: oc.MarklogicGroup.Namespace}, cluster)
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			return false
		}
		return cluster.DeletionTimestamp != nil
	}
	return false
}

func (oc *OperatorContext) releaseDynamicFinalizersWithoutBootstrap() result.ReconcileResult {
	pods, err := oc.listDynamicPods()
	if err != nil {
		return result.Error(err)
	}
	if err := oc.releaseDynamicPodFinalizers(pods); err != nil {
		return result.Error(err)
	}
	if err := oc.setDynamicStatus(dynamicPhaseDeleting, dynamicReasonBootstrapNotReady, "bootstrap infrastructure is unavailable during teardown; releasing dynamic finalizers", false, true, true); err != nil {
		return result.Error(err)
	}
	if _, err := oc.removeDynamicGroupFinalizer(); err != nil {
		return result.Error(err)
	}
	return result.Done()
}

func setDynamicHostStatus(hosts []marklogicv1.DynamicHostStatus, podName, hostFQDN, state, message, hostID string, attempts int32) []marklogicv1.DynamicHostStatus {
	for i := range hosts {
		if hosts[i].PodName != podName {
			continue
		}
		hosts[i].Hostname = hostFQDN
		hosts[i].State = state
		hosts[i].Message = message
		hosts[i].HostID = hostID
		hosts[i].Attempts = attempts
		return hosts
	}
	hosts = append(hosts, marklogicv1.DynamicHostStatus{PodName: podName, Hostname: hostFQDN, State: state, Message: message, HostID: hostID, Attempts: attempts})
	return hosts
}

func incrementDynamicHostAttempts(hosts []marklogicv1.DynamicHostStatus, podName string) int32 {
	for i := range hosts {
		if hosts[i].PodName == podName {
			return hosts[i].Attempts
		}
	}
	return 0
}

func hasFailedDynamicHost(hosts []marklogicv1.DynamicHostStatus) bool {
	for _, host := range hosts {
		if host.State == dynamicHostStateFailed {
			return true
		}
	}
	return false
}

func countLocalReadyPods(pods []corev1.Pod) int32 {
	count := int32(0)
	for i := range pods {
		if isPodLocallyReady(&pods[i]) {
			count++
		}
	}
	return count
}

func dynamicPodFQDN(group *marklogicv1.MarklogicGroup, podName string) string {
	clusterDomain := strings.TrimSpace(group.Spec.ClusterDomain)
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	return fmt.Sprintf("%s.%s.%s.svc.%s", podName, group.Spec.Name, group.Namespace, clusterDomain)
}

func hostnameToPodName(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return ""
	}
	if strings.Contains(hostname, ".") {
		parts := strings.SplitN(hostname, ".", 2)
		return parts[0]
	}
	return hostname
}

func findGroupHostForPod(podName, hostFQDN string, members []mlmanage.GroupHost) (mlmanage.GroupHost, bool) {
	for _, member := range members {
		memberName := strings.ToLower(strings.TrimSpace(member.Name))
		if memberName == "" {
			continue
		}
		if memberName == strings.ToLower(hostFQDN) || memberName == strings.ToLower(podName) || strings.HasPrefix(memberName, strings.ToLower(podName)+".") {
			return member, true
		}
	}
	return mlmanage.GroupHost{}, false
}

func isPodLocallyReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podOrdinal(name string) int {
	index := strings.LastIndex(name, "-")
	if index == -1 || index == len(name)-1 {
		return int(^uint(0) >> 1)
	}
	ordinal, err := strconv.Atoi(name[index+1:])
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return ordinal
}
