#!/usr/bin/env bash
# Live-demo step: fetch the Azure Storage account key and wire it into the Azure objectStorage
# block. This is the "get the credential, put it in a Kubernetes secret" step for Part 2 of
# docs/spec/object-storage/[DEMO] Object Storage.md. Never pass the key material as a literal command-line
# argument you type into a terminal — run this whole file with: bash aks-02-apply-azure-credentials.sh
set -euo pipefail

CONTEXT="pzhou-test-k8s"
RG="K8s-pzhou-test"
STORAGE_ACCOUNT="mltieredpocsa03865"
NS="marklogic"
CLUSTER_NAME="ml-tiered-blob-poc"
SECRET_NAME="ml-azure-credentials"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

echo "Fetching the primary key for storage account ${STORAGE_ACCOUNT}..."
STORAGE_KEY=$(az storage account keys list -n "$STORAGE_ACCOUNT" -g "$RG" --query "[0].value" -o tsv)

export STORAGE_ACCOUNT STORAGE_KEY
[[ -n "$STORAGE_KEY" ]] || { echo "Azure returned an empty storage key" >&2; exit 1; }
apply_credential_secret storageAccount=STORAGE_ACCOUNT storageKey=STORAGE_KEY
unset STORAGE_KEY

kube -n "$NS" patch marklogiccluster "$CLUSTER_NAME" --type merge -p "$(cat <<EOF
spec:
  objectStorage:
    azure:
      authType: secret
      secretName: ${SECRET_NAME}
EOF
)"

wait_applied azure "$SECRET_NAME"
echo "status.objectStorage:"
kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.status.objectStorage}'
echo
echo "Verify against MarkLogic itself with:"
echo "  bash docs/spec/object-storage/demo/verify-credentials.sh '$CONTEXT' $NS $CLUSTER_NAME node azure"
