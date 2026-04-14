// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	// metricsServiceName is the name of the metrics Service installed by the Helm chart.
	metricsServiceName = "marklogic-operator-controller-manager-metrics-service"

	// metricsLocalPort is the local port used for kubectl port-forward.
	// Using a different local port from the main e2e suite to avoid conflicts if run simultaneously.
	metricsLocalPort = "28080"

	// metricsRemotePort is the port the metrics server listens on when metrics.secure=false.
	metricsRemotePort = "8080"
)

// TestMetricsEndpointInsecure verifies that the insecure (HTTP) metrics endpoint
// returns valid Prometheus output when the operator is installed with
// metrics.secure=false (the only supported mode for namespace-scoped installs).
//
// In namespace-scoped mode the ClusterRole required for Kubernetes TokenReview/
// SubjectAccessReview is not available, so metrics.secure=true is unsupported.
// The Helm chart installs with metrics.secure=false which exposes HTTP on :8080
// with no authentication required.
func TestMetricsEndpointInsecure(t *testing.T) {
	trackTest(t)
	feature := features.New("Metrics Endpoint — Insecure (namespace-scoped)").
		WithLabel("type", "metrics")

	feature.Assess("Metrics endpoint is publicly accessible without authentication", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		pf, localAddr, cancel := startMetricsPortForward(t)
		defer func() {
			cancel()
			_ = pf.Wait()
		}()

		waitForMetricsPortForward(t, localAddr)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://%s/metrics", localAddr), nil)
		if err != nil {
			t.Fatalf("failed to build metrics request: %v", err)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("failed to GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200 (no auth required), got %d: %s", resp.StatusCode, body)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read metrics body: %v", err)
		}
		metrics := string(body)
		if !strings.Contains(metrics, "# HELP") || !strings.Contains(metrics, "# TYPE") {
			t.Errorf("response does not look like Prometheus text-format metrics:\n%.500s...", metrics)
		}
		if !strings.Contains(metrics, "controller_runtime_reconcile") {
			t.Errorf("expected controller_runtime_reconcile metric not found")
		}
		t.Logf("Insecure metrics endpoint returned valid Prometheus output (%d bytes)", len(body))
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// startMetricsPortForward opens a kubectl port-forward tunnel to the metrics service
// in the operator namespace (helmNS). Returns the process, local address, and cancel func.
func startMetricsPortForward(t *testing.T) (*exec.Cmd, string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	// #nosec G204 — helmNS, metricsServiceName, metricsLocalPort, metricsRemotePort are package constants
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", helmNS,
		"svc/"+metricsServiceName,
		metricsLocalPort+":"+metricsRemotePort,
	)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("failed to start port-forward for %s: %v", metricsServiceName, err)
	}
	t.Logf("port-forward started: localhost:%s → %s/%s:%s", metricsLocalPort, helmNS, metricsServiceName, metricsRemotePort)
	return cmd, "localhost:" + metricsLocalPort, cancel
}

// waitForMetricsPortForward polls until the forwarded TCP port accepts connections.
func waitForMetricsPortForward(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("port-forward to %s did not become ready within 30s", addr)
}
