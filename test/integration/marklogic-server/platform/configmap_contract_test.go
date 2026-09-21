// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package platform

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

func TestConfigMapFixtureContract(t *testing.T) {
	objects := configMapObjects("owned-namespace")
	if len(objects) != 2 {
		t.Fatalf("expected only a ConfigMap and a Pod, got %d", len(objects))
	}
	for _, object := range objects {
		metadata, err := meta.Accessor(object)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.GetNamespace() != "owned-namespace" {
			t.Fatal("fixture escaped the run namespace")
		}
	}
	config, ok := objects[0].(*corev1.ConfigMap)
	if !ok {
		t.Fatal("missing ConfigMap")
	}
	pod, ok := objects[1].(*corev1.Pod)
	if !ok {
		t.Fatal("missing reader Pod")
	}
	if _, ok := config.Data["missing"]; ok {
		t.Fatal("negative case must refer to an absent key")
	}
	if config.Data["message"] != "hello integration\n" {
		t.Fatal("positive case fixture changed")
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.Volumes) != 1 {
		t.Fatal("expected a single reader and ConfigMap volume")
	}
	reader := pod.Spec.Containers[0]
	volume := pod.Spec.Volumes[0]
	if volume.ConfigMap == nil || volume.ConfigMap.Name != config.Name {
		t.Fatal("reader references the wrong ConfigMap")
	}
	if len(reader.VolumeMounts) != 1 || reader.VolumeMounts[0].Name != volume.Name || reader.VolumeMounts[0].MountPath != "/config" || !reader.VolumeMounts[0].ReadOnly {
		t.Fatal("reader cannot read the expected mount")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("example must not mount cluster credentials")
	}
	if reader.Resources.Requests.Cpu().IsZero() || reader.Resources.Limits.Memory().IsZero() {
		t.Fatal("example must declare small resource bounds")
	}
	// Builders must be independent; one run cannot change the next run's inputs.
	config.Data["message"] = "modified"
	if reflect.DeepEqual(config.Data, configMapObjects("other")[0].(*corev1.ConfigMap).Data) {
		t.Fatal("fixture shares mutable data across runs")
	}
}
