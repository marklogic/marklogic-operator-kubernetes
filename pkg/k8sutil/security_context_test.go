package k8sutil

import (
	"context"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestGenerateContainerDefAppliesFluentBitSecurityContext(t *testing.T) {
	t.Parallel()

	runAsUser := int64(2000)
	containerDefs := generateContainerDef("marklogic-server", containerParameters{
		LogCollection: &marklogicv1.LogCollection{
			Enabled: true,
			Image:   "fluent/fluent-bit:4.1.1",
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: &runAsUser,
			},
		},
	})

	if len(containerDefs) != 2 {
		t.Fatalf("expected marklogic and fluent-bit containers, got %d", len(containerDefs))
	}

	fluentBitContainer := containerDefs[1]
	if fluentBitContainer.Name != "fluent-bit" {
		t.Fatalf("expected fluent-bit container, got %s", fluentBitContainer.Name)
	}
	if fluentBitContainer.SecurityContext == nil {
		t.Fatal("expected fluent-bit securityContext to be set")
	}
	if fluentBitContainer.SecurityContext.RunAsUser == nil || *fluentBitContainer.SecurityContext.RunAsUser != runAsUser {
		t.Fatalf("expected fluent-bit runAsUser %d, got %+v", runAsUser, fluentBitContainer.SecurityContext.RunAsUser)
	}
}

func TestCreateHAProxyDeploymentDefAppliesSecurityContexts(t *testing.T) {
	t.Parallel()

	fsGroup := int64(3000)
	runAsUser := int64(4000)
	pathBasedRouting := false
	replicas := int32(1)
	cc := &ClusterContext{
		Ctx: context.Background(),
		Request: &reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      "ml-cluster",
			Namespace: "default",
		}},
		MarklogicCluster: &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "ml-cluster", Namespace: "default"},
			Spec: marklogicv1.MarklogicClusterSpec{
				HAProxy: &marklogicv1.HAProxy{
					Image:            "haproxytech/haproxy-alpine:3.4.0",
					PathBasedRouting: &pathBasedRouting,
					PodSecurityContext: &corev1.PodSecurityContext{
						FSGroup: &fsGroup,
					},
					ContainerSecurityContext: &corev1.SecurityContext{
						RunAsUser: &runAsUser,
					},
				},
				MarkLogicGroups: []*marklogicv1.MarklogicGroups{
					{
						Name:     "group-1",
						Replicas: &replicas,
					},
				},
			},
		},
	}

	deployment := cc.createHAProxyDeploymentDef(metav1.ObjectMeta{Name: "marklogic-haproxy", Namespace: "default"})
	if deployment.Spec.Template.Spec.SecurityContext == nil {
		t.Fatal("expected HAProxy pod securityContext to be set")
	}
	if deployment.Spec.Template.Spec.SecurityContext.FSGroup == nil || *deployment.Spec.Template.Spec.SecurityContext.FSGroup != fsGroup {
		t.Fatalf("expected HAProxy fsGroup %d, got %+v", fsGroup, deployment.Spec.Template.Spec.SecurityContext.FSGroup)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected one HAProxy container, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	if deployment.Spec.Template.Spec.Containers[0].SecurityContext == nil {
		t.Fatal("expected HAProxy container securityContext to be set")
	}
	if deployment.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser == nil || *deployment.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser != runAsUser {
		t.Fatalf("expected HAProxy runAsUser %d, got %+v", runAsUser, deployment.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser)
	}
}
