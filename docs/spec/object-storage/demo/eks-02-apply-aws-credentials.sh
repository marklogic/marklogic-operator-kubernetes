#!/usr/bin/env bash
# Live-demo step: apply supplied AWS S3 credentials and wire them into the AWS objectStorage block.
# This is the "get the credential, put it in a Kubernetes secret" step for Part 1 of
# docs/spec/object-storage/[DEMO] Object Storage.md. Never pass the key material as a literal command-line
# argument you type into a terminal — run this whole file with: bash eks-02-apply-aws-credentials.sh
set -euo pipefail

CONTEXT="pzhou@marklogic-tiered-poc.us-west-2.eksctl.io"
NS="marklogic"
CLUSTER_NAME="ml-tiered-poc"
REGION="us-west-2"
SECRET_NAME="ml-s3-credentials"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

: "${AWS_ACCESS_KEY_ID:?export AWS_ACCESS_KEY_ID from your credential source}"
: "${AWS_SECRET_ACCESS_KEY:?export AWS_SECRET_ACCESS_KEY from your credential source}"
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
apply_credential_secret accessKey=AWS_ACCESS_KEY_ID secretKey=AWS_SECRET_ACCESS_KEY sessionToken=AWS_SESSION_TOKEN

kube -n "$NS" patch marklogiccluster "$CLUSTER_NAME" --type merge -p "$(cat <<EOF
spec:
  objectStorage:
    aws:
      authType: secret
      secretName: ${SECRET_NAME}
      region: ${REGION}
EOF
)"

wait_applied aws "$SECRET_NAME"
echo "status.objectStorage:"
kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.status.objectStorage}'
echo
echo "Verify against MarkLogic itself with:"
echo "  bash docs/spec/object-storage/demo/verify-credentials.sh '$CONTEXT' $NS $CLUSTER_NAME node aws"
