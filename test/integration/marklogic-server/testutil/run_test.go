// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func ownedNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "scenario-abc", UID: "original", ResourceVersion: "12", Labels: map[string]string{runLabel: "run-1"}}}
}

func TestCleanupRefusesForeignNamespace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		uid   types.UID
		owner string
	}{{"replaced UID", "replacement", "run-1"}, {"changed ownership", "original", "run-2"}, {"missing ownership", "original", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			ns := ownedNamespace()
			ns.UID = tc.uid
			ns.Labels[runLabel] = tc.owner
			client := fake.NewClientset(ns)
			run := &Run{Namespace: ns.Name, ID: "run-1", uid: "original", client: client}
			err := run.cleanup(context.Background())
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("want ownership rejection, got %v", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "delete" {
					t.Fatal("cleanup attempted deletion")
				}
			}
		})
	}
}

func TestCleanupUsesUIDAndResourceVersionPreconditions(t *testing.T) {
	ns := ownedNamespace()
	client := fake.NewClientset(ns)
	checked := false
	client.PrependReactor("delete", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
		options := action.(ktesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != ns.UID || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != ns.ResourceVersion {
			t.Fatalf("unsafe deletion options: %#v", options)
		}
		checked = true
		return false, nil, nil
	})
	run := &Run{Namespace: ns.Name, ID: "run-1", uid: ns.UID, client: client}
	if err := run.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("namespace was not deleted")
	}
	// Repeated cleanup of an already absent namespace is harmless.
	if err := run.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupTracksVolumesAndHonorsDeadline(t *testing.T) {
	for _, policy := range []corev1.PersistentVolumeReclaimPolicy{corev1.PersistentVolumeReclaimDelete, corev1.PersistentVolumeReclaimRetain} {
		t.Run(string(policy), func(t *testing.T) {
			ns := ownedNamespace()
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name, UID: "claim"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "disk"}}
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "disk", UID: "volume"}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, PersistentVolumeReclaimPolicy: policy}}
			client := fake.NewClientset(ns, pvc, pv)
			run := &Run{Namespace: ns.Name, ID: "run-1", uid: ns.UID, client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err := run.cleanup(ctx)
			if err == nil {
				t.Fatal("cleanup passed with a remaining volume")
			}
			if policy == corev1.PersistentVolumeReclaimRetain {
				if !strings.Contains(err.Error(), "Retain") {
					t.Fatalf("unexpected error: %v", err)
				}
				if _, err := client.CoreV1().Namespaces().Get(context.Background(), ns.Name, metav1.GetOptions{}); err != nil {
					t.Fatal("namespace should be retained")
				}
			} else if ctx.Err() == nil {
				t.Fatalf("expected bounded wait for PV deletion, got %v", err)
			}
		})
	}
}
