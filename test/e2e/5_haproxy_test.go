package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marklogic/marklogic-kubernetes-operator/test/utils"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestHAPorxyPathBaseEnabled(t *testing.T) {
	feature := features.New("HAProxy Test with Pathbased Routing Enabled").WithLabel("type", "haproxy-pathbased-enabled")
	namespace := "haproxy-pathbased"
	releaseName := "ml"
	replicas := int32(1)
	trueVal := true

	cr := &databasev1alpha1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.com/v1alpha1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: namespace,
		},
		Spec: databasev1alpha1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &databasev1alpha1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*databasev1alpha1.MarklogicGroups{
				{
					Name:        releaseName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			HAProxy: &databasev1alpha1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &trueVal,
				FrontendPort:     8080,
				AppServers: []databasev1alpha1.AppServers{
					{
						Name: "app-service",
						Port: 8000,
						Path: "/console",
					},
					{
						Name: "admin",
						Port: 8001,
						Path: "/adminUI",
					},
					{
						Name: "manage",
						Port: 8002,
						Path: "/manage",
					},
				},
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
		databasev1alpha1.AddToScheme(client.Resources(namespace).GetScheme())

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

	feature.Assess("HAProxy with PathBased Route is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "ml-0"
		fqdn := fmt.Sprintf("marklogic-haproxy.%s.svc.cluster.local", namespace)
		url := "http://" + fqdn + ":8080/adminUI"
		t.Log("URL for testing haproxy service: ", url)
		// curl command to check if haproxy is working for path based routing
		command := fmt.Sprintf("curl --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		time.Sleep(5 * time.Second)
		_, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute curl command to check haproxy service: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with PathBased Enabled Set Authentication to BASIC", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		svcName := "ml"
		podName := "ml-0"
		fqdn := fmt.Sprintf("%s.%s.%s.svc.cluster.local", podName, svcName, namespace)
		url := "http://" + fqdn + ":8001"
		t.Log("URL for testing authentication method: ", url)
		// curl command to check if haproxy is working for path based routing
		command := fmt.Sprintf("curl -I %s", url)
		res, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute curl command to check authentication method: %v", err)
		}
		if !strings.Contains(res, "WWW-Authenticate: Basic") {
			t.Fatalf("Failed to check authentication method is Basic: %v", err)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestHAPorxWithNoPathBasedDisabled(t *testing.T) {
	feature := features.New("HAProxy Test with Pathbased Routing Disabled").WithLabel("type", "haproxy-pathbased-disabled")
	namespace := "haproxy-test"
	releaseName := "ml"
	replicas := int32(1)
	falseVal := false

	cr := &databasev1alpha1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.com/v1alpha1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: namespace,
		},
		Spec: databasev1alpha1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &databasev1alpha1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*databasev1alpha1.MarklogicGroups{
				{
					Name:        releaseName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			HAProxy: &databasev1alpha1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &falseVal,
				FrontendPort:     8090,
				AppServers: []databasev1alpha1.AppServers{
					{
						Name: "app-service",
						Port: 8000,
						Path: "/console",
					},
					{
						Name: "admin",
						Port: 8001,
						Path: "/adminUI",
					},
					{
						Name: "manage",
						Port: 8002,
						Path: "/manage",
					},
				},
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
		databasev1alpha1.AddToScheme(client.Resources(namespace).GetScheme())

		if err := client.Resources(namespace).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created

		t.Logf("MarklogicCluster CR: %+v", cr.Spec.HAProxy)
		t.Logf("PathBasedRouting CR: %+v", *cr.Spec.HAProxy.PathBasedRouting)
		t.Logf("Enabled CR: %+v", cr.Spec.HAProxy.Enabled)

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

	feature.Assess("HAProxy with PathBased disabled is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "ml-0"
		fqdn := fmt.Sprintf("marklogic-haproxy.%s.svc.cluster.local", namespace)
		url := "http://" + fqdn + ":8001"
		t.Log("URL for haproxy: ", url)
		command := fmt.Sprintf("curl --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		time.Sleep(5 * time.Second)
		_, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute curl command in pod: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with PathBased Disabled Remain the Auth to Digest", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		svcName := "ml"
		podName := "ml-0"
		fqdn := fmt.Sprintf("%s.%s.%s.svc.cluster.local", podName, svcName, namespace)
		url := "http://" + fqdn + ":8001"
		t.Log("URL for testing authentication method: ", url)
		// curl command to check if haproxy is working for path based routing
		command := fmt.Sprintf("curl -I %s", url)
		res, err := utils.ExecCmdInPod(podName, namespace, mlContainerName, command)
		if err != nil {
			t.Fatalf("Failed to execute curl command to check authentication method: %v", err)
		}
		if !strings.Contains(res, "WWW-Authenticate: Digest") {
			t.Fatalf("Failed to check authentication method is Digest: %v", err)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
