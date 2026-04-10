// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	"github.com/tidwall/gjson"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	e2eutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

func TestTlsWithSelfSigned(t *testing.T) {
	feature := features.New("TLS with Self Signed Certificate").WithLabel("type", "tls-self-signed")
	namespace := "tls-self-signed"
	releaseName := "ml"
	replicas := int32(1)

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: namespace,
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
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			Tls: &marklogicv1.Tls{
				EnableOnDefaultAppServers: true,
			},
		},
	}

	// Assessment for MarklogicCluster creation
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		client.Resources(namespace).Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   namespace,
				Labels: namespaceLabels(),
			},
		})
		marklogicv1.AddToScheme(client.Resources(namespace).GetScheme())

		if err := client.Resources(namespace).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "ml-0"
		err := utils.WaitForPod(ctx, t, client, namespace, podName, 120*time.Second, true)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}

		// Wait for TLS to be fully configured on management port 8002
		t.Log("Waiting for TLS configuration to be applied to port 8002...")
		time.Sleep(30 * time.Second)

		// Verify HTTPS is actually configured (not HTTP)
		t.Log("Verifying HTTPS is configured on port 8002...")
		httpsCheck := "curl -k -s -o /dev/null -w '%{http_code}' https://localhost:8002/admin/v1/timestamp"
		var httpsReady bool
		for i := 0; i < 60; i++ {
			output, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, httpsCheck)
			if err == nil && (strings.Contains(output, "200") || strings.Contains(output, "401")) {
				t.Log("HTTPS is configured and responding")
				httpsReady = true
				break
			}
			// Check if still HTTP (should fail)
			httpCheck := "curl -s -o /dev/null -w '%{http_code}' http://localhost:8002/admin/v1/timestamp"
			output, _ = utils.ExecCmdInPod(podName, namespace, mlContainerName, httpCheck)
			if strings.Contains(output, "200") || strings.Contains(output, "401") {
				t.Logf("Port 8002 still using HTTP (attempt %d/60), waiting for TLS configuration...", i+1)
			} else {
				t.Logf("Port 8002 not ready yet (attempt %d/60)...", i+1)
			}

			if i == 59 {
				t.Fatalf("HTTPS not configured on port 8002 after 2 minutes. TLS configuration may have failed.")
			}
			time.Sleep(2 * time.Second)
		}

		if !httpsReady {
			t.Fatal("HTTPS endpoint never became ready")
		}

		return ctx
	})

	feature.Assess("HTTPS connnection enabled", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "ml-0"
		url := "https://localhost:8002/manage/v2/groups"
		command := fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, url)

		_, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute curl command in pod: %v", err)
		}
		// if !strings.Contains(string(output), "<nameref>Default</nameref>") {
		// 	t.Fatal("Groups does not exists on MarkLogic cluster")
		// }
		return ctx
	})

	feature.Assess("HTTPS connnection enabled", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "ml-0"
		url := "https://localhost:8002/manage/v2/hosts?view=status&format=json"
		command := fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, url)

		_, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute curl command in pod: %v", err)
		}
		// if !strings.Contains(string(output), "<nameref>Default</nameref>") {
		// 	t.Fatal("Groups does not exists on MarkLogic cluster")
		// }
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}

func TestTlsWithNamedCert(t *testing.T) {
	feature := features.New("TLS with Named Certificate").WithLabel("type", "tls-named-cert")
	namespace := "marklogic-tlsnamed"
	releaseName := "marklogic"
	replicas := int32(2)

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: namespace,
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
					Replicas:    &replicas,
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
		client.Resources(namespace).Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   namespace,
				Labels: namespaceLabels(),
			},
		})
		marklogicv1.AddToScheme(client.Resources(namespace).GetScheme())

		// Generate certificates and create secrets BEFORE creating MarklogicCluster
		err := utils.GenerateCACertificate("test/test_data/ca_cert")
		if err != nil {
			t.Fatalf("Failed to generate CA certificate: %s", err)
		}
		err = utils.GenerateCertificates("test/test_data/pod_zero_certs", "test/test_data/ca_cert")
		if err != nil {
			t.Fatalf("Failed to generate pod_zero_certs TLS certificates: %s", err)
		}
		err = utils.GenerateCertificates("test/test_data/pod_one_certs", "test/test_data/ca_cert")
		if err != nil {
			t.Fatalf("Failed to generate pod_one_certs TLS certificates: %s", err)
		}
		// Delete existing secrets if they exist (cleanup from previous failed runs)
		e2eutils.RunCommand("kubectl -n marklogic-tlsnamed delete secret ca-cert --ignore-not-found=true")
		e2eutils.RunCommand("kubectl -n marklogic-tlsnamed delete secret marklogic-0-cert --ignore-not-found=true")
		e2eutils.RunCommand("kubectl -n marklogic-tlsnamed delete secret marklogic-1-cert --ignore-not-found=true")

		p := e2eutils.RunCommand("kubectl -n marklogic-tlsnamed create secret generic ca-cert --from-file=test/test_data/ca_cert/cacert.pem")
		if p.Err() != nil {
			t.Fatalf("Failed to create ca-cert secret: %s. Output: %s", p.Err(), p.Result())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsnamed create secret generic marklogic-0-cert --from-file=test/test_data/pod_zero_certs/tls.crt --from-file=test/test_data/pod_zero_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create marklogic-0-cert secret: %s. Output: %s", p.Err(), p.Result())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsnamed create secret generic marklogic-1-cert --from-file=test/test_data/pod_one_certs/tls.crt --from-file=test/test_data/pod_one_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create marklogic-1-cert secret: %s. Output: %s", p.Err(), p.Result())
		}

		// Now create MarklogicCluster CR after secrets are ready
		if err := client.Resources(namespace).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Wait for both pods to be ready (first pod can take longer to initialize cluster)
		t.Log("Waiting for marklogic-0 pod...")
		err := utils.WaitForPod(ctx, t, client, namespace, "marklogic-0", 180*time.Second, true)
		if err != nil {
			t.Fatalf("Failed to wait for marklogic-0 creation: %v", err)
		}

		t.Log("Waiting for marklogic-1 pod...")
		err = utils.WaitForPod(ctx, t, client, namespace, "marklogic-1", 180*time.Second, true)
		if err != nil {
			t.Fatalf("Failed to wait for marklogic-1 creation: %v", err)
		}
		return ctx
	})

	feature.Assess("Verify Named Certificate", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "marklogic-1"
		hostnamesSlice := []string{"marklogic-0.marklogic.marklogic-tlsnamed.svc.cluster.local", "marklogic-1.marklogic.marklogic-tlsnamed.svc.cluster.local"}
		time.Sleep(5 * time.Second)
		url := "https://localhost:8002/manage/v2/certificates?format=json"
		command := fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		certs, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to get certificates list: %v", err)
		}
		certURIs := gjson.Get(certs, `certificate-default-list.list-items.list-item.#.uriref`).Array()
		t.Log("Certificates URL list", certURIs)
		if len(certURIs) < 2 {
			t.Fatalf("Expected at least 2 certificates, found %d", len(certURIs))
		}
		cert0Url := fmt.Sprintf("https://localhost:8002%s?format=json", certURIs[0])
		cert1Url := fmt.Sprintf("https://localhost:8002%s?format=json", certURIs[1])
		command = fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, cert0Url)
		cert0Detail, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute and get first certificate: %v", err)
		}
		cert0Temporary := gjson.Get(cert0Detail, `certificate-default.temporary`).Bool()
		cert0HostName := gjson.Get(cert0Detail, `certificate-default.host-name`).String()

		command = fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, cert1Url)
		cert1Detail, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute and get second certificate: %v", err)
		}
		cert1Temporary := gjson.Get(cert1Detail, `certificate-default.temporary`).Bool()
		cert1HostName := gjson.Get(cert1Detail, `certificate-default.host-name`).String()
		if cert0Temporary || cert1Temporary {
			t.Logf("Certificate 0: %v, Certificate 1: %v", cert0Temporary, cert1Temporary)
			t.Fatalf("Certificate is temporary")
		}
		if !slices.Contains(hostnamesSlice, cert0HostName) || !slices.Contains(hostnamesSlice, cert1HostName) {
			t.Logf("Certificate 0: %v, Certificate 1: %v", cert0HostName, cert1HostName)
			t.Fatalf("Certificate host name is not in the list of hostnames")
		}
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}

func TestTlsWithMultiNode(t *testing.T) {
	feature := features.New("TLS with Multi Node Named Certificate").WithLabel("type", "tls-multi-node")
	namespace := "marklogic-tlsednode"
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
			Namespace: namespace,
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

		// Check if namespace exists and wait if it's terminating
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, namespace, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// Namespace doesn't exist, we can create it
					break
				}
				// Other error - fail the test
				t.Fatalf("Error checking namespace status: %v", err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					// Timeout waiting for namespace to finish terminating
					t.Fatalf("Timeout waiting for namespace %s to finish terminating after 120 seconds", namespace)
				}
				t.Logf("Namespace %s is terminating, waiting... (attempt %d/60)", namespace, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			// Namespace exists and is active, can proceed
			break
		}

		// Create namespace if it doesn't exist
		err := client.Resources(namespace).Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   namespace,
				Labels: namespaceLabels(),
			},
		})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", namespace, err)
		}

		// Delete existing MarklogicCluster if it exists (cleanup from previous failed runs)
		existingCR := &marklogicv1.MarklogicCluster{}
		if err := client.Resources(namespace).Get(ctx, cr.Name, namespace, existingCR); err == nil {
			t.Log("Deleting existing MarklogicCluster from previous run")
			if err := client.Resources(namespace).Delete(ctx, existingCR); err != nil {
				t.Logf("Warning: Failed to delete existing MarklogicCluster: %s", err)
			}
			time.Sleep(10 * time.Second) // Wait for deletion
		}

		if err := client.Resources(namespace).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		err = utils.GenerateCACertificate("test/test_data/ca_cert")
		if err != nil {
			t.Fatalf("Failed to generate CA certificate: %s", err)
		}
		err = utils.GenerateCertificates("test/test_data/enode_zero_certs", "test/test_data/ca_cert")
		if err != nil {
			t.Fatalf("Failed to generate pod_zero_certs TLS certificates: %s", err)
		}
		err = utils.GenerateCertificates("test/test_data/dnode_zero_certs", "test/test_data/ca_cert")
		if err != nil {
			t.Fatalf("Failed to generate pod_one_certs TLS certificates: %s", err)
		}
		// Delete existing secrets if they exist (cleanup from previous failed runs)
		e2eutils.RunCommand("kubectl -n marklogic-tlsednode delete secret ca-cert --ignore-not-found=true")
		e2eutils.RunCommand("kubectl -n marklogic-tlsednode delete secret dnode-0-cert --ignore-not-found=true")
		e2eutils.RunCommand("kubectl -n marklogic-tlsednode delete secret enode-0-cert --ignore-not-found=true")

		p := e2eutils.RunCommand("kubectl -n marklogic-tlsednode create secret generic ca-cert --from-file=test/test_data/ca_cert/cacert.pem")
		if p.Err() != nil {
			t.Fatalf("Failed to create ca-cert secret: %s. Output: %s", p.Err(), p.Result())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsednode create secret generic dnode-0-cert --from-file=test/test_data/dnode_zero_certs/tls.crt --from-file=test/test_data/dnode_zero_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create dnode-0-cert secret: %s. Output: %s", p.Err(), p.Result())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsednode create secret generic enode-0-cert --from-file=test/test_data/enode_zero_certs/tls.crt --from-file=test/test_data/enode_zero_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create enode-0-cert secret: %s. Output: %s", p.Err(), p.Result())
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Wait for both pods to be ready
		t.Log("Waiting for dnode-0 pod...")
		err := utils.WaitForPod(ctx, t, client, namespace, "dnode-0", 180*time.Second, true)
		if err != nil {
			t.Fatalf("Failed to wait for dnode-0 creation: %v", err)
		}

		t.Log("Waiting for enode-0 pod...")
		err = utils.WaitForPod(ctx, t, client, namespace, "enode-0", 180*time.Second, true)
		if err != nil {
			t.Fatalf("Failed to wait for enode-0 creation: %v", err)
		}

		// Wait additional time for enode to join cluster and configure TLS
		t.Log("Waiting for enode to join cluster and configure TLS...")
		time.Sleep(60 * time.Second)

		return ctx
	})

	feature.Assess("Verify Named Cert on Multi Node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "dnode-0"
		hostnamesSlice := []string{"enode-0.enode.marklogic-tlsednode.svc.cluster.local", "dnode-0.dnode.marklogic-tlsednode.svc.cluster.local"}

		// Wait longer for TLS to be fully configured on management port 8002
		t.Log("Waiting for TLS configuration to be applied to port 8002...")
		time.Sleep(30 * time.Second)

		// Verify HTTPS is actually configured (not HTTP)
		t.Log("Verifying HTTPS is configured on port 8002...")
		httpsCheck := "curl -k -s -o /dev/null -w '%{http_code}' https://localhost:8002/admin/v1/timestamp"
		var httpsReady bool
		for i := 0; i < 60; i++ {
			output, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, httpsCheck)
			if err == nil && (strings.Contains(output, "200") || strings.Contains(output, "401")) {
				t.Log("HTTPS is configured and responding")
				httpsReady = true
				break
			}
			// Check if still HTTP (should fail)
			httpCheck := "curl -s -o /dev/null -w '%{http_code}' http://localhost:8002/admin/v1/timestamp"
			output, _ = utils.ExecCmdInPod(podName, namespace, mlContainerName, httpCheck)
			if strings.Contains(output, "200") || strings.Contains(output, "401") {
				t.Logf("Port 8002 still using HTTP (attempt %d/60), waiting for TLS configuration...", i+1)
			} else {
				t.Logf("Port 8002 not ready yet (attempt %d/60)...", i+1)
			}

			if i == 59 {
				t.Fatalf("HTTPS not configured on port 8002 after 2 minutes. TLS configuration may have failed.")
			}
			time.Sleep(2 * time.Second)
		}

		if !httpsReady {
			t.Fatal("HTTPS endpoint never became ready")
		}

		// Now fetch certificates list with HTTPS
		url := "https://localhost:8002/manage/v2/certificates?format=json"
		command := fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		var certs string
		var err error
		for i := 0; i < 10; i++ {
			certs, err = utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
			if err == nil {
				break
			}
			t.Logf("Failed to get certificates (attempt %d/10): %v", i+1, err)
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			t.Fatalf("Failed to get certificates list after HTTPS is ready: %v", err)
		}
		t.Log("Certificates list", certs)
		certURIs := gjson.Get(certs, `certificate-default-list.list-items.list-item.#.uriref`).Array()
		t.Log("Dnode Cert Url", certURIs)
		if len(certURIs) < 2 {
			t.Fatalf("Expected at least 2 certificates, found %d", len(certURIs))
		}
		cert0Url := fmt.Sprintf("https://localhost:8002%s?format=json", certURIs[0])
		cert1Url := fmt.Sprintf("https://localhost:8002%s?format=json", certURIs[1])
		command = fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, cert0Url)
		cert0Detail, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute and get first certificate: %v", err)
		}
		cert0Temporary := gjson.Get(cert0Detail, `certificate-default.temporary`).Bool()
		cert0HostName := gjson.Get(cert0Detail, `certificate-default.host-name`).String()

		command = fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, cert1Url)
		cert1Detail, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute and get second certificate: %v", err)
		}
		cert1Temporary := gjson.Get(cert1Detail, `certificate-default.temporary`).Bool()
		cert1HostName := gjson.Get(cert1Detail, `certificate-default.host-name`).String()
		if cert0Temporary || cert1Temporary {
			t.Logf("Certificate 0: %v, Certificate 1: %v", cert0Temporary, cert1Temporary)
			t.Fatalf("Certificate is temporary")
		}
		if !slices.Contains(hostnamesSlice, cert0HostName) || !slices.Contains(hostnamesSlice, cert1HostName) {
			t.Logf("Certificate 0: %v, Certificate 1: %v", cert0HostName, cert1HostName)
			t.Fatalf("Certificate host name is not in the list of hostnames")
		}
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())

}
