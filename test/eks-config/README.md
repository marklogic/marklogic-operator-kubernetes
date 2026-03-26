# EKS Test Environment — MarkLogic Kubernetes Operator

## Overview

This directory contains configuration and setup tooling for the persistent EKS
cluster (`jenkins-kube-ninjas`) used to run end-to-end tests of the MarkLogic
Kubernetes Operator on AWS EKS.

| File | Purpose |
|---|---|
| `cluster-config.yaml` | eksctl ClusterConfig — declarative cluster definition |
| `setup-eks.sh` | Idempotent one-time bootstrap script |

---

## Cluster Details

| Property | Value |
|---|---|
| **Cluster name** | `jenkins-kube-ninjas` |
| **AWS Region** | `us-west-1` |
| **AWS Account** | set via `AWS_ACCOUNT_ID` environment variable |
| **Node group** | `ml-worker` |
| **Instance type** | `r5.2xlarge` |
| **Node OS** | Amazon Linux 2023 (AL2023) |
| **Min nodes** | 0 (scaled down when idle) |
| **Max nodes** | 6 |
| **Desired (active)** | 3 |
| **Kubernetes version** | 1.34 |

---

## ECR Repositories

All images are stored in ECR in account `$AWS_ACCOUNT_ID` / region `us-west-1`.

| Repository | Purpose |
|---|---|
| `jenkins-kube-ninjas/marklogic-server-ubi` | MarkLogic server image (root) |
| `jenkins-kube-ninjas/marklogic-server-ubi-rootless` | MarkLogic server image (rootless) |
| `jenkins-kube-ninjas/marklogic-kubernetes-operator` | Operator image built by CI |

Full URI prefix: `${AWS_ACCOUNT_ID}.dkr.ecr.us-west-1.amazonaws.com`

---

## Prerequisites

The following tools must be available on the Jenkins agent (`cld-kubernetes`):

- `aws` CLI v2
- `eksctl` >= 0.190
- `kubectl`
- `helm` v3
- `docker`
- `jq`
- `go` (for building the operator)

The Jenkins credential **`KUBE_NINJAS_OPS_AWS_JENKINS`** must have these IAM permissions:

- `eks:*` (cluster describe/update, nodegroup scaling)
- `ecr:*` (login, push, pull)
- `iam:PassRole`, `iam:CreateServiceLinkedRole`
- `elasticloadbalancing:*`
- EC2 permissions for VPC/subnet tagging

---

## One-Time Cluster Bootstrap

Run `setup-eks.sh` once when creating the cluster from scratch. The script is
idempotent — it is safe to re-run at any time.

```bash
cd test/eks-config
AWS_DEFAULT_REGION=us-west-1 \
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text) \
CLUSTER_NAME=jenkins-kube-ninjas \
  bash setup-eks.sh
```

The script performs the following steps:

1. Validates required CLI tools
2. Creates the EKS cluster via `eksctl` using `cluster-config.yaml`
3. Creates the three ECR repositories
4. Creates the IAM policy for the AWS Load Balancer Controller (pinned to v2.8.3)
5. Creates an IAM service account for the LB controller with OIDC
6. Installs the AWS Load Balancer Controller via Helm

---

## Cost Management — Scaling Worker Nodes

The cluster control plane runs 24/7 (fixed cost). Worker nodes are scaled down
to **0** when not in use to avoid EC2 charges.

### Scale down (end of testing)

```bash
make eks-scale-down
```

or directly:

```bash
aws eks update-nodegroup-config \
  --cluster-name jenkins-kube-ninjas \
  --nodegroup-name ml-worker \
  --scaling-config minSize=0,maxSize=6,desiredSize=0 \
  --region us-west-1
```

### Scale up (before testing)

```bash
make eks-scale-up
```

or directly:

```bash
aws eks update-nodegroup-config \
  --cluster-name jenkins-kube-ninjas \
  --nodegroup-name ml-worker \
  --scaling-config minSize=0,maxSize=6,desiredSize=3 \
  --region us-west-1
```

The Jenkins pipeline (`EKS-Setup` stage) runs `make eks-scale-up` automatically
and waits for nodes to reach `Ready` state before proceeding with tests.

---

## ECR Image Management

### Authenticate Docker to ECR

```bash
make ecr-login
```

or directly:

```bash
aws ecr get-login-password --region us-west-1 \
  | docker login --username AWS --password-stdin \
    "$(aws sts get-caller-identity --query Account --output text).dkr.ecr.us-west-1.amazonaws.com"
```

### Build and push the operator image

```bash
make docker-build docker-push \
  IMG=$(aws sts get-caller-identity --query Account --output text).dkr.ecr.us-west-1.amazonaws.com/jenkins-kube-ninjas/marklogic-kubernetes-operator:latest
```

### Build and push the MarkLogic server image

The MarkLogic server image is built and pushed by the **Docker_CI** pipeline in
`KubeNinjas/docker/Docker_CI`. Trigger that pipeline with `PUSH_TO_ECR=true` to
push both `{version}-ubi-rootless-{dockerVersion}` and `latest-{mlMajorVersion}`
tags to ECR.

---

## Running EKS Tests via Jenkins

### Trigger a one-off EKS test run

1. Open the `KubeNinjas/marklogic-operator-kubernetes/Operator_CI` pipeline in
   Jenkins.
2. Click **Build with Parameters**.
3. Check **`TEST_ON_EKS`** — all Minikube stages are skipped; EKS stages run instead.
4. Optionally check **`VERIFY_ISTIO_AMBIENT`** to also run Istio ambient-mode tests.

### Nightly scheduled run

The pipeline runs automatically at **05:30 UTC** daily with:

```
TEST_ON_EKS=true
VERIFY_ISTIO_AMBIENT=true
E2E_MARKLOGIC_IMAGE_VERSION=${AWS_ACCOUNT_ID}.dkr.ecr.us-west-1.amazonaws.com/jenkins-kube-ninjas/marklogic-server-ubi-rootless:latest-12
```

### Concurrent access control

All EKS stages in the pipeline are wrapped with:

```groovy
lock(resource: 'jenkinsKubeNinjasEksCluster', inversePrecedence: true) {
  timeout(time: 3, unit: 'HOURS') {
    // EKS stages
  }
}
```

This ensures only one build holds the shared cluster at a time. A queued build
is aborted after 3 hours rather than waiting indefinitely.

---

## Pipeline Stage Overview

| Stage | Description |
|---|---|
| **EKS-Setup** | Scales up nodes, ECR-logins, builds and pushes operator image, deploys operator |
| **Run-EKS-e2e-Tests** | Runs `make e2e-test-eks` — full suite against EKS |
| **EKS-Cleanup** | Tears down operator and test resources; scales nodes back to 0 (`catchError`) |
| **EKS-Istio-Setup** | Installs Istio ambient mode on the existing cluster |
| **Run-EKS-Istio-e2e-Tests** | Runs `make e2e-test-eks-istio` |
| **EKS-Istio-Cleanup** | Removes Istio and remaining resources; scales down (`catchError`) |

---

## Replicating to a New AWS Account

If the cluster needs to be recreated in a different account:

1. Update `EKS_REGION`, `AWS_ACCOUNT_ID`, and `CLUSTER_NAME` in `Makefile` or
   via environment variables.
2. Update the `KUBE_NINJAS_OPS_AWS_JENKINS` Jenkins credential.
3. Re-run `setup-eks.sh`.
4. Update the nightly cron's `E2E_MARKLOGIC_IMAGE_VERSION` to the new ECR URI.

---

## Teardown (Cluster Deletion)

```bash
# Full cluster deletion — use with caution!
eksctl delete cluster \
  --name jenkins-kube-ninjas \
  --region us-west-1
```

This also deletes all associated VPC resources, IAM roles, and the OIDC provider.
