package e2e

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	coreV1 "k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

var verifyHugePages = flag.Bool("verifyHugePages", false, "Test hugePages configuration")

const (
	groupName       = "node"
	mlNamespace     = "default"
	mlContainerName = "marklogic-server"
)

var (
	replicas         = int32(1)
	logOutput        = "[OUTPUT]\n\tname loki\n\tmatch *\n\thost loki.loki.svc.cluster.local\n\tport 3100\n\tlabels job=fluent-bit\n\thttp_user admin\n\thttp_passwd admin"
	adminUsername    = "admin"
	adminPassword    = "Admin@8001"
	marklogiccluster = &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: mlNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        groupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled: true,
				Image:   "fluent/fluent-bit:3.2.5",
				Files: marklogicv1.LogFilesConfig{
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
	dashboardUID      = ""
	dataSourcePayload = `{
		"name": "Loki",
		"type": "loki",
		"url": "http://loki-gateway.loki.svc.cluster.local",
		"access": "proxy",
		"basicAuth": false
	}`
	dataSourceUID = ""
)

type DashboardResponse struct {
	UID    string `json:"uid"`
	Status string `json:"status"`
}
type DataSourceResponse struct {
	DataSource DataSource `json:"datasource"`
}
type DataSource struct {
	UID     string `json:"uid"`
	Message string `json:"message"`
}

func TestMarklogicCluster(t *testing.T) {
	feature := features.New("Marklogic Cluster Test").WithLabel("type", "cluster-test")

	// Setup Loki and Grafana to verify Logging for Operator
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Setting up Loki and Grafana")
		client := c.Client()
		err := utils.AddHelmRepo("grafana", "https://grafana.github.io/helm-charts")
		if err != nil {
			t.Fatalf("Failed to add helm repo: %v", err)
		}

		err = utils.InstallHelmChart("loki", "grafana/loki", "loki", "6.6.5", "loki.yaml")
		if err != nil {
			t.Fatalf("Failed to install loki helm chart: %v", err)
		}

		err = utils.InstallHelmChart("grafana", "grafana/grafana", "grafana", "8.3.2")
		if err != nil {
			t.Fatalf("Failed to install grafana helm chart: %v", err)
		}
		// Wait for Grafana pod to be ready
		time.Sleep(5 * time.Second) // Give some time for Grafana to start
		podList := &corev1.PodList{}
		if err := client.Resources().List(ctx, podList, func(lo *metav1.ListOptions) {
			lo.FieldSelector = "metadata.namespace=" + "grafana"
		}); err != nil {
			t.Fatal(err)
		}
		if len(podList.Items) == 0 {
			t.Fatal("No Grafana pods found")
		}
		grafanaPodName := podList.Items[0].Name
		err = utils.WaitForPod(ctx, t, client, "grafana", grafanaPodName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for grafana pod creation: %v", err)
		}

		// Get Grafana admin password
		grafanaAdminUser, grafanaAdminPassword, err := utils.GetSecretData(ctx, client, "grafana", "grafana", "admin-user", "admin-password")
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
		} else {
			t.Logf("Datasource created successfully: %s", output)
		}
		var dataSourceResponse DataSourceResponse
		if err := json.Unmarshal([]byte(output), &dataSourceResponse); err != nil {
			t.Fatalf("Failed to unmarshal JSON response: %v", err)
		}
		dataSourceUID = dataSourceResponse.DataSource.UID
		t.Logf("Datasource UID: %s", dataSourceUID)
		return ctx
	})

	// Setup for MarklogicCluster creation
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(mlNamespace).GetScheme())

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
		var marklogiccluster marklogicv1.MarklogicCluster
		if err := client.Resources().Get(ctx, "marklogicclusters", mlNamespace, &marklogiccluster); err != nil {
			t.Log("====MarklogicCluster not found====")
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "node-0"
		err := utils.WaitForPod(ctx, t, client, mlNamespace, podName, 240*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx

	})

	// Assessment to check for logging in MarkLogic Operator
	feature.Assess("Grafana Dashboard created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podList := &corev1.PodList{}
		if err := client.Resources().List(ctx, podList, func(lo *metav1.ListOptions) {
			lo.FieldSelector = "metadata.namespace=" + "grafana"
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second) // Wait for Grafana to be fully ready
		if len(podList.Items) == 0 {
			t.Fatal("No Grafana pods found")
		}
		grafanaPodName := podList.Items[0].Name
		grafanaAdminUser, grafanaAdminPassword, err := utils.GetSecretData(ctx, client, "grafana", "grafana", "admin-user", "admin-password")
		if err != nil {
			t.Fatalf("Failed to get Grafana admin user and password: %v", err)
		}
		time.Sleep(90 * time.Second)
		grafanaURL := "http://localhost:3000"
		url := fmt.Sprintf("%s/api/dashboards/db", grafanaURL)
		curlCommand := fmt.Sprintf(`curl -X POST %s -u %s:%s -H "Content-Type: application/json" -d '%s'`, url, grafanaAdminUser, grafanaAdminPassword, dashboardPayload)
		output, err := utils.ExecCmdInPod(grafanaPodName, "grafana", "grafana", curlCommand)
		if err != nil {
			t.Fatalf("Failed to execute kubectl command in grafana pod: %v", err)
		}
		var dashboardResponse DashboardResponse
		if err := json.Unmarshal([]byte(output), &dashboardResponse); err != nil {
			t.Fatalf("Failed to unmarshal JSON response: %v", err)
		}
		dashboardUID = dashboardResponse.UID
		if dashboardResponse.Status != "success" {
			t.Fatal("Failed to create dashboard with loki and fluent-bit")
		} else {
			t.Logf("Dashboard created successfully with UID: %s", dashboardResponse.UID)
		}

		// Create query to verify MarkLogic logs in Grafana
		payload := map[string]interface{}{
			"queries": []map[string]interface{}{
				{
					"refId":     "A",
					"expr":      "{job=\"fluent-bit\"} |= `Starting MarkLogic Server`",
					"queryType": "range",
					"datasource": map[string]string{
						"type": "loki",
						"uid":  dataSourceUID,
					},
					"editorMode":    "builder",
					"maxLines":      1000,
					"legendFormat":  "",
					"datasourceId":  1,
					"intervalMs":    20000,
					"maxDataPoints": 1073,
				},
			},
			"from": "now-1h",
			"to":   "now",
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("Failed to marshal payload: %v", err)
		}
		queryUrl := fmt.Sprintf("%s/api/ds/query?ds_type=loki", grafanaURL)
		curlCommand = fmt.Sprintf(`curl -X POST %s -u %s:%s -H "Content-Type: application/json" -d '%s'`, queryUrl, grafanaAdminUser, grafanaAdminPassword, payloadBytes)
		maxRetries := 5
		for attempt := 1; attempt <= maxRetries; attempt++ {
			t.Logf("Attempt %d to query datasource", attempt)
			output, err = utils.ExecCmdInPod(grafanaPodName, "grafana", "grafana", curlCommand)
			if err != nil {
				t.Logf("Attempt  %d/%d Failed to execute kubectl command in grafana pod: %v", attempt, 5, err)
				if attempt == maxRetries {
					t.Fatalf("failed to execute kubectl command after %d attempts: %v", maxRetries, err)
				}
				// Exponential backoff: 1s, 2s, 4s, 8s, 16s
				time.Sleep(time.Duration(1<<(attempt-1)) * time.Second)
			}
			t.Logf("Query datasource response: %s", output)
			// Verify MarkLogic logs in Grafana using Loki and Fluent Bit
			if strings.Contains(string(output), "Starting MarkLogic Server") {
				t.Logf("Successfully found MarkLogic logs on attempt %d", attempt)
			} else if attempt == maxRetries {
				t.Fatalf("Failed to find MarkLogic logs in Grafana after %d attempts", maxRetries)
			} else {
				t.Logf("MarkLogic logs not found, retrying...")
				time.Sleep(time.Duration(1<<(attempt-1)) * time.Second) // Exponential backoff
			}
		}

		curlCommand = fmt.Sprintf(`curl -u %s:%s %s/api/dashboards/uid/%s`, grafanaAdminUser, grafanaAdminPassword, grafanaURL, dashboardUID)
		output, err = utils.ExecCmdInPod(grafanaPodName, "grafana", "grafana", curlCommand)
		if err != nil {
			t.Fatalf("Failed to execute kubectl command in grafana pod: %v", err)
		}
		if !strings.Contains(string(output), "Fluent Bit Dashboard") {
			t.Fatal("Failed to associate Fluent Bit as filter in Grafana dashboard")
		}
		return ctx
	})
	// Run hugepages verification tests if verifyHugePages flag is set
	if *verifyHugePages {
		t.Log("Running HugePages verification tests")

		// Update the MarkLogic group resources
		feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			t.Log("Updating MarkLogic group resources")
			client := c.Client()
			var mlcluster marklogicv1.MarklogicCluster
			var resources = coreV1.ResourceRequirements{
				Requests: coreV1.ResourceList{
					"memory": resource.MustParse("8Gi"),
				},
				Limits: coreV1.ResourceList{
					"memory":        resource.MustParse("8Gi"),
					"hugepages-2Mi": resource.MustParse("1Gi"),
				},
			}
			if err := client.Resources().Get(ctx, "marklogicclusters", mlNamespace, &mlcluster); err != nil {
				t.Fatal(err)
			}

			mlcluster.Spec.MarkLogicGroups[0].Resources = &resources
			if err := client.Resources().Update(ctx, &mlcluster); err != nil {
				t.Log("Failed to update MarkLogic group resources")
				t.Fatal(err)
			}
			return ctx
		})

		// Assessment to verify the hugepages is configured
		feature.Assess("Verify Huge pages", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			podName := "node-0"
			containerName := "marklogic-server"
			cmd := fmt.Sprintf("cat /var/opt/MarkLogic/Logs/ErrorLog.txt")

			output, err := utils.ExecCmdInPod(podName, mlNamespace, containerName, cmd)
			if err != nil {
				t.Fatalf("Failed to execute kubectl command in pod: %v", err)
			}
			expectedOutput := "Linux Huge Pages: detected 1280"

			if !strings.Contains(string(output), expectedOutput) {
				t.Fatal("Huge Pages not configured for the MarLogic node")
			}
			return ctx
		})
	}

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(mlNamespace).Delete(ctx, marklogiccluster); err != nil {
			t.Fatalf("Failed to delete MarklogicCluster: %s", err)
		}
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
