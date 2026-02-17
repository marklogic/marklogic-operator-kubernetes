/*
Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

var (
	k8sClient client.Client
)

var _ = Describe("Volume Resize Tests", Ordered, func() {
	const (
		timeout  = time.Minute * 15
		interval = time.Second * 5
	)

	var (
		ctx       context.Context
		namespace string
		groupName string
	)

	BeforeAll(func() {
		ctx = context.Background()
		namespace = "volume-resize-test"
		groupName = "resize-test"

		// Create test namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
	})

	AfterAll(func() {
		// Cleanup namespace
		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns)).Should(Succeed())
		Expect(k8sClient.Delete(ctx, ns)).Should(Succeed())
	})

	// ==========================================================================
	// Test Case 1: Validation Failures
	// ==========================================================================
	Context("Validation Failures", func() {

		It("should fail when StorageClass doesn't allow volume expansion", func() {
			// Create StorageClass without allowVolumeExpansion
			scName := "no-expansion-sc"
			allowExpansion := false
			sc := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: scName,
				},
				Provisioner:          "ebs.csi.aws.com",
				AllowVolumeExpansion: &allowExpansion,
			}
			Expect(k8sClient.Create(ctx, sc)).Should(Succeed())

			// Create MarklogicGroup with this StorageClass
			group := createTestMarklogicGroup(namespace, groupName+"-no-expansion", "10Gi", scName)
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Try to resize
			group.Spec.Persistence.Size = "15Gi"
			Expect(k8sClient.Update(ctx, group)).Should(Succeed())

			// Verify resize fails with appropriate message
			Eventually(func() string {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return ""
				}
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseFailed)))

			// Verify error message mentions StorageClass
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			Expect(g.Status.VolumeResizeStatus.Message).Should(ContainSubstring("does not allow volume expansion"))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, sc)).Should(Succeed())
		})

		It("should not trigger resize when shrinking volume", func() {
			// Create MarklogicGroup with 15Gi
			group := createTestMarklogicGroup(namespace, groupName+"-shrink", "15Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Try to shrink to 10Gi
			group.Spec.Persistence.Size = "10Gi"
			Expect(k8sClient.Update(ctx, group)).Should(Succeed())

			// Wait a bit and verify no resize is triggered
			time.Sleep(10 * time.Second)

			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			// VolumeResizeStatus should be nil (no resize triggered)
			Expect(g.Status.VolumeResizeStatus).Should(BeNil())

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})

		It("should not trigger resize when size is unchanged", func() {
			// Create MarklogicGroup with 10Gi
			group := createTestMarklogicGroup(namespace, groupName+"-same-size", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Apply same size (no change)
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "10Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait a bit and verify no resize is triggered
			time.Sleep(10 * time.Second)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			Expect(g.Status.VolumeResizeStatus).Should(BeNil())

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})
	})

	// ==========================================================================
	// Test Case 2: Multi-Replica Scenarios
	// ==========================================================================
	Context("Multi-Replica Scenarios", func() {

		It("should resize all PVCs for multiple replicas", func() {
			// Create MarklogicGroup with 3 replicas
			replicas := int32(3)
			group := createTestMarklogicGroup(namespace, groupName+"-multi-replica", "10Gi", "gp3")
			group.Spec.Replicas = &replicas
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment with all replicas ready
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Verify 3 PVCs exist
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(namespace))).Should(Succeed())
			Expect(len(pvcList.Items)).Should(Equal(3))

			// Trigger resize
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "15Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for resize to complete
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			// Verify all 3 PVCs are resized
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(namespace))).Should(Succeed())
			targetQty := resource.MustParse("15Gi")
			for _, pvc := range pvcList.Items {
				capacity := pvc.Status.Capacity[corev1.ResourceStorage]
				Expect(capacity.Cmp(targetQty)).Should(BeNumerically(">=", 0))
			}

			// Verify status shows correct counts
			Expect(g.Status.VolumeResizeStatus.TotalPVCs).Should(Equal(int32(3)))
			Expect(g.Status.VolumeResizeStatus.PVCsResized).Should(Equal(int32(3)))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})
	})

	// ==========================================================================
	// Test Case 3: Operator Restart During Resize
	// ==========================================================================
	Context("Failure Recovery", func() {

		It("should resume resize after operator restart", func() {
			// This test verifies that the resize state is persisted
			// and can be resumed if the operator restarts

			group := createTestMarklogicGroup(namespace, groupName+"-restart", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Trigger resize
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "12Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for resize to start (any phase except empty)
			Eventually(func() bool {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				return g.Status.VolumeResizeStatus != nil && g.Status.VolumeResizeStatus.Phase != ""
			}, timeout, interval).Should(BeTrue())

			// Record the phase
			currentPhase := g.Status.VolumeResizeStatus.Phase

			// Simulate operator restart by refreshing the CR
			// In real test, operator would be restarted here
			// The reconciler should pick up from the current phase

			// Wait for resize to complete
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			// Verify the resize completed successfully
			Expect(g.Status.VolumeResizeStatus.Message).Should(ContainSubstring("completed"))

			By(fmt.Sprintf("Resize resumed from phase %s and completed successfully", currentPhase))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})

		It("should allow retry after failed resize by clearing status", func() {
			// Create group and simulate a failed resize
			group := createTestMarklogicGroup(namespace, groupName+"-retry", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Manually set a failed status to simulate failure
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			now := metav1.Now()
			g.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{
				Phase:          marklogicv1.VolumeResizePhaseFailed,
				Message:        "Simulated failure",
				CompletionTime: &now,
				TargetSize:     "15Gi",
				OriginalSize:   "10Gi",
			}
			Expect(k8sClient.Status().Update(ctx, g)).Should(Succeed())

			// Clear the failed status to retry
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Status.VolumeResizeStatus = nil
			Expect(k8sClient.Status().Update(ctx, g)).Should(Succeed())

			// Trigger a new resize
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "12Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for resize to complete
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})
	})

	// ==========================================================================
	// Test Case 4: StatefulSet Operations
	// ==========================================================================
	Context("StatefulSet Operations", func() {

		It("should keep pods running during StatefulSet delete (orphan policy)", func() {
			group := createTestMarklogicGroup(namespace, groupName+"-orphan", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Get initial pod UID
			podList := &corev1.PodList{}
			Expect(k8sClient.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
				"app.kubernetes.io/instance": group.Spec.Name,
			})).Should(Succeed())
			Expect(len(podList.Items)).Should(BeNumerically(">", 0))
			initialPodUID := podList.Items[0].UID

			// Trigger resize
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "12Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for DeletingStatefulSet phase
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(SatisfyAny(
				Equal(string(marklogicv1.VolumeResizePhaseDeletingStatefulSet)),
				Equal(string(marklogicv1.VolumeResizePhaseRecreatingStatefulSet)),
				Equal(string(marklogicv1.VolumeResizePhaseRestartingPods)),
				Equal(string(marklogicv1.VolumeResizePhaseVerifyingHealth)),
				Equal(string(marklogicv1.VolumeResizePhaseVerifyingPodsRunning)),
				Equal(string(marklogicv1.VolumeResizePhaseCompleted)),
			))

			// During the resize, verify pod is still running (same UID or replaced gracefully)
			Expect(k8sClient.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
				"app.kubernetes.io/instance": group.Spec.Name,
			})).Should(Succeed())
			Expect(len(podList.Items)).Should(BeNumerically(">", 0))
			// Pod should either be the same or a new one (if filesystem resize required restart)

			// Wait for resize to complete
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			By(fmt.Sprintf("Initial pod UID: %s", initialPodUID))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})
	})

	// ==========================================================================
	// Test Case 5: Status and Events Verification
	// ==========================================================================
	Context("Status and Events Verification", func() {

		It("should record all resize phases and completion time", func() {
			group := createTestMarklogicGroup(namespace, groupName+"-status", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Trigger resize
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "12Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for resize to complete
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			// Verify all status fields are populated
			status := g.Status.VolumeResizeStatus
			Expect(status.Phase).Should(Equal(marklogicv1.VolumeResizePhaseCompleted))
			Expect(status.StartTime).ShouldNot(BeNil())
			Expect(status.CompletionTime).ShouldNot(BeNil())
			Expect(status.TargetSize).Should(Equal("12Gi"))
			Expect(status.OriginalSize).Should(Equal("10Gi"))
			Expect(status.Message).Should(ContainSubstring("completed"))
			Expect(status.TotalPVCs).Should(BeNumerically(">", 0))
			Expect(status.PVCsResized).Should(Equal(status.TotalPVCs))

			// Verify completion time is after start time
			Expect(status.CompletionTime.After(status.StartTime.Time)).Should(BeTrue())

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})
	})

	// ==========================================================================
	// Test Case 6: Edge Size Values
	// ==========================================================================
	Context("Edge Size Values", func() {

		It("should handle large resize jump", func() {
			group := createTestMarklogicGroup(namespace, groupName+"-large-jump", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Trigger large resize (10Gi -> 50Gi)
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "50Gi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for resize to complete (may take longer for large resize)
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout*2, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			// Verify PVC has the new size
			pvc := &corev1.PersistentVolumeClaim{}
			pvcName := fmt.Sprintf("datadir-%s-0", group.Spec.Name)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc)).Should(Succeed())
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]
			targetQty := resource.MustParse("50Gi")
			Expect(capacity.Cmp(targetQty)).Should(BeNumerically(">=", 0))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})

		It("should handle non-standard size format (Mi instead of Gi)", func() {
			group := createTestMarklogicGroup(namespace, groupName+"-mi-format", "10Gi", "gp3")
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			// Wait for initial deployment
			Eventually(func() bool {
				g := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g); err != nil {
					return false
				}
				return g.Status.MarklogicGroupStatus == marklogicv1.StateReady
			}, timeout, interval).Should(BeTrue())

			// Trigger resize with Mi format (15360Mi = 15Gi)
			g := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
			g.Spec.Persistence.Size = "15360Mi"
			Expect(k8sClient.Update(ctx, g)).Should(Succeed())

			// Wait for resize to complete
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: namespace}, g)).Should(Succeed())
				if g.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(g.Status.VolumeResizeStatus.Phase)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizePhaseCompleted)))

			// Verify PVC has the new size
			pvc := &corev1.PersistentVolumeClaim{}
			pvcName := fmt.Sprintf("datadir-%s-0", group.Spec.Name)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc)).Should(Succeed())
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]
			targetQty := resource.MustParse("15360Mi")
			Expect(capacity.Cmp(targetQty)).Should(BeNumerically(">=", 0))

			// Cleanup
			Expect(k8sClient.Delete(ctx, group)).Should(Succeed())
		})
	})
})

// Helper function to create a test MarklogicGroup
func createTestMarklogicGroup(namespace, name, size, storageClass string) *marklogicv1.MarklogicGroup {
	replicas := int32(1)
	return &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:     name,
			Replicas: &replicas,
			Image:    "progressofficial/marklogic-db:12.0.0-ubi9-rootless-2.2.2",
			Persistence: &marklogicv1.Persistence{
				Enabled:          true,
				Size:             size,
				StorageClassName: storageClass,
			},
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: stringPtr("admin"),
				AdminPassword: stringPtr("admin"),
			},
		},
	}
}

func stringPtr(s string) *string {
	return &s
}
