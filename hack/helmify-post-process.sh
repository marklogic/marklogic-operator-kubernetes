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
TEMPLATES_DIR="${CHART_DIR}/templates"
VALUES_FILE="${CHART_DIR}/values.yaml"
DEPLOYMENT_FILE="${TEMPLATES_DIR}/deployment.yaml"
MANAGER_RBAC_FILE="${TEMPLATES_DIR}/manager-rbac.yaml"
METRICS_AUTH_RBAC_FILE="${TEMPLATES_DIR}/metrics-auth-rbac.yaml"
METRICS_READER_RBAC_FILE="${TEMPLATES_DIR}/metrics-reader-rbac.yaml"
METRICS_SERVICE_FILE="${TEMPLATES_DIR}/metrics-service.yaml"
PROXY_RBAC_FILE="${TEMPLATES_DIR}/proxy-rbac.yaml"

echo "=== helmify post-process: restoring scope support ==="

# ──────────────────────────────────────────────────────────────────────────────
# 0. Remove stale proxy-rbac.yaml (old kube-rbac-proxy artefact)
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${PROXY_RBAC_FILE}" ]; then
    rm "${PROXY_RBAC_FILE}"
    echo "  [proxy-rbac.yaml] Removed stale file."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 1. values.yaml – add scope section if helmify removed it
#                – strip manager.args and metricsService.ports (now in templates)
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
  #          metrics served on :8443 with HTTPS + Kubernetes authn/authz
  # namespace: operator watches only specified namespace(s) (requires Role/RoleBinding)
  #            metrics served on :8080 without authentication (no ClusterRole needed)
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

if ! grep -q "^metrics:" "${VALUES_FILE}"; then
    echo "  [values.yaml] Adding metrics configuration..."
    cat >> "${VALUES_FILE}" << 'YAML_EOF'

# Metrics security configuration
metrics:
  # secure: true  → port 8443, HTTPS, Kubernetes authn/authz (requires scope.type=cluster)
  # secure: false → port 8080, HTTP, no authentication (suitable for dev/test or namespace scope)
  secure: true
YAML_EOF
    echo "  [values.yaml] Done (metrics)."
else
    echo "  [values.yaml] metrics already present – skipping."
fi

# Strip manager.args block if helmify re-added it (now hardcoded in deployment.yaml template)
python3 - "${VALUES_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()
# Remove "    args:\n    - ...\n    - ...\n" block under controllerManager.manager
cleaned = re.sub(r'(  manager:\n)(\s+args:\n(?:\s+-[^\n]+\n)*)', r'\1', content)
if cleaned != content:
    with open(sys.argv[1], 'w') as f:
        f.write(cleaned)
    print("  [values.yaml] Removed manager.args (now in deployment template).")
PYEOF

# Strip metricsService.ports block if helmify re-added it (now in metrics-service.yaml template)
# Only removes the "ports:" key and its list items; preserves "type:" and other keys.
python3 - "${VALUES_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()
# Match "  ports:\n" followed by lines that are list items (start with "  -" or deeper indented continuation)
cleaned = re.sub(r'(?m)^  ports:\n(?:  [ -][^\n]*\n)*', '', content)
if cleaned != content:
    with open(sys.argv[1], 'w') as f:
        f.write(cleaned)
    print("  [values.yaml] Removed metricsService.ports (now in metrics-service template).")
PYEOF

# ──────────────────────────────────────────────────────────────────────────────
# 2. deployment.yaml – scope-conditional manager args, container port, WATCH_NAMESPACE
# ──────────────────────────────────────────────────────────────────────────────

# 2a. Replace plain "args: ..." with scope-conditional args block
if ! grep -q 'metrics-bind-address' "${DEPLOYMENT_FILE}" && ! grep -q '.Values.metrics.secure' "${DEPLOYMENT_FILE}"; then
    echo "  [deployment.yaml] Injecting scope-conditional manager args..."
    python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()

OLD = r'      - args: {{- toYaml \.Values\.controllerManager\.manager\.args \| nindent 8 }}'
NEW = '''      - args:
        {{- if .Values.metrics.secure }}
        - --health-probe-bind-address=:8081
        - --metrics-bind-address=:8443
        - --metrics-secure=true
        - --leader-elect
        {{- else }}
        - --health-probe-bind-address=:8081
        - --metrics-bind-address=:8080
        - --metrics-secure=false
        - --leader-elect
        {{- end }}'''

result = re.sub(OLD, NEW, content, count=1)
if result == content:
    print("  WARNING: args anchor not found – check helmify output format.")
    sys.exit(1)
with open(sys.argv[1], 'w') as f:
    f.write(result)
print("  Done (args).")
PYEOF
else
    echo "  [deployment.yaml] scope-conditional args already present – skipping."
fi

# 2b. Replace static container port with scope-conditional port
if ! grep -q '.Values.metrics.secure' "${DEPLOYMENT_FILE}" || ! grep -q 'containerPort: 8080' "${DEPLOYMENT_FILE}"; then
    python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()

OLD = r'''        ports:
        - containerPort: 8443
          name: https
          protocol: TCP'''
NEW = '''        ports:
        {{- if .Values.metrics.secure }}
        - containerPort: 8443
          name: https
          protocol: TCP
        {{- else }}
        - containerPort: 8080
          name: http
          protocol: TCP
        {{- end }}'''

if OLD in content:
    with open(sys.argv[1], 'w') as f:
        f.write(content.replace(OLD, NEW, 1))
    print("  [deployment.yaml] Done (container port).")
PYEOF
fi

# 2c. Inject WATCH_NAMESPACE env var if not already present
if ! grep -q "WATCH_NAMESPACE" "${DEPLOYMENT_FILE}"; then
    echo "  [deployment.yaml] Injecting WATCH_NAMESPACE env var..."
    python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys

filename = sys.argv[1]
with open(filename, 'r') as f:
    content = f.read()

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
    print("  WARNING: KUBERNETES_CLUSTER_DOMAIN anchor not found in deployment.yaml.")
    sys.exit(1)

with open(filename, 'w') as f:
    f.write(content.replace(ANCHOR, INJECTION, 1))
print("  [deployment.yaml] Done (WATCH_NAMESPACE).")
PYEOF
else
    echo "  [deployment.yaml] WATCH_NAMESPACE already present – skipping."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 3. manager-rbac.yaml – scope-conditional ClusterRole vs Role/RoleBinding
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
      {{- range $ns := splitList "," .Values.scope.watchNamespaces }}
        {{- $namespaces = append $namespaces (trim $ns) }}
      {{- end }}
    {{- else }}
      {{- $namespaces = list .Values.scope.watchNamespaces }}
    {{- end }}
  {{- else if kindIs "slice" .Values.scope.watchNamespaces }}
    {{- $namespaces = .Values.scope.watchNamespaces }}
  {{- end }}
{{- else }}
  {{- $namespaces = list .Release.Namespace }}
{{- end }}
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

# ──────────────────────────────────────────────────────────────────────────────
# 4. metrics-auth-rbac.yaml – wrap with scope conditional (ClusterRole only in cluster mode)
#    TokenReview/SubjectAccessReview are cluster-scoped APIs; not needed in namespace mode.
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "metrics.secure" "${METRICS_AUTH_RBAC_FILE}"; then
    echo "  [metrics-auth-rbac.yaml] Wrapping with metrics.secure+scope conditional..."
    python3 - "${METRICS_AUTH_RBAC_FILE}" << 'PYEOF'
import sys
with open(sys.argv[1], 'r') as f:
    content = f.read()
content = content.strip()
with open(sys.argv[1], 'w') as f:
    f.write('{{- if and (eq .Values.scope.type "cluster") (.Values.metrics.secure) }}\n' + content + '\n{{- end }}\n')
print("  Done.")
PYEOF
else
    echo "  [metrics-auth-rbac.yaml] metrics.secure guard already present – skipping."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 5. metrics-reader-rbac.yaml – wrap with scope conditional (ClusterRole only in cluster mode)
#    /metrics is a non-resource URL; can only be granted via ClusterRole (not needed in namespace mode).
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "metrics.secure" "${METRICS_READER_RBAC_FILE}"; then
    echo "  [metrics-reader-rbac.yaml] Wrapping with metrics.secure+scope conditional..."
    python3 - "${METRICS_READER_RBAC_FILE}" << 'PYEOF'
import sys
with open(sys.argv[1], 'r') as f:
    content = f.read()
content = content.strip()
with open(sys.argv[1], 'w') as f:
    f.write('{{- if and (eq .Values.scope.type "cluster") (.Values.metrics.secure) }}\n' + content + '\n{{- end }}\n')
print("  Done.")
PYEOF
else
    echo "  [metrics-reader-rbac.yaml] metrics.secure guard already present – skipping."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 6. metrics-service.yaml – scope-conditional port (8443/https vs 8080/http)
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "metrics.secure" "${METRICS_SERVICE_FILE}"; then
    echo "  [metrics-service.yaml] Injecting metrics.secure-conditional port..."
    python3 - "${METRICS_SERVICE_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()

OLD = r'  ports:\n  {{- \.Values\.metricsService\.ports \| toYaml \| nindent 2 }}'
NEW = '''  ports:
  {{- if .Values.metrics.secure }}
  - name: https
    port: 8443
    protocol: TCP
    targetPort: https
  {{- else }}
  - name: http
    port: 8080
    protocol: TCP
    targetPort: http
  {{- end }}'''

result = re.sub(OLD, NEW, content)
if result == content:
    # Already has static ports block from a previous regeneration – replace it
    OLD2 = (
        '  ports:\n'
        '  - name: https\n'
        '    port: 8443\n'
        '    protocol: TCP\n'
        '    targetPort: https'
    )
    if OLD2 in content:
        result = content.replace(OLD2, NEW, 1)

if result != content:
    with open(sys.argv[1], 'w') as f:
        f.write(result)
    print("  Done.")
else:
    print("  WARNING: could not find ports anchor in metrics-service.yaml.")
PYEOF
else
    echo "  [metrics-service.yaml] metrics.secure conditional port already present – skipping."
fi

echo "=== Post-processing complete ==="
