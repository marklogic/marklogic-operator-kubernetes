/*
Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestObjectStorageSampleParsesStrictly(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../config/samples/object-storage.yaml")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	cluster := &MarklogicCluster{}
	if err := yaml.UnmarshalStrict(raw, cluster); err != nil {
		t.Fatalf("sample does not match the API types: %v", err)
	}
	config := cluster.Spec.ObjectStorage
	if config == nil || config.AWS == nil || config.Azure == nil {
		t.Fatalf("sample should declare both providers")
	}
	if config.AWS.AuthType != ObjectStorageAuthSecret || config.AWS.SecretName == "" || config.Azure.SecretName == "" {
		t.Fatalf("unexpected sample binding: %+v %+v", config.AWS, config.Azure)
	}
	if strings.Contains(strings.ToLower(string(raw)), "accesskey:") || strings.Contains(strings.ToLower(string(raw)), "storagekey:") {
		t.Fatalf("the sample must not contain credential fields")
	}
}

func TestObjectStorageStatusSerializationDistinguishesFalseFromAbsent(t *testing.T) {
	t.Parallel()
	absent, err := json.Marshal(ObjectStorageProviderStatus{Phase: ObjectStoragePhaseFailed})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), "detachEligible") {
		t.Fatalf("absent eligibility must be omitted: %s", absent)
	}

	explicitFalse := false
	present, err := json.Marshal(ObjectStorageProviderStatus{Phase: ObjectStoragePhaseFailed, DetachEligible: &explicitFalse})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(present), `"detachEligible":false`) {
		t.Fatalf("an explicit false must be serialized: %s", present)
	}
}
