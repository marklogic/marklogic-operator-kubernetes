// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	metricsReaderSA      = "metrics-reader-e2e"
	metricsReaderCRB     = "metrics-reader-e2e-binding"
	metricsReaderRole    = "marklogic-operator-metrics-reader"
	metricsServiceName   = "marklogic-operator-controller-manager-metrics-service"
	metricsLocalPort     = "19443"
	metricsRemotePort    = "8443"
	metricsTokenAudience = "https://kubernetes.default.svc"
)

// TestMetricsEndpoint verifies that the native secure metrics server:
//  1. Serves HTTPS on port 8443 inside the cluster.
//  2. Accepts requests authenticated with a valid Kubernetes ServiceAccount token
//     (validated via the TokenReview API).
//  3. Authorises only service accounts that hold the metrics-reader ClusterRole
//     (enforced via the SubjectAccessReview API).
//  4. Returns valid Prometheus text-format metrics.
func TestMetricsEndpoint(t *testing.T) {
	feature := features.New("Metrics Endpoint").
		WithLabel("type", "metrics")

	// ── Setup: create a dedicated ServiceAccount and bind the metrics-reader role ──
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// ServiceAccount
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      metricsReaderSA,
				Namespace: namespace,
			},
		}
		if err := client.Resources(namespace).Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("failed to create metrics-reader ServiceAccount: %v", err)
		}

		// ClusterRoleBinding: metrics-reader → metricsReaderSA
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: metricsReaderCRB,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     metricsReaderRole,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      metricsReaderSA,
					Namespace: namespace,
				},
			},
		}
		if err := client.Resources().Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("failed to create metrics-reader ClusterRoleBinding: %v", err)
		}

		return ctx
	})

	// ── Assessment 1: metrics endpoint requires authentication ────────────────────
	feature.Assess("Unauthenticated request is rejected", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		pf, localAddr, cancel := startPortForward(t, namespace, metricsServiceName, metricsLocalPort, metricsRemotePort)
		defer func() {
			cancel()
			_ = pf.Wait()
		}()

		waitForPortForward(t, localAddr)

		// #nosec G402 — self-signed cert in test environment
		httpClient := &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s/metrics", localAddr), nil)
		if err != nil {
			t.Fatalf("failed to build metrics request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("unexpected error hitting metrics endpoint without token: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 401/403 without token, got %d", resp.StatusCode)
		}
		t.Logf("Unauthenticated request correctly rejected with HTTP %d", resp.StatusCode)
		return ctx
	})

	// ── Assessment 2: authorised SA can read metrics ───────────────────────────
	feature.Assess("Authorised ServiceAccount can scrape /metrics", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Obtain a bound ServiceAccount token for the metrics-reader SA.
		restCfg, err := rest.InClusterConfig()
		if err != nil {
			// Fallback: use the test kubeconfig (running outside the cluster).
			restCfg = c.Client().RESTConfig()
		}

		token := requestSAToken(ctx, t, restCfg, namespace, metricsReaderSA, metricsTokenAudience)

		pf, localAddr, cancel := startPortForward(t, namespace, metricsServiceName, metricsLocalPort, metricsRemotePort)
		defer func() {
			cancel()
			_ = pf.Wait()
		}()

		waitForPortForward(t, localAddr)

		// #nosec G402 — self-signed cert in test environment
		httpClient := &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s/metrics", localAddr), nil)
		if err != nil {
			t.Fatalf("failed to build metrics request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("failed to GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, body)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read metrics response body: %v", err)
		}
		metrics := string(body)

		// Prometheus text format always contains "# HELP" and "# TYPE" comment lines.
		if !strings.Contains(metrics, "# HELP") || !strings.Contains(metrics, "# TYPE") {
			t.Errorf("response does not look like Prometheus text-format metrics:\n%.500s...", metrics)
		} else {
			t.Logf("Metrics endpoint returned valid Prometheus output (%d bytes)", len(body))
		}

		// Spot-check for a controller-runtime metric that is always present.
		if !strings.Contains(metrics, "controller_runtime_reconcile") {
			t.Errorf("expected controller_runtime_reconcile metric not found in output")
		}

		return ctx
	})

	// ── Assessment 3: unauthorised SA is denied ───────────────────────────────
	feature.Assess("Unauthorised ServiceAccount is denied", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		restCfg := c.Client().RESTConfig()

		// Use the operator controller-manager SA — it has many permissions but
		// is NOT bound to the metrics-reader ClusterRole, so the SAR check must deny it.
		token := requestSAToken(ctx, t, restCfg, namespace, "marklogic-operator-controller-manager", metricsTokenAudience)

		pf, localAddr, cancel := startPortForward(t, namespace, metricsServiceName, metricsLocalPort, metricsRemotePort)
		defer func() {
			cancel()
			_ = pf.Wait()
		}()

		waitForPortForward(t, localAddr)

		// #nosec G402 — self-signed cert in test environment
		httpClient := &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s/metrics", localAddr), nil)
		if err != nil {
			t.Fatalf("failed to build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected HTTP 403 for unauthorised SA, got %d", resp.StatusCode)
		} else {
			t.Logf("Unauthorised SA correctly denied with HTTP 403")
		}

		return ctx
	})

	// ── Teardown: remove the dedicated SA and CRB ────────────────────────────
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: metricsReaderCRB}}
		if err := client.Resources().Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete ClusterRoleBinding %s: %v", metricsReaderCRB, err)
		}

		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: metricsReaderSA, Namespace: namespace}}
		if err := client.Resources(namespace).Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete ServiceAccount %s: %v", metricsReaderSA, err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// startPortForward opens a kubectl port-forward tunnel to the given service.
// It returns the process handle, the local address, and a cancel function.
func startPortForward(t *testing.T, ns, svc, localPort, remotePort string) (*exec.Cmd, string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)

	// #nosec G204 — ns, svc, localPort, remotePort are all test constants
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", ns,
		"svc/"+svc,
		localPort+":"+remotePort,
	)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("failed to start port-forward for %s: %v", svc, err)
	}
	t.Logf("port-forward started: localhost:%s → %s:%s", localPort, svc, remotePort)
	return cmd, "localhost:" + localPort, cancel
}

// waitForPortForward polls until the forwarded port accepts TCP connections.
func waitForPortForward(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// #nosec G402 — only checking TCP connectivity, not validating cert here
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("port-forward to %s did not become ready within 30s", addr)
}

// requestSAToken calls the TokenRequest API to obtain a bound token for the
// given ServiceAccount. Tokens are short-lived (10 minutes) and audience-scoped.
func requestSAToken(ctx context.Context, t *testing.T, cfg *rest.Config, ns, saName, audience string) string {
	t.Helper()

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create kubernetes client: %v", err)
	}

	expirationSeconds := int64(600) // 10 minutes — sufficient for one test run
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}

	result, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to obtain token for ServiceAccount %s/%s: %v", ns, saName, err)
	}
	return result.Status.Token
}
