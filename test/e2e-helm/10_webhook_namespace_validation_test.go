// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	webhookWatchedNamespace   = "ml-ns-test"
	webhookUnwatchedNamespace = "ml-ns-unwatched"
	webhookCertificateName    = "marklogic-operator-webhook-serving-cert"
)

func TestNamespaceValidationWebhook(t *testing.T) {
	trackTest(t)
	feature := features.New("Namespace Validation Webhook").
		WithLabel("type", "webhook-namespace-validation")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: webhookUnwatchedNamespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("failed to create unwatched namespace %s: %v", webhookUnwatchedNamespace, err)
		}
		return ctx
	})

	feature.Assess("CR denied in non-watched namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		manifestPath := writeTempMarklogicClusterManifest(t, webhookUnwatchedNamespace, "webhook-denied")
		defer os.Remove(manifestPath)

		cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", manifestPath)
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected admission denial for namespace %s, but apply succeeded: %s", webhookUnwatchedNamespace, string(output))
		}

		out := string(output)
		if !strings.Contains(out, "outside operator watch scope") {
			t.Fatalf("expected denial message to mention watch scope, got: %s", out)
		}
		if !strings.Contains(out, webhookUnwatchedNamespace) {
			t.Fatalf("expected denial message to include namespace %s, got: %s", webhookUnwatchedNamespace, out)
		}
		return ctx
	})

	feature.Assess("CR accepted in watched namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		manifestPath := writeTempMarklogicClusterManifest(t, webhookWatchedNamespace, "webhook-allowed")
		defer os.Remove(manifestPath)

		cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", manifestPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected apply to succeed in watched namespace %s, got error: %v output: %s", webhookWatchedNamespace, err, string(output))
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: webhookUnwatchedNamespace}}
		if err := client.Resources().Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("warning: failed to delete namespace %s: %v", webhookUnwatchedNamespace, err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestNamespaceValidationWebhookWithCertManager(t *testing.T) {
	trackTest(t)
	if webhookCerts != "certmanager" {
		t.Skip("set E2E_HELM_WEBHOOK_CERT_PROVIDER=certManager to run cert-manager webhook validation")
	}

	feature := features.New("Namespace Validation Webhook with cert-manager").
		WithLabel("type", "webhook-namespace-validation-certmanager")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: webhookUnwatchedNamespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("failed to create unwatched namespace %s: %v", webhookUnwatchedNamespace, err)
		}
		return ctx
	})

	feature.Assess("cert-manager Certificate becomes ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cmd := exec.Command(
			"kubectl", "wait",
			fmt.Sprintf("certificate/%s", webhookCertificateName),
			"-n", helmNS,
			"--for=condition=Ready",
			"--timeout=3m",
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("webhook certificate did not become ready: %v output: %s", err, string(output))
		}
		return ctx
	})

	feature.Assess("validating webhook uses cert-manager CA injection", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cmd := exec.Command(
			"kubectl", "get", "validatingwebhookconfiguration", "marklogic-operator-validating-webhook",
			"-o", `jsonpath={.metadata.annotations.cert-manager\.io/inject-ca-from}`,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("failed reading validating webhook annotation: %v output: %s", err, string(output))
		}

		expected := fmt.Sprintf("%s/%s", helmNS, webhookCertificateName)
		if strings.TrimSpace(string(output)) != expected {
			t.Fatalf("unexpected cert-manager injection annotation value, expected %s got %s", expected, strings.TrimSpace(string(output)))
		}
		return ctx
	})

	feature.Assess("CR denied in non-watched namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		assertWebhookDeniedInUnwatchedNamespace(t, webhookUnwatchedNamespace)
		return ctx
	})

	feature.Assess("CR accepted in watched namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		assertWebhookAllowedInWatchedNamespace(t, webhookWatchedNamespace)
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: webhookUnwatchedNamespace}}
		if err := client.Resources().Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("warning: failed to delete namespace %s: %v", webhookUnwatchedNamespace, err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func writeTempMarklogicClusterManifest(t *testing.T, namespace, name string) string {
	t.Helper()

	manifest := fmt.Sprintf(`apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  auth:
    adminUsername: %s
    adminPassword: %s
  markLogicGroups:
  - name: node
    replicas: 1
    isBootstrap: true
`, name, namespace, marklogicImage, adminUsername, adminPassword)

	f, err := os.CreateTemp("", "ml-webhook-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp manifest: %v", err)
	}
	if _, err := f.WriteString(manifest); err != nil {
		f.Close()
		t.Fatalf("failed to write temp manifest: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp manifest: %v", err)
	}
	return f.Name()
}

func assertWebhookDeniedInUnwatchedNamespace(t *testing.T, namespace string) {
	t.Helper()

	manifestPath := writeTempMarklogicClusterManifest(t, namespace, "webhook-denied")
	defer os.Remove(manifestPath)

	cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", manifestPath)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected admission denial for namespace %s, but apply succeeded: %s", namespace, string(output))
	}

	out := string(output)
	if !strings.Contains(out, "outside operator watch scope") {
		t.Fatalf("expected denial message to mention watch scope, got: %s", out)
	}
	if !strings.Contains(out, namespace) {
		t.Fatalf("expected denial message to include namespace %s, got: %s", namespace, out)
	}
}

func assertWebhookAllowedInWatchedNamespace(t *testing.T, namespace string) {
	t.Helper()

	manifestPath := writeTempMarklogicClusterManifest(t, namespace, "webhook-allowed")
	defer os.Remove(manifestPath)

	cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", manifestPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected apply to succeed in watched namespace %s, got error: %v output: %s", namespace, err, string(output))
	}
}
