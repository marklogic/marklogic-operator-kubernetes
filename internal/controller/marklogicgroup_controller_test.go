/*
Copyright 2024.

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

package controller

import (
	"context"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	Name      = "dnode"
	Namespace = "testns"
	maxSkew   = int32(2)

	timeout  = time.Second * 10
	duration = time.Second * 10
	interval = time.Millisecond * 250

	imageName = "progressofficial/marklogic-db:11.3.0-ubi-rootless"
)

var replicas = int32(2)
var fsGroup = int64(2)
var fsGroupChangePolicy = corev1.FSGroupChangeOnRootMismatch
var runAsUser = int64(1000)
var runAsNonRoot bool = true
var allowPrivilegeEscalation bool = false
var typeNamespaceName = types.NamespacedName{Name: Name, Namespace: Namespace}

const resourceCpuValue = int64(1)
const resourceMemoryValue = int64(268435456)

// 100Mi
const resourceHugepageValue = int64(104857600)

var svcName = Name + "-cluster"
var typeNamespaceNameSvc = types.NamespacedName{Name: svcName, Namespace: Namespace}
var netPolicyName = Name
var typeNsNameNetPolicy = types.NamespacedName{Name: netPolicyName, Namespace: Namespace}

const fluentBitImage = "fluent/fluent-bit:3.2.5"

var groupConfig = &marklogicv1.GroupConfig{
	Name:          "dnode",
	EnableXdqpSsl: true,
}

var hugePages = marklogicv1.HugePages{
	Enabled:   true,
	MountPath: "/dev/hugepages",
}

var _ = Describe("MarkLogicGroup controller", func() {
	Context("When creating an MarklogicGroup", func() {
		ctx := context.Background()
		It("Should create a MarklogicGroup CR, StatefulSet and Service", func() {
			// Create the namespace
			ns := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: Namespace},
			}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())

			// Declaring the Marklogic Group object and create CR
			mlGroup := &marklogicv1.MarklogicGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "MarklogicGroup",
					APIVersion: "marklogic.progress.com/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
				},
				Spec: marklogicv1.MarklogicGroupSpec{
					Replicas:                  &replicas,
					Name:                      Name,
					Image:                     imageName,
					GroupConfig:               groupConfig,
					EnableConverters:          true,
					HugePages:                 &hugePages,
					UpdateStrategy:            "OnDelete",
					Resources:                 &corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("256Mi"), "hugepages-2Mi": resource.MustParse("100Mi")}, Limits: corev1.ResourceList{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("256Mi"), "hugepages-2Mi": resource.MustParse("100Mi")}},
					PriorityClassName:         "high-priority",
					ClusterDomain:             "cluster.local",
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 2, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.ScheduleAnyway}},
					Affinity:                  &corev1.Affinity{PodAffinity: &corev1.PodAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname"}, Weight: 100}}}},
					PodSecurityContext: &corev1.PodSecurityContext{
						FSGroup:             &fsGroup,
						FSGroupChangePolicy: &fsGroupChangePolicy,
					},
					ContainerSecurityContext: &corev1.SecurityContext{
						RunAsUser:                &runAsUser,
						RunAsNonRoot:             &runAsNonRoot,
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					},
					LogCollection: &marklogicv1.LogCollection{Enabled: true, Image: "fluent/fluent-bit:3.2.5", Files: marklogicv1.LogFilesConfig{ErrorLogs: true, AccessLogs: true, RequestLogs: true, CrashLogs: true, AuditLogs: true}, Outputs: "stdout"},
				},
			}
			Expect(k8sClient.Create(ctx, mlGroup)).Should(Succeed())

			// Validating if CR is created successfully
			createdCR := &marklogicv1.MarklogicGroup{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, createdCR)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdCR.Spec.Image).Should(Equal(imageName))
			Expect(createdCR.Spec.Replicas).Should(Equal(&replicas))
			Expect(createdCR.Name).Should(Equal(Name))
			Expect(createdCR.Spec.GroupConfig).Should(Equal(groupConfig))
			Expect(createdCR.Spec.EnableConverters).Should(Equal(true))
			Expect(createdCR.Spec.HugePages.Enabled).Should(Equal(true))
			Expect(createdCR.Spec.HugePages.MountPath).Should(Equal("/dev/hugepages"))
			Expect(createdCR.Spec.Resources.Limits.Cpu().Value()).Should(Equal(resourceCpuValue))
			Expect(createdCR.Spec.Resources.Limits.Memory().Value()).Should(Equal(resourceMemoryValue))
			hugepagesLimit := createdCR.Spec.Resources.Limits["hugepages-2Mi"]
			Expect(hugepagesLimit.Value()).Should(Equal(resourceHugepageValue))
			Expect(createdCR.Spec.Resources.Requests.Cpu().Value()).Should(Equal(resourceCpuValue))
			Expect(createdCR.Spec.Resources.Requests.Memory().Value()).Should(Equal(resourceMemoryValue))
			hugepagesRequest := createdCR.Spec.Resources.Requests["hugepages-2Mi"]
			Expect(hugepagesRequest.Value()).Should(Equal(resourceHugepageValue))
			Expect(createdCR.Spec.UpdateStrategy).Should(Equal(appsv1.OnDeleteStatefulSetStrategyType))
			Expect(createdCR.Spec.PriorityClassName).Should(Equal("high-priority"))
			Expect(createdCR.Spec.ClusterDomain).Should(Equal("cluster.local"))
			Expect(createdCR.Spec.TopologySpreadConstraints[0].MaxSkew).Should(Equal(int32(2)))
			Expect(createdCR.Spec.TopologySpreadConstraints[0].TopologyKey).Should(Equal("kubernetes.io/hostname"))
			Expect(createdCR.Spec.TopologySpreadConstraints[0].WhenUnsatisfiable).Should(Equal(corev1.ScheduleAnyway))
			Expect(createdCR.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight).Should(Equal(int32(100)))
			Expect(createdCR.Spec.PodSecurityContext.FSGroup).Should(Equal(&fsGroup))
			Expect(*createdCR.Spec.PodSecurityContext.FSGroupChangePolicy).Should(Equal(corev1.FSGroupChangeOnRootMismatch))
			Expect(*createdCR.Spec.ContainerSecurityContext.RunAsUser).Should(Equal(int64(1000)))
			Expect(createdCR.Spec.ContainerSecurityContext.RunAsNonRoot).Should(Equal(&runAsNonRoot))
			Expect(createdCR.Spec.ContainerSecurityContext.AllowPrivilegeEscalation).Should(Equal(&allowPrivilegeEscalation))
			Expect(createdCR.Spec.LogCollection.Enabled).Should(Equal(true))
			Expect(createdCR.Spec.LogCollection.Image).Should(Equal(fluentBitImage))

			// Validating if StatefulSet is created successfully
			sts := &appsv1.StatefulSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, sts)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).Should(Equal(imageName))
			Expect(sts.Spec.Template.Spec.Containers[1].Image).Should(Equal(fluentBitImage))
			Expect(sts.Spec.Replicas).Should(Equal(&replicas))
			Expect(sts.Name).Should(Equal(Name))
			Expect(sts.Spec.PodManagementPolicy).Should(Equal(appsv1.ParallelPodManagement))

			// Validating if headless Service is created successfully
			createdSrv := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, createdSrv)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdSrv.Spec.Ports[0].TargetPort).Should(Equal(intstr.FromInt(int(7997))))

			// Validating if Service is created successfully
			createdClusterSrv := &corev1.Service{}
			svcName := sts.Name + "-cluster"
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceNameSvc, createdClusterSrv)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdClusterSrv.Name).Should(Equal(svcName))
			Expect(createdClusterSrv.Spec.Type).Should(Equal(corev1.ServiceTypeClusterIP))

		})

		It("Should create configmap for MarkLogic scripts", func() {
			// Validating if ConfigMap is created successfully
			configMap := &corev1.ConfigMap{}
			configMapName := Name + "-scripts"
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: Namespace}, configMap)
				return err == nil
			}, timeout, interval).Should(BeTrue())
		})

		It("Should update the MarklogicGroup CR", func() {
			// Update the MarklogicGroup CR
			createdCR := &marklogicv1.MarklogicGroup{}

			Expect(k8sClient.Get(ctx, typeNamespaceName, createdCR)).Should(Succeed())
			createdCR.Spec.Replicas = new(int32)
			*createdCR.Spec.Replicas = 3
			Expect(k8sClient.Update(ctx, createdCR)).Should(Succeed())

			// Validating if CR is updated successfully
			updatedCR := &marklogicv1.MarklogicGroup{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, updatedCR)
				return err == nil && *updatedCR.Spec.Replicas == 3
			}, timeout, interval).Should(BeTrue())
		})
	})
})
