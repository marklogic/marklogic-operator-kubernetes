#!/usr/bin/env bash
# Live-demo step: mint AWS S3 credentials and wire them into the AWS objectStorage block.
# This is the "get the credential, put it in a Kubernetes secret" step for Part 1 of
# docs/spec/[DEMO] Object Storage.md. Never pass the key material as a literal command-line
# argument you type into a terminal — run this whole file with: bash eks-02-apply-aws-credentials.sh
set -euo pipefail

CONTEXT="pzhou@marklogic-tiered-poc.us-west-2.eksctl.io"
NS="marklogic"
CLUSTER_NAME="ml-tiered-poc"
IAM_USER="marklogic-objstore-demo"
REGION="us-west-2"
SECRET_NAME="ml-s3-credentials"
# Pre-existing key whose secret value was never recorded (namespace was wiped); deactivated below
# once the new key is confirmed working. Leave empty to skip.
OLD_KEY_ID="AKIAUPUKHJ7Y237J4BGO"

command kubectl config use-context "$CONTEXT"

echo "Minting a new access key for IAM user ${IAM_USER}..."
CREDS_JSON=$(aws iam create-access-key --user-name "$IAM_USER" --output json)
ACCESS_KEY=$(echo "$CREDS_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["AccessKey"]["AccessKeyId"])')
SECRET_KEY=$(echo "$CREDS_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["AccessKey"]["SecretAccessKey"])')
unset CREDS_JSON

command kubectl -n "$NS" create secret generic "$SECRET_NAME" \
  --from-literal=accessKey="$ACCESS_KEY" \
  --from-literal=secretKey="$SECRET_KEY" \
  --dry-run=client -o yaml | command kubectl apply -f -
unset ACCESS_KEY SECRET_KEY

if [[ -n "$OLD_KEY_ID" ]]; then
  echo "Deactivating orphaned old access key ${OLD_KEY_ID}..."
  aws iam update-access-key --user-name "$IAM_USER" --access-key-id "$OLD_KEY_ID" --status Inactive || true
fi

command kubectl -n "$NS" patch marklogiccluster "$CLUSTER_NAME" --type merge -p "$(cat <<EOF
spec:
  objectStorage:
    aws:
      authType: secret
      secretName: ${SECRET_NAME}
      region: ${REGION}
EOF
)"

echo "Waiting a few seconds for the operator to reconcile..."
sleep 5
echo "status.objectStorage:"
command kubectl -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.status.objectStorage}'
echo
echo "Verify against MarkLogic itself with:"
echo "  bash docs/spec/object-storage-demo/verify-credentials.sh '$CONTEXT' $NS $CLUSTER_NAME node aws"
