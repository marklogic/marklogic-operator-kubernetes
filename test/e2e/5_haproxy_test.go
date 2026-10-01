// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"fmt"
	"path"
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

func verifyPathBasedHAProxyRoutes(t *testing.T, namespace, podName, containerName, username, password string, frontendPort int32, appServers []marklogicv1.AppServers) {
	t.Helper()
	fqdn := fmt.Sprintf("marklogic-haproxy.%s.svc.cluster.local", namespace)
	baseURL := fmt.Sprintf("http://%s:%d", fqdn, frontendPort)

	for _, appServer := range appServers {
		targetPort := appServer.TargetPort
		if targetPort == 0 {
			targetPort = appServer.Port
		}
		if targetPort == 8000 {
			continue
		}

		route := appServer.Path
		if targetPort == 8002 {
			route = path.Join(route, "manage/v2")
		}
		command := fmt.Sprintf("curl --fail --silent --show-error --anyauth -u %s:%s -o /dev/null %s%s", username, password, baseURL, route)
		if _, err := utils.ExecCmdInPod(podName, namespace, containerName, command); err != nil {
			t.Fatalf("HAProxy route %s to backend port %d failed: %v", appServer.Path, targetPort, err)
		}
	}

	for _, appServer := range appServers {
		targetPort := appServer.TargetPort
		if targetPort == 0 {
			targetPort = appServer.Port
		}
		if targetPort != 8000 {
			continue
		}

		consoleURL := baseURL + appServer.Path
		command := fmt.Sprintf(`set -eu
base=%q
origin=%q
auth=%q
cookies=/tmp/haproxy-qconsole-cookies
workspaces=/tmp/haproxy-qconsole-workspaces.json
result=/tmp/haproxy-qconsole-result.json
rm -f "$cookies" "$workspaces" "$result"
curl --fail --silent --show-error --anyauth -u "$auth" -H "Origin: $origin" -c "$cookies" "$base/qconsole" -o /dev/null
curl --fail --silent --show-error --anyauth -u "$auth" -H "Origin: $origin" -b "$cookies" -c "$cookies" "$base/qconsole/" -o /dev/null
curl --fail --silent --show-error --anyauth -u "$auth" -H "Origin: $origin" -b "$cookies" -c "$cookies" "$base/qconsole/endpoints/session.sjs" -o /dev/null
csrf=$(awk '$6 ~ /^csrf-token-/ { print $7; exit }' "$cookies")
test -n "$csrf"
curl --fail --silent --show-error --anyauth -u "$auth" -H "Origin: $origin" -H "X-CSRF-Token: $csrf" -b "$cookies" "$base/qconsole/endpoints/workspaces.xqy" -o "$workspaces"
query=$(tr -d '\r\n' < "$workspaces" | sed -n 's/.*"queries":[[:space:]]*\[\({[^}]*}\).*/\1/p')
qid=$(printf '%%s' "$query" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}.*/\1/p')
dbid=$(printf '%%s' "$query" | sed -n 's/.*"database"[[:space:]]*:[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}.*/\1/p')
sid=$(printf '%%s' "$query" | sed -n 's/.*"server"[[:space:]]*:[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}.*/\1/p')
test -n "$qid" && test -n "$dbid" && test -n "$sid"
curl --fail --silent --show-error --anyauth -u "$auth" -H "Origin: $origin" -H "X-CSRF-Token: $csrf" -b "$cookies" -X POST --data-urlencode 'data=xquery version "1.0-ml"; xdmp:version()' "$base/qconsole/endpoints/evaler.xqy?qid=$qid&dbid=$dbid&sid=$sid&crid=1234567890&querytype=xquery&action=eval" -o "$result"
result_json=$(tr -d '\r\n' < "$result")
printf '%%s' "$result_json" | grep -Eq '"resultCount"[[:space:]]*:[[:space:]]*1([,}])'
printf '%%s' "$result_json" | grep -Eq '"results"[[:space:]]*:[[:space:]]*\[[[:space:]]*\{'
printf '%%s' "$result_json" | grep -Eq '"result"[[:space:]]*:[[:space:]]*"[^"]+"'
echo 'xdmp:version() executed successfully'`, consoleURL, baseURL, username+":"+password)
		output, err := utils.ExecCmdInPod(podName, namespace, containerName, command)
		if err != nil {
			t.Fatalf("Query Console execution through %s failed: %v", appServer.Path, err)
		}
		t.Logf("Query Console through %s: %s", appServer.Path, strings.TrimSpace(output))
		return
	}

	t.Fatal("path-based HAProxy configuration does not include a Query Console backend on port 8000")
}

func TestHAPorxyPathBaseEnabled(t *testing.T) {
	trackTest(t)
	runTopLevelParallel(t)
	feature := features.New("HAProxy Test with Pathbased Routing Enabled").WithLabel("type", "haproxy-pathbased-enabled")
	namespace := "haproxy-pathbased"
	releaseName := "ml"
	replicas := int32(1)
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
		ensureMarklogicSchemeRegistered(t, c)

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
		return ctx
	})

	feature.Assess("HAProxy with PathBased Route is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		time.Sleep(5 * time.Second)
		verifyPathBasedHAProxyRoutes(t, namespace, "ml-0", mlContainerName, adminUsername, adminPassword, cr.Spec.HAProxy.FrontendPort, cr.Spec.HAProxy.AppServers)
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
			t.Fatalf("Failed to check authentication method is Basic: %v", res)
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
	trackTest(t)
	runTopLevelParallel(t)
	feature := features.New("HAProxy Test with Pathbased Routing Disabled").WithLabel("type", "haproxy-pathbased-disabled")
	namespace := "haproxy-test"
	releaseName := "ml"
	replicas := int32(1)
	falseVal := false

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
				PathBasedRouting: &falseVal,
				FrontendPort:     8090,
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
		ensureMarklogicSchemeRegistered(t, c)

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
		err := utils.WaitForPod(ctx, t, client, namespace, podName, 120*time.Second, true)
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
			t.Fatalf("Failed to check authentication method is Digest: %v", res)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, namespace)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
