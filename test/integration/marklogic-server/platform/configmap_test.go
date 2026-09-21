// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package platform

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const liveGate = "INTEGRATION_PLATFORM_SMOKE"

// TestPlatformConfigMapLifecycle is a teaching example, not product/PDC coverage.
func TestPlatformConfigMapLifecycle(t *testing.T) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(liveGate)), "true") {
		t.Skipf("set %s=true to run the Kubernetes teaching example", liveGate)
	}
	run := testutil.NewRun(t, "platform-smoke", false)
	run.ApplyObjects(t, configMapObjects(run.Namespace)...)
	run.Stage(t, "readiness")
	testutil.WaitForPodReady(t, run.Namespace, "config-reader", 2*time.Minute)
	run.LogImages(t)
	run.Stage(t, "verify_config")
	run.Case(t, "mounted_value", func(t *testing.T) {
		value := testutil.ExecuteInPod(t, run.Namespace, "config-reader", "reader", "cat", "/config/message")
		if value != "hello integration\n" {
			t.Fatalf("mounted value = %q, want fixture message", value)
		}
	})
	run.Case(t, "absent_key", func(t *testing.T) {
		// An exec failure must fail the test; only a successful explicit absence
		// assertion is accepted. No HTTP or OAuth infrastructure is needed.
		testutil.ExecuteInPod(t, run.Namespace, "config-reader", "reader", "sh", "-ec", "test ! -e /config/missing")
	})
}

// Keep a one-scenario builder local; extract a shared fixture when reused.
func configMapObjects(namespace string) []runtime.Object {
	no := false
	return []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "example-config", Namespace: namespace},
			Data:       map[string]string{"message": "hello integration\n"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "config-reader", Namespace: namespace},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &no,
				RestartPolicy:                corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name: "reader", Image: "busybox:1.37.0", Command: []string{"sleep", "3600"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "example-config"}}}}},
			},
		},
	}
}
