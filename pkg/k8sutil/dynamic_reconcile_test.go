// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
)

type stubDynamicManagementClient struct {
	requestTokenFn func(clusterName, groupName, hostFQDN, duration string) (string, error)
	joinFn         func(hostFQDN, token string) error
	listGroupFn    func(groupName string) ([]mlmanage.GroupHost, error)
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

func (s *stubDynamicManagementClient) ListGroupHosts(ctx context.Context, groupName string) ([]mlmanage.GroupHost, error) {
	if s.listGroupFn == nil {
		return nil, errors.New("listGroupFn is not configured")
	}
	return s.listGroupFn(groupName)
}

func (s *stubDynamicManagementClient) RemoveDynamicHost(ctx context.Context, clusterName, hostID string) error {
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
