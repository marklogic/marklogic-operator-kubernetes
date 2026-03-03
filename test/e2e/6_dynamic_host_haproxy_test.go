package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestDynamicHostWithHAProxy(t *testing.T) {
	feature := features.New("Dynamic Host with HAProxy Test").WithLabel("type", "dynamic-host-haproxy")
	namespace := "dynamic-host-haproxy"
	releaseName := "ml"
	replicas := int32(1)
	dynamicHostReplicas := int32(2)
	trueVal := true

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
			HAProxy: &marklogicv1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &trueVal,
				FrontendPort:     8080,
				AppServers: []marklogicv1.AppServers{
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
			DynamicHost: &marklogicv1.DynamicHost{
				Enabled: true,
				Size:    dynamicHostReplicas,
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

	feature.Assess("Dynamic Host Pods created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		dynamicHostName := cr.GetObjectMeta().GetName() + "-dynamic"

		for i := 0; i < int(dynamicHostReplicas); i++ {
			podName := fmt.Sprintf("%s-%d", dynamicHostName, i)
			err := utils.WaitForPod(ctx, t, client, namespace, podName, 120*time.Second)
			if err != nil {
				t.Fatalf("Failed to wait for dynamic host pod %s creation: %v", podName, err)
			}
		}
		return ctx
	})

	feature.Assess("Verify HAProxy ConfigMap contains Dynamic Hosts", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		configMap := &corev1.ConfigMap{}

		err := client.Resources(namespace).Get(ctx, "marklogic-haproxy", namespace, configMap)
		if err != nil {
			t.Fatalf("Failed to get HAProxy ConfigMap: %v", err)
		}

		haproxyConfig := configMap.Data["haproxy.cfg"]
		if haproxyConfig == "" {
			t.Fatal("HAProxy config is empty")
		}

		dynamicHostName := cr.GetObjectMeta().GetName() + "-dynamic"
		t.Logf("Checking for dynamic hosts with name: %s", dynamicHostName)
		t.Logf("HAProxy config:\n%s", haproxyConfig)

		// Check that each dynamic host replica is in the config
		for i := 0; i < int(dynamicHostReplicas); i++ {
			// The backend server line should look like:
			// server marklogicclusters-dynamic-8000-0 marklogicclusters-dynamic-0.marklogicclusters-dynamic.dynamic-host-haproxy.svc.cluster.local:8000
			expectedServerName := fmt.Sprintf("%s-8000-%d", dynamicHostName, i)
			expectedFQDN := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local:8000", dynamicHostName, i, dynamicHostName, namespace)

			if !strings.Contains(haproxyConfig, expectedServerName) {
				t.Fatalf("HAProxy config does not contain expected server name for dynamic host %d: %s\nConfig:\n%s", i, expectedServerName, haproxyConfig)
			}

			if !strings.Contains(haproxyConfig, expectedFQDN) {
				t.Fatalf("HAProxy config does not contain expected FQDN for dynamic host %d: %s\nConfig:\n%s", i, expectedFQDN, haproxyConfig)
			}

			t.Logf("✓ Found dynamic host %d in HAProxy config", i)
		}

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
