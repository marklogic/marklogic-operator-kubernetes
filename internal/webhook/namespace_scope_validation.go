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
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	marklogicClusterValidatePath = "/validate-marklogic-progress-com-v1-marklogiccluster"
	marklogicGroupValidatePath   = "/validate-marklogic-progress-com-v1-marklogicgroup"
)

// WatchNamespacesFromEnv parses WATCH_NAMESPACE and returns a sorted, de-duplicated list.
// Empty result means cluster-scoped mode (all namespaces allowed).
func WatchNamespacesFromEnv(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	uniq := make(map[string]struct{})
	for _, ns := range strings.Split(raw, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		uniq[ns] = struct{}{}
	}

	if len(uniq) == 0 {
		return nil
	}

	out := make([]string, 0, len(uniq))
	for ns := range uniq {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// IsNamespaceAllowed returns true when a request namespace is allowed by WATCH_NAMESPACE.
// Empty watch list means cluster-scoped mode where all namespaces are allowed.
func IsNamespaceAllowed(watchNamespaces []string, requestNamespace string) bool {
	if len(watchNamespaces) == 0 {
		return true
	}
	for _, ns := range watchNamespaces {
		if ns == requestNamespace {
			return true
		}
	}
	return false
}

type namespaceScopeValidator struct {
	watchNamespaces   []string
	validationEnabled bool
}

func (v *namespaceScopeValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	// When namespace validation is disabled the handler is still registered so the
	// webhook server answers the ValidatingWebhookConfiguration paths correctly.
	// Returning Allowed here avoids 404/connection failures that would block CR
	// create/update when failurePolicy: Fail is set on the webhook configuration.
	if !v.validationEnabled {
		return admission.Allowed("namespace scope validation is disabled")
	}

	if IsNamespaceAllowed(v.watchNamespaces, req.Namespace) {
		return admission.Allowed("namespace is in operator watch scope")
	}

	allowed := strings.Join(v.watchNamespaces, ",")
	msg := fmt.Sprintf("namespace %q is outside operator watch scope (%s)", req.Namespace, allowed)
	return admission.Denied(msg)
}

// RegisterNamespaceScopeValidationWebhooks always registers validating admission
// handlers for MarklogicCluster and MarklogicGroup create/update requests.
// The ENABLE_NAMESPACE_WEBHOOK_VALIDATION env var controls whether the handler
// enforces scope or passes all requests through — the paths are always served
// so the API server never receives a 404/connection error.
//
// watchNamespaceRaw must be the same raw value resolved by cmd/main.go (from
// --watch-namespace or WATCH_NAMESPACE fallback) so webhook enforcement matches
// the controller cache scope.
func RegisterNamespaceScopeValidationWebhooks(server ctrlwebhook.Server, watchNamespaceRaw string) {
	enabled := parseValidationEnabled(os.Getenv("ENABLE_NAMESPACE_WEBHOOK_VALIDATION"))
	validator := &namespaceScopeValidator{
		watchNamespaces:   WatchNamespacesFromEnv(watchNamespaceRaw),
		validationEnabled: enabled,
	}
	server.Register(marklogicClusterValidatePath, &admission.Webhook{Handler: validator})
	server.Register(marklogicGroupValidatePath, &admission.Webhook{Handler: validator})
}

// parseValidationEnabled parses ENABLE_NAMESPACE_WEBHOOK_VALIDATION.
// An empty or unset value defaults to enabled (true).
func parseValidationEnabled(raw string) bool {
	v := strings.TrimSpace(raw)
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "false", "0", "no":
		return false
	default:
		return true
	}
}
