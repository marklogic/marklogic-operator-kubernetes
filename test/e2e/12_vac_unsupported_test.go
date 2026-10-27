package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	vacUnsupportedNamespace = "ml-vac-unsupported"
	vacUnsupportedGroup     = "vacnode"
)

func volumeAttributesClassAPIServed(config *envconf.Config) (bool, error) {
	client, err := discovery.NewDiscoveryClientForConfig(config.Client().RESTConfig())
	if err != nil {
		return false, err
	}
	for _, version := range []string{"v1", "v1beta1"} {
		resources, err := client.ServerResourcesForGroupVersion("storage.k8s.io/" + version)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, resource := range resources.APIResources {
			if resource.Name == "volumeattributesclasses" {
				return true, nil
			}
		}
	}
	return false, nil
}

// TestVolumeAttributesClassUnsupportedEnvironment runs only where no VAC API is served, such as Kubernetes 1.30 or 1.31-1.33 without the beta API.
func TestVolumeAttributesClassUnsupportedEnvironment(t *testing.T) {
	trackTest(t)
	runTopLevelParallel(t)
	feature := features.New("VAC unsupported environment").WithLabel("type", "vac-unsupported")
	replicas := int32(1)
	adminUser, adminPass := "admin", "Admin@8001"
	className := "ml-e2e-vac-unsupported"
	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "ml-vac-unsupported", Namespace: vacUnsupportedNamespace},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image:       marklogicImage,
			Auth:        &marklogicv1.AdminAuth{AdminUsername: &adminUser, AdminPassword: &adminPass},
			Persistence: &marklogicv1.Persistence{Enabled: true, Size: "10Gi", VolumeAttributesClassName: &className},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: vacUnsupportedGroup, Replicas: &replicas, IsBootstrap: true},
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		served, err := volumeAttributesClassAPIServed(c)
		if err != nil {
			t.Fatalf("VAC API discovery failed: %v", err)
		}
		if served {
			t.Skip("VolumeAttributesClass API is served; the unsupported-environment case needs a cluster without it")
		}
		if err := ensureFreshNamespace(ctx, c, vacUnsupportedNamespace); err != nil {
			t.Fatalf("Failed to reset namespace %s: %v", vacUnsupportedNamespace, err)
		}
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: vacUnsupportedNamespace, Labels: namespaceLabels()}}
		if err := c.Client().Resources().Create(ctx, namespace); err != nil {
			t.Fatalf("Failed to create namespace: %v", err)
		}
		if err := c.Client().Resources(vacUnsupportedNamespace).Create(ctx, cluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		return ctx
	})

	feature.Assess("Cluster Ready without VAC", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := utils.WaitForPod(ctx, t, client, vacUnsupportedNamespace, vacUnsupportedGroup+"-0", 10*time.Minute, true); err != nil {
			t.Fatalf("MarkLogic Pod did not become Ready: %v", err)
		}
		claim := &corev1.PersistentVolumeClaim{}
		if err := client.Resources().Get(ctx, "datadir-"+vacUnsupportedGroup+"-0", vacUnsupportedNamespace, claim); err != nil {
			t.Fatal(err)
		}
		if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeAttributesClassName != nil {
			t.Fatalf("PVC must be Bound and provisioned without a VAC, got phase=%s class=%v", claim.Status.Phase, claim.Spec.VolumeAttributesClassName)
		}
		return ctx
	})

	feature.Assess("Unsupported Warning condition and event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		deadline := time.Now().Add(3 * time.Minute)
		var lastErr error
		for time.Now().Before(deadline) {
			lastErr = func() error {
				group := &marklogicv1.MarklogicGroup{}
				if err := client.Resources().Get(ctx, vacUnsupportedGroup, vacUnsupportedNamespace, group); err != nil {
					return err
				}
				validation := meta.FindStatusCondition(group.Status.Conditions, "StorageAttributesValidated")
				if validation == nil || validation.Reason != "VACUnsupported" || validation.Status != metav1.ConditionFalse || validation.ObservedGeneration != group.Generation {
					return fmt.Errorf("unexpected VAC validation condition: %+v", validation)
				}
				if ready := meta.FindStatusCondition(group.Status.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionTrue {
					return fmt.Errorf("group must remain Ready, got %+v", ready)
				}
				events := &corev1.EventList{}
				if err := client.Resources(vacUnsupportedNamespace).List(ctx, events); err != nil {
					return err
				}
				for _, event := range events.Items {
					if event.InvolvedObject.Kind == "MarklogicGroup" && event.InvolvedObject.Name == vacUnsupportedGroup &&
						event.Type == corev1.EventTypeWarning && event.Reason == "VACUnsupported" {
						return nil
					}
				}
				return fmt.Errorf("Warning event VACUnsupported not found")
			}()
			if lastErr == nil {
				return ctx
			}
			time.Sleep(10 * time.Second)
		}
		t.Fatalf("unsupported VAC environment was not reported as expected: %v", lastErr)
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		utils.DeleteNS(ctx, c, vacUnsupportedNamespace)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
