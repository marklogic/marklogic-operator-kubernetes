/*
Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVolumeShrinkingRejected(t *testing.T) {
	tests := []struct {
		name         string
		oldSize      string
		newSize      string
		shouldReject bool
	}{
		{
			name:         "shrink from 5Gi to 2Gi",
			oldSize:      "5Gi",
			newSize:      "2Gi",
			shouldReject: true,
		},
		{
			name:         "shrink with unit conversion (5Gi to 2000Mi)",
			oldSize:      "5Gi",
			newSize:      "2000Mi",
			shouldReject: true,
		},
		{
			name:         "expand from 2Gi to 5Gi",
			oldSize:      "2Gi",
			newSize:      "5Gi",
			shouldReject: false,
		},
		{
			name:         "no change (same size)",
			oldSize:      "5Gi",
			newSize:      "5Gi",
			shouldReject: false,
		},
		{
			name:         "expand with unit conversion (2Gi to 3000Mi)",
			oldSize:      "2Gi",
			newSize:      "3000Mi",
			shouldReject: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldGroup := &MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: MarklogicGroupSpec{
					Persistence: &Persistence{
						Size: tt.oldSize,
					},
				},
			}

			newGroup := &MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: MarklogicGroupSpec{
					Persistence: &Persistence{
						Size: tt.newSize,
					},
				},
			}

			_, err := newGroup.ValidateUpdate(context.Background(), oldGroup, newGroup)

			if tt.shouldReject && err == nil {
				t.Errorf("test '%s': expected validation error but got none", tt.name)
			}
			if !tt.shouldReject && err != nil {
				t.Errorf("test '%s': expected validation to pass but got error: %v", tt.name, err)
			}
		})
	}
}
