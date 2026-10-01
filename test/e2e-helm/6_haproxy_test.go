// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
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

func cleanupHAProxyNamespaceArtifacts(ns string) {
	// Remove stale resources from interrupted runs so each test starts from a clean workload state.
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster --all --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete statefulset --all --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete deployment marklogic-haproxy --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete service marklogic-haproxy ml ml-cluster --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete pod --all --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete pvc --all --ignore-not-found=true --wait=false", ns))

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pods := strings.TrimSpace(e2eutils.RunCommand(fmt.Sprintf("kubectl get pods -n %s -o name --ignore-not-found=true 2>/dev/null", ns)).Result())
		sts := strings.TrimSpace(e2eutils.RunCommand(fmt.Sprintf("kubectl get statefulsets -n %s -o name --ignore-not-found=true 2>/dev/null", ns)).Result())
		if pods == "" && sts == "" {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// TestHAProxyPathBasedEnabled verifies that path-based HAProxy routing works correctly
// in a watched namespace with namespace-scoped RBAC.
func TestHAProxyPathBasedEnabled(t *testing.T) {
	trackTest(t)
	feature := features.New("HAProxy with Path-Based Routing Enabled").WithLabel("type", "haproxy-pathbased-enabled")
	haProxyPathNS := "ml-ns-haproxy-path" // must be in watchedNamespaces
	releaseName := "ml"
	haReplicas := int32(1)
	trueVal := true

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: haProxyPathNS,
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
					Replicas:    &haReplicas,
					IsBootstrap: true,
				},
			},
			HAProxy: &marklogicv1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &trueVal,
				FrontendPort:     8080,
				AppServers: []marklogicv1.AppServers{
					{Name: "app-service", Port: 8000, Path: "/console"},
					{Name: "admin", Port: 8001, Path: "/adminUI"},
					{Name: "manage", Port: 8002, Path: "/manage"},
				},
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, haProxyPathNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", haProxyPathNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", haProxyPathNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", haProxyPathNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}
		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: haProxyPathNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", haProxyPathNS, err)
		}
		cleanupHAProxyNamespaceArtifacts(haProxyPathNS)
		marklogicv1.AddToScheme(client.Resources(haProxyPathNS).GetScheme())
		e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster %s --ignore-not-found=true --wait=false", haProxyPathNS, cr.Name))
		if err := client.Resources(haProxyPathNS).Create(ctx, cr); err != nil {
			if apierrors.IsAlreadyExists(err) {
				e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster %s --ignore-not-found=true --wait=false", haProxyPathNS, cr.Name))
				time.Sleep(3 * time.Second)
				if retryErr := client.Resources(haProxyPathNS).Create(ctx, cr); retryErr != nil {
					t.Fatalf("Failed to create MarklogicCluster after replacing stale resource: %v", retryErr)
				}
			} else {
				t.Fatalf("Failed to create MarklogicCluster: %v", err)
			}
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
		if err := utils.WaitForPod(ctx, t, c.Client(), haProxyPathNS, "ml-0", 300*time.Second, true); err != nil {
			logDiagnostics(t, haProxyPathNS)
			t.Fatalf("ml-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		time.Sleep(5 * time.Second)
		verifyPathBasedHAProxyRoutes(t, haProxyPathNS, "ml-0", mlContainerName, adminUsername, adminPassword, cr.Spec.HAProxy.FrontendPort, cr.Spec.HAProxy.AppServers)
		return ctx
	})

	feature.Assess("HAProxy with path-based routing sets authentication to BASIC", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("ml-0.ml.%s.svc.cluster.local", haProxyPathNS)
		url := "http://" + fqdn + ":8001"
		cmd := fmt.Sprintf("curl -I %s", url)
		res, err := utils.ExecCmdInPod("ml-0", haProxyPathNS, mlContainerName, cmd)
		if err != nil {
			t.Fatalf("Failed to check authentication method: %v", err)
		}
		if !strings.Contains(res, "WWW-Authenticate: Basic") {
			t.Fatalf("Expected Basic auth header, got: %s", res)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Delete the actual MarklogicCluster created in Setup. The CR's metadata
		// name must match what was created (see cr.ObjectMeta above) — using a
		// different name results in a silent NotFound and the owned pods never
		// get garbage-collected, causing the wait below to time out.
		mlc := &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cr.Name,
				Namespace: haProxyPathNS,
			},
		}
		if err := c.Client().Resources().Delete(ctx, mlc); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("failed to delete MarklogicCluster %s/%s: %v", haProxyPathNS, mlc.Name, err)
		}

		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			current := &marklogicv1.MarklogicCluster{}
			err := c.Client().Resources().Get(ctx, mlc.Name, haProxyPathNS, current)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				t.Fatalf("failed waiting for MarklogicCluster %s/%s deletion: %v", haProxyPathNS, mlc.Name, err)
			}
			time.Sleep(5 * time.Second)
		}

		for time.Now().Before(deadline) {
			pod := &corev1.Pod{}
			err := c.Client().Resources().Get(ctx, "ml-0", haProxyPathNS, pod)
			if apierrors.IsNotFound(err) {
				return ctx
			}
			if err != nil {
				t.Fatalf("failed waiting for owned pod ml-0 deletion in namespace %s: %v", haProxyPathNS, err)
			}
			time.Sleep(5 * time.Second)
		}

		t.Fatalf("timed out waiting for MarklogicCluster owned resources to be removed in namespace %s", haProxyPathNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestHAProxyPathBasedDisabled verifies that HAProxy with path-based routing disabled
// works correctly in a watched namespace.
func TestHAProxyPathBasedDisabled(t *testing.T) {
	trackTest(t)
	feature := features.New("HAProxy with Path-Based Routing Disabled").WithLabel("type", "haproxy-pathbased-disabled")
	haProxyNS := "ml-ns-haproxy" // must be in watchedNamespaces
	releaseName := "ml"
	haReplicas := int32(1)
	falseVal := false

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: haProxyNS,
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
					Replicas:    &haReplicas,
					IsBootstrap: true,
				},
			},
			HAProxy: &marklogicv1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &falseVal,
				FrontendPort:     8090,
				AppServers: []marklogicv1.AppServers{
					{Name: "app-service", Port: 8000, Path: "/console"},
					{Name: "admin", Port: 8001, Path: "/adminUI"},
					{Name: "manage", Port: 8002, Path: "/manage"},
				},
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, haProxyNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", haProxyNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", haProxyNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", haProxyNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}
		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: haProxyNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", haProxyNS, err)
		}
		cleanupHAProxyNamespaceArtifacts(haProxyNS)
		marklogicv1.AddToScheme(client.Resources(haProxyNS).GetScheme())
		e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster %s --ignore-not-found=true --wait=false", haProxyNS, cr.Name))
		if err := client.Resources(haProxyNS).Create(ctx, cr); err != nil {
			if apierrors.IsAlreadyExists(err) {
				e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster %s --ignore-not-found=true --wait=false", haProxyNS, cr.Name))
				time.Sleep(3 * time.Second)
				if retryErr := client.Resources(haProxyNS).Create(ctx, cr); retryErr != nil {
					t.Fatalf("Failed to create MarklogicCluster after replacing stale resource: %v", retryErr)
				}
			} else {
				t.Fatalf("Failed to create MarklogicCluster: %v", err)
			}
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
		if err := utils.WaitForPod(ctx, t, c.Client(), haProxyNS, "ml-0", 300*time.Second, true); err != nil {
			logDiagnostics(t, haProxyNS)
			t.Fatalf("ml-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing disabled is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("marklogic-haproxy.%s.svc.cluster.local", haProxyNS)
		url := "http://" + fqdn + ":8001"
		cmd := fmt.Sprintf("curl --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		time.Sleep(5 * time.Second)
		if _, err := utils.ExecCmdInPod("ml-0", haProxyNS, mlContainerName, cmd); err != nil {
			t.Fatalf("HAProxy request failed: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing disabled keeps Digest authentication", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("ml-0.ml.%s.svc.cluster.local", haProxyNS)
		url := "http://" + fqdn + ":8001"
		cmd := fmt.Sprintf("curl -I %s", url)
		res, err := utils.ExecCmdInPod("ml-0", haProxyNS, mlContainerName, cmd)
		if err != nil {
			t.Fatalf("Failed to check authentication method: %v", err)
		}
		if !strings.Contains(res, "WWW-Authenticate: Digest") {
			t.Fatalf("Expected Digest auth header, got: %s", res)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Keep the watched namespace and RBAC in place, but clean workload resources
		// so re-runs start from a known-good state.
		t.Logf("Teardown: deleting MarklogicCluster %s/%s", haProxyNS, cr.Name)
		e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster %s --ignore-not-found=true --wait=false", haProxyNS, cr.Name))
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			current := &marklogicv1.MarklogicCluster{}
			err := c.Client().Resources(haProxyNS).Get(ctx, cr.Name, haProxyNS, current)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				t.Logf("Warning: failed waiting for MarklogicCluster deletion in %s: %v", haProxyNS, err)
				break
			}
			time.Sleep(5 * time.Second)
		}
		t.Logf("Teardown: cleaning HAProxy namespace artifacts in %s", haProxyNS)
		cleanupHAProxyNamespaceArtifacts(haProxyNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
