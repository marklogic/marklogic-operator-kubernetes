package e2e

import (
	"context"
	// "strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	coreV1 "k8s.io/api/core/v1"
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
			Image: "marklogicdb/marklogic-db:11.2.0-ubi",
		},
	}
)

func TestMarklogicCluster(t *testing.T) {
    podCreationSig := make(chan *coreV1.Pod)

	feature := features.New("MarklogicCluster Resource")

	// Assessment for MarklogicCluster creation
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
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
			wait.WithInterval(5*time.Second),
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

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		close(podCreationSig)
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}
