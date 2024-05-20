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

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	Name      = "dnode"
	Namespace = "testns"

	timeout  = time.Second * 60
	duration = time.Second * 30
	interval = time.Millisecond * 250
)

var rep = int32(1)
var typeNamespaceName = types.NamespacedName{Name: Name, Namespace: Namespace}
var imageName = "marklogicdb/marklogic-db:11.1.0-centos-1.1.1"

var _ = Describe("MarkLogicGroup controller", func() {
	Context("When creating an MarklogicGroup", func() {
		ctx := context.Background()
		It("Should create a MarklogicGroup CR, StatefulSet and Service", func() {

			// Create the namespace
			ns := corev1.Namespace{
				ObjectMeta: v1.ObjectMeta{Name: Namespace},
			}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())

			// Declaring the Marklogic Group object and create CR
			mlGroup := &databasev1alpha1.MarklogicGroup{
				TypeMeta: v1.TypeMeta{
					Kind:       "MarklogicGroup",
					APIVersion: "operator.marklogic.com/v1alpha1",
				},
				ObjectMeta: v1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
				},
				Spec: databasev1alpha1.MarklogicGroupSpec{
					Replicas: &rep,
					Name:     Name,
					Image:    "marklogicdb/marklogic-db:11.1.0-centos-1.1.1",
				},
				Status: databasev1alpha1.MarklogicGroupStatus{},
			}
			Expect(k8sClient.Create(ctx, mlGroup)).Should(Succeed())

			// Validating if CR is created successfully
			createdCR := &databasev1alpha1.MarklogicGroup{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, createdCR)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdCR.Spec.Image).Should(Equal(imageName))
			Expect(createdCR.Spec.Replicas).Should(Equal(&rep))
			Expect(createdCR.Name).Should(Equal(Name))

			// Validating if StatefulSet is created successfully
			createdSts := &appsv1.StatefulSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, createdSts)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdSts.Spec.Template.Spec.Containers[0].Image).Should(Equal(imageName))
			Expect(createdSts.Spec.Replicas).Should(Equal(&rep))

			// Validating if Service is created successfully
			createdSrv := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespaceName, createdSrv)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			Expect(createdSrv.Spec.Ports[0].TargetPort).Should(Equal(intstr.FromInt(int(7997))))
		})
	})
})
