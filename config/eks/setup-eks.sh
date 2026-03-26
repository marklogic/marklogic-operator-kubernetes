#!/usr/bin/env bash
# Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.
#
# One-time idempotent setup script for the Jenkins EKS test environment.
# Safe to re-run — every step checks for the resource before creating it.
#
# What this script sets up:
#   1. EKS cluster (via eksctl + cluster-config.yaml)
#   2. ECR repositories for MarkLogic server images and the Operator image
#   3. IAM policy + service account for the AWS Load Balancer Controller
#   4. AWS Load Balancer Controller (Helm)
#
# To replicate in a different AWS account:
#   Set the following environment variables before running:
#     AWS_ACCOUNT_ID   — target account (default: resolved via aws sts)
#     AWS_DEFAULT_REGION — target region (default: us-west-1)
#     CLUSTER_NAME     — EKS cluster name (default: jenkins-kube-ninjas)
#
# Prerequisites (must be installed and on PATH):
#   aws, eksctl, kubectl, helm, docker, jq
#
# Usage:
#   export AWS_ACCESS_KEY_ID=...
#   export AWS_SECRET_ACCESS_KEY=...
#   ./config/eks/setup-eks.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration — override via environment variables
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-jenkins-kube-ninjas}"
AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-west-1}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_CONFIG="${SCRIPT_DIR}/cluster-config.yaml"
LB_POLICY_NAME="AWSLoadBalancerControllerIAMPolicy"
# Pinned IAM policy document for the AWS Load Balancer Controller v2.8.x
LB_POLICY_URL="https://raw.githubusercontent.com/kubernetes-sigs/aws-load-balancer-controller/v2.8.3/docs/install/iam_policy.json"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log()  { echo "[$(date '+%H:%M:%S')] $*"; }
ok()   { echo "[$(date '+%H:%M:%S')] ✓ $*"; }
fail() { echo "[$(date '+%H:%M:%S')] ✗ $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 1. Prerequisites check
# ---------------------------------------------------------------------------
log "Checking prerequisites..."
for tool in aws eksctl kubectl helm docker jq; do
  command -v "$tool" >/dev/null 2>&1 || fail "Required tool not found: $tool"
done
ok "All prerequisites present"

# Resolve AWS account ID (use env var if already set to avoid an extra API call)
if [[ -z "${AWS_ACCOUNT_ID:-}" ]]; then
  AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
fi
log "AWS account: ${AWS_ACCOUNT_ID}, region: ${AWS_DEFAULT_REGION}"

# ---------------------------------------------------------------------------
# 2. EKS cluster
# ---------------------------------------------------------------------------
log "Checking if EKS cluster '${CLUSTER_NAME}' exists..."
if eksctl get cluster --name "${CLUSTER_NAME}" --region "${AWS_DEFAULT_REGION}" >/dev/null 2>&1; then
  ok "EKS cluster '${CLUSTER_NAME}' already exists — skipping creation"
else
  log "Creating EKS cluster '${CLUSTER_NAME}' (this takes ~15 minutes)..."
  eksctl create cluster -f "${CLUSTER_CONFIG}"
  ok "EKS cluster created"
fi

# ---------------------------------------------------------------------------
# 3. ECR repositories
# ---------------------------------------------------------------------------
ECR_REGISTRY="${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_DEFAULT_REGION}.amazonaws.com"

create_ecr_repo() {
  local repo_name="$1"
  if aws ecr describe-repositories --repository-names "${repo_name}" --region "${AWS_DEFAULT_REGION}" >/dev/null 2>&1; then
    ok "ECR repo '${repo_name}' already exists"
  else
    log "Creating ECR repo: ${repo_name}"
    aws ecr create-repository \
      --repository-name "${repo_name}" \
      --region "${AWS_DEFAULT_REGION}" \
      --image-scanning-configuration scanOnPush=true \
      --encryption-configuration encryptionType=AES256 \
      >/dev/null
    ok "ECR repo created: ${ECR_REGISTRY}/${repo_name}"
  fi
}

create_ecr_repo "${CLUSTER_NAME}/marklogic-server-ubi"
create_ecr_repo "${CLUSTER_NAME}/marklogic-server-ubi-rootless"
create_ecr_repo "${CLUSTER_NAME}/marklogic-kubernetes-operator"

# ---------------------------------------------------------------------------
# 4. IAM policy for the AWS Load Balancer Controller
# ---------------------------------------------------------------------------
log "Checking IAM policy '${LB_POLICY_NAME}'..."
if aws iam get-policy \
     --policy-arn "arn:aws:iam::${AWS_ACCOUNT_ID}:policy/${LB_POLICY_NAME}" \
     >/dev/null 2>&1; then
  ok "IAM policy '${LB_POLICY_NAME}' already exists"
else
  log "Downloading IAM policy document from pinned URL..."
  TMP_POLICY="$(mktemp)"
  curl -fsSL "${LB_POLICY_URL}" -o "${TMP_POLICY}"
  log "Creating IAM policy '${LB_POLICY_NAME}'..."
  aws iam create-policy \
    --policy-name "${LB_POLICY_NAME}" \
    --policy-document "file://${TMP_POLICY}" \
    >/dev/null
  rm -f "${TMP_POLICY}"
  ok "IAM policy created"
fi
LB_POLICY_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:policy/${LB_POLICY_NAME}"

# ---------------------------------------------------------------------------
# 5. IAM service account for the AWS Load Balancer Controller
# ---------------------------------------------------------------------------
log "Checking IAM service account 'aws-load-balancer-controller' in kube-system..."
if eksctl get iamserviceaccount \
     --cluster "${CLUSTER_NAME}" \
     --namespace kube-system \
     --name aws-load-balancer-controller \
     --region "${AWS_DEFAULT_REGION}" 2>/dev/null | grep -q aws-load-balancer-controller; then
  ok "IAM service account already exists"
else
  log "Creating IAM service account..."
  # Associate OIDC provider first if not already associated
  eksctl utils associate-iam-oidc-provider \
    --cluster "${CLUSTER_NAME}" \
    --region "${AWS_DEFAULT_REGION}" \
    --approve

  eksctl create iamserviceaccount \
    --cluster "${CLUSTER_NAME}" \
    --namespace kube-system \
    --name aws-load-balancer-controller \
    --attach-policy-arn "${LB_POLICY_ARN}" \
    --region "${AWS_DEFAULT_REGION}" \
    --override-existing-serviceaccounts \
    --approve
  ok "IAM service account created"
fi

# ---------------------------------------------------------------------------
# 6. AWS Load Balancer Controller (Helm)
# ---------------------------------------------------------------------------
log "Updating kubeconfig for cluster '${CLUSTER_NAME}'..."
aws eks update-kubeconfig \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_DEFAULT_REGION}"

log "Checking AWS Load Balancer Controller Helm release..."
if helm status aws-load-balancer-controller -n kube-system >/dev/null 2>&1; then
  ok "AWS Load Balancer Controller already installed"
else
  log "Adding EKS Helm repo..."
  helm repo add eks https://aws.github.io/eks-charts >/dev/null 2>&1 || true
  helm repo update eks >/dev/null

  log "Applying Load Balancer Controller CRDs..."
  kubectl apply -k "github.com/aws/eks-charts/stable/aws-load-balancer-controller/crds?ref=master"

  log "Installing AWS Load Balancer Controller via Helm..."
  helm upgrade --install aws-load-balancer-controller eks/aws-load-balancer-controller \
    -n kube-system \
    --set clusterName="${CLUSTER_NAME}" \
    --set serviceAccount.create=false \
    --set serviceAccount.name=aws-load-balancer-controller

  # Only wait for pods if worker nodes are available; they may be scaled to 0
  # when the cluster is idle and will come up automatically on eks-scale-up.
  if kubectl get nodes --no-headers 2>/dev/null | grep -q Ready; then
    log "Waiting for Load Balancer Controller pods to be ready..."
    kubectl wait --for=condition=Available deployment/aws-load-balancer-controller \
      -n kube-system --timeout=120s
    ok "AWS Load Balancer Controller installed and ready"
  else
    ok "AWS Load Balancer Controller installed (pods will start once nodes are scaled up)"
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo "  EKS environment setup complete"
echo "============================================================"
echo "  Cluster:       ${CLUSTER_NAME} (${AWS_DEFAULT_REGION})"
echo "  ECR registry:  ${ECR_REGISTRY}"
echo "  ECR repos:"
echo "    ${CLUSTER_NAME}/marklogic-server-ubi"
echo "    ${CLUSTER_NAME}/marklogic-server-ubi-rootless"
echo "    ${CLUSTER_NAME}/marklogic-kubernetes-operator"
echo ""
echo "  Worker nodes are currently at their initial capacity."
echo "  To scale down to 0 when idle:"
echo "    eksctl scale nodegroup --cluster ${CLUSTER_NAME} --name ml-worker --nodes 0 --region ${AWS_DEFAULT_REGION}"
echo ""
echo "  To scale back up for a test run:"
echo "    eksctl scale nodegroup --cluster ${CLUSTER_NAME} --name ml-worker --nodes 3 --region ${AWS_DEFAULT_REGION}"
echo "============================================================"
