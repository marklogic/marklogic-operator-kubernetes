package k8sutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func storageWithClass(className string) *marklogicv1.Persistence {
	return &marklogicv1.Persistence{Enabled: true, Size: "20Gi", StorageClassName: "gp3", VolumeAttributesClassName: &className}
}

func TestDay1VolumeAttributesClassProvisioning(t *testing.T) {
	tests := []struct {
		name             string
		clusterStorage   *marklogicv1.Persistence
		groupPersistence *marklogicv1.Persistence
		wantClass        string
	}{
		{name: "unconfigured"},
		{name: "empty cluster class", clusterStorage: storageWithClass("")},
		{name: "cluster inheritance", clusterStorage: storageWithClass("ml-standard"), wantClass: "ml-standard"},
		{name: "group override", clusterStorage: storageWithClass("ml-standard"), groupPersistence: storageWithClass("ml-premium"), wantClass: "ml-premium"},
		{name: "group only", groupPersistence: storageWithClass("ml-premium"), wantClass: "ml-premium"},
		{name: "explicit opt out", clusterStorage: storageWithClass("ml-standard"), groupPersistence: storageWithClass("")},
		{
			name:             "persistence override does not inherit class",
			clusterStorage:   storageWithClass("ml-standard"),
			groupPersistence: &marklogicv1.Persistence{Enabled: true, Size: "50Gi", StorageClassName: "other"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			replicas := int32(3)
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: "testns"},
				Spec: marklogicv1.MarklogicClusterSpec{
					Image:         "marklogic:12.0.3",
					ClusterDomain: "cluster.local",
					Persistence:   &marklogicv1.Persistence{Enabled: true, Size: "20Gi", StorageClassName: "gp3"},
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{
						{Name: "dnode", Replicas: &replicas, IsBootstrap: true, GroupConfig: &marklogicv1.GroupConfig{Name: "Default"}},
						{Name: "enode", Replicas: &replicas, Persistence: test.groupPersistence, GroupConfig: &marklogicv1.GroupConfig{Name: "query"}},
					},
				},
			}
			if test.clusterStorage != nil {
				cluster.Spec.Persistence = test.clusterStorage
			}
			context := &ClusterContext{MarklogicCluster: cluster}
			clusterParams := generateMarkLogicClusterParams(cluster)
			for index := range cluster.Spec.MarkLogicGroups {
				params := generateMarkLogicGroupParams(cluster, index, clusterParams)
				group := context.GenerateMarkLogicGroupDef(cluster, index, params)
				wantClass := test.wantClass
				if index == 0 {
					wantClass = ""
					if test.clusterStorage != nil && test.clusterStorage.VolumeAttributesClassName != nil {
						wantClass = *test.clusterStorage.VolumeAttributesClassName
					}
				}
				claim := generateStatefulSetsParams(group).PersistentVolumeClaim
				if claim.Name != "datadir" {
					t.Fatalf("expected primary datadir claim, got %q", claim.Name)
				}
				assertPVCClass(t, claim, wantClass)
				if index == 1 && test.groupPersistence != nil && claim.Spec.Resources.Requests.Storage().String() != test.groupPersistence.Size {
					t.Fatal("VAC selection must preserve the complete group persistence override")
				}
				if group.Spec.Persistence.VolumeAttributesClassName != nil && wantClass != "" {
					*group.Spec.Persistence.VolumeAttributesClassName = "changed-after-generation"
					assertPVCClass(t, claim, wantClass)
					if *params.Persistence.VolumeAttributesClassName != wantClass {
						t.Fatal("generated child storage must not share its class pointer with parameters")
					}
				}
			}
		})
	}
}

func assertPVCClass(t *testing.T, claim corev1.PersistentVolumeClaim, want string) {
	t.Helper()
	if want == "" {
		if claim.Spec.VolumeAttributesClassName != nil {
			t.Fatalf("expected no class reference, got %q", *claim.Spec.VolumeAttributesClassName)
		}
	} else if claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != want {
		t.Fatalf("expected class %q, got %v", want, claim.Spec.VolumeAttributesClassName)
	}
	encoded, err := json.Marshal(claim.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	_, present := fields["volumeAttributesClassName"]
	if present != (want != "") {
		t.Fatalf("unexpected serialized class field: %s", encoded)
	}
}

func TestDay1StorageStandaloneAndTemplateIsolation(t *testing.T) {
	replicas := int32(2)
	auxiliary := []corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "logsdir"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fluent-bit"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: storageWithClass("logs-profile").VolumeAttributesClassName}},
	}
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns"},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:                           "dnode",
			Replicas:                       &replicas,
			Image:                          "marklogic:12.0.3",
			Persistence:                    &marklogicv1.Persistence{Enabled: true, Size: "20Gi"},
			GroupConfig:                    &marklogicv1.GroupConfig{Name: "Default"},
			HugePages:                      &marklogicv1.HugePages{},
			LogCollection:                  &marklogicv1.LogCollection{},
			AdditionalVolumeClaimTemplates: &auxiliary,
		},
	}
	baseline := generateStatefulSetsDef(group.ObjectMeta, generateStatefulSetsParams(group), metav1.OwnerReference{}, generateContainerParams(group))
	group.Spec.Persistence.VolumeAttributesClassName = storageWithClass("ml-standard").VolumeAttributesClassName
	configured := generateStatefulSetsDef(group.ObjectMeta, generateStatefulSetsParams(group), metav1.OwnerReference{}, generateContainerParams(group))
	assertPVCClass(t, configured.Spec.VolumeClaimTemplates[0], "ml-standard")
	if !reflect.DeepEqual(configured.Spec.VolumeClaimTemplates[1:], auxiliary) {
		t.Fatal("VAC must not alter auxiliary or Fluent Bit claim templates")
	}
	if !reflect.DeepEqual(baseline.Spec.Template, configured.Spec.Template) {
		t.Fatal("VAC must not change the Pod template")
	}
	for _, persistence := range []*marklogicv1.Persistence{nil, {Enabled: false, Size: "20Gi"}} {
		group.Spec.Persistence = persistence
		params := generateStatefulSetsParams(group)
		if params.PersistentVolumeClaim.Spec.VolumeAttributesClassName != nil {
			t.Fatal("disabled persistence must not receive a VAC")
		}
		statefulSet := generateStatefulSetsDef(group.ObjectMeta, params, metav1.OwnerReference{}, generateContainerParams(group))
		if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, auxiliary) {
			t.Fatal("disabled persistence must not create a primary data claim")
		}
	}
}

func TestDay1VACStatefulSetReconciliation(t *testing.T) {
	tests := []struct {
		name           string
		liveStorage    *marklogicv1.Persistence
		desiredStorage *marklogicv1.Persistence
		create         bool
	}{
		{name: "create with class", desiredStorage: storageWithClass("ml-standard"), create: true},
		{name: "create without class", create: true},
		{name: "create opted out", desiredStorage: storageWithClass(""), create: true},
		{name: "changed class", liveStorage: storageWithClass("ml-standard"), desiredStorage: storageWithClass("ml-premium")},
		{name: "newly selected class", desiredStorage: storageWithClass("ml-standard")},
		{name: "removed storage", liveStorage: storageWithClass("ml-standard")},
		{name: "explicit opt out", liveStorage: storageWithClass("ml-standard"), desiredStorage: storageWithClass("")},
		{name: "unchanged class", liveStorage: storageWithClass("ml-standard"), desiredStorage: storageWithClass("ml-standard")},
		{name: "unconfigured"},
	}
	for _, test := range tests {
		for _, desiredReplicas := range []int32{1, 3} {
			t.Run(fmt.Sprintf("%s/replicas-%d", test.name, desiredReplicas), func(t *testing.T) {
				scheme := runtime.NewScheme()
				for _, addToScheme := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
					if err := addToScheme(scheme); err != nil {
						t.Fatal(err)
					}
				}
				liveReplicas := int32(1)
				auxiliary := []corev1.PersistentVolumeClaim{
					{ObjectMeta: metav1.ObjectMeta{Name: "logsdir"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: storageWithClass("logs-profile").VolumeAttributesClassName}},
				}
				group := &marklogicv1.MarklogicGroup{
					ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "dnode-uid"},
					Spec: marklogicv1.MarklogicGroupSpec{
						Name:                           "dnode",
						Replicas:                       &liveReplicas,
						Image:                          "marklogic:12.0.3",
						Persistence:                    &marklogicv1.Persistence{Enabled: true, Size: "20Gi"},
						GroupConfig:                    &marklogicv1.GroupConfig{Name: "Default"},
						HugePages:                      &marklogicv1.HugePages{},
						LogCollection:                  &marklogicv1.LogCollection{},
						AdditionalVolumeClaimTemplates: &auxiliary,
					},
				}
				if test.liveStorage != nil {
					group.Spec.Persistence.VolumeAttributesClassName = test.liveStorage.VolumeAttributesClassName
				}
				labels := getSelectorLabelsByComponent(group.Spec.Name, group.Spec.IsDynamic)
				labels["app.kubernetes.io/instance"] = group.Spec.Name
				labels["app.kubernetes.io/component"] = getMarkLogicComponentLabel(group.Spec.IsDynamic)
				current := generateStatefulSetsDef(
					generateObjectMeta(group.Spec.Name, group.Namespace, labels, nil),
					generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
				).DeepCopy()
				group.Spec.Persistence.VolumeAttributesClassName = nil
				if test.desiredStorage != nil {
					group.Spec.Persistence.VolumeAttributesClassName = test.desiredStorage.VolumeAttributesClassName
				}
				group.Spec.Replicas = &desiredReplicas
				newClaimTemplate := generateStatefulSetsParams(group).PersistentVolumeClaim
				precreate := !test.create && !reflect.DeepEqual(newClaimTemplate.Spec.VolumeAttributesClassName, current.Spec.VolumeClaimTemplates[0].Spec.VolumeAttributesClassName)
				objects := []client.Object{group}
				if !test.create {
					objects = append(objects, current)
				}
				fakeClient := fake.NewClientBuilder().
					WithScheme(scheme).
					WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
					WithObjects(objects...).
					WithInterceptorFuncs(interceptor.Funcs{
						Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.UpdateOption) error {
							if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
								if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
									return fmt.Errorf("StatefulSet volumeClaimTemplates are immutable")
								}
								if precreate {
									for ordinal := liveReplicas; ordinal < *statefulSet.Spec.Replicas; ordinal++ {
										claim := &corev1.PersistentVolumeClaim{}
										key := client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("datadir-%s-%d", current.Name, ordinal)}
										if err := underlying.Get(ctx, key, claim); err != nil {
											return fmt.Errorf("PVC must exist before scale-up: %w", err)
										}
										if !reflect.DeepEqual(claim.Spec, newClaimTemplate.Spec) {
											return fmt.Errorf("scale-up PVC does not use the current persistence and VAC settings")
										}
									}
								}
							}
							return underlying.Update(ctx, object, options...)
						},
					}).Build()
				operatorContext := &OperatorContext{
					Ctx: context.Background(), Client: fakeClient, Scheme: scheme,
					MarklogicGroup: group, Recorder: record.NewFakeRecorder(10),
				}
				if _, err := operatorContext.ReconcileStatefulset(); err != nil {
					t.Fatal(err)
				}
				actual := &appsv1.StatefulSet{}
				if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
					t.Fatal(err)
				}
				wantStorage := test.liveStorage
				if test.create {
					wantStorage = test.desiredStorage
				}
				wantClass := ""
				if wantStorage != nil && wantStorage.VolumeAttributesClassName != nil {
					wantClass = *wantStorage.VolumeAttributesClassName
				}
				assertPVCClass(t, actual.Spec.VolumeClaimTemplates[0], wantClass)
				if !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates[1:], auxiliary) {
					t.Fatal("reconciliation must not alter auxiliary claim templates")
				}
				if actual.Spec.Replicas == nil || *actual.Spec.Replicas != desiredReplicas {
					t.Fatalf("expected %d replicas, got %v", desiredReplicas, actual.Spec.Replicas)
				}
				claims := &corev1.PersistentVolumeClaimList{}
				if err := fakeClient.List(operatorContext.Ctx, claims, client.InNamespace(group.Namespace)); err != nil {
					t.Fatal(err)
				}
				wantClaims := 0
				if precreate {
					wantClaims = int(desiredReplicas - liveReplicas)
				}
				if len(claims.Items) != wantClaims {
					t.Fatalf("expected %d precreated data PVCs, got %d", wantClaims, len(claims.Items))
				}
				for _, claim := range claims.Items {
					if !reflect.DeepEqual(claim.Spec, newClaimTemplate.Spec) {
						t.Fatal("new data PVC must use the current persistence and VAC settings")
					}
				}
			})
		}
	}
}

func TestAdditionalVolumeAttributesClassInheritance(t *testing.T) {
	clusterTemplates := []corev1.PersistentVolumeClaim{generatePVCTemplate(storageWithClass("logs-standard")), generatePVCTemplate(storageWithClass("backup-standard"))}
	clusterTemplates[0].Name, clusterTemplates[1].Name = "logsdir", "backup"
	override := []corev1.PersistentVolumeClaim{*clusterTemplates[0].DeepCopy()}
	override[0].Spec.VolumeAttributesClassName = storageWithClass("logs-premium").VolumeAttributesClassName
	empty := []corev1.PersistentVolumeClaim{}
	for _, test := range []struct {
		name     string
		override *[]corev1.PersistentVolumeClaim
		want     []corev1.PersistentVolumeClaim
	}{
		{name: "inherit complete cluster list", want: clusterTemplates},
		{name: "replace rather than merge list", override: &override, want: override},
		{name: "explicit empty list", override: &empty, want: empty},
	} {
		t.Run(test.name, func(t *testing.T) {
			replicas := int32(2)
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: "testns"},
				Spec: marklogicv1.MarklogicClusterSpec{
					Persistence: storageWithClass("primary-standard"), AdditionalVolumeClaimTemplates: &clusterTemplates,
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{{
						Name: "dnode", IsBootstrap: true, Replicas: &replicas,
						Persistence: &marklogicv1.Persistence{Enabled: false, Size: "20Gi"}, AdditionalVolumeClaimTemplates: test.override,
					}},
				},
			}
			context := &ClusterContext{MarklogicCluster: cluster}
			parameters := generateMarkLogicGroupParams(cluster, 0, generateMarkLogicClusterParams(cluster))
			group := context.GenerateMarkLogicGroupDef(cluster, 0, parameters)
			actual := generateStatefulSetsParams(group)
			if !reflect.DeepEqual(*actual.AdditionalVolumeClaimTemplates, test.want) {
				t.Fatal("additional profiles must follow complete list inheritance/override independently of persistence")
			}
			if actual.PersistentVolumeClaim.Name != "" {
				t.Fatal("disabled primary persistence must not create datadir")
			}
			if len(test.want) > 0 {
				*(*actual.AdditionalVolumeClaimTemplates)[0].Spec.VolumeAttributesClassName = "mutated"
				if *test.want[0].Spec.VolumeAttributesClassName == "mutated" {
					t.Fatal("generated additional templates must not share class pointers with the CR")
				}
			}
		})
	}
}

func TestAdditionalVolumeAttributesClassReconciliation(t *testing.T) {
	for _, test := range []struct {
		name           string
		create         bool
		replicas       int32
		disablePrimary bool
		retained       bool
		failCreate     bool
		optOut         bool
		changePrimary  bool
		partialCreate  bool
	}{
		{name: "initial independent profiles", create: true, replicas: 3},
		{name: "existing immutable templates", replicas: 1},
		{name: "scale multiple profiles", replicas: 3},
		{name: "scale primary and additional profiles", replicas: 3, changePrimary: true},
		{name: "additional volumes without primary persistence", replicas: 3, disablePrimary: true},
		{name: "reuse retained additional claim", replicas: 3, retained: true},
		{name: "additional creation failure blocks replicas", replicas: 3, failCreate: true},
		{name: "partial additional creation blocks replicas", replicas: 3, partialCreate: true},
		{name: "additional opt out", replicas: 3, optOut: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, register := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
				if err := register(scheme); err != nil {
					t.Fatal(err)
				}
			}
			liveReplicas := int32(1)
			additional := []corev1.PersistentVolumeClaim{generatePVCTemplate(storageWithClass("logs-standard")), generatePVCTemplate(storageWithClass(""))}
			additional[0].Name, additional[1].Name = "logsdir", "backup"
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid"},
				Spec: marklogicv1.MarklogicGroupSpec{
					Name: "dnode", Replicas: &liveReplicas, Persistence: storageWithClass("ml-standard"),
					AdditionalVolumeClaimTemplates: &additional,
					GroupConfig:                    &marklogicv1.GroupConfig{Name: "Default"}, HugePages: &marklogicv1.HugePages{}, LogCollection: &marklogicv1.LogCollection{},
				},
			}
			group.Spec.Persistence.Enabled = !test.disablePrimary
			current := generateStatefulSetsDef(
				generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabels(group.Spec.Name), nil),
				generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
			).DeepCopy()
			objects := []client.Object{group}
			if !test.create {
				objects = append(objects, current)
			}
			var retained *corev1.PersistentVolumeClaim
			if test.retained {
				retained = additional[0].DeepCopy()
				retained.Name, retained.Namespace = "logsdir-dnode-1", group.Namespace
				retained.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(group.UID)}
				objects = append(objects, retained)
			}
			additional[0].Spec.VolumeAttributesClassName = storageWithClass("logs-premium").VolumeAttributesClassName
			additional[1].Spec.VolumeAttributesClassName = storageWithClass("backup-profile").VolumeAttributesClassName
			if test.optOut {
				additional[0].Spec.VolumeAttributesClassName = storageWithClass("").VolumeAttributesClassName
			}
			if test.changePrimary {
				group.Spec.Persistence.VolumeAttributesClassName = storageWithClass("ml-premium").VolumeAttributesClassName
			}
			group.Spec.Replicas = &test.replicas
			desired := generateStatefulSetsParams(group)
			expectedTemplates := append([]corev1.PersistentVolumeClaim{}, (*desired.AdditionalVolumeClaimTemplates)...)
			if test.changePrimary {
				expectedTemplates = append(expectedTemplates, desired.PersistentVolumeClaim)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&marklogicv1.MarklogicGroup{}).WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if _, claim := object.(*corev1.PersistentVolumeClaim); claim && (test.failCreate || test.partialCreate && object.GetName() == "backup-dnode-1") {
							return fmt.Errorf("additional PVC creation denied")
						}
						return underlying.Create(ctx, object, options...)
					},
					Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
							if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
								return fmt.Errorf("claim templates are immutable")
							}
							for _, template := range expectedTemplates {
								for ordinal := liveReplicas; ordinal < *statefulSet.Spec.Replicas; ordinal++ {
									claim := &corev1.PersistentVolumeClaim{}
									if err := underlying.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("%s-dnode-%d", template.Name, ordinal)}, claim); err != nil {
										return fmt.Errorf("all additional claims must exist before scaling: %w", err)
									}
								}
							}
						}
						return underlying.Update(ctx, object, options...)
					},
				}).Build()
			operator := &OperatorContext{Ctx: context.Background(), Client: fakeClient, Scheme: scheme, MarklogicGroup: group, Recorder: record.NewFakeRecorder(10)}
			if _, err := operator.ReconcileStatefulset(); (err != nil) != (test.failCreate || test.partialCreate) {
				t.Fatalf("unexpected reconciliation error: %v", err)
			}
			actual := &appsv1.StatefulSet{}
			if err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
				t.Fatal(err)
			}
			wantReplicas := test.replicas
			if test.failCreate || test.partialCreate {
				wantReplicas = liveReplicas
			}
			if *actual.Spec.Replicas != wantReplicas {
				t.Fatal("PVC preparation must precede replica updates")
			}
			if !test.create && !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
				t.Fatal("existing primary and additional templates must remain unchanged")
			}
			if test.create {
				for index, template := range *desired.AdditionalVolumeClaimTemplates {
					if !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates[len(actual.Spec.VolumeClaimTemplates)-2+index].Spec, template.Spec) {
						t.Fatal("initial additional PVCs must use independently configured classes")
					}
				}
			}
			claims := &corev1.PersistentVolumeClaimList{}
			if err := fakeClient.List(operator.Ctx, claims); err != nil {
				t.Fatal(err)
			}
			for _, claim := range claims.Items {
				if retained != nil && claim.Name == retained.Name {
					if !reflect.DeepEqual(claim.Spec, retained.Spec) {
						t.Fatal("retained additional claim must not be retargeted")
					}
					continue
				}
				matched := false
				for _, template := range expectedTemplates {
					for ordinal := liveReplicas; ordinal < test.replicas; ordinal++ {
						if claim.Name == fmt.Sprintf("%s-dnode-%d", template.Name, ordinal) {
							matched = reflect.DeepEqual(claim.Spec, template.Spec)
						}
					}
				}
				if !matched {
					t.Fatal("each additional scale-up PVC must use only its own template profile")
				}
			}
			if test.optOut && *additional[0].Spec.VolumeAttributesClassName != "" {
				t.Fatal("normalizing an additional opt-out must not mutate the CR")
			}
		})
	}
}

func TestScaleUpVACPrecreationSafety(t *testing.T) {
	tests := []struct {
		name           string
		start          int32
		existing       bool
		unverified     bool
		foreignOwner   bool
		wrongClass     bool
		terminating    bool
		failCreate     bool
		partialCreate  bool
		alreadyExists  bool
		failUpdateOnce bool
		unowned        bool
		wantError      bool
	}{
		{name: "nonzero starting ordinal", start: 5},
		{name: "retained claim is unchanged", existing: true},
		{name: "unverified retained claim blocks scaling", existing: true, unverified: true, wantError: true},
		{name: "foreign owner blocks scaling", existing: true, foreignOwner: true, wantError: true},
		{name: "incompatible retained claim blocks scaling", existing: true, wrongClass: true, wantError: true},
		{name: "terminating retained claim blocks scaling", existing: true, terminating: true, wantError: true},
		{name: "create failure blocks scaling", failCreate: true, wantError: true},
		{name: "retry after partial PVC creation", partialCreate: true},
		{name: "already exists race is reread", alreadyExists: true},
		{name: "retry after replica update failure", failUpdateOnce: true},
		{name: "unowned StatefulSet blocks precreation", unowned: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, addToScheme := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
				if err := addToScheme(scheme); err != nil {
					t.Fatal(err)
				}
			}
			liveReplicas, desiredReplicas := int32(1), int32(3)
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid"},
				Spec: marklogicv1.MarklogicGroupSpec{
					Name: "dnode", Replicas: &liveReplicas, Image: "marklogic:12.0.3",
					Persistence: &marklogicv1.Persistence{
						Enabled: true, Size: "20Gi", StorageClassName: "gp3",
						VolumeAttributesClassName: storageWithClass("ml-standard").VolumeAttributesClassName,
						AccessModes:               []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Annotations:               map[string]string{"storage-test": "preserved"},
					},
					GroupConfig: &marklogicv1.GroupConfig{Name: "Default"},
					HugePages:   &marklogicv1.HugePages{}, LogCollection: &marklogicv1.LogCollection{},
				},
			}
			current := generateStatefulSetsDef(
				generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabels(group.Spec.Name), nil),
				generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
			).DeepCopy()
			current.UID = "statefulset-uid"
			if test.unowned {
				current.OwnerReferences = nil
			}
			current.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: test.start}
			current.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			}
			group.Spec.Persistence.VolumeAttributesClassName = storageWithClass("ml-premium").VolumeAttributesClassName
			group.Spec.Replicas = &desiredReplicas
			newTemplate := generateStatefulSetsParams(group).PersistentVolumeClaim
			objects := []client.Object{group, current}
			active := current.Spec.VolumeClaimTemplates[0].DeepCopy()
			active.Name = fmt.Sprintf("datadir-%s-%d", current.Name, test.start)
			active.Namespace = group.Namespace
			objects = append(objects, active)
			var retained *corev1.PersistentVolumeClaim
			if test.existing {
				retained = active.DeepCopy()
				retained.Name = fmt.Sprintf("datadir-%s-%d", current.Name, test.start+1)
				retained.Annotations[dataPVCGroupUIDAnnotation] = string(group.UID)
				retained.Status.Phase = corev1.ClaimPending
				if test.unverified {
					delete(retained.Annotations, dataPVCGroupUIDAnnotation)
				}
				if test.foreignOwner {
					retained.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", UID: "unrelated-statefulset"}}
				}
				if test.wrongClass {
					className := "unrelated-storage-class"
					retained.Spec.StorageClassName = &className
				}
				if test.terminating {
					now := metav1.Now()
					retained.DeletionTimestamp = &now
					retained.Finalizers = []string{"kubernetes.io/pvc-protection"}
				}
				objects = append(objects, retained)
			}
			failedUpdate := false
			createCalls := 0
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
				WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if _, ok := object.(*corev1.PersistentVolumeClaim); ok {
							createCalls++
							if test.failCreate || test.partialCreate && createCalls == 2 {
								return apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName(), fmt.Errorf("PVC creation denied"))
							}
							if test.alreadyExists {
								if err := underlying.Create(ctx, object, options...); err != nil {
									return err
								}
								return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName())
							}
						}
						return underlying.Create(ctx, object, options...)
					},
					Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
							if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
								return fmt.Errorf("StatefulSet volumeClaimTemplates are immutable")
							}
							if test.failUpdateOnce && !failedUpdate {
								failedUpdate = true
								return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, current.Name, fmt.Errorf("replica update conflict"))
							}
							for ordinal := test.start + liveReplicas; ordinal < test.start+desiredReplicas; ordinal++ {
								claim := &corev1.PersistentVolumeClaim{}
								if err := underlying.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("datadir-%s-%d", current.Name, ordinal)}, claim); err != nil {
									return fmt.Errorf("PVC must exist before scale-up: %w", err)
								}
							}
						}
						return underlying.Update(ctx, object, options...)
					},
				}).Build()
			operatorContext := &OperatorContext{Ctx: context.Background(), Client: fakeClient, Scheme: scheme, MarklogicGroup: group, Recorder: record.NewFakeRecorder(10)}
			_, err := operatorContext.ReconcileStatefulset()
			if test.failUpdateOnce {
				if !apierrors.IsConflict(err) {
					t.Fatalf("expected replica update conflict, got %v", err)
				}
				_, err = operatorContext.ReconcileStatefulset()
			}
			if test.partialCreate {
				if !apierrors.IsForbidden(err) {
					t.Fatalf("expected partial PVC creation failure, got %v", err)
				}
				_, err = operatorContext.ReconcileStatefulset()
			}
			if (err != nil) != test.wantError {
				t.Fatalf("unexpected reconciliation error: %v", err)
			}
			actual := &appsv1.StatefulSet{}
			if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
				t.Fatal(err)
			}
			wantReplicas := desiredReplicas
			if test.wantError {
				wantReplicas = liveReplicas
			}
			if *actual.Spec.Replicas != wantReplicas {
				t.Fatalf("expected %d replicas, got %d", wantReplicas, *actual.Spec.Replicas)
			}
			if !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) || !reflect.DeepEqual(actual.Spec.Ordinals, current.Spec.Ordinals) || !reflect.DeepEqual(actual.Spec.PersistentVolumeClaimRetentionPolicy, current.Spec.PersistentVolumeClaimRetentionPolicy) {
				t.Fatal("scale-up must preserve claim templates, starting ordinals, and retention policy")
			}
			if !test.wantError {
				for ordinal := test.start + liveReplicas; ordinal < test.start+desiredReplicas; ordinal++ {
					claim := &corev1.PersistentVolumeClaim{}
					if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("datadir-%s-%d", current.Name, ordinal)}, claim); err != nil {
						t.Fatal(err)
					}
					if retained != nil && claim.Name == retained.Name {
						if !reflect.DeepEqual(claim.Spec, retained.Spec) {
							t.Fatal("reactivated Pending PVC must not be retargeted")
						}
					} else if !reflect.DeepEqual(claim.Spec, newTemplate.Spec) || claim.Annotations["storage-test"] != "preserved" || claim.Annotations[dataPVCGroupUIDAnnotation] != string(group.UID) || len(claim.OwnerReferences) != 0 {
						t.Fatal("new PVC must preserve current settings, metadata, and retention semantics")
					}
				}
			}
			unchanged := &corev1.PersistentVolumeClaim{}
			if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(active), unchanged); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unchanged.Spec, active.Spec) {
				t.Fatal("existing active data PVC must remain unchanged")
			}
		})
	}
}

func TestDay1SampleProvisioning(t *testing.T) {
	sample, err := os.Open("../../config/samples/volume-attributes-class.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer sample.Close()
	decoder := k8syaml.NewYAMLOrJSONDecoder(sample, 4096)
	clusters := 0
	claims := 0
	for {
		var cluster marklogicv1.MarklogicCluster
		if err := decoder.Decode(&cluster); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		context := &ClusterContext{MarklogicCluster: &cluster}
		clusterParams := generateMarkLogicClusterParams(&cluster)
		wantClass := ""
		if cluster.Spec.Persistence != nil && cluster.Spec.Persistence.VolumeAttributesClassName != nil {
			wantClass = *cluster.Spec.Persistence.VolumeAttributesClassName
		}
		for index := range cluster.Spec.MarkLogicGroups {
			params := generateMarkLogicGroupParams(&cluster, index, clusterParams)
			group := context.GenerateMarkLogicGroupDef(&cluster, index, params)
			assertPVCClass(t, generateStatefulSetsParams(group).PersistentVolumeClaim, wantClass)
			claims += int(*group.Spec.Replicas)
		}
		clusters++
	}
	if clusters != 2 || claims != 4 {
		t.Fatalf("expected two sample clusters and four data claims, got %d and %d", clusters, claims)
	}
}
