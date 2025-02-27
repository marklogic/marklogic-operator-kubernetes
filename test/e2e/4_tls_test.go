package e2e

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
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
			APIVersion: "marklogic.com/v1alpha1",
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
				Name: namespace,
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
		err := utils.WaitForPod(ctx, t, client, namespace, podName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
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
			APIVersion: "marklogic.com/v1alpha1",
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
				Name: namespace,
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
		p := e2eutils.RunCommand("kubectl -n marklogic-tlsnamed create secret generic ca-cert --from-file=test/test_data/ca_cert/cacert.pem")
		if p.Err() != nil {
			t.Fatalf("Failed to create ca-cert secret: %s", p.Err())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsnamed create secret generic marklogic-0-cert --from-file=test/test_data/pod_zero_certs/tls.crt --from-file=test/test_data/pod_zero_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create marklogic-0-cert secret: %s", p.Err())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsnamed create secret generic marklogic-1-cert --from-file=test/test_data/pod_one_certs/tls.crt --from-file=test/test_data/pod_one_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create marklogic-1-cert secret: %s", p.Err())
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "marklogic-1"
		err := utils.WaitForPod(ctx, t, client, namespace, podName, 150*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
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
			APIVersion: "marklogic.com/v1alpha1",
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
		client.Resources(namespace).Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
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
		err := utils.GenerateCACertificate("test/test_data/ca_cert")
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
		p := e2eutils.RunCommand("kubectl -n marklogic-tlsednode create secret generic ca-cert --from-file=test/test_data/ca_cert/cacert.pem")
		if p.Err() != nil {
			t.Fatalf("Failed to create ca-cert secret: %s", p.Err())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsednode create secret generic dnode-0-cert --from-file=test/test_data/dnode_zero_certs/tls.crt --from-file=test/test_data/dnode_zero_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create marklogic-0-cert secret: %s", p.Err())
		}
		p = e2eutils.RunCommand("kubectl -n marklogic-tlsednode create secret generic enode-0-cert --from-file=test/test_data/enode_zero_certs/tls.crt --from-file=test/test_data/enode_zero_certs/tls.key")
		if p.Err() != nil {
			t.Fatalf("Failed to create marklogic-1-cert secret: %s", p.Err())
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "enode-0"
		err := utils.WaitForPod(ctx, t, client, namespace, podName, 180*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx
	})

	feature.Assess("Verify Named Cert on Multi Node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "dnode-0"
		hostnamesSlice := []string{"enode-0.enode.marklogic-tlsednode.svc.cluster.local", "dnode-0.dnode.marklogic-tlsednode.svc.cluster.local"}
		time.Sleep(10 * time.Second)
		url := "https://localhost:8002/manage/v2/certificates?format=json"
		command := fmt.Sprintf("curl -k --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		certs, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to get certificates list: %v", err)
		}
		certURIs := gjson.Get(certs, `certificate-default-list.list-items.list-item.#.uriref`).Array()
		t.Log("Dnode Cert Url", certURIs)
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
