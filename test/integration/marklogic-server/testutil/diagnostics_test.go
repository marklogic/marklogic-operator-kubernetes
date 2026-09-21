// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestDiagnosticsCollectsPreviousLogsWithoutPersistingPodSecrets(t *testing.T) {
	const secret = "fixture-sensitive-value"
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": secret}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Env: []corev1.EnvVar{{Name: "PASSWORD", Value: secret}}, Args: []string{secret}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "server", Image: "example:1", ImageID: "sha256:abc", RestartCount: 1, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: secret}}}}}}
	var mu sync.Mutex
	var current, previous bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/test/pods":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{pod}})
		case "/api/v1/namespaces/test/events":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}, Items: []corev1.Event{{Reason: "BackOff", Message: "retry password=" + secret}}})
		case "/api/v1/namespaces/test/pods/app/log":
			if r.URL.Query().Get("container") != "server" || r.URL.Query().Get("limitBytes") != "65536" || r.URL.Query().Get("tailLines") != "200" {
				t.Error("log request lost bounds or container selection")
			}
			mu.Lock()
			if r.URL.Query().Get("previous") == "true" {
				previous = true
			} else {
				current = true
			}
			mu.Unlock()
			w.Write([]byte("starting container\nAuthorization: Bearer dynamic-token\npassword=" + secret + "\n"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("INTEGRATION_RESULTS_DIR", t.TempDir())
	report, err := newRunReport("id", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	report.redactor.add(secret)
	run := &Run{Namespace: "test", client: client, report: report}
	run.collectDiagnostics(t)
	mu.Lock()
	gotCurrent, gotPrevious := current, previous
	mu.Unlock()
	if !gotCurrent || !gotPrevious {
		t.Fatal("did not collect both current and previous logs")
	}
	for _, name := range []string{"diagnostics.json", "app-server.log", "app-server-previous.log"} {
		contents, err := os.ReadFile(filepath.Join(report.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{secret, "dynamic-token", "last-applied-configuration"} {
			if strings.Contains(string(contents), forbidden) {
				t.Errorf("%s contains sensitive input", name)
			}
		}
	}
	contents, err := os.ReadFile(filepath.Join(report.dir, "diagnostics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result diagnostics
	if err := json.Unmarshal(contents, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) > 0 || len(result.Logs) != 2 || result.Pods[0].Containers[0].Reason != "CrashLoopBackOff" {
		t.Fatalf("diagnostics incomplete: %#v", result)
	}
}
