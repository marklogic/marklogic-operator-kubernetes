package k8sutil

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMarklogicClusterReconcileOwnerReferences(t *testing.T) {
	for _, populatedTypeMeta := range []bool{false, true} {
		t.Run(fmt.Sprintf("populatedTypeMeta=%t", populatedTypeMeta), func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := marklogicv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default", UID: "cluster-uid"},
			}
			if populatedTypeMeta {
				cluster.TypeMeta = metav1.TypeMeta{APIVersion: marklogicv1.GroupVersion.String(), Kind: "MarklogicCluster"}
			}
			cluster.Spec.Persistence = &marklogicv1.Persistence{Enabled: true, Size: "20Gi"}
			cluster.Spec.MarkLogicGroups = []*marklogicv1.MarklogicGroups{{Name: "group", IsBootstrap: true}}
			apiClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
			creates, updates, apiReads := 0, 0, 0
			validateOwnerReferences := func(object client.Object) error {
				return apivalidation.ValidateOwnerReferences(object.GetOwnerReferences(), field.NewPath("metadata", "ownerReferences")).ToAggregate()
			}
			controllerClient := interceptor.NewClient(apiClient, interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if err := validateOwnerReferences(object); err != nil {
						return err
					}
					creates++
					return underlying.Create(ctx, object, options...)
				},
				Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					if err := validateOwnerReferences(object); err != nil {
						return err
					}
					updates++
					return underlying.Update(ctx, object, options...)
				},
			})
			apiReader := interceptor.NewClient(apiClient, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					apiReads++
					if err := underlying.Get(ctx, key, object, options...); err != nil {
						return err
					}
					if parent, ok := object.(*marklogicv1.MarklogicCluster); ok {
						parent.TypeMeta = metav1.TypeMeta{}
					}
					return nil
				},
			})
			clusterContext := &ClusterContext{
				Ctx: ctx, Client: controllerClient, APIReader: apiReader, Scheme: scheme,
				MarklogicCluster: cluster, ReqLogger: logr.Discard(),
			}
			for _, replicas := range []int32{1, 2} {
				cluster.Spec.MarkLogicGroups[0].Replicas = &replicas
				if _, err := clusterContext.ReconsileMarklogicCluster(); err != nil {
					t.Fatal(err)
				}
				group := &marklogicv1.MarklogicGroup{}
				if err := apiClient.Get(ctx, types.NamespacedName{Name: "group", Namespace: cluster.Namespace}, group); err != nil {
					t.Fatal(err)
				}
				if group.Spec.Replicas == nil || *group.Spec.Replicas != replicas {
					t.Fatalf("child replicas must follow the reconciled parent: got %v, want %d", group.Spec.Replicas, replicas)
				}
				owners := group.GetOwnerReferences()
				if len(owners) != 1 {
					t.Fatalf("expected one cluster owner, got %v", owners)
				}
				owner := owners[0]
				if owner.APIVersion != marklogicv1.GroupVersion.String() || owner.Kind != "MarklogicCluster" ||
					owner.Name != cluster.Name || owner.UID != cluster.UID || owner.Controller == nil || !*owner.Controller {
					t.Fatalf("invalid cluster owner reference: %+v", owner)
				}
			}
			if creates != 1 || updates != 1 || apiReads != 0 {
				t.Fatalf("expected one create/update using the reconciled parent and cached client: creates=%d updates=%d apiReads=%d", creates, updates, apiReads)
			}
		})
	}
}

func TestMarklogicClusterReconcileVACOptOut(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	className := "vac-quick-performance"
	replicas := int32(2)
	cluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "vac-quick", Namespace: "vac-quick-test", UID: "cluster-uid"},
	}
	cluster.Spec.Persistence = &marklogicv1.Persistence{
		Enabled:                   true,
		Size:                      "20Gi",
		VolumeAttributesClassName: &className,
	}
	cluster.Spec.MarkLogicGroups = []*marklogicv1.MarklogicGroups{{
		Name: "vac-quick-node", Replicas: &replicas, IsBootstrap: true,
	}}
	apiClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	clusterContext := &ClusterContext{
		Ctx: ctx, Client: apiClient, Scheme: scheme,
		MarklogicCluster: cluster, ReqLogger: logr.Discard(),
	}
	if _, err := clusterContext.ReconsileMarklogicCluster(); err != nil {
		t.Fatal(err)
	}
	groupKey := types.NamespacedName{Name: "vac-quick-node", Namespace: cluster.Namespace}
	group := &marklogicv1.MarklogicGroup{}
	if err := apiClient.Get(ctx, groupKey, group); err != nil {
		t.Fatal(err)
	}
	if group.Spec.Persistence == nil || group.Spec.Persistence.VolumeAttributesClassName == nil {
		t.Fatal("initial child must inherit the selected VAC")
	}
	cluster.Spec.Persistence.VolumeAttributesClassName = nil
	if _, err := clusterContext.ReconsileMarklogicCluster(); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Get(ctx, groupKey, group); err != nil {
		t.Fatal(err)
	}
	if group.Spec.Persistence == nil || group.Spec.Persistence.VolumeAttributesClassName != nil {
		t.Fatal("child must inherit parent VAC opt-out")
	}
	cluster.Spec.Persistence.VolumeAttributesClassName = &className
	if _, err := clusterContext.ReconsileMarklogicCluster(); err != nil {
		t.Fatal(err)
	}
	group = &marklogicv1.MarklogicGroup{}
	if err := apiClient.Get(ctx, groupKey, group); err != nil {
		t.Fatal(err)
	}
	if group.Spec.Persistence.VolumeAttributesClassName == nil {
		t.Fatal("child must inherit the reselected VAC")
	}
	cluster.Spec.Persistence.VolumeAttributesClassName = nil
	if _, err := clusterContext.ReconsileMarklogicCluster(); err != nil {
		t.Fatal(err)
	}
	group = &marklogicv1.MarklogicGroup{}
	if err := apiClient.Get(ctx, groupKey, group); err != nil {
		t.Fatal(err)
	}
	if group.Spec.Persistence.VolumeAttributesClassName != nil {
		t.Fatal("child must inherit repeated opt-out without a controller restart")
	}
}

func TestMarklogicGroupPatchDetectsVACOptOut(t *testing.T) {
	className := "vac-quick-performance"
	current := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "vac-quick-node", Namespace: "vac-quick-test"},
		Spec: marklogicv1.MarklogicGroupSpec{
			Persistence: &marklogicv1.Persistence{
				Enabled:                   true,
				Size:                      "20Gi",
				VolumeAttributesClassName: &className,
			},
		},
	}
	if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(current); err != nil {
		t.Fatal(err)
	}
	desired := current.DeepCopy()
	desired.Annotations = nil
	desired.Spec.Persistence.VolumeAttributesClassName = nil
	diff, err := patch.DefaultPatchMaker.Calculate(current, desired,
		patch.IgnoreStatusFields(),
		patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
		patch.IgnoreField("kind"))
	if err != nil {
		t.Fatal(err)
	}
	if diff.IsEmpty() {
		t.Fatal("removing the desired VAC must trigger a child group update")
	}
}

func TestImmutableMarklogicGroupSpecMismatch(t *testing.T) {
	t.Run("returns nil when isDynamic is unchanged", func(t *testing.T) {
		current := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: true},
		}
		desired := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: true},
		}

		if err := immutableMarklogicGroupSpecMismatch(current, desired); err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	})

	t.Run("returns actionable error when isDynamic changes", func(t *testing.T) {
		current := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: false},
		}
		desired := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: true},
		}

		err := immutableMarklogicGroupSpecMismatch(current, desired)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "cannot change isDynamic") {
			t.Fatalf("expected immutable field error, got %v", err)
		}
		if !strings.Contains(err.Error(), "delete the child MarklogicGroup") {
			t.Fatalf("expected actionable remediation in error, got %v", err)
		}
	})
}
