package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/homedir"

	"github.com/marklogic/marklogic-kubernetes-operator/test/utils"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	dnodeGrpName = "dnode"
	enodeGrpName = "enode"
	mlClusterNs  = "ednode"
)

var (
	kubeconfig      *string
	home            = homedir.HomeDir()
	initialPodCount int
	incrReplica     = int32(2)
	marklogicgroups = []*databasev1alpha1.MarklogicGroups{
		{
			Name:        dnodeGrpName,
			Replicas:    &replicas,
			IsBootstrap: true,
			GroupConfig: &databasev1alpha1.GroupConfig{
				Name:          "dnode",
				EnableXdqpSsl: true,
			},
		},
		{
			Name:     enodeGrpName,
			Replicas: &replicas,
			GroupConfig: &databasev1alpha1.GroupConfig{
				Name:          "enode",
				EnableXdqpSsl: true,
			},
		},
	}
	mlcluster = &databasev1alpha1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.com/v1alpha1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mlclusterednode",
			Namespace: mlClusterNs,
		},
		Spec: databasev1alpha1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &databasev1alpha1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: marklogicgroups,
		},
	}
)

func TestMlClusterWithEdnode(t *testing.T) {
	feature := features.New("MarklogicCluster Resource with 2 MarkLogicGroups (Ednode and dnode)")

	// Setup for MarklogicCluster creation
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: mlClusterNs,
			},
		}
		if err := client.Resources().Create(ctx, namespace); err != nil {
			t.Fatalf("Failed to create namespace: %s", err)
		}
		databasev1alpha1.AddToScheme(client.Resources(mlClusterNs).GetScheme())

		if err := client.Resources(mlClusterNs).Create(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}
		// wait for resource to be created
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlcluster, func(object k8s.Object) bool {
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
	feature.Assess("MarklogicCluster with 2 MarkLogicGroups deployed Ok", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var mlcluster databasev1alpha1.MarklogicCluster
		if err := client.Resources().Get(ctx, "mlclusterednode", mlClusterNs, &mlcluster); err != nil {
			t.Log("====MarklogicCluster not found====")
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster Pod created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podName := "dnode-0"
		err := utils.WaitForPod(ctx, t, client, mlClusterNs, podName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		epodName := "enode-0"
		err = utils.WaitForPod(ctx, t, client, mlClusterNs, epodName, 180*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx
	})

	// Assessment to check for MarkLogic groups are created
	feature.Assess("MarkLogic groups created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Checking MarkLogic groups")
		podName := "dnode-0"
		url := "http://localhost:8002/manage/v2/groups"
		curlCommand := fmt.Sprintf("curl %s --anyauth -u %s:%s", url, adminUsername, adminPassword)

		output, err := utils.ExecCmdInPod(podName, mlClusterNs, mlContainerName, curlCommand)
		if err != nil {
			t.Fatalf("Failed to execute curl command in pod: %v", err)
		}
		if !strings.Contains(string(output), "<nameref>dnode</nameref>") && !strings.Contains(string(output), "<nameref>enode</nameref>") {
			t.Fatal("Groups does not exists on MarkLogic cluster")
		}
		return ctx
	})

	// Scale the MarkLogic group
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var mlcluster databasev1alpha1.MarklogicCluster
		if err := client.Resources().Get(ctx, "mlclusterednode", mlClusterNs, &mlcluster); err != nil {
			t.Fatal(err)
		}
		// Scale the MarkLogic groups to 2
		mlcluster.Spec.MarkLogicGroups[0].Replicas = &incrReplica
		mlcluster.Spec.MarkLogicGroups[1].Replicas = &incrReplica
		if err := client.Resources().Update(ctx, &mlcluster); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("New Pods created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podNameOne := "dnode-1"
		err := utils.WaitForPod(ctx, t, client, mlClusterNs, podNameOne, 60*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod %s creation: %v", podNameOne, err)
		}
		epodNameTwo := "enode-1"
		err = utils.WaitForPod(ctx, t, client, mlClusterNs, epodNameTwo, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod %s creation: %v", epodNameTwo, err)
		}
		return ctx
	})

	feature.Assess("Check number of pods after scaling", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podList := &corev1.PodList{}
		if err := client.Resources().List(ctx, podList, func(lo *metav1.ListOptions) {
			lo.LabelSelector = "app.kubernetes.io/name=marklogic"
			lo.FieldSelector = "metadata.namespace=" + mlClusterNs
		}); err != nil {
			t.Fatal(err)
		}

		newPodCount := len(podList.Items)
		t.Logf("Number of pods after scaling: %d", newPodCount)
		if newPodCount != 4 {
			t.Fatalf("Expected 4 pods, but found %d", newPodCount)
		}
		return ctx
	})

	// Using feature.Teardown to clean up
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(mlClusterNs).Delete(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to delete MarklogicCluster: %s", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mlClusterNs}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		return ctx
	})

	// submit the feature to be tested
	testEnv.Test(t, feature.Feature())
}
