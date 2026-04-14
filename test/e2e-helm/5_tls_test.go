// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	"github.com/tidwall/gjson"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	e2eutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

// TestTlsWithSelfSigned verifies that the operator correctly enables TLS with a
// self-signed certificate on the default app servers in a watched namespace.
func TestTlsWithSelfSigned(t *testing.T) {
	trackTest(t)
	feature := features.New("TLS with Self Signed Certificate").WithLabel("type", "tls-self-signed")
	tlsNamespace := "ml-ns-tls" // must be in watchedNamespaces
	releaseName := "ml"
	tlsReplicas := int32(1)

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: tlsNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        releaseName,
					Replicas:    &tlsReplicas,
					IsBootstrap: true,
				},
			},
			Tls: &marklogicv1.Tls{
				EnableOnDefaultAppServers: true,
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Wait for any previous terminating namespace to finish before creating a new one.
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, tlsNamespace, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", tlsNamespace, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", tlsNamespace)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", tlsNamespace, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}

		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: tlsNamespace, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", tlsNamespace, err)
		}
		marklogicv1.AddToScheme(client.Resources(tlsNamespace).GetScheme())
		if err := client.Resources(tlsNamespace).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("pod ml-0 is ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), tlsNamespace, "ml-0", 120*time.Second, true); err != nil {
			logDiagnostics(t, tlsNamespace)
			t.Fatalf("ml-0 not ready: %v", err)
		}
		// Allow time for TLS to be fully configured on management port 8002.
		t.Log("Waiting for TLS configuration to propagate...")
		time.Sleep(30 * time.Second)

		httpsCheck := "curl -k -s -o /dev/null -w '%{http_code}' https://localhost:8002/admin/v1/timestamp"
		var httpsReady bool
		for i := 0; i < 60; i++ {
			out, err := utils.ExecCmdInPod("ml-0", tlsNamespace, mlContainerName, httpsCheck)
			if err == nil && (strings.Contains(out, "200") || strings.Contains(out, "401")) {
				t.Log("HTTPS is configured and responding")
				httpsReady = true
				break
			}
			if i == 59 {
				t.Fatal("HTTPS not configured on port 8002 after 2 minutes")
			}
			time.Sleep(2 * time.Second)
		}
		if !httpsReady {
			t.Fatal("HTTPS endpoint never became ready")
		}
		return ctx
	})

	feature.Assess("HTTPS connection enabled on /manage/v2/groups", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cmd := fmt.Sprintf("curl -k --anyauth -u %s:%s https://localhost:8002/manage/v2/groups", adminUsername, adminPassword)
		if _, err := utils.ExecCmdInPod("ml-0", tlsNamespace, mlContainerName, cmd); err != nil {
			t.Fatalf("HTTPS request to /manage/v2/groups failed: %v", err)
		}
		return ctx
	})

	feature.Assess("HTTPS connection enabled on /manage/v2/hosts", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cmd := fmt.Sprintf("curl -k --anyauth -u %s:%s https://localhost:8002/manage/v2/hosts?view=status&format=json", adminUsername, adminPassword)
		if _, err := utils.ExecCmdInPod("ml-0", tlsNamespace, mlContainerName, cmd); err != nil {
			t.Fatalf("HTTPS request to /manage/v2/hosts failed: %v", err)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, tlsNamespace)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestTlsWithNamedCert verifies that the operator correctly applies per-pod named
// TLS certificates in a watched namespace.
func TestTlsWithNamedCert(t *testing.T) {
	trackTest(t)
	feature := features.New("TLS with Named Certificate").WithLabel("type", "tls-named-cert")
	namedNS := "ml-ns-tls-named" // must be in watchedNamespaces
	releaseName := "marklogic"
	namedReplicas := int32(2)

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: namedNS,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        releaseName,
					Replicas:    &namedReplicas,
					IsBootstrap: true,
				},
			},
			Tls: &marklogicv1.Tls{
				EnableOnDefaultAppServers: true,
				CertSecretNames:           []string{"marklogic-0-cert", "marklogic-1-cert"},
				CaSecretName:              "ca-cert",
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Wait for any previous terminating namespace to finish before creating a new one.
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, namedNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", namedNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", namedNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", namedNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}

		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namedNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", namedNS, err)
		}
		marklogicv1.AddToScheme(client.Resources(namedNS).GetScheme())

		if err := utils.GenerateCACertificate("test/test_data/ca_cert"); err != nil {
			t.Fatalf("Failed to generate CA certificate: %v", err)
		}
		if err := utils.GenerateCertificates("test/test_data/helm_pod_zero_certs", "test/test_data/ca_cert"); err != nil {
			t.Fatalf("Failed to generate helm_pod_zero_certs: %v", err)
		}
		if err := utils.GenerateCertificates("test/test_data/helm_pod_one_certs", "test/test_data/ca_cert"); err != nil {
			t.Fatalf("Failed to generate helm_pod_one_certs: %v", err)
		}

		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete secret ca-cert --ignore-not-found=true", namedNS))
		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete secret marklogic-0-cert --ignore-not-found=true", namedNS))
		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete secret marklogic-1-cert --ignore-not-found=true", namedNS))

		if p := e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s create secret generic ca-cert --from-file=test/test_data/ca_cert/cacert.pem", namedNS)); p.Err() != nil {
			t.Fatalf("Failed to create ca-cert secret: %s", p.Result())
		}
		if p := e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s create secret generic marklogic-0-cert --from-file=test/test_data/helm_pod_zero_certs/tls.crt --from-file=test/test_data/helm_pod_zero_certs/tls.key", namedNS)); p.Err() != nil {
			t.Fatalf("Failed to create marklogic-0-cert: %s", p.Result())
		}
		if p := e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s create secret generic marklogic-1-cert --from-file=test/test_data/helm_pod_one_certs/tls.crt --from-file=test/test_data/helm_pod_one_certs/tls.key", namedNS)); p.Err() != nil {
			t.Fatalf("Failed to create marklogic-1-cert: %s", p.Result())
		}

		if err := client.Resources(namedNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("pods marklogic-0 and marklogic-1 are ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), namedNS, "marklogic-0", 180*time.Second, true); err != nil {
			logDiagnostics(t, namedNS)
			t.Fatalf("marklogic-0 not ready: %v", err)
		}
		if err := utils.WaitForPod(ctx, t, c.Client(), namedNS, "marklogic-1", 180*time.Second, true); err != nil {
			logDiagnostics(t, namedNS)
			t.Fatalf("marklogic-1 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("Named certificates are applied", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "marklogic-1"
		hostnamesSlice := []string{
			fmt.Sprintf("marklogic-0.marklogic.%s.svc.cluster.local", namedNS),
			fmt.Sprintf("marklogic-1.marklogic.%s.svc.cluster.local", namedNS),
		}
		time.Sleep(5 * time.Second)
		cmd := fmt.Sprintf("curl -k --anyauth -u %s:%s https://localhost:8002/manage/v2/certificates?format=json", adminUsername, adminPassword)
		certs, err := utils.ExecCmdInPod(podName, namedNS, mlContainerName, cmd)
		if err != nil {
			t.Fatalf("Failed to get certificates list: %v", err)
		}
		certURIs := gjson.Get(certs, `certificate-default-list.list-items.list-item.#.uriref`).Array()
		if len(certURIs) < 2 {
			t.Fatalf("Expected at least 2 certificates, found %d", len(certURIs))
		}
		for i, uri := range certURIs[:2] {
			certURL := fmt.Sprintf("https://localhost:8002%s?format=json", uri)
			detail, err := utils.ExecCmdInPod(podName, namedNS, mlContainerName,
				fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, certURL))
			if err != nil {
				t.Fatalf("Failed to get certificate %d: %v", i, err)
			}
			if gjson.Get(detail, `certificate-default.temporary`).Bool() {
				t.Fatalf("Certificate %d is still temporary (named cert not applied)", i)
			}
			hostname := gjson.Get(detail, `certificate-default.host-name`).String()
			if !slices.Contains(hostnamesSlice, hostname) {
				t.Fatalf("Certificate %d hostname %q not in expected list %v", i, hostname, hostnamesSlice)
			}
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namedNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestTlsWithMultiNode verifies that TLS with per-pod named certificates works correctly
// in a two-group (dnode + enode) cluster deployed in a watched namespace.
func TestTlsWithMultiNode(t *testing.T) {
	trackTest(t)
	feature := features.New("TLS with Multi Node Named Certificate").WithLabel("type", "tls-multi-node")
	tlsEdNS := "ml-ns-tls-ednode" // must be in watchedNamespaces
	enodeName := "enode"
	dnodeName := "dnode"
	enodeSize := int32(1)
	dnodeSize := int32(1)

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: tlsEdNS,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:     dnodeName,
					Replicas: &dnodeSize,
					GroupConfig: &marklogicv1.GroupConfig{
						Name:          dnodeName,
						EnableXdqpSsl: true,
					},
					IsBootstrap: true,
					Tls: &marklogicv1.Tls{
						EnableOnDefaultAppServers: true,
						CertSecretNames:           []string{"dnode-0-cert"},
						CaSecretName:              "ca-cert",
					},
				},
				{
					Name:     enodeName,
					Replicas: &enodeSize,
					GroupConfig: &marklogicv1.GroupConfig{
						Name:          enodeName,
						EnableXdqpSsl: true,
					},
					IsBootstrap: false,
					Tls: &marklogicv1.Tls{
						EnableOnDefaultAppServers: true,
						CertSecretNames:           []string{"enode-0-cert"},
						CaSecretName:              "ca-cert",
					},
				},
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Wait for any previous terminating namespace to finish.
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, tlsEdNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace status: %v", err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", tlsEdNS)
				}
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}

		if err := client.Resources(tlsEdNS).Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: tlsEdNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", tlsEdNS, err)
		}
		marklogicv1.AddToScheme(client.Resources(tlsEdNS).GetScheme())

		// Generate and populate TLS secrets before creating the CR so the operator
		// never reconciles against missing secrets.
		if err := utils.GenerateCACertificate("test/test_data/ca_cert"); err != nil {
			t.Fatalf("GenerateCACertificate failed: %v", err)
		}
		if err := utils.GenerateCertificates("test/test_data/helm_enode_zero_certs", "test/test_data/ca_cert"); err != nil {
			t.Fatalf("GenerateCertificates (enode) failed: %v", err)
		}
		if err := utils.GenerateCertificates("test/test_data/helm_dnode_zero_certs", "test/test_data/ca_cert"); err != nil {
			t.Fatalf("GenerateCertificates (dnode) failed: %v", err)
		}

		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete secret ca-cert --ignore-not-found=true", tlsEdNS))
		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete secret dnode-0-cert --ignore-not-found=true", tlsEdNS))
		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete secret enode-0-cert --ignore-not-found=true", tlsEdNS))

		if p := e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s create secret generic ca-cert --from-file=test/test_data/ca_cert/cacert.pem", tlsEdNS)); p.Err() != nil {
			t.Fatalf("Failed to create ca-cert: %s", p.Result())
		}
		if p := e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s create secret generic dnode-0-cert --from-file=test/test_data/helm_dnode_zero_certs/tls.crt --from-file=test/test_data/helm_dnode_zero_certs/tls.key", tlsEdNS)); p.Err() != nil {
			t.Fatalf("Failed to create dnode-0-cert: %s", p.Result())
		}
		if p := e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s create secret generic enode-0-cert --from-file=test/test_data/helm_enode_zero_certs/tls.crt --from-file=test/test_data/helm_enode_zero_certs/tls.key", tlsEdNS)); p.Err() != nil {
			t.Fatalf("Failed to create enode-0-cert: %s", p.Result())
		}

		if err := client.Resources(tlsEdNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("dnode-0 and enode-0 pods are ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), tlsEdNS, "dnode-0", 180*time.Second, true); err != nil {
			logDiagnostics(t, tlsEdNS)
			t.Fatalf("dnode-0 not ready: %v", err)
		}
		if err := utils.WaitForPod(ctx, t, c.Client(), tlsEdNS, "enode-0", 180*time.Second, true); err != nil {
			logDiagnostics(t, tlsEdNS)
			t.Fatalf("enode-0 not ready: %v", err)
		}
		t.Log("Waiting for enode to join cluster and configure TLS...")
		time.Sleep(60 * time.Second)
		return ctx
	})

	feature.Assess("Named certs applied on multi-node cluster", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "dnode-0"
		hostnamesSlice := []string{
			fmt.Sprintf("enode-0.enode.%s.svc.cluster.local", tlsEdNS),
			fmt.Sprintf("dnode-0.dnode.%s.svc.cluster.local", tlsEdNS),
		}
		t.Log("Waiting for TLS configuration on port 8002...")
		time.Sleep(30 * time.Second)

		httpsCheck := "curl -k -s -o /dev/null -w '%{http_code}' https://localhost:8002/admin/v1/timestamp"
		for i := 0; i < 60; i++ {
			out, err := utils.ExecCmdInPod(podName, tlsEdNS, mlContainerName, httpsCheck)
			if err == nil && (strings.Contains(out, "200") || strings.Contains(out, "401")) {
				break
			}
			if i == 59 {
				t.Fatal("HTTPS not configured on port 8002 after 2 minutes")
			}
			time.Sleep(2 * time.Second)
		}

		url := "https://localhost:8002/manage/v2/certificates?format=json"
		cmd := fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		var certs string
		var err error
		for i := 0; i < 10; i++ {
			certs, err = utils.ExecCmdInPod(podName, tlsEdNS, mlContainerName, cmd)
			if err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			t.Fatalf("Failed to get certificates list: %v", err)
		}
		certURIs := gjson.Get(certs, `certificate-default-list.list-items.list-item.#.uriref`).Array()
		if len(certURIs) < 2 {
			t.Fatalf("Expected at least 2 certificates, found %d", len(certURIs))
		}
		for i, uri := range certURIs[:2] {
			certURL := fmt.Sprintf("https://localhost:8002%s?format=json", uri)
			detail, err := utils.ExecCmdInPod(podName, tlsEdNS, mlContainerName,
				fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, certURL))
			if err != nil {
				t.Fatalf("Failed to get certificate %d: %v", i, err)
			}
			if gjson.Get(detail, `certificate-default.temporary`).Bool() {
				t.Fatalf("Certificate %d is still temporary", i)
			}
			hostname := gjson.Get(detail, `certificate-default.host-name`).String()
			if !slices.Contains(hostnamesSlice, hostname) {
				t.Fatalf("Certificate %d hostname %q not in expected list %v", i, hostname, hostnamesSlice)
			}
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, tlsEdNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
