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

const (
	groupName   = "node"
	mlNamespace = "default"
)

var (
	replicas         = int32(1)
	logOutput        = "[OUTPUT]\nname loki\nmatch *\nhost loki.loki.svc.cluster.local\nport 3100\nlabels job=fluent-bit\nhttp_user admin\nhttp_passwd admin"
	marklogiccluster = &databasev1alpha1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.com/v1alpha1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: mlNamespace,
		},
		Spec: databasev1alpha1.MarklogicClusterSpec{
			Image: marklogicImage,
			MarkLogicGroups: []*databasev1alpha1.MarklogicGroups{
				{
					Name:        groupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			LogCollection: &databasev1alpha1.LogCollection{
				Enabled: true,
				Image:   "fluent/fluent-bit:3.1.1",
				Files: databasev1alpha1.LogFilesConfig{
					ErrorLogs:   true,
					AccessLogs:  true,
					RequestLogs: true,
					CrashLogs:   true,
					AuditLogs:   true,
				},
				Outputs: logOutput,
			},
		},
	}
	dashboardPayload = `{
		"dashboard": {
			"panels": [
				{
					"type": "graph",
					"title": "Fluent Bit Logs",
					"targets": [
						{
							"expr": "rate({job=\"fluent-bit\"}[5m])",
							"legendFormat": "{{job}}"
						}
					]
				}
			],
			"title": "Fluent Bit Dashboard"
		},
			"overwrite": true
	}`
	dataSourcePayload = `{"name": "Loki","type": "loki","url": "http://loki-gateway.loki.svc.cluster.local", "access": "proxy","basicAuth": false}`
)

func TestMarklogicCluster(t *testing.T) {
	feature := features.New("MarklogicCluster Resource")

	// Setup Loki and Grafana to verify Logging for Operator
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Setting up Loki and Grafana")
		client := c.Client()
		err := utils.AddHelmRepo("grafana", "https://grafana.github.io/helm-charts")
		if err != nil {
			t.Fatalf("Failed to add helm repo: %v", err)
		}

		err = utils.InstallHelmChart("grafana", "grafana/grafana", "grafana", "8.3.2")
		if err != nil {
			t.Fatalf("Failed to install grafana helm chart: %v", err)
		}

		err = utils.InstallHelmChart("loki", "grafana/loki", "loki", "6.6.5", "loki.yaml")
		if err != nil {
			t.Fatalf("Failed to install loki helm chart: %v", err)
		}

		podList := &corev1.PodList{}
		if err := client.Resources().List(ctx, podList, func(lo *metav1.ListOptions) {
			lo.FieldSelector = "metadata.namespace=" + "grafana"
		}); err != nil {
			t.Fatal(err)
		}

		grafanaPodName := podList.Items[0].Name
		err = utils.WaitForPod(ctx, t, client, "grafana", grafanaPodName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for grafana pod creation: %v", err)
		}

		// Get Grafana admin password
		grafanaAdminUser, grafanaAdminPassword, err := utils.GetSecretData(ctx, client, "grafana", "grafana", "admin-username", "admin-password")
		if err != nil {
			t.Fatalf("Failed to get Grafana admin user and password: %v", err)
		}

		//Check Grafana Health before creating datasource
		start := time.Now()
		timeout := 2 * time.Minute
		grafanaURL := "http://localhost:3000"
		for {
			if time.Since(start) > timeout {
				t.Fatalf("Grafana is not ready after %v", timeout)
			}
			curlCommand := fmt.Sprintf(`curl -s -o /dev/null -w "%%{http_code}" %s/api/health`, grafanaURL)
			output, err := utils.ExecCmdInPod(grafanaPodName, "grafana", "grafana", curlCommand)
			if err != nil {
				t.Logf("Grafana is not ready yet...an error occurred: %v", err)
			}
			if output == "200" {
				t.Log("Grafana is ready")
				break
			}
			time.Sleep(5 * time.Second)
		}

		// Create datasource for Grafana
		url := fmt.Sprintf("%s/api/datasources", grafanaURL)
		curlCommand := fmt.Sprintf(`curl -X POST %s -u %s:%s -H "Content-Type: application/json" -d '%s'`, url, grafanaAdminUser, grafanaAdminPassword, dataSourcePayload)
		output, err := utils.ExecCmdInPod(grafanaPodName, "grafana", "grafana", curlCommand)
		if err != nil {
			t.Fatalf("Failed to execute kubectl command grafana in pod: %v", err)
		}
		if !(strings.Contains(string(output), "Datasource added") && strings.Contains(string(output), "Loki")) {
			t.Fatal("Failed to create datasource for Grafana")
		}

		url = fmt.Sprintf("%s/api/dashboards/db", grafanaURL)
		curlCommand = fmt.Sprintf(`curl -X POST %s -u %s:%s -H "Content-Type: application/json" -d '%s'`, url, grafanaAdminUser, grafanaAdminPassword, dashboardPayload)
		output, err = utils.ExecCmdInPod(grafanaPodName, "grafana", "grafana", curlCommand)
		if err != nil {
			t.Fatalf("Failed to execute kubectl command in grafana pod: %v", err)
		}
		if !strings.Contains(string(output), `"status":"success"`) {
			t.Fatal("Failed to create dashboard with loki and fluent-bit")
		}
		return ctx
	})

	// Setup for MarklogicCluster creation
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		databasev1alpha1.AddToScheme(client.Resources(mlNamespace).GetScheme())

		if err := client.Resources(mlNamespace).Create(ctx, marklogiccluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(marklogiccluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Assessment to check for MarklogicCluster deployment
	feature.Assess("MarklogicCluster deployed Ok", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var marklogiccluster databasev1alpha1.MarklogicCluster
		if err := client.Resources().Get(ctx, "marklogicclusters", mlNamespace, &marklogiccluster); err != nil {
			t.Log("====MarklogicCluster not found====")
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "node-0"
		err := utils.WaitForPod(ctx, t, client, mlNamespace, podName, 90*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx

	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "grafana"}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "loki"}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}
