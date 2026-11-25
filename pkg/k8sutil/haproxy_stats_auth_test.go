// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GehirnInc/crypt"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clusterWithStatsAuth builds a cluster whose HAProxy requires authentication for the stats page.
func clusterWithStatsAuth(t *testing.T, username, password string) *marklogicv1.MarklogicCluster {
	t.Helper()
	cr := newReloadHarness(t).cc.MarklogicCluster.DeepCopy()
	cr.UID = "cluster-uid-1"
	cr.Spec.HAProxy.Stats = marklogicv1.Stats{
		Enabled: true, Port: 1024,
		Auth: marklogicv1.StatsAuth{Enabled: true, Username: username, Password: password},
	}
	return cr
}

func render(cr *marklogicv1.MarklogicCluster) string {
	return generateHAProxyConfigMapData(context.Background(), cr)["haproxy.cfg"]
}

func TestStatsPasswordNeverAppearsInTheConfiguration(t *testing.T) {
	const password = "S3cret pass$word'\"!"
	cfg := render(clusterWithStatsAuth(t, "admin", password))
	for _, leaked := range []string{password, "S3cret", "stats auth"} {
		if strings.Contains(cfg, leaked) {
			t.Fatalf("the rendered configuration (which ends up in ConfigMaps and pods) contains %q:\n%s", leaked, cfg)
		}
	}
	for _, want := range []string{"userlist stats-users", "user admin password '$6$", "http_auth(stats-users)", `realm "HAProxy Statistics"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("configuration is missing %q:\n%s", want, cfg)
		}
	}
}

func TestStatsHashVerifiesTheOriginalPassword(t *testing.T) {
	for _, password := range []string{"admin", "with space", "p@$$w0rd'\"\\", "line\nbreak", strings.Repeat("long", 100)} {
		cr := clusterWithStatsAuth(t, "admin", password)
		hash, err := statsPasswordHash(cr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(hash, "$6$") {
			t.Fatalf("expected SHA-512 crypt, got %q", hash)
		}
		if err := crypt.SHA512.New().Verify(hash, []byte(password)); err != nil {
			t.Fatalf("hash does not verify its password %q: %v", password, err)
		}
		if err := crypt.SHA512.New().Verify(hash, []byte(password+"x")); err == nil {
			t.Fatalf("hash verifies a different password for %q", password)
		}
	}
}

func TestStatsHashIsStableSoReconcilesDoNotReload(t *testing.T) {
	a, b := clusterWithStatsAuth(t, "admin", "secret"), clusterWithStatsAuth(t, "admin", "secret")
	if render(a) != render(b) {
		t.Fatal("the same credentials must render identically, or every reconcile would trigger a reload")
	}
	if render(a) == render(clusterWithStatsAuth(t, "admin", "other")) {
		t.Fatal("a changed password must change the configuration so that it is reloaded")
	}
	other := clusterWithStatsAuth(t, "admin", "secret")
	other.UID = "another-cluster"
	hashA, _ := statsPasswordHash(a)
	hashB, _ := statsPasswordHash(other)
	if hashA == hashB {
		t.Fatal("different clusters must not share a salt")
	}
	byName := clusterWithStatsAuth(t, "admin", "secret")
	byName.UID = ""
	byName.ObjectMeta = metav1.ObjectMeta{Name: "c", Namespace: "n"}
	if h1, _ := statsPasswordHash(byName); h1 == "" {
		t.Fatal("a cluster without a UID (for example in tests) must still hash")
	}
}

func TestStatsCredentialsCannotInjectConfiguration(t *testing.T) {
	// The CR author controls these strings; none of them may add a line to the configuration.
	cfg := render(clusterWithStatsAuth(t, "admin", "x'\n  http-request allow\nbackend evil\n  server s 1.2.3.4:1"))
	for _, injected := range []string{"http-request allow", "backend evil", "1.2.3.4"} {
		if strings.Contains(cfg, injected) {
			t.Fatalf("a password injected %q into the configuration:\n%s", injected, cfg)
		}
	}
	for _, username := range []string{"", "a b", "admin\nhttp-request allow", "ad'min", "adm:in", `ad"min`, "ad$min", strings.Repeat("a", 65)} {
		cr := clusterWithStatsAuth(t, username, "secret")
		if err := validateHAProxyStatsAuth(cr); err == nil {
			t.Fatalf("user name %q must be rejected", username)
		}
		cfg := render(cr)
		if strings.Contains(cfg, "\n  user ") || strings.Contains(cfg, "http-request allow") {
			t.Fatalf("an invalid user name must not reach the configuration (%q):\n%s", username, cfg)
		}
		if !strings.Contains(cfg, "http_auth(stats-users)") {
			t.Fatalf("an invalid user name must leave the page locked, not open (%q)", username)
		}
	}
	for _, username := range []string{"admin", "stats.user", "ops_team-1", "a@example.com"} {
		if err := validateHAProxyStatsAuth(clusterWithStatsAuth(t, username, "secret")); err != nil {
			t.Fatalf("user name %q must be accepted: %v", username, err)
		}
	}
}

func TestStatsAuthValidationOnlyAppliesWhenEnabled(t *testing.T) {
	cr := clusterWithStatsAuth(t, "bad name", "")
	cr.Spec.HAProxy.Stats.Auth.Enabled = false
	if err := validateHAProxyStatsAuth(cr); err != nil {
		t.Fatalf("disabled authentication must not be validated: %v", err)
	}
	if cfg := render(cr); strings.Contains(cfg, "userlist") || strings.Contains(cfg, "http_auth") {
		t.Fatalf("no authentication was requested:\n%s", cfg)
	}
	cr.Spec.HAProxy.Stats.Auth.Enabled, cr.Spec.HAProxy.Stats.Enabled = true, false
	if err := validateHAProxyStatsAuth(cr); err != nil {
		t.Fatalf("a disabled stats page needs no credentials: %v", err)
	}
	if err := validateHAProxyStatsAuth(clusterWithStatsAuth(t, "admin", "")); err == nil ||
		!strings.Contains(err.Error(), "password") {
		t.Fatalf("an empty password is as good as none: %v", err)
	}
}

func TestInvalidStatsCredentialsFailTheReconcileWithAnExplanation(t *testing.T) {
	h := newReloadHarness(t)
	h.updateCluster(t, func(cr *marklogicv1.MarklogicCluster) {
		cr.Spec.HAProxy.Stats = marklogicv1.Stats{
			Enabled: true, Port: 1024, Auth: marklogicv1.StatsAuth{Enabled: true, Username: "bad user", Password: "x"},
		}
	})
	step := h.cc.ReconcileHAProxy()
	if !step.Completed() {
		t.Fatal("invalid credentials must stop the HAProxy reconcile")
	}
	if _, err := step.Output(); err == nil || !strings.Contains(err.Error(), "stats.auth.username") {
		t.Fatalf("the error must say what to fix: %v", err)
	}
	if status := h.status(t); status == nil || status.Phase != "Failed" || !strings.Contains(status.Message, "stats.auth.username") {
		t.Fatalf("the problem must be visible in the CR status: %+v", status)
	}
}

func TestHardStopAfterBoundsDrainingWithoutCuttingLongRequests(t *testing.T) {
	cr := newReloadHarness(t).cc.MarklogicCluster.DeepCopy()
	for _, tc := range []struct {
		client, server int32
		want           int
	}{
		{0, 0, 3600}, {60, 60, 3600}, {600, 600, 3600}, {1800, 600, 3600}, {3000, 600, 6000}, {600, 7200, 14400},
	} {
		cr.Spec.HAProxy.Timeout.Client, cr.Spec.HAProxy.Timeout.Server = tc.client, tc.server
		if got := haproxyHardStopAfter(cr); got != tc.want {
			t.Fatalf("timeouts %d/%d: got %d want %d", tc.client, tc.server, got, tc.want)
		}
	}
	cr.Spec.HAProxy.Timeout.Client, cr.Spec.HAProxy.Timeout.Server = 600, 600
	cfg := render(cr)
	if strings.Count(cfg, "hard-stop-after") != 1 {
		t.Fatalf("expected exactly one hard-stop-after:\n%s", cfg)
	}
	global := cfg[strings.Index(cfg, "global"):strings.Index(cfg, "defaults")]
	if !strings.Contains(global, "  hard-stop-after 3600s\n") {
		t.Fatalf("hard-stop-after belongs in the global section:\n%s", global)
	}
	if haproxyHardStopAfter(&marklogicv1.MarklogicCluster{}) != 3600 {
		t.Fatal("a cluster without HAProxy settings must not panic")
	}
}

func TestGeneratedConfigurationPassesRealHAProxyValidation(t *testing.T) {
	binary, err := exec.LookPath("haproxy")
	if err != nil {
		t.Skip("requires the haproxy binary")
	}
	for name, cr := range map[string]*marklogicv1.MarklogicCluster{
		"default":              newReloadHarness(t).cc.MarklogicCluster.DeepCopy(),
		"stats auth enabled":   clusterWithStatsAuth(t, "admin", "p@ss w0rd$x'\""),
		"stats auth, bad name": clusterWithStatsAuth(t, "bad name", "x"),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "haproxy.cfg")
			if err := os.WriteFile(file, []byte(render(cr)), 0600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command(binary, "-c", "-f", file).CombinedOutput(); err != nil {
				t.Fatalf("HAProxy rejects the generated configuration: %v\n%s\n--- configuration:\n%s", err, output, render(cr))
			}
		})
	}
}
