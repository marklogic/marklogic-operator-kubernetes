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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

var clusterName = "marklogic-cluster-test"
var clusterNS = "cluster-test-ns"
var clusterTestNSName = types.NamespacedName{Name: clusterName, Namespace: clusterNS}
var clusterHugePages = &marklogicv1.HugePages{
	Enabled:   true,
	MountPath: "/dev/hugepages",
}
var trueVal = true
var enodeReplicas = int32(2)
var dnodeReplicas = int32(1)
var policy = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}

var marklogicGroups = []*marklogicv1.MarklogicGroups{
	{
		Name: "dnode",
		GroupConfig: &marklogicv1.GroupConfig{
			Name:          "dnode",
			EnableXdqpSsl: true,
		},
		Replicas:    &dnodeReplicas,
		Service:     marklogicv1.Service{Type: corev1.ServiceTypeClusterIP},
		IsBootstrap: true,
	},
	{
		Name: "enode",
		GroupConfig: &marklogicv1.GroupConfig{
			Name:          "enode",
			EnableXdqpSsl: true,
		},
		Replicas: &enodeReplicas,
		Service:  marklogicv1.Service{Type: corev1.ServiceTypeClusterIP},
	},
}

var _ = Describe("MarklogicCluster Controller", func() {
	Context("When reconciling a resource", func() {
		ctx := context.Background()
		It("should successfully reconcile the resource", func() {
			ns := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: clusterNS},
			}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())
			mlCluster := &marklogicv1.MarklogicCluster{
				TypeMeta: metav1.TypeMeta{
					Kind:       "MarklogicCluster",
					APIVersion: "marklogic.progress.com/v1alpha1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: clusterNS,
				},
				Spec: marklogicv1.MarklogicClusterSpec{
					Image:            imageName,
					Resources:        &corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("256Mi"), "hugepages-2Mi": resource.MustParse("100Mi")}, Limits: corev1.ResourceList{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("256Mi"), "hugepages-2Mi": resource.MustParse("100Mi")}},
					HugePages:        clusterHugePages,
					EnableConverters: true,
					MarkLogicGroups:  marklogicGroups,
					LogCollection:    &marklogicv1.LogCollection{Enabled: true, Image: "fluent/fluent-bit:3.2.5", Files: marklogicv1.LogFilesConfig{ErrorLogs: true, AccessLogs: true, RequestLogs: true, CrashLogs: true, AuditLogs: true}, Outputs: "stdout"},
					HAProxy: &marklogicv1.HAProxy{
						Enabled:          true,
						ReplicaCount:     1,
						FrontendPort:     80,
						PathBasedRouting: &[]bool{true}[0],
						AppServers: []marklogicv1.AppServers{
							{Name: "AppServices", Type: "http", Port: 8000, TargetPort: 8000, Path: "/console"},
							{Name: "Admin", Type: "http", Port: 8001, TargetPort: 8001, Path: "/adminUI"},
							{Name: "Manage", Type: "http", Port: 8002, TargetPort: 8002, Path: "/manage"},
						},
						Ingress: marklogicv1.Ingress{
							Enabled:          true,
							IngressClassName: "alb",
							Host:             "marklogic-cluster-test.cluster.local",
						}},
					NetworkPolicy: marklogicv1.NetworkPolicy{
						Enabled:     true,
						PolicyTypes: policy,
						PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "marklogic", "app.kubernetes.io/instance": "dnode"}},
						Ingress: []networkingv1.NetworkPolicyIngressRule{
							{From: []networkingv1.NetworkPolicyPeer{{
								PodSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app.kubernetes.io/name":     "marklogic",
										"app.kubernetes.io/instance": "dnode",
									},
								},
							}},
								Ports: []networkingv1.NetworkPolicyPort{{Port: &intstr.IntOrString{IntVal: 8000}}},
							},
						},
					},
					Tls: &marklogicv1.Tls{
						EnableOnDefaultAppServers: true,
						CertSecretNames: []string{
							"cert-secret-1",
							"cert-secret-2",
						},
						CaSecretName: "ca-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, mlCluster)).Should(Succeed())
			clusterCR := &marklogicv1.MarklogicCluster{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, clusterTestNSName, clusterCR)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(clusterCR.Spec.Image).Should(Equal(imageName))
			Expect(clusterCR.Spec.EnableConverters).Should(Equal(true))
			Expect(clusterCR.Spec.HugePages.Enabled).Should(Equal(true))
			Expect(clusterCR.Spec.HugePages.MountPath).Should(Equal("/dev/hugepages"))
			Expect(clusterCR.Spec.Resources.Limits.Cpu().Value()).Should(Equal(resourceCpuValue))
			Expect(clusterCR.Spec.Resources.Limits.Memory().Value()).Should(Equal(resourceMemoryValue))
			hugepagesLimit := clusterCR.Spec.Resources.Limits["hugepages-2Mi"]
			Expect(hugepagesLimit.Value()).Should(Equal(resourceHugepageValue))
			Expect(clusterCR.Spec.Resources.Requests.Cpu().Value()).Should(Equal(resourceCpuValue))
			Expect(clusterCR.Spec.Resources.Requests.Memory().Value()).Should(Equal(resourceMemoryValue))
			hugepagesRequest := clusterCR.Spec.Resources.Requests["hugepages-2Mi"]
			Expect(hugepagesRequest.Value()).Should(Equal(resourceHugepageValue))
			Expect(clusterCR.Spec.MarkLogicGroups).Should(Equal(marklogicGroups))
			Expect(clusterCR.Spec.LogCollection.Enabled).Should(Equal(true))
			Expect(clusterCR.Spec.LogCollection.Image).Should(Equal(fluentBitImage))
			Expect(clusterCR.Spec.HAProxy.Enabled).Should(Equal(true))
			Expect(clusterCR.Spec.HAProxy.ReplicaCount).Should(Equal(int32(1)))
			Expect(clusterCR.Spec.HAProxy.FrontendPort).Should(Equal(int32(80)))
			Expect(clusterCR.Spec.HAProxy.PathBasedRouting).Should(Equal(&trueVal))
			Expect(clusterCR.Spec.HAProxy.AppServers[0].Name).Should(Equal("AppServices"))
			Expect(clusterCR.Spec.HAProxy.AppServers[0].Type).Should(Equal("http"))
			Expect(clusterCR.Spec.HAProxy.AppServers[0].Port).Should(Equal(int32(8000)))
			// Validating if Ingress is created successfully
			Expect(clusterCR.Spec.HAProxy.Ingress.Enabled).Should(Equal(true))
			Expect(clusterCR.Spec.HAProxy.Ingress.IngressClassName).Should(Equal("alb"))
			Expect(clusterCR.Spec.HAProxy.Ingress.Host).Should(Equal("marklogic-cluster-test.cluster.local"))
			// Validating if NetworkPolicy is created successfully
			Expect(clusterCR.Spec.NetworkPolicy.PolicyTypes).Should(Equal(policy))
			Expect(clusterCR.Spec.NetworkPolicy.PodSelector).Should(Equal(metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "marklogic", "app.kubernetes.io/instance": "dnode"}}))
			Expect(clusterCR.Spec.NetworkPolicy.Ingress[0].From[0].PodSelector.MatchLabels).Should(Equal(map[string]string{"app.kubernetes.io/name": "marklogic", "app.kubernetes.io/instance": "dnode"}))
			Expect(clusterCR.Spec.NetworkPolicy.Ingress[0].Ports[0].Port).Should(Equal(&intstr.IntOrString{IntVal: 8000}))
			Expect(clusterCR.Spec.Tls.EnableOnDefaultAppServers).Should(Equal(true))
			Expect(clusterCR.Spec.Tls.CertSecretNames).Should(ContainElements("cert-secret-1", "cert-secret-2"))
			Expect(clusterCR.Spec.Tls.CaSecretName).Should(Equal("ca-secret"))
		})

		It("Should create a secret for MarkLogic Admin User", func() {
			// Validating if Secret is created successfully
			secret := &corev1.Secret{}
			secretName := clusterName + "-admin"
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: clusterNS}, secret)
				return err == nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})
