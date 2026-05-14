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

	imageName = "progressofficial/marklogic-db:12.0.0-ubi9-rootless-2.2.2"
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

const fluentBitImage = "fluent/fluent-bit:4.1.1"

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
					ReadinessProbe:            marklogicv1.ContainerProbe{Enabled: true, InitialDelaySeconds: 10, TimeoutSeconds: 5, PeriodSeconds: 30, SuccessThreshold: 1, FailureThreshold: 3},
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
					LogCollection: &marklogicv1.LogCollection{Enabled: true, Image: "fluent/fluent-bit:4.1.1", Files: marklogicv1.LogFilesConfig{ErrorLogs: true, AccessLogs: true, RequestLogs: true, CrashLogs: true, AuditLogs: true}, Outputs: "stdout"},
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
			Expect(sts.Spec.Selector.MatchLabels["app.kubernetes.io/component"]).Should(Equal("database"))
			Expect(sts.Spec.Template.Labels["app.kubernetes.io/component"]).Should(Equal("database"))
			Expect(sts.Spec.Template.Spec.Containers[0].ReadinessProbe.Exec).ShouldNot(BeNil())
			Expect(sts.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket).Should(BeNil())
			Expect(findEnvVar(sts.Spec.Template.Spec.Containers[0].Env, "MARKLOGIC_DYNAMIC_HOST")).Should(BeNil())

			// Validating if headless Service is created successfully
			createdSrv := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, createdSrv)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdSrv.Spec.Ports[0].TargetPort).Should(Equal(intstr.FromInt(int(7997))))
			Expect(createdSrv.Spec.Selector["app.kubernetes.io/component"]).Should(Equal("database"))
			Expect(createdSrv.Labels["app.kubernetes.io/component"]).Should(Equal("database"))

			// Validating if Service is created successfully
			createdClusterSrv := &corev1.Service{}
			svcName := sts.Name + "-cluster"
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceNameSvc, createdClusterSrv)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdClusterSrv.Name).Should(Equal(svcName))
			Expect(createdClusterSrv.Spec.Type).Should(Equal(corev1.ServiceTypeClusterIP))
			Expect(createdClusterSrv.Spec.Selector["app.kubernetes.io/component"]).Should(Equal("database"))
			Expect(createdClusterSrv.Labels["app.kubernetes.io/component"]).Should(Equal("database"))

		})

		It("Should render dynamic group StatefulSet and Services with dynamic-host identity", func() {
			dynamicNamespace := "testns-dynamic"
			dynamicName := "dynamic-group"
			dynamicTypeNamespaceName := types.NamespacedName{Name: dynamicName, Namespace: dynamicNamespace}
			dynamicClusterServiceName := dynamicName + "-cluster"
			dynamicClusterServiceNsName := types.NamespacedName{Name: dynamicClusterServiceName, Namespace: dynamicNamespace}

			ns := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: dynamicNamespace},
			}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())

			mlGroup := &marklogicv1.MarklogicGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "MarklogicGroup",
					APIVersion: "marklogic.progress.com/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      dynamicName,
					Namespace: dynamicNamespace,
				},
				Spec: marklogicv1.MarklogicGroupSpec{
					Replicas:       &replicas,
					Name:           dynamicName,
					Image:          imageName,
					ClusterDomain:  "cluster.local",
					GroupConfig:    groupConfig,
					IsDynamic:      true,
					ReadinessProbe: marklogicv1.ContainerProbe{Enabled: true, InitialDelaySeconds: 10, TimeoutSeconds: 5, PeriodSeconds: 30, SuccessThreshold: 1, FailureThreshold: 3},
					UpdateStrategy: appsv1.RollingUpdateStatefulSetStrategyType,
					Service:        marklogicv1.Service{Type: corev1.ServiceTypeClusterIP},
				},
			}
			Expect(k8sClient.Create(ctx, mlGroup)).Should(Succeed())

			dynamicSts := &appsv1.StatefulSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, dynamicTypeNamespaceName, dynamicSts)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(dynamicSts.Spec.Selector.MatchLabels["app.kubernetes.io/component"]).Should(Equal("dynamic-host"))
			Expect(dynamicSts.Spec.Template.Labels["app.kubernetes.io/component"]).Should(Equal("dynamic-host"))
			Expect(dynamicSts.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket).ShouldNot(BeNil())
			Expect(dynamicSts.Spec.Template.Spec.Containers[0].ReadinessProbe.Exec).Should(BeNil())
			Expect(dynamicSts.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntVal).Should(Equal(int32(8001)))
			dynamicEnv := findEnvVar(dynamicSts.Spec.Template.Spec.Containers[0].Env, "MARKLOGIC_DYNAMIC_HOST")
			Expect(dynamicEnv).ShouldNot(BeNil())
			Expect(dynamicEnv.Value).Should(Equal("true"))

			dynamicHeadlessService := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, dynamicTypeNamespaceName, dynamicHeadlessService)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(dynamicHeadlessService.Spec.Selector["app.kubernetes.io/component"]).Should(Equal("dynamic-host"))
			Expect(dynamicHeadlessService.Labels["app.kubernetes.io/component"]).Should(Equal("dynamic-host"))

			dynamicClusterService := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, dynamicClusterServiceNsName, dynamicClusterService)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(dynamicClusterService.Spec.Selector["app.kubernetes.io/component"]).Should(Equal("dynamic-host"))
			Expect(dynamicClusterService.Labels["app.kubernetes.io/component"]).Should(Equal("dynamic-host"))
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

	Context("When validating volume resize requests", func() {
		ctx := context.Background()

		It("Should initialize resize operation status for growth request", func() {
			nsName := "resize-init-ns"
			groupName := "resize-init"
			nn := types.NamespacedName{Name: groupName, Namespace: nsName}

			ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())

			group := newPersistentGroup(nsName, groupName, "20Gi", appsv1.OnDeleteStatefulSetStrategyType)
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			Eventually(func() bool {
				sts := &appsv1.StatefulSet{}
				return k8sClient.Get(ctx, nn, sts) == nil
			}, timeout, interval).Should(BeTrue())

			pvc := newPersistentPVC(nsName, groupName, "20Gi")
			Expect(k8sClient.Create(ctx, pvc)).Should(Succeed())

			current := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, nn, current)).Should(Succeed())
			current.Spec.Persistence.Size = "30Gi"
			Expect(k8sClient.Update(ctx, current)).Should(Succeed())

			Eventually(func() string {
				updated := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, nn, updated); err != nil || updated.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(updated.Status.VolumeResizeStatus.Reason)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizeReasonPVCNotBound)))

			updated := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, nn, updated)).Should(Succeed())
			status := updated.Status.VolumeResizeStatus
			Expect(status).ShouldNot(BeNil())
			Expect(status.OperationID).ShouldNot(BeEmpty())
			Expect(status.CurrentSize).Should(Equal("20Gi"))
			Expect(status.TargetSize).Should(Equal("30Gi"))
			Expect(status.Phase).Should(Equal(marklogicv1.VolumeResizePhaseStalled))
			Expect(status.Reason).Should(Equal(marklogicv1.VolumeResizeReasonPVCNotBound))
		})

		It("Should reject shrink resize requests", func() {
			nsName := "resize-shrink-ns"
			groupName := "resize-shrink"
			nn := types.NamespacedName{Name: groupName, Namespace: nsName}

			ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())

			group := newPersistentGroup(nsName, groupName, "20Gi", appsv1.OnDeleteStatefulSetStrategyType)
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			Eventually(func() bool {
				sts := &appsv1.StatefulSet{}
				return k8sClient.Get(ctx, nn, sts) == nil
			}, timeout, interval).Should(BeTrue())

			pvc := newPersistentPVC(nsName, groupName, "20Gi")
			Expect(k8sClient.Create(ctx, pvc)).Should(Succeed())

			current := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, nn, current)).Should(Succeed())
			current.Spec.Persistence.Size = "10Gi"
			Expect(k8sClient.Update(ctx, current)).Should(Succeed())

			Eventually(func() string {
				updated := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, nn, updated); err != nil || updated.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(updated.Status.VolumeResizeStatus.Reason)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizeReasonShrinkNotSupported)))

			updated := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, nn, updated)).Should(Succeed())
			status := updated.Status.VolumeResizeStatus
			Expect(status).ShouldNot(BeNil())
			Expect(status.Phase).Should(Equal(marklogicv1.VolumeResizePhaseFailed))
			Expect(status.Reason).Should(Equal(marklogicv1.VolumeResizeReasonShrinkNotSupported))
		})

		It("Should reject resize when updateStrategy is not OnDelete", func() {
			nsName := "resize-strategy-ns"
			groupName := "resize-strategy"
			nn := types.NamespacedName{Name: groupName, Namespace: nsName}

			ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())

			group := newPersistentGroup(nsName, groupName, "20Gi", appsv1.OnDeleteStatefulSetStrategyType)
			Expect(k8sClient.Create(ctx, group)).Should(Succeed())

			Eventually(func() bool {
				sts := &appsv1.StatefulSet{}
				return k8sClient.Get(ctx, nn, sts) == nil
			}, timeout, interval).Should(BeTrue())

			pvc := newPersistentPVC(nsName, groupName, "20Gi")
			Expect(k8sClient.Create(ctx, pvc)).Should(Succeed())

			current := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, nn, current)).Should(Succeed())
			current.Spec.Persistence.Size = "30Gi"
			current.Spec.UpdateStrategy = appsv1.RollingUpdateStatefulSetStrategyType
			Expect(k8sClient.Update(ctx, current)).Should(Succeed())

			Eventually(func() string {
				updated := &marklogicv1.MarklogicGroup{}
				if err := k8sClient.Get(ctx, nn, updated); err != nil || updated.Status.VolumeResizeStatus == nil {
					return ""
				}
				return string(updated.Status.VolumeResizeStatus.Reason)
			}, timeout, interval).Should(Equal(string(marklogicv1.VolumeResizeReasonInvalidResizeRequest)))

			updated := &marklogicv1.MarklogicGroup{}
			Expect(k8sClient.Get(ctx, nn, updated)).Should(Succeed())
			status := updated.Status.VolumeResizeStatus
			Expect(status).ShouldNot(BeNil())
			Expect(status.Phase).Should(Equal(marklogicv1.VolumeResizePhaseFailed))
			Expect(status.Reason).Should(Equal(marklogicv1.VolumeResizeReasonInvalidResizeRequest))
		})
	})
})

func newPersistentGroup(namespace, name, size string, strategy appsv1.StatefulSetUpdateStrategyType) *marklogicv1.MarklogicGroup {
	replicas := int32(1)
	return &marklogicv1.MarklogicGroup{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MarklogicGroup",
			APIVersion: "marklogic.progress.com/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Replicas:       &replicas,
			Name:           name,
			Image:          imageName,
			UpdateStrategy: strategy,
			Persistence: &marklogicv1.Persistence{
				Enabled: true,
				Size:    size,
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
			},
		},
	}
}

func newPersistentPVC(namespace, groupName, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "datadir-" + groupName + "-0",
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}

func findEnvVar(envVars []corev1.EnvVar, envName string) *corev1.EnvVar {
	for i := range envVars {
		if envVars[i].Name == envName {
			return &envVars[i]
		}
	}
	return nil
}
