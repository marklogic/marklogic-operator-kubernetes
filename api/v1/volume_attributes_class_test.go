package v1

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
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
