#!/usr/bin/env bash
# One-time (idempotent) AKS bring-up for the Azure Blob side of the object storage demo
# (docs/spec/object-storage/[DEMO] Object Storage.md). Safe to re-run any time: it hashes the operator source
# tree, compares that hash against an annotation on the live Deployment, and only
# rebuilds/pushes/redeploys when the source actually changed. Also skips creating the
# MarklogicCluster if it already exists. Run from the repo root: bash docs/spec/object-storage/demo/aks-01-provision.sh
set -euo pipefail

RG="K8s-pzhou-test"
AKS_NAME="pzhou-test-k8s"
ACR="pzhouacr"
CONTEXT="pzhou-test-k8s"
IMG="${ACR}.azurecr.io/marklogic-operator-kubernetes:objectstorage-poc"
NS="marklogic"
CLUSTER_NAME="ml-tiered-blob-poc"
DEPLOY_NS="marklogic-operator-system"
DEPLOY_NAME="marklogic-operator-controller-manager"
HASH_ANNOTATION="marklogic.progress.com/source-hash"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"
cd "$SCRIPT_DIR/../../../.."

# Fast path once both clusters are already fully provisioned: just point kubectl/az at this
# one and exit, instead of re-running the build/deploy/bootstrap checks below.
if [[ "${1:-}" == "--switch" || "${SWITCH_ONLY:-}" == "1" ]]; then
  az aks get-credentials --resource-group "$RG" --name "$AKS_NAME" --overwrite-existing
  kube config use-context "$CONTEXT"
  echo "Switched to AKS context ${CONTEXT} — no provisioning performed."
  exit 0
fi

az aks get-credentials --resource-group "$RG" --name "$AKS_NAME" --overwrite-existing

SRC_HASH="$(compute_source_hash)"
DEPLOYED_HASH=$(kube -n "$DEPLOY_NS" get deploy "$DEPLOY_NAME" \
  -o jsonpath="{.metadata.annotations.${HASH_ANNOTATION//./\\.}}" 2>/dev/null || true)

echo "Local source hash:    ${SRC_HASH}"
echo "Deployed source hash: ${DEPLOYED_HASH:-<none — not deployed yet>}"

if [[ -n "$DEPLOYED_HASH" && "$DEPLOYED_HASH" == "$SRC_HASH" ]] \
   && kube -n "$DEPLOY_NS" rollout status deploy/"$DEPLOY_NAME" --timeout=5s >/dev/null 2>&1; then
  echo "Operator already running the current source — skipping build/push/redeploy."
else
  echo "Source changed (or first run) — building, pushing, and redeploying."

  az acr login --name "$ACR"
  make kustomize
  make docker-build IMG="$IMG"
  make docker-push IMG="$IMG"

  # CRDs/RBAC on this cluster were originally installed by Helm; force-conflicts takes ownership.
  bin/kustomize build config/crd | kube apply --server-side --force-conflicts -f -

  RENDER_DIR=$(mktemp -d)
  trap 'rm -rf "$RENDER_DIR"' EXIT
  cp -R config "$RENDER_DIR/config"
  KUSTOMIZE="$PWD/bin/kustomize"
  (cd "$RENDER_DIR/config/manager" && "$KUSTOMIZE" edit set image controller="$IMG")
  bin/kustomize build "$RENDER_DIR/config/default" | kube apply --server-side --force-conflicts -f -
  kube -n "$DEPLOY_NS" set image deploy/"$DEPLOY_NAME" manager="$IMG"
  kube -n "$DEPLOY_NS" patch deploy "$DEPLOY_NAME" --type merge \
    -p '{"spec":{"template":{"spec":{"serviceAccountName":"marklogic-operator-controller-manager"}}}}'
  # A fixed demo tag must be pulled after each rebuild.
  kube -n "$DEPLOY_NS" patch deploy "$DEPLOY_NAME" \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"Always"}]}}}}'
  # The image tag never changes (static ":objectstorage-poc"), so `set image` alone doesn't
  # trigger a new Pod — force one so the fresh push actually gets pulled (imagePullPolicy: Always).
  kube -n "$DEPLOY_NS" rollout restart deploy/"$DEPLOY_NAME"
fi

# Refresh the ACR imagePullSecret every run (cheap, and admin credentials can expire/rotate
# independent of whether the source changed).
ACR_USER=$(az acr credential show -n "$ACR" -g "$RG" --query username -o tsv)
ACR_PASS=$(az acr credential show -n "$ACR" -g "$RG" --query "passwords[0].value" -o tsv)
SERVER=$(az acr show -n "$ACR" -g "$RG" --query loginServer -o tsv)
export ACR_USER ACR_PASS SERVER
python3 - "$DEPLOY_NS" <<'PYREGISTRY' | kube apply --server-side --field-manager=object-storage-demo -f -
import base64, json, os, sys
user, password, server = (os.environ[k] for k in ('ACR_USER', 'ACR_PASS', 'SERVER'))
if not all((user, password, server)):
    raise SystemExit('ACR returned incomplete registry credentials')
auth = base64.b64encode((user + ':' + password).encode()).decode()
config = json.dumps({'auths': {server: {'username': user, 'password': password, 'auth': auth}}})
print(json.dumps({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {
    'name': 'acr-pull', 'namespace': sys.argv[1]}, 'type': 'kubernetes.io/dockerconfigjson',
    'data': {'.dockerconfigjson': base64.b64encode(config.encode()).decode()}}))
PYREGISTRY
unset ACR_PASS ACR_USER

kube -n "$DEPLOY_NS" patch deploy "$DEPLOY_NAME" --type=strategic \
  -p '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"acr-pull"}]}}}}'
kube -n "$DEPLOY_NS" rollout status deploy/"$DEPLOY_NAME" --timeout=180s
kube -n "$DEPLOY_NS" annotate deploy "$DEPLOY_NAME" "${HASH_ANNOTATION}=${SRC_HASH}" --overwrite

kube create namespace "$NS" --dry-run=client -o yaml | kube apply -f -

# Recreate option: wipe the existing MarklogicCluster + its PVCs (data loss) so the block below
# bootstraps a brand-new cluster, while the AKS cluster itself is left untouched.
if [[ "${1:-}" == "--recreate" || "${RECREATE_CLUSTER:-}" == "1" ]]; then
  recreate_demo_cluster
fi

if kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" >/dev/null 2>&1; then
  echo "MarklogicCluster/${CLUSTER_NAME} already exists in ns ${NS} — skipping bootstrap."
else
  kube apply -f - <<EOF
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
fi
echo "Waiting for bootstrap pod node-0 to become Ready (this can take a few minutes)..."
echo "Waiting for the operator to create pod/node-0..."
for _ in $(seq 1 60); do
  kube -n "$NS" get pod node-0 >/dev/null 2>&1 && break
  sleep 5
done
kube -n "$NS" wait --for=condition=Ready pod/node-0 --timeout=600s

echo "AKS provisioning done: MarklogicCluster/${CLUSTER_NAME} in ns ${NS} bootstrap pod is Ready."
echo "Next: bash docs/spec/object-storage/demo/aks-02-apply-azure-credentials.sh"
