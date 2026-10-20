package v1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestPersistenceVolumeAttributesClassJSON(t *testing.T) {
	for _, className := range []*string{nil, pointerToClass(""), pointerToClass("ml-standard")} {
		group := MarklogicGroup{Spec: MarklogicGroupSpec{
			Persistence: &Persistence{Enabled: true, Size: "10Gi", VolumeAttributesClassName: className},
			AdditionalVolumeClaimTemplates: &[]corev1.PersistentVolumeClaim{{
				Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: pointerToClass("ml-premium")},
			}},
		}}
		encoded, err := json.Marshal(group)
		if err != nil {
			t.Fatal(err)
		}
		var decoded MarklogicGroup
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.Spec.Persistence.VolumeAttributesClassName, className) || *(*decoded.Spec.AdditionalVolumeClaimTemplates)[0].Spec.VolumeAttributesClassName != "ml-premium" {
			t.Fatal("primary and additional volume selections must round-trip independently")
		}
		var fields map[string]json.RawMessage
		persistence, err := json.Marshal(group.Spec.Persistence)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(persistence, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["volumeAttributesClassName"]; present != (className != nil) {
			t.Fatal("nil must be omitted while explicit empty selections remain distinguishable")
		}
	}
}

func pointerToClass(value string) *string { return &value }

func TestVolumeAttributesClassStatusDeepCopy(t *testing.T) {
	storage := &StorageStatus{VolumeAttributesClass: &VolumeAttributesClassStatus{
		State:  VolumeAttributesClassInProgress,
		PVCs:   []PVCVolumeAttributesClassStatus{{Name: "data-0", CurrentClassName: pointerToClass("standard"), RequestedClassName: pointerToClass("premium"), ModifyVolumeError: true}},
		Groups: []GroupVolumeAttributesClassStatus{{Name: "data", State: VolumeAttributesClassPending}},
	}}
	storage.UnusedPVCs = []UnusedPVCStatus{{Name: "data-1", VolumeAttributesClassName: pointerToClass("premium")}}
	storage.Operation = &StorageOperationStatus{ID: "dispatch", PVCName: "data-0", PVCUID: "claim-uid"}
	group := (&MarklogicGroup{Status: MarklogicGroupStatus{Storage: storage}}).DeepCopy()
	cluster := (&MarklogicCluster{Status: MarklogicClusterStatus{Storage: storage}}).DeepCopy()
	storage.Operation.ID = "changed"
	storage.UnusedPVCs[0].Name = "changed"
	*storage.UnusedPVCs[0].VolumeAttributesClassName = "changed"
	storage.VolumeAttributesClass.PVCs[0].Name = "changed"
	storage.VolumeAttributesClass.PVCs[0].ModifyVolumeError = false
	*storage.VolumeAttributesClass.PVCs[0].CurrentClassName = "changed"
	*storage.VolumeAttributesClass.PVCs[0].RequestedClassName = "changed"
	storage.VolumeAttributesClass.Groups[0].Name = "changed"
	for _, copied := range []*StorageStatus{group.Status.Storage, cluster.Status.Storage} {
		if copied.Operation.ID != "dispatch" {
			t.Fatal("storage operation guards must deep-copy independently")
		}
		if copied.UnusedPVCs[0].Name != "data-1" || *copied.UnusedPVCs[0].VolumeAttributesClassName != "premium" {
			t.Fatal("unused claim reports must deep-copy their records and class pointers")
		}
		if copied.VolumeAttributesClass.PVCs[0].Name != "data-0" || !copied.VolumeAttributesClass.PVCs[0].ModifyVolumeError || *copied.VolumeAttributesClass.PVCs[0].CurrentClassName != "standard" || *copied.VolumeAttributesClass.PVCs[0].RequestedClassName != "premium" || copied.VolumeAttributesClass.Groups[0].Name != "data" {
			t.Fatal("VAC progress must deep-copy all nested observations")
		}
	}
}

func TestStorageDeepCopy(t *testing.T) {
	tests := []struct {
		name string
		copy func(*Persistence) *Persistence
	}{
		{
			name: "cluster",
			copy: func(storage *Persistence) *Persistence {
				cluster := &MarklogicCluster{Spec: MarklogicClusterSpec{Persistence: storage}}
				return cluster.DeepCopy().Spec.Persistence
			},
		},
		{
			name: "embedded group",
			copy: func(storage *Persistence) *Persistence {
				cluster := &MarklogicCluster{Spec: MarklogicClusterSpec{
					MarkLogicGroups: []*MarklogicGroups{{Persistence: storage}},
				}}
				return cluster.DeepCopy().Spec.MarkLogicGroups[0].Persistence
			},
		},
		{
			name: "standalone group",
			copy: func(storage *Persistence) *Persistence {
				group := &MarklogicGroup{Spec: MarklogicGroupSpec{Persistence: storage}}
				return group.DeepCopy().Spec.Persistence
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.copy(nil) != nil {
				t.Fatal("expected omitted storage to stay nil")
			}
			empty := test.copy(&Persistence{})
			if empty == nil || empty.VolumeAttributesClassName != nil {
				t.Fatal("expected an omitted class to stay nil")
			}
			className := "ml-standard"
			original := &Persistence{VolumeAttributesClassName: &className}
			copied := test.copy(original)
			if copied == original || copied.VolumeAttributesClassName == original.VolumeAttributesClassName {
				t.Fatal("storage and class pointers must be copied independently")
			}
			className = "ml-premium"
			if *copied.VolumeAttributesClassName != "ml-standard" {
				t.Fatal("changing the original class must not change its copy")
			}
		})
	}
}

func TestStorageSchemas(t *testing.T) {
	tests := []struct {
		name       string
		nativePath string
		helmPath   string
		embedded   bool
	}{
		{
			name:       "cluster",
			nativePath: "../../config/crd/bases/marklogic.progress.com_marklogicclusters.yaml",
			helmPath:   "../../charts/marklogic-operator-kubernetes/templates/marklogiccluster-crd.yaml",
		},
		{
			name:       "embedded group",
			nativePath: "../../config/crd/bases/marklogic.progress.com_marklogicclusters.yaml",
			helmPath:   "../../charts/marklogic-operator-kubernetes/templates/marklogiccluster-crd.yaml",
			embedded:   true,
		},
		{
			name:       "standalone group",
			nativePath: "../../config/crd/bases/marklogic.progress.com_marklogicgroups.yaml",
			helmPath:   "../../charts/marklogic-operator-kubernetes/templates/marklogicgroup-crd.yaml",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var nativeStorage apiextensionsv1.JSONSchemaProps
			var nativeClaim *apiextensionsv1.JSONSchemaProps
			for _, path := range []string{test.nativePath, test.helmPath} {
				crd := readStorageCRD(t, path)
				parent := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
				if test.embedded {
					parent = *parent.Properties["markLogicGroups"].Items.Schema
				}
				if _, exists := parent.Properties["storage"]; exists {
					t.Fatalf("obsolete separate storage schema in %s", path)
				}
				storage, exists := parent.Properties["persistence"]
				if !exists {
					t.Fatalf("missing storage schema in %s", path)
				}
				for _, required := range parent.Required {
					if required == "persistence" {
						t.Fatalf("storage must be optional in %s", path)
					}
				}
				class := storage.Properties["volumeAttributesClassName"]
				if class.Type != "string" || class.MaxLength == nil || *class.MaxLength != 253 || class.Nullable || class.Default != nil || !reflect.DeepEqual(storage.Required, []string{"size"}) {
					t.Fatalf("unexpected optional class schema in %s: %+v", path, class)
				}
				if path == test.nativePath {
					nativeStorage = storage
				} else if !reflect.DeepEqual(nativeStorage, storage) {
					t.Fatalf("native and Helm storage schemas differ at %s", test.name)
				}
				validateStorageSchema(t, apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"volumeAttributesClassName": class}})
				additional := parent.Properties["additionalVolumeClaimTemplates"].Items.Schema.Properties["spec"].Properties["volumeAttributesClassName"]
				if additional.Type != "string" || additional.Default != nil {
					t.Fatalf("missing optional additional-volume class in %s", path)
				}
				claimSchema := parent.Properties["additionalVolumeClaimTemplates"].Items.Schema
				if path == test.nativePath {
					nativeClaim = claimSchema
				} else if !reflect.DeepEqual(nativeClaim, claimSchema) {
					t.Fatalf("native and Helm additional claim schemas differ at %s", test.name)
				}
				var internal apiextensions.JSONSchemaProps
				if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(claimSchema, &internal, nil); err != nil {
					t.Fatal(err)
				}
				structural, err := schema.NewStructural(&internal)
				if err != nil {
					t.Fatal(err)
				}
				metadata := map[string]any{"name": "logs", "labels": map[string]any{"purpose": "audit"}, "annotations": map[string]any{"storage": "important"}}
				claim := map[string]any{"metadata": metadata}
				pruning.Prune(claim, structural, false)
				if metadata["name"] != "logs" || metadata["labels"] == nil || metadata["annotations"] == nil {
					t.Fatalf("additional claim metadata was pruned by %s: %#v", path, metadata)
				}
			}
		})
	}
}

func readStorageCRD(t *testing.T, path string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "/charts/") {
		functions := template.FuncMap{
			"include": func(string, any) string { return "app.kubernetes.io/name: test" },
			"nindent": func(width int, value string) string { return "\n" + strings.Repeat(" ", width) + value },
		}
		renderer, err := template.New(path).Funcs(functions).Parse(string(contents))
		if err != nil {
			t.Fatal(err)
		}
		var rendered bytes.Buffer
		if err := renderer.Execute(&rendered, nil); err != nil {
			t.Fatal(err)
		}
		contents = rendered.Bytes()
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096).Decode(&crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Schema == nil {
		t.Fatalf("unexpected CRD versions in %s", path)
	}
	return crd
}

func TestVolumeAttributesClassStatusSchemas(t *testing.T) {
	for _, kind := range []string{"cluster", "group"} {
		t.Run(kind, func(t *testing.T) {
			paths := []string{"../../config/crd/bases/marklogic.progress.com_marklogic" + kind + "s.yaml", "../../charts/marklogic-operator-kubernetes/templates/marklogic" + kind + "-crd.yaml"}
			var native apiextensionsv1.JSONSchemaProps
			for index, path := range paths {
				crd := readStorageCRD(t, path)
				status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
				storage := status.Properties["storage"]
				if storage.Properties["volumeAttributesClass"].Type != "object" {
					t.Fatalf("missing VAC status in %s", path)
				}
				if index == 0 {
					native = storage
				} else if !reflect.DeepEqual(native, storage) {
					t.Fatalf("native and Helm VAC status schemas differ for %s", kind)
				}
				var internal apiextensions.JSONSchemaProps
				if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&status, &internal, nil); err != nil {
					t.Fatal(err)
				}
				validator, _, err := validation.NewSchemaValidator(&internal)
				if err != nil {
					t.Fatal(err)
				}
				structural, err := schema.NewStructural(&internal)
				if err != nil {
					t.Fatal(err)
				}
				value := map[string]any{"storage": &StorageStatus{VolumeAttributesClass: &VolumeAttributesClassStatus{
					ObservedGeneration: 2, State: VolumeAttributesClassInProgress, Reason: "VACInProgress", Message: "Waiting for CSI",
					PVCs:   []PVCVolumeAttributesClassStatus{{Name: "data-0", UID: "claim-uid", TemplateName: "data", DesiredClassName: "premium", RequestedClassName: pointerToClass("premium"), CurrentClassName: pointerToClass("standard"), ModifyVolumeError: true, State: VolumeAttributesClassInProgress, Reason: "VACInProgress", Message: "Waiting", LastProgressTime: metav1.Now()}},
					Groups: []GroupVolumeAttributesClassStatus{{Name: "data", ObservedGeneration: 1, State: VolumeAttributesClassInProgress, Reason: "VACInProgress", Message: "Waiting"}},
				}}}
				value["storage"].(*StorageStatus).UnusedPVCs = []UnusedPVCStatus{{Name: "data-1", UID: "unused-uid", TemplateName: "data", Phase: corev1.ClaimBound, VolumeName: "pv-retained", VolumeAttributesClassName: pointerToClass("premium"), Reason: "OutsideReplicaRange", Message: "Retained for review; may contain data", FirstObservedTime: metav1.Now()}}
				value["storage"].(*StorageStatus).Operation = &StorageOperationStatus{ID: "dispatch", PVCName: "data-0", PVCUID: "claim-uid"}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var object, original map[string]any
				if err := json.Unmarshal(encoded, &object); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &original); err != nil {
					t.Fatal(err)
				}
				if failures := validation.ValidateCustomResource(field.NewPath("status"), object, validator); len(failures) != 0 {
					t.Fatalf("valid VAC status rejected by %s: %v", path, failures)
				}
				pruning.Prune(object, structural, true)
				if !reflect.DeepEqual(object, original) {
					t.Fatalf("VAC progress was pruned by %s", path)
				}
				progress := object["storage"].(map[string]any)["volumeAttributesClass"].(map[string]any)
				for _, record := range []map[string]any{progress, progress["pvcs"].([]any)[0].(map[string]any), progress["groups"].([]any)[0].(map[string]any)} {
					previous := record["state"]
					record["state"] = "Unrecognized"
					if failures := validation.ValidateCustomResource(field.NewPath("status"), object, validator); len(failures) == 0 {
						t.Fatalf("unknown VAC states must be rejected by %s", path)
					}
					record["state"] = previous
				}
			}
		})
	}
}

func TestVolumeAttributesClassRBAC(t *testing.T) {
	for _, namespaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespaced=%t", namespaced), func(t *testing.T) {
			path := "../../config/rbac/role.yaml"
			if namespaced {
				path = "../../config/rbac/role_namespaced.yaml"
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertVolumeAttributesClassRBAC(t, contents, namespaced)
		})
	}
}

func TestVolumeAttributesClassHelmifyRBAC(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("helmify post-processing requires python3")
	}
	script, err := filepath.Abs("../../hack/helmify-post-process.sh")
	if err != nil {
		t.Fatal(err)
	}
	const chartPath = "charts/marklogic-operator-kubernetes"
	const managerPath = "templates/manager-rbac.yaml"
	expected, err := os.ReadFile(filepath.Join("../..", chartPath, managerPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprintf("fresh=%t", fresh), func(t *testing.T) {
			root := t.TempDir()
			chart := filepath.Join(root, chartPath)
			if err := os.CopyFS(chart, os.DirFS(filepath.Join("../..", chartPath))); err != nil {
				t.Fatal(err)
			}
			seed := bytes.ReplaceAll(expected, []byte("volumeattributesclasses"), []byte("legacy-vac"))
			if fresh {
				seed, err = os.ReadFile("../../config/rbac/role.yaml")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(chart, managerPath), seed, 0600); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				command := exec.Command("bash", script)
				command.Dir = root
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("post-processing failed: %v\n%s", err, output)
				}
				actual, err := os.ReadFile(filepath.Join(chart, managerPath))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(actual, expected) {
					t.Fatalf("generated manager RBAC differs from the checked-in template on pass %d", attempt+1)
				}
			}
			t.Run("render", func(t *testing.T) {
				if _, err := exec.LookPath("helm"); err != nil {
					t.Skip("rendering RBAC requires helm")
				}
				for _, namespaced := range []bool{false, true} {
					args := []string{"template", "vac-rbac", chart, "--namespace", "operator-ns", "--show-only", managerPath}
					if namespaced {
						args = append(args, "--set", "scope.type=namespace", "--set", "metrics.secure=false", "--set", "scope.watchNamespaces[0]=data-ns", "--set", "scope.watchNamespaces[1]=other-ns")
					}
					output, err := exec.Command("helm", args...).CombinedOutput()
					if err != nil {
						t.Fatalf("rendering namespaced=%t failed: %v\n%s", namespaced, err, output)
					}
					assertVolumeAttributesClassRBAC(t, output, namespaced)
				}
			})
		})
	}
}

func assertVolumeAttributesClassRBAC(t *testing.T, contents []byte, namespaced bool) {
	t.Helper()
	managerRoles, storageRoles := 0, 0
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
	for {
		var role rbacv1.Role
		if err := decoder.Decode(&role); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if role.Kind != "Role" && role.Kind != "ClusterRole" {
			continue
		}
		permissions := map[string]map[string]bool{}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					key := group + "/" + resource
					if permissions[key] == nil {
						permissions[key] = map[string]bool{}
					}
					for _, verb := range rule.Verbs {
						permissions[key][verb] = true
					}
				}
			}
		}
		storage := map[string][]string{
			"/persistentvolumes":                     {"get"},
			"storage.k8s.io/storageclasses":          {"get", "list", "watch"},
			"storage.k8s.io/volumeattributesclasses": {"get"},
		}
		required := map[string][]string{}
		if namespaced && role.Kind == "Role" || !namespaced && role.Kind == "ClusterRole" {
			managerRoles++
			required["/persistentvolumeclaims"] = []string{"create", "get", "list", "patch", "update", "watch"}
			required["/persistentvolumeclaims/status"] = []string{"get"}
			required["/events"] = []string{"create", "patch", "update"}
			required["events.k8s.io/events"] = []string{"create", "patch", "update"}
			required["marklogic.progress.com/marklogicgroups/status"] = []string{"get", "patch", "update"}
			required["marklogic.progress.com/marklogicclusters/status"] = []string{"get", "patch", "update"}
		}
		if role.Kind == "ClusterRole" {
			storageRoles++
			for resource, verbs := range storage {
				required[resource] = verbs
			}
		} else {
			for resource := range storage {
				if len(permissions[resource]) != 0 {
					t.Fatalf("namespaced Role %s must not grant cluster-scoped %s access", role.Name, resource)
				}
			}
		}
		for resource, verbs := range required {
			want := map[string]bool{}
			for _, verb := range verbs {
				want[verb] = true
			}
			if !reflect.DeepEqual(permissions[resource], want) {
				t.Fatalf("%s %s: %s permissions = %v, want %v", role.Kind, role.Name, resource, permissions[resource], want)
			}
		}
	}
	if managerRoles == 0 || storageRoles != 1 {
		t.Fatalf("expected manager roles and one cluster-scoped storage grant, got %d managers and %d storage roles", managerRoles, storageRoles)
	}
}

func validateStorageSchema(t *testing.T, storage apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&storage, &internal, nil); err != nil {
		t.Fatal(err)
	}
	validator, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"", true}, {"ml-standard", true}, {"ml.premium", true}, {strings.Repeat("a", 253), true},
		{"UPPERCASE", false}, {"has_underscore", false}, {"-leading", false}, {"trailing-", false},
		{"double..dot", false}, {"has space", false}, {strings.Repeat("a", 254), false},
	} {
		object := map[string]any{"volumeAttributesClassName": test.value}
		errors := validation.ValidateCustomResource(field.NewPath("storage"), object, validator)
		if (len(errors) == 0) != test.valid {
			t.Fatalf("unexpected validation for %q: %v", test.value, errors)
		}
	}
	if errors := validation.ValidateCustomResource(field.NewPath("storage"), map[string]any{}, validator); len(errors) != 0 {
		t.Fatalf("empty storage must be valid: %v", errors)
	}
	structural, err := schema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{"volumeAttributesClassName": nil}
	defaulting.PruneNonNullableNullsWithoutDefaults(object, structural)
	if _, exists := object["volumeAttributesClassName"]; exists {
		t.Fatal("explicit null must be pruned to an absent class")
	}
}
