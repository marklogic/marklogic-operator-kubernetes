#!/usr/bin/env bash
# One-time (idempotent) EKS bring-up for the AWS S3 side of the object storage demo
# (docs/spec/[DEMO] Object Storage.md). Safe to re-run any time: it hashes the operator source
# tree, compares that hash against an annotation on the live Deployment, and only
# rebuilds/pushes/redeploys when the source actually changed. MarklogicCluster bootstrap is
# skipped if it already exists. Run from the repo root: bash docs/spec/object-storage-demo/eks-01-provision.sh
set -euo pipefail

CONTEXT="pzhou@marklogic-tiered-poc.us-west-2.eksctl.io"
REGION="us-west-2"
ACCOUNT_ID="308453789681"
ECR_HOST="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"
IMG="${ECR_HOST}/marklogic-tiered-poc/marklogic-operator:objectstorage-demo"
NS="marklogic"
CLUSTER_NAME="ml-tiered-poc"
DEPLOY_NS="marklogic-operator-system"
DEPLOY_NAME="marklogic-operator-controller-manager"
HASH_ANNOTATION="marklogic.progress.com/source-hash"

# Fast path once both clusters are already fully provisioned: just point kubectl at this one
# and exit, instead of re-running the build/deploy/bootstrap checks below.
if [[ "${1:-}" == "--switch" || "${SWITCH_ONLY:-}" == "1" ]]; then
  command kubectl config use-context "$CONTEXT"
  echo "Switched to EKS context ${CONTEXT} — no provisioning performed."
  exit 0
fi

# Always operate from the repo root, regardless of how/where this script was invoked from —
# `make`, `bin/kustomize`, and the source-hash below all rely on repo-root-relative paths.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/../../.."

command kubectl config use-context "$CONTEXT"

_sha256() { command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256; }

# Content hash of everything that affects the built image, so local edits (committed or not)
# are detected the same way a `git diff` would show them.
compute_source_hash() {
  local paths=(api cmd internal config Dockerfile Makefile go.mod go.sum) existing=()
  for p in "${paths[@]}"; do [[ -e "$p" ]] && existing+=("$p"); done
  find "${existing[@]}" -type f 2>/dev/null | sort | xargs cat | _sha256 | awk '{print $1}' | cut -c1-12
}

SRC_HASH="$(compute_source_hash)"
DEPLOYED_HASH=$(command kubectl -n "$DEPLOY_NS" get deploy "$DEPLOY_NAME" \
  -o jsonpath="{.metadata.annotations.${HASH_ANNOTATION//./\\.}}" 2>/dev/null || true)

echo "Local source hash:    ${SRC_HASH}"
echo "Deployed source hash: ${DEPLOYED_HASH:-<none — not deployed yet>}"

if [[ -n "$DEPLOYED_HASH" && "$DEPLOYED_HASH" == "$SRC_HASH" ]] \
   && command kubectl -n "$DEPLOY_NS" rollout status deploy/"$DEPLOY_NAME" --timeout=5s >/dev/null 2>&1; then
  echo "Operator already running the current source — skipping build/push/redeploy."
else
  echo "Source changed (or first run) — building, pushing, and redeploying."

  aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$ECR_HOST"

  make docker-build IMG="$IMG"
  make docker-push IMG="$IMG"

  # CRDs/RBAC may be owned by an earlier Helm install on this cluster; force-conflicts takes ownership.
  bin/kustomize build config/crd | command kubectl apply --server-side --force-conflicts -f -

  (cd config/manager && ../../bin/kustomize edit set image controller="$IMG")
  bin/kustomize build config/default | command kubectl apply --server-side --force-conflicts -f - || {
    echo "kustomize apply of config/default hit a conflict (likely an immutable Deployment selector" \
         "from a prior Helm install) — falling back to a direct image/serviceAccount patch." >&2
    command kubectl -n "$DEPLOY_NS" set image deploy/"$DEPLOY_NAME" manager="$IMG"
    command kubectl -n "$DEPLOY_NS" patch deploy "$DEPLOY_NAME" --type merge \
      -p '{"spec":{"template":{"spec":{"serviceAccountName":"marklogic-operator-controller-manager"}}}}'
  }

  command kubectl -n "$DEPLOY_NS" set image deploy/"$DEPLOY_NAME" manager="$IMG"
  # The config/default kustomize apply above may have hit a Helm-selector conflict and fallen
  # through to just the image/serviceAccount patches, which never apply manager.yaml's
  # imagePullPolicy — set it explicitly so a restart is guaranteed to actually re-pull.
  command kubectl -n "$DEPLOY_NS" patch deploy "$DEPLOY_NAME" \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"Always"}]}}}}'
  command kubectl -n "$DEPLOY_NS" annotate deploy "$DEPLOY_NAME" "${HASH_ANNOTATION}=${SRC_HASH}" --overwrite
  # The image tag never changes (static ":objectstorage-demo"), so `set image` alone doesn't
  # trigger a new Pod — force one so the fresh push actually gets pulled (imagePullPolicy: Always).
  command kubectl -n "$DEPLOY_NS" rollout restart deploy/"$DEPLOY_NAME"

  command kubectl -n "$DEPLOY_NS" rollout status deploy/"$DEPLOY_NAME" --timeout=180s
fi

command kubectl create namespace "$NS" --dry-run=client -o yaml | command kubectl apply -f -

# Recreate option: wipe the existing MarklogicCluster + its PVCs (data loss) so the block below
# bootstraps a brand-new cluster, while the EKS cluster itself is left untouched.
if [[ "${1:-}" == "--recreate" || "${RECREATE_CLUSTER:-}" == "1" ]]; then
  echo "Recreating MarklogicCluster/${CLUSTER_NAME}: deleting CR and its datadir PVCs (data will be lost)."
  command kubectl -n "$NS" delete marklogiccluster "$CLUSTER_NAME" --ignore-not-found --wait=true --timeout=180s
  command kubectl -n "$NS" get pvc -o name | { grep "^persistentvolumeclaim/datadir-${CLUSTER_NAME}-" || true; } | \
    xargs -r command kubectl -n "$NS" delete --wait=true
fi

if command kubectl -n "$NS" get marklogiccluster "$CLUSTER_NAME" >/dev/null 2>&1; then
  echo "MarklogicCluster/${CLUSTER_NAME} already exists in ns ${NS} — skipping bootstrap."
else
  command kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: ${CLUSTER_NAME}
  namespace: ${NS}
spec:
  image: "progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6"
  persistence:
    enabled: true
    size: 20Gi
  markLogicGroups:
  - replicas: 1
    name: node
    isBootstrap: true
EOF

  echo "Waiting for bootstrap pod node-0 to become Ready (this can take a few minutes)..."
  echo "Waiting for the operator to create pod/node-0..."
  for _ in $(seq 1 60); do
    command kubectl -n "$NS" get pod node-0 >/dev/null 2>&1 && break
    sleep 5
  done
  command kubectl -n "$NS" wait --for=condition=Ready pod/node-0 --timeout=600s
fi

echo "EKS provisioning done: MarklogicCluster/${CLUSTER_NAME} in ns ${NS} is Ready, no objectStorage configured yet."
echo "Next: bash docs/spec/object-storage-demo/eks-02-apply-aws-credentials.sh"
