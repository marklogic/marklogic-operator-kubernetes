// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"fmt"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHAProxyPreservesAuthorizationHeaderCase(t *testing.T) {
	for _, pathBased := range []bool{false, true} {
		t.Run(fmt.Sprintf("pathBased=%t", pathBased), func(t *testing.T) {
			replicas := int32(2)
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "marklogic", Namespace: "test"},
				Spec: marklogicv1.MarklogicClusterSpec{
					HAProxy: &marklogicv1.HAProxy{
						PathBasedRouting: &pathBased,
						FrontendPort:     8000,
						AppServers: []marklogicv1.AppServers{
							{Name: "oauth", Port: 8013, Path: "/oauth"},
							{Name: "manage", Port: 8002, Path: "/manage"},
						},
					},
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{{Name: "marklogic", Replicas: &replicas}},
				},
			}
			config := generateHAProxyConfigMapData(context.Background(), cluster)["haproxy.cfg"]
			if strings.Count(config, "h1-case-adjust authorization Authorization") != 1 {
				t.Fatal("missing unique Authorization case mapping")
			}
			backends := strings.Split(config, "\nbackend ")[1:]
			if len(backends) != 2 {
				t.Fatalf("expected two HTTP backends, got %d", len(backends))
			}
			for _, backend := range backends {
				if !strings.Contains(backend, "option h1-case-adjust-bogus-server") {
					t.Fatal("HTTP backend does not enable Authorization case adjustment")
				}
			}
		})
	}
}
