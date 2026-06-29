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
	watchNamespaces []string
}

func (v *namespaceScopeValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if IsNamespaceAllowed(v.watchNamespaces, req.Namespace) {
		return admission.Allowed("namespace is in operator watch scope")
	}

	allowed := strings.Join(v.watchNamespaces, ",")
	msg := fmt.Sprintf("namespace %q is outside operator watch scope (%s)", req.Namespace, allowed)
	return admission.Denied(msg)
}

// RegisterNamespaceScopeValidationWebhooks registers validating admission handlers
// for MarklogicCluster and MarklogicGroup create/update requests.
func RegisterNamespaceScopeValidationWebhooks(server ctrlwebhook.Server) {
	validator := &namespaceScopeValidator{watchNamespaces: WatchNamespacesFromEnv(os.Getenv("WATCH_NAMESPACE"))}
	server.Register(marklogicClusterValidatePath, &admission.Webhook{Handler: validator})
	server.Register(marklogicGroupValidatePath, &admission.Webhook{Handler: validator})
}
