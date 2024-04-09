package controllers

import (
	"context"
	"fmt"
	"time"

	operatorv1alpha1 "github.com/example/marklogic-operator/api/v1alpha1"
	"github.com/example/marklogic-operator/pkg/k8sutil"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	Name      = "marklogic-group"
	Namespace = "default"
	timeout   = time.Second * 60
	duration  = time.Second * 30
	interval  = time.Millisecond * 250
)

var rep = int32(1)
var typeNamespaceGroupName = types.NamespacedName{Name: Name, Namespace: Namespace}
var typeNamespaceGroupSts = types.NamespacedName{Name: "dnode", Namespace: Namespace}

var _ = Describe("MarkLogicGroup controller", func() {
	Context("When creating an MarklogicGroup", func() {
		ctx := context.Background()
		It("Should create a MarklogicGroup CR", func() {
			fmt.Println("Test MarklogicGroup Controller")

			trueVar := true
			mlGroup := &operatorv1alpha1.MarklogicGroup{
				TypeMeta: v1.TypeMeta{
					Kind:       "MarklogicGroup",
					APIVersion: "operator.marklogic.com/v1alpha1",
				},
				ObjectMeta: v1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name:       Name,
							Kind:       "MarkLogicGroup",
							APIVersion: "operator.marklogic.com/v1alpha1",
							UID:        "foo-UID",
							Controller: &trueVar,
						},
					},
				},
				Spec: operatorv1alpha1.MarklogicGroupSpec{
					Replicas: &rep,
					Name:     "dnode",
					Image:    "marklogicdb/marklogic-db:11.1.0-centos-1.1.1",
				},
				Status: operatorv1alpha1.MarklogicGroupStatus{},
			}

			ns := &corev1.Namespace{}
			groupNamespace := mlGroup.ObjectMeta.Namespace
			err := k8sClient.Get(ctx, client.ObjectKey{Name: Namespace}, ns)
			Expect(err).ToNot(HaveOccurred(), "Failed to fetch namespace: %s", groupNamespace)
			Expect(k8sClient.Create(ctx, mlGroup)).Should(Succeed())

			By("Checking if the custom resource was successfully created")
			Eventually(func() error {
				found := &operatorv1alpha1.MarklogicGroup{}
				return k8sClient.Get(ctx, typeNamespaceGroupName, found)
			}, time.Minute, time.Second).Should(Succeed())

		})

		It("should create a statefulset", func() {
			imageName := "marklogicdb/marklogic-db:11.1.0-centos-1.1.1"
			statefulsetsNum, err := k8sutil.GenerateK8sClient().AppsV1().StatefulSets(Namespace).Get(ctx, "dnode", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(statefulsetsNum.Spec.Template.Spec.Containers[0].Image).Should(Equal(imageName))
			Expect(statefulsetsNum.Spec.Replicas).Should(Equal(&rep))
		})

		It("should create a service", func() {
			svx, err := k8sutil.GenerateK8sClient().CoreV1().Services(Namespace).Get(context.TODO(), "dnode", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(svx.Spec.Ports[0].TargetPort).Should(Equal(intstr.FromInt(int(7997))))
		})

		_, err := k8sutil.GetPodsForStatefulSet(Namespace, Name)
		Expect(err).ToNot(HaveOccurred())

	})
})
