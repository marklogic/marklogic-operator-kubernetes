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
)

var NewDynamicManagementClient = func(opts mlmanage.ClientOptions) mlmanage.Client {
	return mlmanage.NewClient(opts)
}

const (
	dynamicPhasePending    = "pending"
	dynamicPhaseReconciling = "reconciling"
	dynamicPhaseDegraded   = "degraded"
	dynamicPhaseFailed     = "failed"
	dynamicPhaseConfigured = "configured"

	dynamicReasonBootstrapNotReady   = "BootstrapNotReady"
	dynamicReasonInvalidConfig       = "InvalidConfiguration"
	dynamicReasonGroupConfigFailed   = "GroupConfigFailed"
	dynamicReasonJoinFailed          = "JoinFailed"
	dynamicReasonRetryBudgetExceeded = "RetryBudgetExhausted"

	dynamicHostStatePending = "pending"
	dynamicHostStateJoining = "joining"
	dynamicHostStateJoined  = "joined"
	dynamicHostStateFailed  = "failed"

	minimumSupportedMarkLogicVersion = 12
	dynamicJoinRetryBudget           = int32(3)
	dynamicJoinRequeueSeconds        = 2
)

var statusCodeRegex = regexp.MustCompile(`status\s+(\d{3})`)

func (oc *OperatorContext) ReconcileDynamicGroupConfig() result.ReconcileResult {
	if !oc.MarklogicGroup.Spec.IsDynamic {
		return result.Continue()
	}

	if oc.MarklogicGroup.Status.Dynamic == nil {
		if err := oc.setDynamicStatus(dynamicPhasePending, "", "waiting for bootstrap readiness", false, false, false); err != nil {
			return result.Error(err)
		}
	}

	clusterName, err := oc.getOwningClusterName()
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, err.Error(), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
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
	return oc.reconcileDynamicScaleUp(groupClient, clusterName, groupName, tokenDuration, desiredReplicas)
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

	statuses := make([]marklogicv1.DynamicHostStatus, 0, len(pods))
	joinCandidates := []corev1.Pod{}
	localReadyReplicas := int32(0)
	readyReplicas := int32(0)

	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
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
		} else {
			hostStatus.State = dynamicHostStatePending
			hostStatus.Message = ""
			joinCandidates = append(joinCandidates, pod)
		}

		statuses = append(statuses, hostStatus)
	}

	return statuses, localReadyReplicas, readyReplicas, joinCandidates
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
