#!/usr/bin/env bash
# Live-demo step: fetch the Azure Storage account key and wire it into the Azure objectStorage
# block. This is the "get the credential, put it in a Kubernetes secret" step for Part 2 of
# docs/spec/[DEMO] Object Storage.md. Never pass the key material as a literal command-line
# argument you type into a terminal — run this whole file with: bash aks-02-apply-azure-credentials.sh
set -euo pipefail

CONTEXT="pzhou-test-k8s"
RG="K8s-pzhou-test"
STORAGE_ACCOUNT="mltieredpocsa03865"
NS="marklogic"
CLUSTER_NAME="ml-tiered-blob-poc"
SECRET_NAME="ml-azure-credentials"

command kubectl config use-context "$CONTEXT"

echo "Fetching the primary key for storage account ${STORAGE_ACCOUNT}..."
STORAGE_KEY=$(az storage account keys list -n "$STORAGE_ACCOUNT" -g "$RG" --query "[0].value" -o tsv)

command kubectl -n "$NS" create secret generic "$SECRET_NAME" \
  --from-literal=storageAccount="$STORAGE_ACCOUNT" \
  --from-literal=storageKey="$STORAGE_KEY" \
  --dry-run=client -o yaml | command kubectl apply -f -
unset STORAGE_KEY

command kubectl -n "$NS" patch marklogiccluster "$CLUSTER_NAME" --type merge -p "$(cat <<EOF
spec:
  objectStorage:
    azure:
      authType: secret
      secretName: ${SECRET_NAME}
EOF
)"

echo "Waiting a few seconds for the operator to reconcile..."
sleep 5
echo "status.objectStorage:"
command kubectl -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.status.objectStorage}'
echo
echo "Verify against MarkLogic itself with:"
echo "  bash docs/spec/object-storage-demo/verify-credentials.sh '$CONTEXT' $NS $CLUSTER_NAME node azure"
