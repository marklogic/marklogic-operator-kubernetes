// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"sigs.k8s.io/yaml"
)

func validCluster(t *testing.T) *marklogicv1.MarklogicCluster {
	t.Helper()
	cr := newReloadHarness(t).cc.MarklogicCluster.DeepCopy()
	cr.Spec.ClusterDomain = "cluster.local"
	cr.Spec.HAProxy.Stats = marklogicv1.Stats{Enabled: true, Port: 1024}
	cr.Spec.HAProxy.AppServers = []marklogicv1.AppServers{{Name: "manage", Port: 8002, TargetPort: 8002, Path: "/manage"}}
	return cr
}

func TestHAProxySpecAcceptsValidValues(t *testing.T) {
	cr := validCluster(t)
	cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: "my-tls.example", CertFileName: "tls_cert-1.pem"}
	cr.Spec.HAProxy.TcpPorts = &marklogicv1.Tcpports{Enabled: true, Ports: []marklogicv1.TcpPort{{Port: 5432, Name: "odbc"}}}
	cr.Spec.HAProxy.AppServers = append(cr.Spec.HAProxy.AppServers,
		marklogicv1.AppServers{Port: 8000, Path: "/console"}, marklogicv1.AppServers{Port: 8001, Path: "/adminUI/v2.1_x~y"})
	if err := validateHAProxySpec(cr); err != nil {
		t.Fatalf("valid values were rejected: %v", err)
	}
	if err := validateHAProxySpec(&marklogicv1.MarklogicCluster{}); err != nil {
		t.Fatalf("a cluster without HAProxy settings has nothing to validate: %v", err)
	}
}

func TestHAProxySpecRejectsEveryValueThatCouldCorruptTheConfiguration(t *testing.T) {
	hostile := []string{"new\nline", "with space", `quote"`, "single'", "$(cmd)", "semi;colon", "back\\slash", "hash#", "{{ .Template }}", "tab\there"}
	group := func(cr *marklogicv1.MarklogicCluster) *marklogicv1.MarklogicGroups { return cr.Spec.MarkLogicGroups[0] }
	cases := map[string]func(*marklogicv1.MarklogicCluster, string){
		"spec.haproxy.appServers[0].path": func(cr *marklogicv1.MarklogicCluster, v string) { cr.Spec.HAProxy.AppServers[0].Path = "/" + v },
		"spec.markLogicGroups[0].haproxy.appServers[0].path": func(cr *marklogicv1.MarklogicCluster, v string) {
			group(cr).HAProxy = &marklogicv1.HAProxyGroup{AppServers: []marklogicv1.AppServers{{Port: 8002, Path: "/" + v}}}
		},
		"spec.markLogicGroups[0].name": func(cr *marklogicv1.MarklogicCluster, v string) { group(cr).Name = v },
		"spec.clusterDomain":           func(cr *marklogicv1.MarklogicCluster, v string) { cr.Spec.ClusterDomain = v },
		"spec.haproxy.tls.secretName": func(cr *marklogicv1.MarklogicCluster, v string) {
			cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: v, CertFileName: "ok.pem"}
		},
		"spec.haproxy.tls.certFileName": func(cr *marklogicv1.MarklogicCluster, v string) {
			cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: "ok", CertFileName: v}
		},
	}
	for path, set := range cases {
		for _, value := range hostile {
			t.Run(path+"/"+strings.NewReplacer("\n", "\\n", "\t", "\\t").Replace(value), func(t *testing.T) {
				cr := validCluster(t)
				set(cr, value)
				err := validateHAProxySpec(cr)
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("%q must be rejected with its path %s: %v", value, path, err)
				}
			})
		}
	}
	// Tokens that are not injection but still cannot be a valid name or path.
	for name, mutate := range map[string]func(*marklogicv1.MarklogicCluster){
		"path without a leading slash": func(cr *marklogicv1.MarklogicCluster) { cr.Spec.HAProxy.AppServers[0].Path = "manage" },
		"path longer than 128": func(cr *marklogicv1.MarklogicCluster) {
			cr.Spec.HAProxy.AppServers[0].Path = "/" + strings.Repeat("a", 128)
		},
		"upper-case group name":          func(cr *marklogicv1.MarklogicCluster) { group(cr).Name = "Group" },
		"group name longer than a label": func(cr *marklogicv1.MarklogicCluster) { group(cr).Name = strings.Repeat("a", 64) },
		"cluster domain with underscore": func(cr *marklogicv1.MarklogicCluster) { cr.Spec.ClusterDomain = "cluster_local" },
		"cert file name is a directory": func(cr *marklogicv1.MarklogicCluster) {
			cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: "s", CertFileName: ".."}
		},
		"cert file name with a slash": func(cr *marklogicv1.MarklogicCluster) {
			cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: "s", CertFileName: "a/b.pem"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cr := validCluster(t)
			mutate(cr)
			if err := validateHAProxySpec(cr); err == nil {
				t.Fatal("must be rejected")
			}
		})
	}
}

func TestHAProxySpecChecksNumbersAndReportsEveryProblemOnce(t *testing.T) {
	cr := validCluster(t)
	cr.Spec.HAProxy.Stats.Port = -1
	cr.Spec.HAProxy.AppServers = []marklogicv1.AppServers{{Port: 70000}, {Port: 8002, TargetPort: -3}}
	cr.Spec.HAProxy.Timeout = marklogicv1.Timeout{Client: -5, Connect: 5, Server: maxHAProxyTimeoutSeconds + 1}
	cr.Spec.HAProxy.TcpPorts = &marklogicv1.Tcpports{Enabled: true, Ports: []marklogicv1.TcpPort{{Port: 0}}}
	cr.Spec.ClusterDomain = "bad domain"
	err := validateHAProxySpec(cr)
	if err == nil {
		t.Fatal("must be rejected")
	}
	for _, want := range []string{
		"spec.haproxy.stats.port", "spec.haproxy.appServers[0].port", "spec.haproxy.appServers[1].targetPort",
		"spec.haproxy.timeout.client", "spec.haproxy.timeout.server", "spec.haproxy.tcpPorts.ports[0].port", "spec.clusterDomain",
	} {
		if strings.Count(err.Error(), want) != 1 {
			t.Errorf("expected exactly one report for %s in: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "spec.haproxy.timeout.connect") {
		t.Errorf("a valid value must not be reported: %v", err)
	}
	again := validateHAProxySpec(cr)
	if again == nil || again.Error() != err.Error() {
		t.Fatal("the message must be identical on every call, or the status would change on every reconcile")
	}
	// Disabled features are not validated, so existing clusters that never used them are unaffected.
	cr.Spec.HAProxy.Stats.Enabled = false
	cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: false, SecretName: "bad name"}
	if err := validateHAProxySpec(cr); err != nil && (strings.Contains(err.Error(), "stats.port") || strings.Contains(err.Error(), "tls")) {
		t.Fatalf("disabled stats and TLS must not be validated: %v", err)
	}
}

func TestInvalidHAProxySpecStopsTheReconcileBeforeAnythingIsRendered(t *testing.T) {
	h := newReloadHarness(t)
	h.updateCluster(t, func(cr *marklogicv1.MarklogicCluster) {
		cr.Spec.HAProxy.AppServers = []marklogicv1.AppServers{{Name: "manage", Port: 8002, Path: "/ok\n  http-request allow"}}
	})
	before := h.getConfigMap(t, "marklogic-haproxy").Data
	step := h.cc.ReconcileHAProxy()
	if !step.Completed() {
		t.Fatal("an unsafe value must stop the reconcile")
	}
	if _, err := step.Output(); err == nil || !strings.Contains(err.Error(), "spec.haproxy.appServers[0].path") {
		t.Fatalf("the error must name the field: %v", err)
	}
	if status := h.status(t); status == nil || status.Phase != "Failed" || !strings.Contains(status.Message, "appServers[0].path") {
		t.Fatalf("the problem must be visible in the CR status: %+v", status)
	}
	after := h.getConfigMap(t, "marklogic-haproxy").Data
	if before["haproxy.cfg"] != after["haproxy.cfg"] || strings.Contains(after["haproxy.cfg"], "http-request allow") {
		t.Fatal("an unsafe value reached the committed configuration")
	}
	if candidate := h.getConfigMap(t, haproxyCandidateConfigMap); strings.Contains(candidate.Data["haproxy.cfg"], "http-request allow") {
		t.Fatal("an unsafe value reached the candidate")
	}
}

func TestEveryShippedSampleStillPassesValidation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no samples found: %v", err)
	}
	checked := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, document := range strings.Split(string(data), "\n---") {
			cr := &marklogicv1.MarklogicCluster{}
			if err := yaml.Unmarshal([]byte(document), cr); err != nil || cr.Kind != "MarklogicCluster" || cr.Spec.HAProxy == nil || !cr.Spec.HAProxy.Enabled {
				continue
			}
			checked++
			if err := validateHAProxySpec(cr); err != nil {
				t.Errorf("%s (document %d) is a shipped example but is now rejected: %v", filepath.Base(file), i, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no sample with HAProxy enabled was found, so this test proves nothing")
	}
	t.Logf("validated %d sample clusters with HAProxy enabled", checked)
}
