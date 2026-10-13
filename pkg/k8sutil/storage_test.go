package k8sutil

import (
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func storageWithClass(className string) *marklogicv1.Storage {
	return &marklogicv1.Storage{VolumeAttributesClassName: &className}
}

func TestDay1VolumeAttributesClassProvisioning(t *testing.T) {
	tests := []struct {
		name             string
		clusterStorage   *marklogicv1.Storage
		groupStorage     *marklogicv1.Storage
		groupPersistence *marklogicv1.Persistence
		wantClass        string
	}{
		{name: "unconfigured"},
		{name: "empty cluster storage", clusterStorage: &marklogicv1.Storage{}},
		{name: "empty cluster class", clusterStorage: storageWithClass("")},
		{name: "cluster inheritance", clusterStorage: storageWithClass("ml-standard"), wantClass: "ml-standard"},
		{name: "empty group storage inherits", clusterStorage: storageWithClass("ml-standard"), groupStorage: &marklogicv1.Storage{}, wantClass: "ml-standard"},
		{name: "group override", clusterStorage: storageWithClass("ml-standard"), groupStorage: storageWithClass("ml-premium"), wantClass: "ml-premium"},
		{name: "group only", groupStorage: storageWithClass("ml-premium"), wantClass: "ml-premium"},
		{name: "explicit opt out", clusterStorage: storageWithClass("ml-standard"), groupStorage: storageWithClass("")},
		{
			name:             "persistence override still inherits class",
			clusterStorage:   storageWithClass("ml-standard"),
			groupPersistence: &marklogicv1.Persistence{Enabled: true, Size: "50Gi", StorageClassName: "other"},
			wantClass:        "ml-standard",
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
					Storage:       test.clusterStorage,
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{
						{Name: "dnode", Replicas: &replicas, IsBootstrap: true, GroupConfig: &marklogicv1.GroupConfig{Name: "Default"}},
						{Name: "enode", Replicas: &replicas, Storage: test.groupStorage, Persistence: test.groupPersistence, GroupConfig: &marklogicv1.GroupConfig{Name: "query"}},
					},
				},
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
				if index == 1 && test.groupPersistence != nil && claim.Spec.Resources.Requests.Storage().String() != "50Gi" {
					t.Fatal("class inheritance must not change group persistence")
				}
				if group.Spec.Storage != nil && group.Spec.Storage.VolumeAttributesClassName != nil && wantClass != "" {
					*group.Spec.Storage.VolumeAttributesClassName = "changed-after-generation"
					assertPVCClass(t, claim, wantClass)
					if *params.Storage.VolumeAttributesClassName != wantClass {
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
	group.Spec.Storage = storageWithClass("ml-standard")
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
		if cluster.Spec.Storage != nil && cluster.Spec.Storage.VolumeAttributesClassName != nil {
			wantClass = *cluster.Spec.Storage.VolumeAttributesClassName
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
