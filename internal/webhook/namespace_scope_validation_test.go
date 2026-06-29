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

package webhook

import (
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestWatchNamespacesFromEnv(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty means cluster scoped", raw: "", want: nil},
		{name: "single namespace", raw: "ml", want: []string{"ml"}},
		{name: "csv trims and deduplicates", raw: " ml,prod, ml ,dev ", want: []string{"dev", "ml", "prod"}},
		{name: "ignores blank entries", raw: ",,ml,,", want: []string{"ml"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WatchNamespacesFromEnv(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("unexpected length: got=%v want=%v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("unexpected value at %d: got=%q want=%q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsNamespaceAllowed(t *testing.T) {
	if !IsNamespaceAllowed(nil, "any") {
		t.Fatal("expected cluster-scoped mode to allow any namespace")
	}
	if !IsNamespaceAllowed([]string{"ml", "prod"}, "ml") {
		t.Fatal("expected watched namespace to be allowed")
	}
	if IsNamespaceAllowed([]string{"ml", "prod"}, "dev") {
		t.Fatal("expected non-watched namespace to be denied")
	}
}

func TestNamespaceScopeValidatorHandle(t *testing.T) {
	validator := &namespaceScopeValidator{watchNamespaces: []string{"ml", "prod"}, validationEnabled: true}

	allowedResp := validator.Handle(t.Context(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Namespace: "ml"},
	})
	if !allowedResp.Allowed {
		t.Fatal("expected watched namespace request to be allowed")
	}

	deniedResp := validator.Handle(t.Context(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Namespace: "dev"},
	})
	if deniedResp.Allowed {
		t.Fatal("expected non-watched namespace request to be denied")
	}
	if !strings.Contains(deniedResp.Result.Message, "outside operator watch scope") {
		t.Fatalf("expected denial message to mention watch scope, got %q", deniedResp.Result.Message)
	}
	if !strings.Contains(deniedResp.Result.Message, "ml,prod") {
		t.Fatalf("expected denial message to contain allowed namespaces, got %q", deniedResp.Result.Message)
	}
}

func TestNamespaceScopeValidatorHandle_ValidationDisabled(t *testing.T) {
	// When validationEnabled=false the handler must return Allowed for any namespace,
	// including non-watched ones, so the webhook paths answer without blocking CRs.
	validator := &namespaceScopeValidator{watchNamespaces: []string{"ml", "prod"}, validationEnabled: false}

	resp := validator.Handle(t.Context(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Namespace: "dev"},
	})
	if !resp.Allowed {
		t.Fatalf("expected non-watched namespace to be allowed when validation is disabled, got denied: %s", resp.Result.Message)
	}
	if !strings.Contains(resp.Result.Message, "disabled") {
		t.Fatalf("expected message to mention 'disabled', got %q", resp.Result.Message)
	}
}
