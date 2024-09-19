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

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
)

var clusterName = "marklogic-cluster-test"
var clusterNS = "cluster-test-ns"
var clusterTestNSName = types.NamespacedName{Name: clusterName, Namespace: clusterNS}
var clusterHugePages = &databasev1alpha1.HugePages{
	Enabled:   true,
	MountPath: "/dev/hugepages",
}
var enodeReplicas = int32(2)
var dnodeReplicas = int32(1)
var marklogicGroups = []*databasev1alpha1.MarklogicGroups{
	{
		Name:     "dnode",
		Replicas: &dnodeReplicas,
	},
	{
		Name:     "enode",
		Replicas: &enodeReplicas,
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
			mlCluster := &databasev1alpha1.MarklogicCluster{
				TypeMeta: metav1.TypeMeta{
					Kind:       "MarklogicCluster",
					APIVersion: "database.marklogic.com/v1alpha1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: clusterNS,
				},
				Spec: databasev1alpha1.MarklogicClusterSpec{
					Image:            imageName,
					Resources:        &corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("256Mi"), "hugepages-2Mi": resource.MustParse("100Mi")}, Limits: corev1.ResourceList{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("256Mi"), "hugepages-2Mi": resource.MustParse("100Mi")}},
					HugePages:        clusterHugePages,
					EnableConverters: true,
					MarkLogicGroups:  marklogicGroups,
					LogCollection:    &databasev1alpha1.LogCollection{Enabled: true, Image: "fluent/fluent-bit:3.1.1", Files: databasev1alpha1.LogFilesConfig{ErrorLogs: true, AccessLogs: true, RequestLogs: true, CrashLogs: true, AuditLogs: true}, Outputs: "stdout"},
				},
			}
			Expect(k8sClient.Create(ctx, mlCluster)).Should(Succeed())
			clusterCR := &databasev1alpha1.MarklogicCluster{}
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
		})
	})
})
