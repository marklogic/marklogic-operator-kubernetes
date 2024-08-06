package e2eframework

import (
	"context"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	coreV1 "k8s.io/api/core/v1"
	apiextensionsV1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

var replicas = int32(1)
var (
	marklogiccluster = &databasev1alpha1.MarklogicCluster{
		TypeMeta: metaV1.TypeMeta{
			APIVersion: "marklogic.com/v1alpha1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: namespace,
		},
		Spec: databasev1alpha1.MarklogicClusterSpec{
			MarkLogicGroups: []*databasev1alpha1.MarklogicGroups{
				{
					MarklogicGroupSpec: &databasev1alpha1.MarklogicGroupSpec{
						Replicas: &replicas,
						Name:     "marklogicgroups",
						Image:    "marklogicdb/marklogic-db:11.2.0-ubi",
					},
				},
			},
		},
	}
)

func TestMarklogicCluster(t *testing.T) {
	podCreationSig := make(chan *coreV1.Pod)

	feature := features.New("MarklogicCluster Controller")

	// Use feature.Setup to define pre-test configuration
	feature.Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()
		if err := client.Resources(namespace).Watch(&coreV1.PodList{}).WithAddFunc(func(obj interface{}) {
			pod := obj.(*coreV1.Pod)
			if strings.HasPrefix(pod.Name, "marklogic-operator-controller") {
				podCreationSig <- pod
			}
		}).Start(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Assessment to check for CRD in cluster
	feature.Assess("CRD installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		apiextensionsV1.AddToScheme(client.Resources().GetScheme())
		name := "marklogicclusters.database.marklogic.com"
		var crd apiextensionsV1.CustomResourceDefinition
		if err := client.Resources().Get(ctx, name, "", &crd); err != nil {
			t.Fatalf("CRD not found: %s", err)
		}
		if condition := crd.Spec.Names.Kind; condition != "MarklogicCluster" {
			t.Fatalf("MarklogicCluster CRD has unexpected kind: %s", condition)
		}
		return ctx
	})

	// Assessment for MarklogicCluster creation
	feature.Assess("MarklogicCluster creation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		databasev1alpha1.AddToScheme(client.Resources(namespace).GetScheme())

		if err := client.Resources().Create(ctx, marklogiccluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(marklogiccluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(30*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Assessment to check for MarklogicCluster deployment
	feature.Assess("MarklogicCluster deployed Ok", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var marklogicclusterLive databasev1alpha1.MarklogicCluster
		if err := client.Resources().Get(ctx, "marklogicclusters", namespace, &marklogicclusterLive); err != nil {
			t.Log("====MarklogicCluster not found====")
			t.Fatal(err)
		}
		return ctx
	})

	// Assessment to check for the creation of the pod
	feature.Assess("Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		select {
		case <-time.After(60 * time.Second):
			t.Error("Timed out wating for pod creation by MarklogicCluster contoller")
		case pod := <-podCreationSig:
			t.Log("Pod created by MarklogicCluster controller")
			refname := pod.GetOwnerReferences()[0].Name
			if !strings.HasPrefix(refname, "marklogic-operator") {
				t.Fatalf("Pod has unexpected owner ref: %#v", refname)
			}
		}
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		close(podCreationSig)
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}
