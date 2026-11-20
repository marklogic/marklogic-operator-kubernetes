// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fixtureExample() fixture {
	return fixture{
		AWS: rotation{
			Initial: material{"accessKey": "initial-access-sentinel", "secretKey": "initial-secret-sentinel", "sessionToken": "token-sentinel"},
			Rotated: material{"accessKey": "rotated-access-sentinel", "secretKey": "rotated-secret-sentinel"},
		},
		Azure: rotation{
			Initial: material{"storageAccount": "account-sentinel", "storageKey": "initial-storage-sentinel"},
			Rotated: material{"storageAccount": "account-sentinel", "storageKey": "rotated-storage-sentinel"},
		},
	}
}

func TestFixtureValidationDoesNotEchoUntrustedInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fixture)
		raw    string
	}{
		{name: "malformed", raw: `{"CONFIDENTIAL-malformed":`},
		{name: "unknown-field", raw: `{"CONFIDENTIAL-unknown": "CONFIDENTIAL-value"}`},
		{name: "missing-provider", mutate: func(f *fixture) { f.Azure = rotation{} }},
		{name: "empty-required", mutate: func(f *fixture) { f.AWS.Initial["secretKey"] = " \n\t" }},
		{name: "unknown-key", mutate: func(f *fixture) { f.Azure.Initial["CONFIDENTIAL-key"] = "CONFIDENTIAL-value" }},
		{name: "equivalent-rotation", mutate: func(f *fixture) {
			f.AWS.Rotated = material{"accessKey": " initial-access-sentinel ", "secretKey": " initial-secret-sentinel ", "sessionToken": "token-sentinel"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureExample()
			data := []byte(tc.raw)
			if tc.mutate != nil {
				tc.mutate(&f)
				data, _ = json.Marshal(f)
			}
			if _, err := decodeFixture(data); err == nil {
				t.Fatal("invalid fixture accepted")
			} else if strings.Contains(err.Error(), "CONFIDENTIAL") || strings.Contains(err.Error(), "sentinel") {
				t.Fatal("fixture error exposed input (value suppressed)")
			}
		})
	}
	data, _ := json.Marshal(fixtureExample())
	if _, err := decodeFixture(append(data, []byte(` {"CONFIDENTIAL":"extra"}`)...)); err == nil {
		t.Fatal("multiple JSON documents accepted")
	}
	if _, err := decodeFixture(data); err != nil {
		t.Fatal("valid two-provider rotation fixture rejected")
	}
	if f, err := loadFixture(""); err != nil || f != nil {
		t.Fatal("missing optional fixture should return nil")
	}
	if _, err := loadFixture("relative.json"); err == nil {
		t.Fatal("relative fixture path accepted")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, make([]byte, (1<<20)+1), 0600); err != nil {
		t.Fatal("cannot write test fixture")
	}
	if _, err := loadFixture(path); err == nil {
		t.Fatal("oversized fixture accepted")
	}
}

func TestMaterialScanCoversEncodedSecretValues(t *testing.T) {
	value := "credential-\"quote\\slash\nnewline-sentinel"
	encoded, _ := json.Marshal(map[string]string{"message": value})
	for _, data := range [][]byte{[]byte(value), encoded, []byte(base64.StdEncoding.EncodeToString([]byte(value)))} {
		if !containsMaterial(data, []string{value}) {
			t.Fatal("known credential material was not detected (value suppressed)")
		}
	}
	if containsMaterial([]byte("{ \"phase\": \"Applied\" }\n\t"), []string{"", " ", "\n\t", value}) {
		t.Fatal("benign status falsely flagged")
	}
	f := fixtureExample()
	for _, formatted := range []string{fmt.Sprintf("%v", f), fmt.Sprintf("%#v", f), fmt.Sprintf("%v", f.AWS.Initial), fmt.Sprintf("%#v", f.Azure.Rotated)} {
		if strings.Contains(formatted, "sentinel") {
			t.Fatal("fixture formatting leaked material")
		}
	}
}

func TestFixtureRejectsAmbiguousCredentialJSON(t *testing.T) {
	data, err := json.Marshal(fixtureExample())
	if err != nil {
		t.Fatal("cannot encode fixture")
	}
	valid := string(data)
	for _, tc := range []struct{ name, raw string }{
		{"duplicate provider", strings.Replace(valid, `"aws":`, `"aws":{},"aws":`, 1)},
		{"duplicate revision", strings.Replace(valid, `"initial":`, `"initial":{},"initial":`, 1)},
		{"duplicate access key", strings.Replace(valid, `"accessKey":`, `"accessKey":"CONFIDENTIAL-duplicate","accessKey":`, 1)},
		{"duplicate token", strings.Replace(valid, `"sessionToken":`, `"sessionToken":"CONFIDENTIAL-duplicate","sessionToken":`, 1)},
		{"provider case alias", strings.Replace(valid, `"aws":`, `"AWS":`, 1)},
		{"revision case alias", strings.Replace(valid, `"initial":`, `"Initial":`, 1)},
		{"null token", strings.Replace(valid, `"sessionToken":"token-sentinel"`, `"sessionToken":null`, 1)},
		{"nonstring token", strings.Replace(valid, `"sessionToken":"token-sentinel"`, `"sessionToken":false`, 1)},
		{"oversize direct decode", strings.Repeat(" ", fixtureReadLimit) + valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.raw == valid {
				t.Fatal("invalid fixture mutation did not change input")
			}
			if _, err := decodeFixture([]byte(tc.raw)); err == nil {
				t.Fatal("ambiguous credential fixture accepted (input suppressed)")
			} else if strings.Contains(err.Error(), "CONFIDENTIAL") || strings.Contains(err.Error(), "sentinel") {
				t.Fatal("fixture error exposed credential material")
			}
		})
	}
}

func TestCleanupDoesNotDeleteClusterWhoseCreationFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	if ml.AddToScheme(scheme) != nil || corev1.AddToScheme(scheme) != nil {
		t.Fatal("cannot build test scheme")
	}
	existing := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: "collision", Namespace: "ns", UID: types.UID("someone-else")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	s := &scenario{ctx: context.Background(), client: c, name: "collision", opts: Options{Namespace: "ns"}}
	s.cleanup(t)
	if c.Get(context.Background(), s.key("collision"), &ml.MarklogicCluster{}) != nil {
		t.Fatal("cleanup deleted a cluster not created by this run")
	}
}
