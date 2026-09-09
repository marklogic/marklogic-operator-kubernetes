#!/usr/bin/env bash
# Confirms applied object storage credentials directly against MarkLogic's Management API,
# independent of the operator's own status reporting. Works for either cloud/cluster.
#
# Usage: bash verify-credentials.sh <kube-context> <namespace> <marklogicCluster-name> <service-name> <aws|azure>
# Example: bash verify-credentials.sh pzhou-test-k8s marklogic ml-tiered-blob-poc node azure
set -euo pipefail

CONTEXT="${1:?kube context required}"
NS="${2:?namespace required}"
CLUSTER_NAME="${3:?MarklogicCluster name required}"
SVC="${4:?service name required}"
PROVIDER="${5:?provider required: aws or azure}"

ADMIN_SECRET="${CLUSTER_NAME}-admin"

ADMIN_USER=$(command kubectl --context "$CONTEXT" -n "$NS" get secret "$ADMIN_SECRET" -o jsonpath='{.data.username}' | base64 -d)
ADMIN_PASS=$(command kubectl --context "$CONTEXT" -n "$NS" get secret "$ADMIN_SECRET" -o jsonpath='{.data.password}' | base64 -d)

command kubectl --context "$CONTEXT" -n "$NS" port-forward "svc/${SVC}" 8002:8002 >/tmp/pf-verify-credentials.log 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 3

echo "GET /manage/v2/credentials/properties?type=${PROVIDER}"
curl -s -u "$ADMIN_USER:$ADMIN_PASS" --anyauth \
  "http://localhost:8002/manage/v2/credentials/properties?type=${PROVIDER}&format=json"
echo
unset ADMIN_PASS
