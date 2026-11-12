#!/usr/bin/env bash
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.
#
# Post-processing script for Helmify output.
# Restores scope-awareness (cluster-scoped vs namespace-scoped) and
# metrics-security (secure HTTPS vs plain HTTP) that helmify cannot generate,
# because kustomize only exposes the cluster-scoped/secure configuration to helmify.
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
MARKLOGICCLUSTER_CRD_FILE="${TEMPLATES_DIR}/marklogiccluster-crd.yaml"
MARKLOGICGROUP_CRD_FILE="${TEMPLATES_DIR}/marklogicgroup-crd.yaml"

echo "=== helmify post-process: restoring scope + metrics-security support ==="

# ──────────────────────────────────────────────────────────────────────────────
# 0. Rename proxy-rbac.yaml → metrics-auth-rbac.yaml (helmify may still use the
#    old name even though the ClusterRole is now called metrics-auth-role).
#    If helmify has already generated the correct name, skip.
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${PROXY_RBAC_FILE}" ] && [ ! -f "${METRICS_AUTH_RBAC_FILE}" ]; then
    mv "${PROXY_RBAC_FILE}" "${METRICS_AUTH_RBAC_FILE}"
    echo "  [proxy-rbac.yaml] Renamed to metrics-auth-rbac.yaml."
elif [ -f "${PROXY_RBAC_FILE}" ] && [ -f "${METRICS_AUTH_RBAC_FILE}" ]; then
    rm "${PROXY_RBAC_FILE}"
    echo "  [proxy-rbac.yaml] Removed stale file (metrics-auth-rbac.yaml already present)."
else
    echo "  [metrics-auth-rbac.yaml] Already correctly named."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 0.5. marklogiccluster-crd.yaml – restore top-level spec: after labels template
#      Helmify places CRD properties (group, names, scope, versions) under
#      metadata instead of under spec. This fix re-parents them to spec.
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${MARKLOGICCLUSTER_CRD_FILE}" ]; then
    if ! grep -q '^spec:' "${MARKLOGICCLUSTER_CRD_FILE}"; then
        echo "  [marklogiccluster-crd.yaml] Fixing CRD structure: restoring top-level spec:..."
        python3 - "${MARKLOGICCLUSTER_CRD_FILE}" << 'PYEOF'
import sys

crd_file = sys.argv[1]
with open(crd_file, 'r') as f:
    lines = f.readlines()

# Find the labels template line
labels_template_idx = None
for i, line in enumerate(lines):
    if '{{- include "marklogic-operator-kubernetes.labels"' in line:
        labels_template_idx = i
        break

if labels_template_idx is None:
    print("  ERROR: Could not find labels template in marklogiccluster-crd.yaml")
    sys.exit(1)

# Find the first property after labels that should be under spec (group, names, scope, versions)
first_spec_prop_idx = None
for i in range(labels_template_idx + 1, len(lines)):
    line = lines[i]
    if line.lstrip().startswith(('group:', 'names:', 'scope:', 'versions:')):
        first_spec_prop_idx = i
        break

if first_spec_prop_idx is None:
    print("  ERROR: Could not find CRD spec properties after labels template")
    sys.exit(1)

# Verify that first_spec_prop is at indent 2 (currently child of metadata)
first_prop_indent = len(lines[first_spec_prop_idx]) - len(lines[first_spec_prop_idx].lstrip())
if first_prop_indent != 2:
    print(f"  INFO: First spec property is at indent {first_prop_indent}, not the expected 2. Skipping fix.")
    sys.exit(0)

# Find the end of spec properties (first line at indent < 2 after first_spec_prop_idx, or end of file)
last_spec_prop_idx = len(lines) - 1
for i in range(first_spec_prop_idx, len(lines)):
    line = lines[i]
    if line.strip():  # Non-empty line
        current_indent = len(line) - len(line.lstrip())
        if current_indent < 2:
            last_spec_prop_idx = i - 1
            break
    else:  # Empty line, might be followed by non-spec content
        pass

# Insert "spec:\n" right before the first spec property
# Remove the indentation from the original line to insert at proper place
result = []
result.extend(lines[:first_spec_prop_idx])
result.append('spec:\n')
result.extend(lines[first_spec_prop_idx:])

with open(crd_file, 'w') as f:
    f.writelines(result)

print(f"  Done: Added spec: before line {first_spec_prop_idx + 1}.")
PYEOF
    else
        echo "  [marklogiccluster-crd.yaml] spec: already at root level – skipping."
    fi
else
    echo "  WARNING: ${MARKLOGICCLUSTER_CRD_FILE} not found – skipping CRD fix."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 0.6. marklogicgroup-crd.yaml – add missing tolerations field after tls
#      Helmify excludes the tolerations property from the spec schema,
#      breaking the new per-group toleration override feature.
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${MARKLOGICGROUP_CRD_FILE}" ]; then
    if ! grep -q "tolerations:" "${MARKLOGICGROUP_CRD_FILE}"; then
        echo "  [marklogicgroup-crd.yaml] Adding missing tolerations field..."
        python3 - "${MARKLOGICGROUP_CRD_FILE}" << 'PYEOF'
import sys

crd_file = sys.argv[1]
with open(crd_file, 'r') as f:
    lines = f.readlines()

# Find the tls property close (type: object) and topologySpreadConstraints start
tls_close_idx = None
topology_start_idx = None

for i, line in enumerate(lines):
    # Find where tls block ends (look for "type: object" after tls property)
    if 'tls:' in line and not tls_close_idx:
        tls_start = i
        # Find the corresponding close for tls object
        for j in range(i + 1, len(lines)):
            if lines[j].strip() == 'type: object' and 'tls' in ''.join(lines[i:j]):
                tls_close_idx = j
                break
    
    # Find where topologySpreadConstraints starts
    if 'topologySpreadConstraints:' in line and not topology_start_idx:
        topology_start_idx = i
        break

if tls_close_idx is None or topology_start_idx is None:
    print("  ERROR: Could not find tls and/or topologySpreadConstraints in marklogicgroup-crd.yaml")
    sys.exit(1)

# Get indentation from topologySpreadConstraints to match it for tolerations
topo_line = lines[topology_start_idx]
topo_indent = len(topo_line) - len(topo_line.lstrip())

# Extract the tolerations definition from base CRD
# Since we don't have access to base CRD here, we'll construct it based on Kubernetes Toleration spec
tolerations_yaml = f'''              tolerations:
                description: |-
                  The pod this Toleration is attached to tolerates any taint that matches
                  the triple <key,value,effect> using the matching operator <operator>.
                items:
                  description: |-
                    A toleration is a way for a pod to "tolerate" (or "forgive") a taint.
                  properties:
                    effect:
                      description: Effect indicates the taint effect to match. Empty means match all taint effects.
                      enum:
                      - NoSchedule
                      - NoExecute
                      - PreferNoSchedule
                      type: string
                    key:
                      description: Key is the taint key that the toleration applies to.
                      type: string
                    operator:
                      description: Operator represents a key's relationship to the value. Valid operators are Exists and Equal.
                      enum:
                      - Exists
                      - Equal
                      type: string
                    tolerationSeconds:
                      description: TolerationSeconds represents the period of time the toleration (which must be of effect NoExecute, otherwise this field is ignored) tolerates the taint.
                      format: int64
                      type: integer
                    value:
                      description: Value is the taint value the toleration matches to.
                      type: string
                  type: object
                type: array
'''

# Insert tolerations right before topologySpreadConstraints
result = []
result.extend(lines[:topology_start_idx])
result.append(tolerations_yaml)
result.extend(lines[topology_start_idx:])

with open(crd_file, 'w') as f:
    f.writelines(result)

print(f"  Done: Added tolerations field before topologySpreadConstraints (line {topology_start_idx + 1}).")
PYEOF
    else
        echo "  [marklogicgroup-crd.yaml] tolerations field already present – skipping."
    fi
else
    echo "  WARNING: ${MARKLOGICGROUP_CRD_FILE} not found – skipping MarklogicGroup CRD fix."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 1. values.yaml – add scope + metrics sections if helmify removed them
#               – strip manager.args and metricsService.ports (now in templates)
# ──────────────────────────────────────────────────────────────────────────────
if ! grep -q "^scope:" "${VALUES_FILE}"; then
    echo "  [values.yaml] Adding scope configuration..."
    cat >> "${VALUES_FILE}" << 'YAML_EOF'

# Operator scope configuration
scope:
  # type: "cluster" (default) — operator watches all namespaces.
  #        Requires ClusterRole/ClusterRoleBinding. Metrics can be secure or insecure.
  # type: "namespace"         — operator watches only specified namespace(s).
  #        Uses Role/RoleBinding per namespace. Metrics must be insecure (metrics.secure=false).
  type: cluster

  # watchNamespaces: which namespace(s) to watch when scope.type = "namespace".
  # Accepts a single name, a comma-separated string, or a YAML list.
  # When empty, defaults to the release namespace.
  watchNamespaces: ""
YAML_EOF
    echo "  [values.yaml] Done (scope)."
else
    echo "  [values.yaml] scope already present – skipping."
fi

if ! grep -q "^metrics:" "${VALUES_FILE}"; then
    echo "  [values.yaml] Adding metrics configuration..."
    cat >> "${VALUES_FILE}" << 'YAML_EOF'

# Metrics endpoint security
metrics:
  # secure: true  (default) — HTTPS on :8443, Kubernetes TokenReview/SubjectAccessReview
  #                            authentication enforced. Requires scope.type=cluster.
  # secure: false            — HTTP on :8080, no authentication. Safe for namespace scope
  #                            or development environments that have no cluster-level RBAC.
  secure: true
YAML_EOF
    echo "  [values.yaml] Done (metrics)."
else
    echo "  [values.yaml] metrics already present – skipping."
fi

# Strip manager.args block if helmify re-added it (hardcoded in deployment.yaml template)
python3 - "${VALUES_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()
cleaned = re.sub(r'(  manager:\n)(\s+args:\n(?:\s+-[^\n]+\n)*)', r'\1', content)
if cleaned != content:
    with open(sys.argv[1], 'w') as f:
        f.write(cleaned)
    print("  [values.yaml] Removed manager.args (now in deployment template).")
PYEOF

# Strip metricsService.ports block if helmify re-added it (now in metrics-service.yaml template)
python3 - "${VALUES_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()
cleaned = re.sub(r'(?m)^  ports:\n(?:  [ -][^\n]*\n)*', '', content)
if cleaned != content:
    with open(sys.argv[1], 'w') as f:
        f.write(cleaned)
    print("  [values.yaml] Removed metricsService.ports (now in metrics-service template).")
PYEOF

# ──────────────────────────────────────────────────────────────────────────────
# 2. deployment.yaml – scope/metrics-conditional args, port, and env vars
# ──────────────────────────────────────────────────────────────────────────────

# 2a. Replace static manager args with metrics.secure-conditional block
if ! grep -q 'metrics.secure' "${DEPLOYMENT_FILE}"; then
    echo "  [deployment.yaml] Injecting metrics.secure-conditional manager args..."
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
    print("  WARNING: manager args anchor not found – check helmify output format.")
    sys.exit(1)
with open(sys.argv[1], 'w') as f:
    f.write(result)
print("  Done (args).")
PYEOF
else
    echo "  [deployment.yaml] metrics.secure args already present – skipping."
fi

# 2b. Replace static container port with metrics.secure-conditional port
if ! grep -q 'containerPort: 8080' "${DEPLOYMENT_FILE}"; then
    python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys
with open(sys.argv[1], 'r') as f:
    content = f.read()

OLD = (
    '        ports:\n'
    '        - containerPort: 8443\n'
    '          name: https\n'
    '          protocol: TCP'
)
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
else:
    print("  [deployment.yaml] Static port anchor not found – may already be conditional.")
PYEOF
fi

# 2c. Inject POD_NAMESPACE (downward API) + scope-conditional WATCH_NAMESPACE
if ! grep -q "WATCH_NAMESPACE" "${DEPLOYMENT_FILE}"; then
    echo "  [deployment.yaml] Injecting POD_NAMESPACE + WATCH_NAMESPACE env vars..."
    python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys

filename = sys.argv[1]
with open(filename, 'r') as f:
    content = f.read()

ANCHOR = '          value: {{ quote .Values.kubernetesClusterDomain }}\n'

if ANCHOR not in content:
    print("  WARNING: KUBERNETES_CLUSTER_DOMAIN anchor not found in deployment.yaml.")
    sys.exit(1)

INJECTION = """          value: {{ quote .Values.kubernetesClusterDomain }}
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        {{- if eq .Values.scope.type "namespace" }}
        {{- $ns := .Values.scope.watchNamespaces }}
        {{- if not $ns }}
          {{- $ns = .Release.Namespace }}
        {{- else if kindIs "slice" $ns }}
          {{- $ns = join "," $ns }}
        {{- end }}
        - name: WATCH_NAMESPACE
          value: {{ $ns | quote }}
        {{- end }}
"""

with open(filename, 'w') as f:
    f.write(content.replace(ANCHOR, INJECTION, 1))
print("  [deployment.yaml] Done (POD_NAMESPACE + WATCH_NAMESPACE).")
PYEOF
else
    echo "  [deployment.yaml] WATCH_NAMESPACE already present – skipping."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 3. manager-rbac.yaml – scope-conditional ClusterRole vs Role/RoleBinding
# ──────────────────────────────────────────────────────────────────────────────
echo "  [manager-rbac.yaml] Refreshing scope-conditional storage and manager RBAC..."
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
  - persistentvolumes
  verbs:
  - get
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
  - ""
  resources:
  - persistentvolumeclaims
  verbs:
  - create
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - ""
  resources:
  - persistentvolumeclaims/status
  verbs:
  - get
- apiGroups:
  - ""
  - events.k8s.io
  resources:
  - events
  verbs:
  - create
  - patch
  - update
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
- apiGroups:
  - storage.k8s.io
  resources:
  - storageclasses
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - storage.k8s.io
  resources:
  - volumeattributesclasses
  verbs:
  - get
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
Namespace-scoped mode: Role + RoleBinding created in each watched namespace.
Supports a single name, a comma-separated string, or a YAML list.
The operator's own namespace (.Release.Namespace) is always included because
cmd/main.go adds it to the controller-runtime cache DefaultNamespaces for
leader-election and CRD informer syncing.
*/}}
{{- $namespaces := list }}
{{- if .Values.scope.watchNamespaces }}
  {{- if kindIs "string" .Values.scope.watchNamespaces }}
    {{- range $ns := splitList "," .Values.scope.watchNamespaces }}
      {{- $namespaces = append $namespaces (trim $ns) }}
    {{- end }}
  {{- else if kindIs "slice" .Values.scope.watchNamespaces }}
    {{- $namespaces = .Values.scope.watchNamespaces }}
  {{- end }}
{{- else }}
  {{- $namespaces = list .Release.Namespace }}
{{- end }}
{{- /* Always include the operator's own namespace so its cache can watch CRs there */}}
{{- if not (has .Release.Namespace $namespaces) }}
  {{- $namespaces = append $namespaces .Release.Namespace }}
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
  - ""
  resources:
  - persistentvolumeclaims
  verbs:
  - create
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - ""
  resources:
  - persistentvolumeclaims/status
  verbs:
  - get
- apiGroups:
  - ""
  - events.k8s.io
  resources:
  - events
  verbs:
  - create
  - patch
  - update
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
{{- /*
PersistentVolumes, StorageClasses and VolumeAttributesClasses are cluster-scoped.
A dedicated ClusterRole + ClusterRoleBinding grants storage validation reads
in namespace mode.
*/}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ printf "%s-storageclass-reader" (include "marklogic-operator-kubernetes.fullname" .) | trunc 63 | trimSuffix "-" }}
  labels:
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
rules:
- apiGroups:
  - ""
  resources:
  - persistentvolumes
  verbs:
  - get
- apiGroups:
  - storage.k8s.io
  resources:
  - storageclasses
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - storage.k8s.io
  resources:
  - volumeattributesclasses
  verbs:
  - get
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ printf "%s-storageclass-reader" (include "marklogic-operator-kubernetes.fullname" .) | trunc 63 | trimSuffix "-" }}
  labels:
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ printf "%s-storageclass-reader" (include "marklogic-operator-kubernetes.fullname" .) | trunc 63 | trimSuffix "-" }}
subjects:
- kind: ServiceAccount
  name: '{{ include "marklogic-operator-kubernetes.serviceAccountName" . }}'
  namespace: '{{ .Release.Namespace }}'
{{- end }}
TMPL_EOF
echo "  [manager-rbac.yaml] Done."

# ──────────────────────────────────────────────────────────────────────────────
# 4. metrics-auth-rbac.yaml – guard with scope=cluster AND metrics.secure=true.
#    TokenReview/SubjectAccessReview are cluster-scoped APIs unavailable in
#    namespace scope, and only needed when the secure endpoint is enabled.
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${METRICS_AUTH_RBAC_FILE}" ] && ! grep -q "metrics.secure" "${METRICS_AUTH_RBAC_FILE}"; then
    echo "  [metrics-auth-rbac.yaml] Wrapping with scope+metrics.secure conditional..."
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
    if [ ! -f "${METRICS_AUTH_RBAC_FILE}" ]; then
        echo "  WARNING: ${METRICS_AUTH_RBAC_FILE} not found – skipping."
    else
        echo "  [metrics-auth-rbac.yaml] guard already present – skipping."
    fi
fi

# ──────────────────────────────────────────────────────────────────────────────
# 5. metrics-reader-rbac.yaml – same guard: cluster scope + secure metrics only.
#    /metrics is a non-resource URL; can only be granted via ClusterRole.
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${METRICS_READER_RBAC_FILE}" ] && ! grep -q "metrics.secure" "${METRICS_READER_RBAC_FILE}"; then
    echo "  [metrics-reader-rbac.yaml] Wrapping with scope+metrics.secure conditional..."
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
    if [ ! -f "${METRICS_READER_RBAC_FILE}" ]; then
        echo "  WARNING: ${METRICS_READER_RBAC_FILE} not found – skipping."
    else
        echo "  [metrics-reader-rbac.yaml] guard already present – skipping."
    fi
fi

# ──────────────────────────────────────────────────────────────────────────────
# 6. metrics-service.yaml – scope/metrics-conditional port (8443/https vs 8080/http)
# ──────────────────────────────────────────────────────────────────────────────
if [ -f "${METRICS_SERVICE_FILE}" ] && ! grep -q "metrics.secure" "${METRICS_SERVICE_FILE}"; then
    echo "  [metrics-service.yaml] Injecting metrics.secure-conditional port..."
    python3 - "${METRICS_SERVICE_FILE}" << 'PYEOF'
import sys, re
with open(sys.argv[1], 'r') as f:
    content = f.read()

# helmify may render ports from values or as a static block
STATIC_HTTPS = (
    '  ports:\n'
    '  - name: https\n'
    '    port: 8443\n'
    '    protocol: TCP\n'
    '    targetPort: https'
)
FROM_VALUES = '  ports:\n  {{- .Values.metricsService.ports | toYaml | nindent 2 }}'

CONDITIONAL = '''  ports:
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

if STATIC_HTTPS in content:
    content = content.replace(STATIC_HTTPS, CONDITIONAL, 1)
    print("  [metrics-service.yaml] Done (replaced static https port).")
elif FROM_VALUES in content:
    content = content.replace(FROM_VALUES, CONDITIONAL, 1)
    print("  [metrics-service.yaml] Done (replaced values-driven ports).")
else:
    print("  WARNING: could not find ports anchor in metrics-service.yaml.")
    sys.exit(0)

with open(sys.argv[1], 'w') as f:
    f.write(content)
PYEOF
else
    if [ ! -f "${METRICS_SERVICE_FILE}" ]; then
        echo "  WARNING: ${METRICS_SERVICE_FILE} not found – skipping."
    else
        echo "  [metrics-service.yaml] metrics.secure conditional port already present – skipping."
    fi
fi

echo "=== Post-processing complete ==="
