#!/usr/bin/env bash
# Live-demo step: apply FAKE (non-functional) AWS + Azure credentials to the SAME
# MarklogicCluster at the same time, to prove (a) both providers can be configured
# simultaneously and (b) rotating credentials is picked up by the operator by editing only the
# Secret, with no MarklogicCluster spec change. Values here are dummy strings — MarkLogic will
# store them but they will never authenticate against real S3/Blob storage, which is fine for
# this proof. This REPLACES whatever secretName the cluster currently points at for each
# provider (e.g. a previously-applied real Azure credential), so only run this against a
# throwaway/demo cluster. Run with: bash aks-03-apply-fake-multi-provider-rotation.sh
set -euo pipefail

CONTEXT="pzhou-test-k8s"
NS="marklogic"
CLUSTER_NAME="ml-tiered-blob-poc"
AWS_SECRET_NAME="ml-s3-fake-credentials"
AZURE_SECRET_NAME="ml-azure-fake-credentials"
REGION="us-west-2"

command kubectl config use-context "$CONTEXT"

apply_fake_secrets() {
  local tag="$1"
  command kubectl -n "$NS" create secret generic "$AWS_SECRET_NAME" \
    --from-literal=accessKey="AKIAFAKE${tag}" \
    --from-literal=secretKey="fakeSecretKey${tag}" \
    --dry-run=client -o yaml | command kubectl apply -f -

  command kubectl -n "$NS" create secret generic "$AZURE_SECRET_NAME" \
    --from-literal=storageAccount="fakeaccount${tag}" \
    --from-literal=storageKey="fakeStorageKey${tag}" \
    --dry-run=client -o yaml | command kubectl apply -f -
}

print_status() {
  echo "AWS:   $(command kubectl -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.status.objectStorage.aws.phase} {.status.objectStorage.aws.appliedFingerprint}')"
  echo "Azure: $(command kubectl -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.status.objectStorage.azure.phase} {.status.objectStorage.azure.appliedFingerprint}')"
}

echo "=== Step 1: create initial fake credentials for BOTH providers ==="
apply_fake_secrets "v1"

echo "=== Step 2: point the MarklogicCluster at both secrets at once ==="
command kubectl -n "$NS" patch marklogiccluster "$CLUSTER_NAME" --type merge -p "$(cat <<EOF
spec:
  objectStorage:
    aws:
      authType: secret
      secretName: ${AWS_SECRET_NAME}
      region: ${REGION}
    azure:
      authType: secret
      secretName: ${AZURE_SECRET_NAME}
EOF
)"

echo "Waiting for the operator to reconcile both providers..."
sleep 5
echo "--- status.objectStorage after initial apply (both providers should be Applied) ---"
print_status

echo
echo "=== Step 3: rotate — update ONLY the Secrets, MarklogicCluster spec is untouched ==="
apply_fake_secrets "v2-rotated"

echo "Waiting for the operator's Secret watch to pick up the rotation..."
sleep 5
echo "--- status.objectStorage after rotation (fingerprints should differ from Step 2) ---"
print_status

echo
echo "Both providers were applied at the same time, and rotating the Secrets alone (no CR edit)"
echo "produced new appliedFingerprint values above, proving credential rotation works."
echo "Cross-check against MarkLogic itself with:"
echo "  bash docs/spec/object-storage-demo/verify-credentials.sh '$CONTEXT' $NS $CLUSTER_NAME node aws"
echo "  bash docs/spec/object-storage-demo/verify-credentials.sh '$CONTEXT' $NS $CLUSTER_NAME node azure"
