package e2e

import (
	"context"
	// "strings"
	"fmt"
	"testing"
	"time"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	// "sigs.k8s.io/e2e-framework/support/utils"
)

var replicas = int32(1)

const (
	groupName   = "dnode"
	mlNamespace = "default"
)

var (
	marklogiccluster = &databasev1alpha1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.com/v1alpha1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: mlNamespace,
		},
		Spec: databasev1alpha1.MarklogicClusterSpec{
			Image: marklogicImage,
			MarkLogicGroups: []*databasev1alpha1.MarklogicGroups{
				{
					Name:        groupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
		},
	}
)

func TestMarklogicCluster(t *testing.T) {
	feature := features.New("MarklogicCluster Resource")

	// Assessment for MarklogicCluster creation
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		databasev1alpha1.AddToScheme(client.Resources(mlNamespace).GetScheme())

		if err := client.Resources(mlNamespace).Create(ctx, marklogiccluster); err != nil {
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
		var marklogiccluster databasev1alpha1.MarklogicCluster
		if err := client.Resources().Get(ctx, "marklogicclusters", mlNamespace, &marklogiccluster); err != nil {
			t.Log("====MarklogicCluster not found====")
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "dnode-0"
		err := waitForPod(ctx, t, client, mlNamespace, podName, 60*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx

	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}

func waitForPod(ctx context.Context, t *testing.T, client klient.Client, namespace, podName string, timeout time.Duration) error {
	start := time.Now()
	pod := &corev1.Pod{}
	for {
		err := client.Resources(namespace).Get(ctx, podName, namespace, pod)
		t.Logf("Pod %s is in phase %s", pod.Name, pod.Status.Phase)
		if err == nil {
			if pod.Status.Phase == "Running" {
				return nil
			}
		} else if !errors.IsNotFound(err) {
			t.Logf("Failed to get pod %s: %v", podName, err)
			continue
		}

		if time.Since(start) > timeout {
			return fmt.Errorf("timed out waiting for pod %s to be created", podName)
		}

		time.Sleep(5 * time.Second)
	}
}
