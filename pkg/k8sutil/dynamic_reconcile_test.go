// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type stubDynamicManagementClient struct {
	requestTokenFn func(clusterName, groupName, hostFQDN, duration string) (string, error)
	joinFn         func(hostFQDN, token string) error
	listGroupFn    func(groupName string) ([]mlmanage.GroupHost, error)
	resolveNameFn  func() (string, error)
	removeFn       func(clusterName, hostID string) error
}

func (s *stubDynamicManagementClient) ListHostsStatus(ctx context.Context) ([]mlmanage.HostStatus, error) {
	return nil, nil
}

func (s *stubDynamicManagementClient) GetHostGroupName(ctx context.Context, hostName string) (string, error) {
	return "Default", nil
}

func (s *stubDynamicManagementClient) GetGroup(ctx context.Context, groupName string) (mlmanage.GroupInfo, error) {
	return mlmanage.GroupInfo{}, nil
}

func (s *stubDynamicManagementClient) CreateGroup(ctx context.Context, groupName string) error {
	return nil
}

func (s *stubDynamicManagementClient) EnableDynamicHosts(ctx context.Context, groupName string) error {
	return nil
}

func (s *stubDynamicManagementClient) EnableAdminAPITokenAuthentication(ctx context.Context, groupName string) error {
	return nil
}

func (s *stubDynamicManagementClient) EnsureManageAdminUser(ctx context.Context, username, password string) error {
	return nil
}

func (s *stubDynamicManagementClient) ResolveClusterName(ctx context.Context) (string, error) {
	if s.resolveNameFn == nil {
		return "", errors.New("resolveNameFn is not configured")
	}
	return s.resolveNameFn()
}

func (s *stubDynamicManagementClient) RequestDynamicHostToken(ctx context.Context, clusterName, groupName, hostFQDN, duration string) (string, error) {
	if s.requestTokenFn == nil {
		return "", errors.New("requestTokenFn is not configured")
	}
	return s.requestTokenFn(clusterName, groupName, hostFQDN, duration)
}

func (s *stubDynamicManagementClient) JoinDynamicHost(ctx context.Context, hostFQDN, token string) error {
	if s.joinFn == nil {
		return errors.New("joinFn is not configured")
	}
	return s.joinFn(hostFQDN, token)
}

func TestBuildDynamicHostStatusesClearsFailedStateWhenPodRecoveredAndOnline(t *testing.T) {
	podCreation := metav1.NewTime(time.Now())
	lastUpdated := metav1.NewTime(podCreation.Add(2 * time.Minute))

	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{Name: "dynamic", ClusterDomain: "cluster.local", IsDynamic: true},
		},
	}

	pods := []corev1.Pod{dynamicReadyPodForTest("dynamic-0", podCreation)}
	members := []mlmanage.GroupHost{{Name: "dynamic-0.dynamic.default.svc.cluster.local", HostID: "host-id-stable", Online: true}}
	previous := []marklogicv1.DynamicHostStatus{{
		PodName:     "dynamic-0",
		Hostname:    "dynamic-0.dynamic.default.svc.cluster.local",
		HostID:      "host-id-stable",
		State:       dynamicHostStateFailed,
		Message:     "restart recovery rejoin failed",
		Attempts:    2,
		LastUpdated: &lastUpdated,
	}}

	hosts, localReady, ready, joinCandidates := oc.buildDynamicHostStatuses(pods, members, previous)

	if localReady != 1 {
		t.Fatalf("expected localReady=1, got %d", localReady)
	}
	if ready != 1 {
		t.Fatalf("expected ready=1, got %d", ready)
	}
	if len(joinCandidates) != 0 {
		t.Fatalf("expected no join candidates after recovery, got %d", len(joinCandidates))
	}

	host, found := findDynamicHostStatusByPod(hosts, "dynamic-0")
	if !found {
		t.Fatalf("expected host status for dynamic-0")
	}
	if host.State != dynamicHostStateJoined {
		t.Fatalf("expected recovered host state %q, got %q", dynamicHostStateJoined, host.State)
	}
	if host.Attempts != 0 {
		t.Fatalf("expected attempts reset to 0 after recovery, got %d", host.Attempts)
	}
	if host.Message != "" {
		t.Fatalf("expected message cleared after recovery, got %q", host.Message)
	}
}

func (s *stubDynamicManagementClient) ListGroupHosts(ctx context.Context, groupName string) ([]mlmanage.GroupHost, error) {
	if s.listGroupFn == nil {
		return nil, errors.New("listGroupFn is not configured")
	}
	return s.listGroupFn(groupName)
}

func (s *stubDynamicManagementClient) RemoveDynamicHost(ctx context.Context, clusterName, hostID string) error {
	if s.removeFn != nil {
		return s.removeFn(clusterName, hostID)
	}
	return nil
}

func TestJoinDynamicPodSuccess(t *testing.T) {
	oc := &OperatorContext{Ctx: context.Background()}

	hostFQDN := "dynamic-0.dynamic.default.svc.cluster.local"
	requestCalls := 0
	joinCalls := 0
	listCalls := 0
	client := &stubDynamicManagementClient{
		requestTokenFn: func(clusterName, groupName, requestedHost, duration string) (string, error) {
			requestCalls++
			if clusterName != "cluster" || groupName != "DynamicGroup" || requestedHost != hostFQDN || duration != "PT15M" {
				return "", fmt.Errorf("unexpected token request arguments: %s %s %s %s", clusterName, groupName, requestedHost, duration)
			}
			return "token-1", nil
		},
		joinFn: func(requestedHost, token string) error {
			joinCalls++
			if requestedHost != hostFQDN || token != "token-1" {
				return fmt.Errorf("unexpected join arguments: %s %s", requestedHost, token)
			}
			return nil
		},
		listGroupFn: func(groupName string) ([]mlmanage.GroupHost, error) {
			listCalls++
			if groupName != "DynamicGroup" {
				return nil, fmt.Errorf("unexpected group in list call: %s", groupName)
			}
			return []mlmanage.GroupHost{{Name: hostFQDN, HostID: "host-id-1", Online: true}}, nil
		},
	}

	host, err := oc.joinDynamicPod(client, "cluster", "DynamicGroup", hostFQDN, "PT15M")
	if err != nil {
		t.Fatalf("joinDynamicPod returned error: %v", err)
	}
	if host.HostID != "host-id-1" {
		t.Fatalf("expected host-id-1, got %s", host.HostID)
	}
	if requestCalls != 1 || joinCalls != 1 || listCalls != 1 {
		t.Fatalf("expected 1 request/join/list call, got %d/%d/%d", requestCalls, joinCalls, listCalls)
	}
}

func TestJoinDynamicPodRetriesTokenExpired(t *testing.T) {
	oc := &OperatorContext{Ctx: context.Background()}

	hostFQDN := "dynamic-0.dynamic.default.svc.cluster.local"
	requestCalls := 0
	joinCalls := 0
	client := &stubDynamicManagementClient{
		requestTokenFn: func(clusterName, groupName, requestedHost, duration string) (string, error) {
			requestCalls++
			return fmt.Sprintf("token-%d", requestCalls), nil
		},
		joinFn: func(requestedHost, token string) error {
			joinCalls++
			if joinCalls == 1 {
				return errors.New("token expired")
			}
			if token != "token-2" {
				return fmt.Errorf("expected second token on retry, got %s", token)
			}
			return nil
		},
		listGroupFn: func(groupName string) ([]mlmanage.GroupHost, error) {
			return []mlmanage.GroupHost{{Name: hostFQDN, HostID: "host-id-2", Online: true}}, nil
		},
	}

	host, err := oc.joinDynamicPod(client, "cluster", "DynamicGroup", hostFQDN, "PT15M")
	if err != nil {
		t.Fatalf("joinDynamicPod returned error: %v", err)
	}
	if host.HostID != "host-id-2" {
		t.Fatalf("expected host-id-2, got %s", host.HostID)
	}
	if requestCalls != 2 {
		t.Fatalf("expected 2 token requests, got %d", requestCalls)
	}
	if joinCalls != 2 {
		t.Fatalf("expected 2 join attempts, got %d", joinCalls)
	}
}

func TestJoinDynamicPodFailsWhenJoinedHostMissingFromMembership(t *testing.T) {
	oc := &OperatorContext{Ctx: context.Background()}

	hostFQDN := "dynamic-0.dynamic.default.svc.cluster.local"
	client := &stubDynamicManagementClient{
		requestTokenFn: func(clusterName, groupName, requestedHost, duration string) (string, error) {
			return "token-1", nil
		},
		joinFn: func(requestedHost, token string) error {
			return nil
		},
		listGroupFn: func(groupName string) ([]mlmanage.GroupHost, error) {
			return nil, nil
		},
	}

	_, err := oc.joinDynamicPod(client, "cluster", "DynamicGroup", hostFQDN, "PT15M")
	if err == nil {
		t.Fatal("expected error when joined host is missing from group membership")
	}
	if expected := "not yet visible in group membership"; err != nil && !strings.Contains(err.Error(), expected) {
		t.Fatalf("expected error containing %q, got %v", expected, err)
	}
}

func TestJoinDynamicPodFallsBackToBootstrapHostForToken(t *testing.T) {
	hostFQDN := "dynamic-0.dynamic.default.svc.cluster.local"
	bootstrapHost := "node-0.node.default.svc.cluster.local"
	oc := &OperatorContext{
		Ctx: context.Background(),
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{BootstrapHost: bootstrapHost},
		},
	}

	requestedHosts := make([]string, 0, 2)
	client := &stubDynamicManagementClient{
		requestTokenFn: func(clusterName, groupName, requestedHost, duration string) (string, error) {
			requestedHosts = append(requestedHosts, requestedHost)
			switch requestedHost {
			case hostFQDN:
				return "", errors.New("management api POST /manage/v2/clusters/cluster/dynamic-host-token returned status 404: {\"errorResponse\":{\"messageCode\":\"XDMP-NOSUCHHOST\"}}")
			case bootstrapHost:
				return "token-bootstrap", nil
			default:
				return "", fmt.Errorf("unexpected token host: %s", requestedHost)
			}
		},
		joinFn: func(requestedHost, token string) error {
			if requestedHost != hostFQDN {
				return fmt.Errorf("expected join host %s, got %s", hostFQDN, requestedHost)
			}
			if token != "token-bootstrap" {
				return fmt.Errorf("expected fallback token, got %s", token)
			}
			return nil
		},
		listGroupFn: func(groupName string) ([]mlmanage.GroupHost, error) {
			return []mlmanage.GroupHost{{Name: hostFQDN, HostID: "host-id-fallback", Online: true}}, nil
		},
	}

	host, err := oc.joinDynamicPod(client, "cluster", "DynamicGroup", hostFQDN, "PT15M")
	if err != nil {
		t.Fatalf("joinDynamicPod returned error: %v", err)
	}
	if host.HostID != "host-id-fallback" {
		t.Fatalf("expected host-id-fallback, got %s", host.HostID)
	}
	if len(requestedHosts) != 2 || requestedHosts[0] != hostFQDN || requestedHosts[1] != bootstrapHost {
		t.Fatalf("expected token host fallback sequence [%s %s], got %v", hostFQDN, bootstrapHost, requestedHosts)
	}
}

func TestJoinDynamicPodRetriesWithResolvedClusterNameForNoSuchCluster(t *testing.T) {
	oc := &OperatorContext{Ctx: context.Background()}
	hostFQDN := "dynamic-0.dynamic.default.svc.cluster.local"

	requestedClusters := make([]string, 0, 2)
	client := &stubDynamicManagementClient{
		requestTokenFn: func(clusterName, groupName, requestedHost, duration string) (string, error) {
			requestedClusters = append(requestedClusters, clusterName)
			switch clusterName {
			case "ml-dynamic-cluster":
				return "", errors.New("management api POST /manage/v2/clusters/ml-dynamic-cluster/dynamic-host-token returned status 404: {\"errorResponse\":{\"messageCode\":\"XDMP-NOSUCHCLUSTER\"}}")
			case "local-cluster-default":
				return "token-resolved", nil
			default:
				return "", fmt.Errorf("unexpected cluster in token request: %s", clusterName)
			}
		},
		resolveNameFn: func() (string, error) {
			return "local-cluster-default", nil
		},
		joinFn: func(requestedHost, token string) error {
			if requestedHost != hostFQDN {
				return fmt.Errorf("expected join host %s, got %s", hostFQDN, requestedHost)
			}
			if token != "token-resolved" {
				return fmt.Errorf("expected resolved-cluster token, got %s", token)
			}
			return nil
		},
		listGroupFn: func(groupName string) ([]mlmanage.GroupHost, error) {
			return []mlmanage.GroupHost{{Name: hostFQDN, HostID: "host-id-resolved", Online: true}}, nil
		},
	}

	host, err := oc.joinDynamicPod(client, "ml-dynamic-cluster", "DynamicGroup", hostFQDN, "PT15M")
	if err != nil {
		t.Fatalf("joinDynamicPod returned error: %v", err)
	}
	if host.HostID != "host-id-resolved" {
		t.Fatalf("expected host-id-resolved, got %s", host.HostID)
	}
	if len(requestedClusters) != 2 || requestedClusters[0] != "ml-dynamic-cluster" || requestedClusters[1] != "local-cluster-default" {
		t.Fatalf("expected token cluster fallback sequence [ml-dynamic-cluster local-cluster-default], got %v", requestedClusters)
	}
}

func TestRemoveDynamicHostWithClusterFallbackRetriesResolvedClusterName(t *testing.T) {
	oc := &OperatorContext{Ctx: context.Background()}
	removeClusters := make([]string, 0, 2)

	client := &stubDynamicManagementClient{
		removeFn: func(clusterName, hostID string) error {
			removeClusters = append(removeClusters, clusterName)
			if clusterName == "ml-dynamic-cluster" {
				return errors.New("management api DELETE /manage/v2/clusters/ml-dynamic-cluster/dynamic-hosts returned status 404: {\"errorResponse\":{\"messageCode\":\"XDMP-NOSUCHCLUSTER\"}}")
			}
			if clusterName != "local-cluster-default" {
				return fmt.Errorf("unexpected cluster for remove call: %s", clusterName)
			}
			return nil
		},
		resolveNameFn: func() (string, error) {
			return "local-cluster-default", nil
		},
	}

	err := oc.removeDynamicHostWithClusterFallback(client, "ml-dynamic-cluster", "host-1")
	if err != nil {
		t.Fatalf("removeDynamicHostWithClusterFallback returned error: %v", err)
	}
	if len(removeClusters) != 2 || removeClusters[0] != "ml-dynamic-cluster" || removeClusters[1] != "local-cluster-default" {
		t.Fatalf("expected remove cluster fallback sequence [ml-dynamic-cluster local-cluster-default], got %v", removeClusters)
	}
}

func TestDynamicPVCRestartCleanupJobNameAddsOrdinalAndHashWhenTruncated(t *testing.T) {
	groupName := "dynamic-pool-with-an-extremely-long-name-that-will-force-job-name-truncation-and-needs-uniqueness"
	podName := "dynamic-pool-with-an-extremely-long-name-that-will-force-job-name-truncation-and-needs-uniqueness-12"

	name := dynamicPVCRestartCleanupJobName(groupName, podName)
	if len(name) > 63 {
		t.Fatalf("expected name length <= 63, got %d (%s)", len(name), name)
	}
	if !strings.Contains(name, "-12-") {
		t.Fatalf("expected ordinal-preserving suffix in truncated name, got %s", name)
	}

	segments := strings.Split(name, "-")
	if len(segments) < 2 {
		t.Fatalf("expected hashed suffix segments in name, got %s", name)
	}
	hashPart := segments[len(segments)-1]
	if len(hashPart) != 8 {
		t.Fatalf("expected 8-char hash suffix, got %q in %s", hashPart, name)
	}
}

func TestDynamicPVCRestartCleanupJobNameIsUniqueAcrossPodOrdinals(t *testing.T) {
	groupName := "dynamic-group-with-a-very-long-name-that-forces-truncation-in-cleanup-job-name-generation"
	podZero := "dynamic-group-with-a-very-long-name-that-forces-truncation-in-cleanup-job-name-generation-0"
	podOne := "dynamic-group-with-a-very-long-name-that-forces-truncation-in-cleanup-job-name-generation-1"

	nameZero := dynamicPVCRestartCleanupJobName(groupName, podZero)
	nameOne := dynamicPVCRestartCleanupJobName(groupName, podOne)
	if nameZero == nameOne {
		t.Fatalf("expected unique job names for different pods, got %s", nameZero)
	}

	nameZeroRepeat := dynamicPVCRestartCleanupJobName(groupName, podZero)
	if nameZero != nameZeroRepeat {
		t.Fatalf("expected deterministic naming for same input, got %s and %s", nameZero, nameZeroRepeat)
	}
}

func TestIsValidDynamicTokenDuration(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{name: "valid default", value: "PT15M", expected: true},
		{name: "valid composite", value: "P1DT2H30M", expected: true},
		{name: "valid fractional seconds", value: "PT0.5S", expected: true},
		{name: "invalid arbitrary", value: "fifteen minutes", expected: false},
		{name: "invalid bare P", value: "P", expected: false},
		{name: "invalid bare PT", value: "PT", expected: false},
		{name: "invalid empty", value: "", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := isValidDynamicTokenDuration(tt.value)
			if actual != tt.expected {
				t.Fatalf("isValidDynamicTokenDuration(%q) = %t, expected %t", tt.value, actual, tt.expected)
			}
		})
	}
}

func TestBuildDynamicHostStatusesClearsFailedStateForRecreatedPod(t *testing.T) {
	recreatedAt := metav1.NewTime(time.Now())
	lastUpdated := metav1.NewTime(recreatedAt.Add(-5 * time.Minute))

	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{Name: "dynamic", ClusterDomain: "cluster.local", IsDynamic: true},
		},
	}

	pods := []corev1.Pod{dynamicReadyPodForTest("dynamic-0", recreatedAt)}
	members := []mlmanage.GroupHost{{Name: "dynamic-0.dynamic.default.svc.cluster.local", HostID: "host-id-new", Online: true}}
	previous := []marklogicv1.DynamicHostStatus{{
		PodName:     "dynamic-0",
		Hostname:    "dynamic-0.dynamic.default.svc.cluster.local",
		HostID:      "host-id-old",
		State:       dynamicHostStateFailed,
		Message:     "pod did not reach local readiness before startup timeout",
		Attempts:    3,
		LastUpdated: &lastUpdated,
	}}

	hosts, localReady, ready, joinCandidates := oc.buildDynamicHostStatuses(pods, members, previous)
	if localReady != 1 {
		t.Fatalf("expected localReadyReplicas=1, got %d", localReady)
	}
	if ready != 1 {
		t.Fatalf("expected readyReplicas=1, got %d", ready)
	}
	if len(joinCandidates) != 0 {
		t.Fatalf("expected no join candidates, got %d", len(joinCandidates))
	}

	host, found := findDynamicHostStatusByPod(hosts, "dynamic-0")
	if !found {
		t.Fatalf("expected host status for dynamic-0")
	}
	if host.State != dynamicHostStateJoined {
		t.Fatalf("expected recreated pod host state to reset to %q, got %q", dynamicHostStateJoined, host.State)
	}
	if host.HostID != "host-id-new" {
		t.Fatalf("expected host-id-new after recovery, got %q", host.HostID)
	}
	if host.Attempts != 0 {
		t.Fatalf("expected attempts reset to 0, got %d", host.Attempts)
	}
	if host.Message != "" {
		t.Fatalf("expected message cleared on recovery, got %q", host.Message)
	}
}

func TestBuildDynamicHostStatusesPreservesFailedStateForSamePod(t *testing.T) {
	podCreation := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	lastUpdated := metav1.NewTime(time.Now())

	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{Name: "dynamic", ClusterDomain: "cluster.local", IsDynamic: true},
		},
	}

	pods := []corev1.Pod{dynamicReadyPodForTest("dynamic-0", podCreation)}
	members := []mlmanage.GroupHost{{Name: "dynamic-0.dynamic.default.svc.cluster.local", HostID: "host-id-stable", Online: true}}
	previous := []marklogicv1.DynamicHostStatus{{
		PodName:     "dynamic-0",
		Hostname:    "dynamic-0.dynamic.default.svc.cluster.local",
		HostID:      "host-id-stable",
		State:       dynamicHostStateFailed,
		Message:     "retry budget exhausted",
		Attempts:    3,
		LastUpdated: &lastUpdated,
	}}

	hosts, _, _, joinCandidates := oc.buildDynamicHostStatuses(pods, members, previous)
	if len(joinCandidates) != 0 {
		t.Fatalf("expected no join candidates, got %d", len(joinCandidates))
	}

	host, found := findDynamicHostStatusByPod(hosts, "dynamic-0")
	if !found {
		t.Fatalf("expected host status for dynamic-0")
	}
	if host.State != dynamicHostStateFailed {
		t.Fatalf("expected failed state to be preserved for same pod lifecycle, got %q", host.State)
	}
	if host.Attempts != 3 {
		t.Fatalf("expected attempts to remain 3, got %d", host.Attempts)
	}
	if host.Message != "retry budget exhausted" {
		t.Fatalf("expected failure message to be preserved, got %q", host.Message)
	}
}

func TestBuildDynamicHostStatusesRetriesNoSuchClusterFailedState(t *testing.T) {
	podCreation := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	lastUpdated := metav1.NewTime(time.Now().Add(-2 * time.Minute))

	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{Name: "dynamic", ClusterDomain: "cluster.local", IsDynamic: true},
		},
	}

	pods := []corev1.Pod{dynamicReadyPodForTest("dynamic-0", podCreation)}
	previous := []marklogicv1.DynamicHostStatus{{
		PodName:     "dynamic-0",
		Hostname:    "dynamic-0.dynamic.default.svc.cluster.local",
		State:       dynamicHostStateFailed,
		Message:     "retry budget exhausted for dynamic-0: management api POST /manage/v2/clusters/ml-dynamic-cluster/dynamic-host-token returned status 404: {\"errorResponse\":{\"messageCode\":\"XDMP-NOSUCHCLUSTER\"}}",
		Attempts:    dynamicJoinRetryBudget,
		LastUpdated: &lastUpdated,
	}}

	hosts, _, _, joinCandidates := oc.buildDynamicHostStatuses(pods, nil, previous)
	if len(joinCandidates) != 1 {
		t.Fatalf("expected retry join candidate for NOSUCHCLUSTER failed state, got %d", len(joinCandidates))
	}

	host, found := findDynamicHostStatusByPod(hosts, "dynamic-0")
	if !found {
		t.Fatalf("expected host status for dynamic-0")
	}
	if host.State != dynamicHostStatePending {
		t.Fatalf("expected host state %q for NOSUCHCLUSTER retry, got %q", dynamicHostStatePending, host.State)
	}
}

func TestIsNoSuchClusterFailureMessage(t *testing.T) {
	if !isNoSuchClusterFailureMessage("management api returned XDMP-NOSUCHCLUSTER") {
		t.Fatalf("expected NOSUCHCLUSTER marker to be detected")
	}
	if !isNoSuchClusterFailureMessage("no such cluster") {
		t.Fatalf("expected generic no such cluster marker to be detected")
	}
	if isNoSuchClusterFailureMessage("retry budget exhausted for transient timeout") {
		t.Fatalf("did not expect unrelated message to match")
	}
}

func dynamicReadyPodForTest(name string, createdAt metav1.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: createdAt},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func findDynamicHostStatusByPod(hosts []marklogicv1.DynamicHostStatus, podName string) (marklogicv1.DynamicHostStatus, bool) {
	for _, host := range hosts {
		if host.PodName == podName {
			return host, true
		}
	}
	return marklogicv1.DynamicHostStatus{}, false
}

func TestShouldSuppressBootstrapTransientDegradeWhenHealthyIdle(t *testing.T) {
	replicas := int32(2)
	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{Replicas: &replicas, IsDynamic: true},
			Status: marklogicv1.MarklogicGroupStatus{
				Dynamic: &marklogicv1.DynamicGroupStatus{
					Phase:         dynamicPhaseIdle,
					ReadyReplicas: 2,
					Hosts:         []marklogicv1.DynamicHostStatus{{PodName: "dynamic-0", State: dynamicHostStateJoined}},
				},
			},
		},
	}

	err := errors.New("bootstrap readiness check failed: dial tcp: lookup node-0.node.ml-dynamic-host.svc.cluster.local: no such host")
	if !oc.shouldSuppressBootstrapTransientDegrade(err) {
		t.Fatalf("expected transient bootstrap degrade suppression for healthy idle status")
	}
}

func TestShouldNotSuppressBootstrapTransientDegradeWhenNotHealthy(t *testing.T) {
	replicas := int32(2)
	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{Replicas: &replicas, IsDynamic: true},
			Status: marklogicv1.MarklogicGroupStatus{
				Dynamic: &marklogicv1.DynamicGroupStatus{
					Phase:         dynamicPhaseReconciling,
					ReadyReplicas: 1,
					Hosts:         []marklogicv1.DynamicHostStatus{{PodName: "dynamic-0", State: dynamicHostStateFailed}},
				},
			},
		},
	}

	err := errors.New("bootstrap readiness check failed: dial tcp: lookup node-0.node.ml-dynamic-host.svc.cluster.local: no such host")
	if oc.shouldSuppressBootstrapTransientDegrade(err) {
		t.Fatalf("expected no suppression when dynamic group is not healthy idle")
	}
}

func TestShouldSuppressBootstrapHostOfflineDegradeWhenHealthyIdle(t *testing.T) {
	replicas := int32(2)
	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{Replicas: &replicas, IsDynamic: true},
			Status: marklogicv1.MarklogicGroupStatus{
				Dynamic: &marklogicv1.DynamicGroupStatus{
					Phase:         dynamicPhaseIdle,
					ReadyReplicas: 2,
					Hosts:         []marklogicv1.DynamicHostStatus{{PodName: "dynamic-0", State: dynamicHostStateJoined}},
				},
			},
		},
	}

	if !oc.shouldSuppressBootstrapHostOfflineDegrade() {
		t.Fatalf("expected bootstrap host offline degrade suppression for healthy idle status")
	}
}

func TestShouldNotSuppressBootstrapHostOfflineDegradeWhenNotHealthy(t *testing.T) {
	replicas := int32(2)
	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{Replicas: &replicas, IsDynamic: true},
			Status: marklogicv1.MarklogicGroupStatus{
				Dynamic: &marklogicv1.DynamicGroupStatus{
					Phase:         dynamicPhaseReconciling,
					ReadyReplicas: 1,
					Hosts:         []marklogicv1.DynamicHostStatus{{PodName: "dynamic-0", State: dynamicHostStateFailed}},
				},
			},
		},
	}

	if oc.shouldSuppressBootstrapHostOfflineDegrade() {
		t.Fatalf("expected no suppression when dynamic group is not healthy idle")
	}
}

func TestShouldSuppressBootstrapHostOfflineDegradeDuringRestartRecovery(t *testing.T) {
	replicas := int32(2)
	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{Replicas: &replicas, IsDynamic: true},
			Status: marklogicv1.MarklogicGroupStatus{
				Dynamic: &marklogicv1.DynamicGroupStatus{
					Phase:         dynamicPhaseReconciling,
					Reason:        dynamicReasonClusterRestart,
					ReadyReplicas: 2,
					Hosts:         []marklogicv1.DynamicHostStatus{{PodName: "dynamic-1", State: dynamicHostStateRejoined}},
				},
			},
		},
	}

	if !oc.shouldSuppressBootstrapHostOfflineDegrade() {
		t.Fatalf("expected suppression during healthy restart recovery")
	}
}

func TestShouldSuppressBootstrapTransientDegradeDuringRestartRecovery(t *testing.T) {
	replicas := int32(2)
	oc := &OperatorContext{
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			Spec: marklogicv1.MarklogicGroupSpec{Replicas: &replicas, IsDynamic: true},
			Status: marklogicv1.MarklogicGroupStatus{
				Dynamic: &marklogicv1.DynamicGroupStatus{
					Phase:         dynamicPhaseReconciling,
					Reason:        dynamicReasonClusterRestart,
					ReadyReplicas: 2,
					Hosts:         []marklogicv1.DynamicHostStatus{{PodName: "dynamic-1", State: dynamicHostStateRejoined}},
				},
			},
		},
	}

	err := errors.New("bootstrap readiness check failed: dial tcp: lookup node-0.node.ml-dynamic-host.svc.cluster.local: no such host")
	if !oc.shouldSuppressBootstrapTransientDegrade(err) {
		t.Fatalf("expected suppression for transient bootstrap error during healthy restart recovery")
	}
}

func TestIsBootstrapHostStatusMatchesFQDNAndShortName(t *testing.T) {
	bootstrap := "node-0.node.ml-dynamic-host.svc.cluster.local"

	if !isBootstrapHostStatus("node-0.node.ml-dynamic-host.svc.cluster.local", bootstrap) {
		t.Fatalf("expected exact bootstrap FQDN match")
	}

	if !isBootstrapHostStatus("node-0", bootstrap) {
		t.Fatalf("expected short host name to match bootstrap host")
	}

	if !isBootstrapHostStatus("node-0.node.ml-dynamic-host.svc.cluster.local:8002", bootstrap) {
		t.Fatalf("expected host with management port to match bootstrap host")
	}
}

func TestIsBootstrapHostStatusDoesNotMatchOtherHosts(t *testing.T) {
	bootstrap := "node-0.node.ml-dynamic-host.svc.cluster.local"

	if isBootstrapHostStatus("dynamic-1.dynamic.ml-dynamic-host.svc.cluster.local", bootstrap) {
		t.Fatalf("expected non-bootstrap dynamic host to not match bootstrap host")
	}
}

func TestIsTransientManagementErrorRecognizesNoSuchCluster(t *testing.T) {
	err := errors.New("management api POST /manage/v2/clusters/ml-dynamic-cluster/dynamic-host-token returned status 404: {\"errorResponse\":{\"messageCode\":\"XDMP-NOSUCHCLUSTER\"}}")
	if !isTransientManagementError(err) {
		t.Fatalf("expected XDMP-NOSUCHCLUSTER 404 to be treated as transient")
	}
}

func TestIsTransientManagementErrorDoesNotTreatArbitrary404AsTransient(t *testing.T) {
	err := errors.New("management api POST /manage/v2/clusters/ml-dynamic-cluster/dynamic-host-token returned status 404: {\"errorResponse\":{\"messageCode\":\"SOME-OTHER-404\"}}")
	if isTransientManagementError(err) {
		t.Fatalf("expected arbitrary 404 to remain non-transient")
	}
}
