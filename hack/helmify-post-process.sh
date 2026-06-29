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
WEBHOOK_SERVICE_FILE="${TEMPLATES_DIR}/webhook-service.yaml"
WEBHOOK_CERTS_FILE="${TEMPLATES_DIR}/webhook-certs.yaml"
WEBHOOK_VALIDATION_FILE="${TEMPLATES_DIR}/webhook-validation.yaml"

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

if ! grep -q "^webhook:" "${VALUES_FILE}"; then
    echo "  [values.yaml] Adding webhook configuration..."
    cat >> "${VALUES_FILE}" << 'YAML_EOF'

# Admission webhook configuration
webhook:
  enabled: true
  failurePolicy: Fail
  timeoutSeconds: 10

  namespaceValidation:
    enabled: true

  certs:
    # selfSigned | certManager
    provider: selfSigned
    secretName: marklogic-operator-webhook-server-cert
    serviceName: marklogic-operator-webhook-service
    port: 9443

    selfSigned:
      validityDays: 365

    certManager:
      enabled: false
      certificateName: marklogic-operator-webhook-serving-cert
      issuerRef:
        kind: ClusterIssuer
        name: marklogic-operator-selfsigned
        group: cert-manager.io
      duration: 8760h
      renewBefore: 720h
YAML_EOF
    echo "  [values.yaml] Done (webhook)."
else
    echo "  [values.yaml] webhook already present – skipping."
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

# 2d. Add webhook-specific args/env/mounts/volumes used by the admission server.
if ! grep -q -- "--webhook-port=9443" "${DEPLOYMENT_FILE}"; then
  echo "  [deployment.yaml] Injecting webhook port arg..."
  python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys
path = sys.argv[1]
with open(path, 'r') as f:
  content = f.read()
content = content.replace('- --leader-elect\n', '- --leader-elect\n        - --webhook-port=9443\n')
with open(path, 'w') as f:
  f.write(content)
print("  [deployment.yaml] Done (webhook arg).")
PYEOF
else
  echo "  [deployment.yaml] webhook port arg already present – skipping."
fi

if ! grep -q "ENABLE_NAMESPACE_WEBHOOK_VALIDATION" "${DEPLOYMENT_FILE}"; then
  echo "  [deployment.yaml] Injecting ENABLE_NAMESPACE_WEBHOOK_VALIDATION env var..."
  python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys
path = sys.argv[1]
with open(path, 'r') as f:
  content = f.read()
anchor = '        - name: POD_NAMESPACE\n          valueFrom:\n            fieldRef:\n              fieldPath: metadata.namespace\n'
insert = anchor + '        - name: ENABLE_NAMESPACE_WEBHOOK_VALIDATION\n          value: {{ .Values.webhook.namespaceValidation.enabled | quote }}\n'
if anchor in content:
  content = content.replace(anchor, insert, 1)
  with open(path, 'w') as f:
    f.write(content)
  print("  [deployment.yaml] Done (webhook env).")
else:
  print("  WARNING: could not find POD_NAMESPACE anchor for webhook env injection.")
PYEOF
else
  echo "  [deployment.yaml] webhook env already present – skipping."
fi

if ! grep -q "serving-certs" "${DEPLOYMENT_FILE}"; then
  echo "  [deployment.yaml] Injecting webhook cert volume mount + secret volume..."
  python3 - "${DEPLOYMENT_FILE}" << 'PYEOF'
import sys
path = sys.argv[1]
with open(path, 'r') as f:
  content = f.read()

container_anchor = '        securityContext: {{- toYaml .Values.controllerManager.manager.containerSecurityContext\n          | nindent 10 }}\n'
container_insert = container_anchor + '        volumeMounts:\n        - mountPath: /tmp/k8s-webhook-server/serving-certs\n          name: webhook-server-cert\n          readOnly: true\n'

pod_anchor = '      topologySpreadConstraints: {{- toYaml .Values.controllerManager.topologySpreadConstraints\n        | nindent 8 }}\n'
pod_insert = pod_anchor + '      volumes:\n      - name: webhook-server-cert\n        secret:\n          secretName: {{ .Values.webhook.certs.secretName }}\n'

changed = False
if container_anchor in content:
  content = content.replace(container_anchor, container_insert, 1)
  changed = True
if pod_anchor in content:
  content = content.replace(pod_anchor, pod_insert, 1)
  changed = True

if changed:
  with open(path, 'w') as f:
    f.write(content)
  print("  [deployment.yaml] Done (webhook mount/volume).")
else:
  print("  WARNING: could not find deployment anchors for webhook mount/volume injection.")
PYEOF
else
  echo "  [deployment.yaml] webhook mount/volume already present – skipping."
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
  - ""
  resources:
  - persistentvolumeclaims
  verbs:
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
storageclass is a cluster-scoped resource; a namespaced Role cannot grant access
to it. A dedicated ClusterRole + ClusterRoleBinding is required in namespace mode
so the operator can read allowVolumeExpansion and perform PVC resize operations.
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
  - storage.k8s.io
  resources:
  - storageclasses
  verbs:
  - get
  - list
  - watch
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
else
    echo "  [manager-rbac.yaml] scope logic already present – skipping."
fi

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

# ──────────────────────────────────────────────────────────────────────────────
# 7. Webhook templates – generated post-helmify so make helm does not overwrite.
# ──────────────────────────────────────────────────────────────────────────────
echo "  [webhook templates] Writing webhook service/certs/validation templates..."
cat > "${WEBHOOK_SERVICE_FILE}" << 'TMPL_EOF'
{{- if .Values.webhook.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Values.webhook.certs.serviceName }}
  labels:
    app.kubernetes.io/component: webhook
    control-plane: controller-manager
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
spec:
  ports:
  - name: webhook
    port: {{ .Values.webhook.certs.port }}
    protocol: TCP
    targetPort: 9443
  selector:
    control-plane: controller-manager
  {{- include "marklogic-operator-kubernetes.selectorLabels" . | nindent 4 }}
{{- end }}
TMPL_EOF

cat > "${WEBHOOK_CERTS_FILE}" << 'TMPL_EOF'
{{- if and .Values.webhook.enabled (eq .Values.webhook.certs.provider "selfSigned") }}
{{- $secretName := .Values.webhook.certs.secretName }}
{{- $serviceName := .Values.webhook.certs.serviceName }}
{{- $namespace := .Release.Namespace }}
{{- $validity := int .Values.webhook.certs.selfSigned.validityDays }}
{{- $existing := lookup "v1" "Secret" $namespace $secretName }}
{{- $tlsCrt := "" }}
{{- $tlsKey := "" }}
{{- $caCrt := "" }}
{{- if and $existing (hasKey $existing.data "tls.crt") (hasKey $existing.data "tls.key") (hasKey $existing.data "ca.crt") }}
  {{- $tlsCrt = (index $existing.data "tls.crt") }}
  {{- $tlsKey = (index $existing.data "tls.key") }}
  {{- $caCrt = (index $existing.data "ca.crt") }}
{{- else }}
  {{- $ca := genCA (printf "%s-webhook-ca" (include "marklogic-operator-kubernetes.fullname" .)) $validity }}
  {{- $cert := genSignedCert $serviceName nil (list (printf "%s.%s" $serviceName $namespace) (printf "%s.%s.svc" $serviceName $namespace) (printf "%s.%s.svc.cluster.local" $serviceName $namespace)) $validity $ca }}
  {{- $tlsCrt = b64enc $cert.Cert }}
  {{- $tlsKey = b64enc $cert.Key }}
  {{- $caCrt = b64enc $ca.Cert }}
{{- end }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ $secretName }}
  labels:
    app.kubernetes.io/component: webhook
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
type: kubernetes.io/tls
data:
  tls.crt: {{ $tlsCrt }}
  tls.key: {{ $tlsKey }}
  ca.crt: {{ $caCrt }}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: marklogic-operator-validating-webhook
  labels:
    app.kubernetes.io/component: webhook
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
webhooks:
- name: vmarklogicclusters.marklogic.progress.com
  admissionReviewVersions:
  - v1
  sideEffects: None
  failurePolicy: {{ .Values.webhook.failurePolicy }}
  timeoutSeconds: {{ .Values.webhook.timeoutSeconds }}
  rules:
  - apiGroups:
    - marklogic.progress.com
    apiVersions:
    - v1
    operations:
    - CREATE
    - UPDATE
    resources:
    - marklogicclusters
  clientConfig:
    service:
      namespace: {{ .Release.Namespace }}
      name: {{ .Values.webhook.certs.serviceName }}
      path: /validate-marklogic-progress-com-v1-marklogiccluster
      port: {{ .Values.webhook.certs.port }}
    caBundle: {{ $caCrt }}
- name: vmarklogicgroups.marklogic.progress.com
  admissionReviewVersions:
  - v1
  sideEffects: None
  failurePolicy: {{ .Values.webhook.failurePolicy }}
  timeoutSeconds: {{ .Values.webhook.timeoutSeconds }}
  rules:
  - apiGroups:
    - marklogic.progress.com
    apiVersions:
    - v1
    operations:
    - CREATE
    - UPDATE
    resources:
    - marklogicgroups
  clientConfig:
    service:
      namespace: {{ .Release.Namespace }}
      name: {{ .Values.webhook.certs.serviceName }}
      path: /validate-marklogic-progress-com-v1-marklogicgroup
      port: {{ .Values.webhook.certs.port }}
    caBundle: {{ $caCrt }}
{{- end }}
---
{{- if and .Values.webhook.enabled (eq .Values.webhook.certs.provider "certManager") .Values.webhook.certs.certManager.enabled }}
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ .Values.webhook.certs.certManager.certificateName }}
  labels:
    app.kubernetes.io/component: webhook
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
spec:
  secretName: {{ .Values.webhook.certs.secretName }}
  issuerRef:
    kind: {{ .Values.webhook.certs.certManager.issuerRef.kind }}
    name: {{ .Values.webhook.certs.certManager.issuerRef.name }}
    group: {{ .Values.webhook.certs.certManager.issuerRef.group }}
  duration: {{ .Values.webhook.certs.certManager.duration }}
  renewBefore: {{ .Values.webhook.certs.certManager.renewBefore }}
  dnsNames:
  - {{ .Values.webhook.certs.serviceName }}
  - {{ printf "%s.%s" .Values.webhook.certs.serviceName .Release.Namespace }}
  - {{ printf "%s.%s.svc" .Values.webhook.certs.serviceName .Release.Namespace }}
  - {{ printf "%s.%s.svc.cluster.local" .Values.webhook.certs.serviceName .Release.Namespace }}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: marklogic-operator-validating-webhook
  labels:
    app.kubernetes.io/component: webhook
  {{- include "marklogic-operator-kubernetes.labels" . | nindent 4 }}
  annotations:
    cert-manager.io/inject-ca-from: {{ printf "%s/%s" .Release.Namespace .Values.webhook.certs.certManager.certificateName }}
webhooks:
- name: vmarklogicclusters.marklogic.progress.com
  admissionReviewVersions:
  - v1
  sideEffects: None
  failurePolicy: {{ .Values.webhook.failurePolicy }}
  timeoutSeconds: {{ .Values.webhook.timeoutSeconds }}
  rules:
  - apiGroups:
    - marklogic.progress.com
    apiVersions:
    - v1
    operations:
    - CREATE
    - UPDATE
    resources:
    - marklogicclusters
  clientConfig:
    service:
      namespace: {{ .Release.Namespace }}
      name: {{ .Values.webhook.certs.serviceName }}
      path: /validate-marklogic-progress-com-v1-marklogiccluster
      port: {{ .Values.webhook.certs.port }}
- name: vmarklogicgroups.marklogic.progress.com
  admissionReviewVersions:
  - v1
  sideEffects: None
  failurePolicy: {{ .Values.webhook.failurePolicy }}
  timeoutSeconds: {{ .Values.webhook.timeoutSeconds }}
  rules:
  - apiGroups:
    - marklogic.progress.com
    apiVersions:
    - v1
    operations:
    - CREATE
    - UPDATE
    resources:
    - marklogicgroups
  clientConfig:
    service:
      namespace: {{ .Release.Namespace }}
      name: {{ .Values.webhook.certs.serviceName }}
      path: /validate-marklogic-progress-com-v1-marklogicgroup
      port: {{ .Values.webhook.certs.port }}
{{- end }}
TMPL_EOF

cat > "${WEBHOOK_VALIDATION_FILE}" << 'TMPL_EOF'
{{- if and .Values.webhook.enabled (not (or (eq .Values.webhook.certs.provider "selfSigned") (eq .Values.webhook.certs.provider "certManager"))) }}
{{- fail "Invalid configuration: webhook.certs.provider must be one of [selfSigned, certManager]." }}
{{- end }}

{{- if and .Values.webhook.enabled (eq .Values.webhook.certs.provider "certManager") (not .Values.webhook.certs.certManager.enabled) }}
{{- fail "Invalid configuration: webhook.certs.provider=certManager requires webhook.certs.certManager.enabled=true." }}
{{- end }}
TMPL_EOF
echo "  [webhook templates] Done."

echo "=== Post-processing complete ==="
