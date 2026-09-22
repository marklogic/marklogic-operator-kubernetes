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
	"reflect"
	"testing"
)

func TestObjectStorageConfigReferencedSecretNames(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config   *ObjectStorageConfig
		expected []string
	}{
		"nil config": {
			config:   nil,
			expected: nil,
		},
		"no providers": {
			config:   &ObjectStorageConfig{},
			expected: []string{},
		},
		"aws only": {
			config:   &ObjectStorageConfig{AWS: &AWSObjectStorage{SecretName: "ml-s3"}},
			expected: []string{"ml-s3"},
		},
		"azure only": {
			config:   &ObjectStorageConfig{Azure: &AzureObjectStorage{SecretName: "ml-azure"}},
			expected: []string{"ml-azure"},
		},
		"both providers": {
			config: &ObjectStorageConfig{
				AWS:   &AWSObjectStorage{SecretName: "ml-s3"},
				Azure: &AzureObjectStorage{SecretName: "ml-azure"},
			},
			expected: []string{"ml-s3", "ml-azure"},
		},
		"provider present but secret name empty": {
			config: &ObjectStorageConfig{
				AWS:   &AWSObjectStorage{AuthType: ObjectStorageAuthSecret},
				Azure: &AzureObjectStorage{SecretName: "ml-azure"},
			},
			expected: []string{"ml-azure"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := test.config.ReferencedSecretNames()
			if !reflect.DeepEqual(got, test.expected) {
				t.Fatalf("expected %v, got %v", test.expected, got)
			}
		})
	}
}
