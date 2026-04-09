#!/usr/bin/env bash
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.
#
# Post-processing script for Helmify output.
# Restores scope-awareness (cluster-scoped vs namespace-scoped) that helmify cannot generate,
# because kustomize only exposes the cluster-scoped RBAC to helmify.
#
# Run automatically by: make helm
# Safe to run multiple times (idempotent).

set -euo pipefail

CHART_DIR="charts/marklogic-operator-kubernetes"
VALUES_FILE="${CHART_DIR}/values.yaml"
DEPLOYMENT_FILE="${CHART_DIR}/templates/deployment.yaml"
MANAGER_RBAC_FILE="${CHART_DIR}/templates/manager-rbac.yaml"

echo "=== helmify post-process: restoring scope support ==="

# ──────────────────────────────────────────────────────────────────────────────
# 1. values.yaml – add scope section if helmify removed it
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "^scope:" "${VALUES_FILE}"; then
    echo "  [values.yaml] Adding scope configuration..."
    cat >> "${VALUES_FILE}" << 'YAML_EOF'

# Operator scope configuration
# scope can be "cluster" (default) or "namespace"
# When scope is "namespace", the operator will only watch resources in the specified namespace(s)
scope:
  # Deployment scope: "cluster" or "namespace"
  # cluster: operator watches all namespaces (requires ClusterRole/ClusterRoleBinding)
  # namespace: operator watches only specified namespace(s) (requires Role/RoleBinding)
  type: cluster

  # watchNamespaces defines which namespace(s) the operator should watch
  # Only applicable when scope.type is "namespace"
  # Can be a single string, comma-separated string, or array of strings
  # If empty, defaults to the release namespace
  #
  # Examples:
  #   watchNamespaces: "marklogic-prod"                              # Single namespace
  #   watchNamespaces: "marklogic-prod,marklogic-dev"                # Multiple (comma-separated)
  #   watchNamespaces: ["marklogic-prod", "marklogic-dev"]           # Multiple (array)
  watchNamespaces: ""
YAML_EOF
    echo "  [values.yaml] Done."
else
    echo "  [values.yaml] scope already present – skipping."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 2. deployment.yaml – inject WATCH_NAMESPACE env var block
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "WATCH_NAMESPACE" "${DEPLOYMENT_FILE}"; then
    echo "  [deployment.yaml] Injecting WATCH_NAMESPACE env var..."
    python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys

filename = sys.argv[1]
with open(filename, 'r') as f:
    content = f.read()

# The anchor text produced by helmify for the manager container's first env var.
# We insert the WATCH_NAMESPACE block immediately after it (only once).
ANCHOR = '          value: {{ quote .Values.kubernetesClusterDomain }}\n'

INJECTION = r"""          value: {{ quote .Values.kubernetesClusterDomain }}
        {{- if eq .Values.scope.type "namespace" }}
        {{- $namespaces := list }}
        {{- if .Values.scope.watchNamespaces }}
          {{- if kindIs "string" .Values.scope.watchNamespaces }}
            {{- if contains "," .Values.scope.watchNamespaces }}
              {{- /* Already comma-separated – pass through as-is */}}
              {{- $namespaces = list .Values.scope.watchNamespaces }}
            {{- else }}
              {{- /* Single namespace */}}
              {{- $namespaces = list .Values.scope.watchNamespaces }}
            {{- end }}
          {{- else if kindIs "slice" .Values.scope.watchNamespaces }}
            {{- /* Array – join into comma-separated string */}}
            {{- $namespaces = list (join "," .Values.scope.watchNamespaces) }}
          {{- end }}
        {{- else }}
          {{- /* Default to release namespace when watchNamespaces is unset */}}
          {{- $namespaces = list .Release.Namespace }}
        {{- end }}
        - name: WATCH_NAMESPACE
          value: {{ index $namespaces 0 | quote }}
        {{- end }}
"""

if ANCHOR not in content:
    print("  WARNING: expected anchor text not found in deployment.yaml – check helmify output format.")
    sys.exit(1)

new_content = content.replace(ANCHOR, INJECTION, 1)
with open(filename, 'w') as f:
    f.write(new_content)
print("  Done.")
PYEOF
else
    echo "  [deployment.yaml] WATCH_NAMESPACE already present – skipping."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 3. manager-rbac.yaml – rewrite with cluster/namespace conditional logic
#    Helmify always emits a plain ClusterRole; we replace it entirely.
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "scope.type" "${MANAGER_RBAC_FILE}"; then
    echo "  [manager-rbac.yaml] Rewriting with scope-conditional RBAC..."
    cat > "${MANAGER_RBAC_FILE}" << 'TMPL_EOF'
{{- if eq .Values.scope.type "cluster" }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: marklogic-operator-manager-role
  labels:
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
rules:
- apiGroups:
  - ""
  resources:
  - configmaps
  - pods
  - secrets
  - serviceaccounts
  - services
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - apps
  resources:
  - daemonsets
  - deployments
  - replicasets
  - statefulsets
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - marklogic.progress.com
  resources:
  - marklogicclusters
  - marklogicgroups
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - marklogic.progress.com
  resources:
  - marklogicclusters/finalizers
  - marklogicgroups/finalizers
  verbs:
  - update
- apiGroups:
  - marklogic.progress.com
  resources:
  - marklogicclusters/status
  - marklogicgroups/status
  verbs:
  - get
  - patch
  - update
- apiGroups:
  - networking.k8s.io
  resources:
  - ingresses
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: marklogic-operator-manager-rolebinding
  labels:
    app.kubernetes.io/component: rbac
    app.kubernetes.io/created-by: marklogic-operator-kubernetes
    app.kubernetes.io/part-of: marklogic-operator-kubernetes
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: marklogic-operator-manager-role
subjects:
- kind: ServiceAccount
  name: '{{ include "marklogic-operator-kubernetes.serviceAccountName" . }}'
  namespace: '{{ .Release.Namespace }}'
{{- else }}
{{- /*
Namespace-scoped mode: Create Role and RoleBinding in each watched namespace.
Supports single namespace, comma-separated list, or array of namespaces.
*/}}
{{- $namespaces := list }}
{{- if .Values.scope.watchNamespaces }}
  {{- if kindIs "string" .Values.scope.watchNamespaces }}
    {{- if contains "," .Values.scope.watchNamespaces }}
      {{- /* Comma-separated string – split into individual entries */}}
      {{- range $ns := splitList "," .Values.scope.watchNamespaces }}
        {{- $namespaces = append $namespaces (trim $ns) }}
      {{- end }}
    {{- else }}
      {{- /* Single namespace string */}}
      {{- $namespaces = list .Values.scope.watchNamespaces }}
    {{- end }}
  {{- else if kindIs "slice" .Values.scope.watchNamespaces }}
    {{- /* Array of namespaces */}}
    {{- $namespaces = .Values.scope.watchNamespaces }}
  {{- end }}
{{- else }}
  {{- /* Default to the release namespace if watchNamespaces is unset */}}
  {{- $namespaces = list .Release.Namespace }}
{{- end }}
{{- /* Create Role and RoleBinding for each watched namespace */}}
{{- range $ns := $namespaces }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: marklogic-operator-manager-role
  namespace: {{ $ns }}
  labels:
  {{- include "marklogic-operator-kubernetes.labels" $ | nindent 4 }}
rules:
- apiGroups:
  - ""
  resources:
  - configmaps
  - pods
  - secrets
  - serviceaccounts
  - services
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - apps
  resources:
  - daemonsets
  - deployments
  - replicasets
  - statefulsets
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - marklogic.progress.com
  resources:
  - marklogicclusters
  - marklogicgroups
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - marklogic.progress.com
  resources:
  - marklogicclusters/finalizers
  - marklogicgroups/finalizers
  verbs:
  - update
- apiGroups:
  - marklogic.progress.com
  resources:
  - marklogicclusters/status
  - marklogicgroups/status
  verbs:
  - get
  - patch
  - update
- apiGroups:
  - networking.k8s.io
  resources:
  - ingresses
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: marklogic-operator-manager-rolebinding
  namespace: {{ $ns }}
  labels:
    app.kubernetes.io/component: rbac
    app.kubernetes.io/created-by: marklogic-operator-kubernetes
    app.kubernetes.io/part-of: marklogic-operator-kubernetes
  {{- include "marklogic-operator-kubernetes.labels" $ | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: marklogic-operator-manager-role
subjects:
- kind: ServiceAccount
  name: '{{ include "marklogic-operator-kubernetes.serviceAccountName" $ }}'
  namespace: '{{ $.Release.Namespace }}'
{{- end }}
{{- end }}
TMPL_EOF
    echo "  [manager-rbac.yaml] Done."
else
    echo "  [manager-rbac.yaml] scope logic already present – skipping."
fi

echo "=== Post-processing complete ==="
