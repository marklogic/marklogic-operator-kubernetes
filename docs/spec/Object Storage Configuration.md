# Functional Spec: MarkLogic Operator Object Storage Configuration

## Introduction

### Overview

This specification defines the requirements, API contract, and controller workflow for configuring MarkLogic object storage access (AWS S3 and Azure Blob) declaratively through the MarkLogic Operator for Kubernetes.

MarkLogic stores the credentials used to reach external object storage as a **cluster-wide security setting**, not as a per-host or per-forest property. The operator therefore models object storage configuration as a single `spec.objectStorage` block on `MarklogicCluster` and reconciles it **once against the bootstrap host's Management API**, in the same style as the existing idempotent `Ensure*` security operations (for example `EnsureOAuthExternalSecurity`). Credentials are supplied through Kubernetes Secrets or, where the platform supports it, through cloud workload identity, so no secret value is ever written into the custom resource, controller logs, or status fields.

The feature makes object storage access part of cluster provisioning so that clusters are usable for supported object storage scenarios — most importantly scheduled backups to S3/Azure — immediately after they become `Ready`, without manual post-deployment configuration.

### Goals

1.  Allow users to declare AWS S3 and/or Azure Blob credentials as part of the `MarklogicCluster` spec.
2.  Apply that configuration automatically during cluster reconciliation, after the cluster is initialized and secured.
3.  Source all secret material from Kubernetes Secrets (or cloud workload identity), never from inline spec fields.
4.  Never expose secret values in logs, events, or status.
5.  Provide a clear, queryable status model that reports whether each provider's configuration succeeded or failed.
6.  Support credential rotation through GitOps by detecting Secret changes and re-applying.
7.  Preserve the existing `MarklogicCluster -> MarklogicGroup -> workload` ownership model and the current bootstrap/security initialization flow.

### Compatibility

Object storage credential configuration uses the MarkLogic Management API `PUT /manage/v2/credentials/properties`, which is available on all currently supported MarkLogic images used by the operator. No minimum-version bump beyond the operator's existing baseline is introduced for the static-credential path.

Keyless (workload-identity) authentication has platform-specific maturity:

1.  **AWS S3 via IRSA / instance profile** is viable today. When static keys are absent, MarkLogic falls back to the AWS credential provider chain (instance metadata / projected service-account token on EKS), so the operator's job for this mode is primarily to wire the pod service account and leave the stored keys empty.
2.  **Azure Blob via managed identity** is treated as forward-looking. Whether it can be expressed through the credentials API depends on the MarkLogic image version, so v1 supports Azure through storage-account + storage-key and flags managed identity as a follow-up pending MarkLogic support confirmation.

## Requirement Review

Before the design, the following observations refine the original business requirement. They are recorded here so the accepted scope is explicit.

### Confirmed and Aligned

1.  Modeling credentials in the cluster spec and applying them during reconciliation matches how MarkLogic actually stores object storage credentials — cluster-wide, via the Management API. The requirement's intent ("configure at cluster creation time") is directly achievable.
2.  Kubernetes-native secret handling (Secrets and, where supported, workload identity) is the correct mechanism and matches the operator's existing `auth.secretName` conventions.
3.  Declarative, GitOps-compatible configuration is naturally satisfied because the spec references Secrets by name rather than embedding values.

### Issues and Clarifications

1.  **Credentials are cluster-wide, not per-group.** The Management API endpoint `PUT /manage/v2/credentials/properties` sets one AWS credential set and one Azure credential set for the whole cluster. The feature must therefore live on `MarklogicCluster` and be reconciled against the bootstrap host, not fanned out per `MarklogicGroup`. Modeling it per group would misrepresent MarkLogic behavior.

2.  **"Supported forest storage scenarios" must be scoped down.** MarkLogic's stable, well-defined surface for object storage is *credential configuration*. The operator directly configuring object-storage-backed forests (fast/large data directories, journaling, replica forests) involves version- and scenario-specific constraints and is error-prone to automate generically. v1 therefore scopes the operator to **credential configuration** — which is the prerequisite that unblocks backups and any supported forest-on-object-storage usage the administrator then configures — and does **not** create object-storage forests on the user's behalf. This keeps the acceptance criterion "supports configuring storage access for backups and supported forest storage scenarios" satisfied at the *access* layer while avoiding unsupported automation.

3.  **Required privileges differ from the dynamic-host feature.** `PUT /manage/v2/credentials/properties` requires the `manage-admin` **and** `security` roles (or the `credentials-set-aws` / `credentials-set-azure` privileges). The `manage-admin`-only user introduced for dynamic hosts is therefore **not sufficient** on its own. v1 applies object storage credentials using the existing admin-capable bootstrap credential; a dedicated least-privilege credential (manage-admin + security, or the credentials-set privileges) is documented as an optional hardening path.

4.  **Drift detection cannot rely on reading the secret back.** The Management API masks secret material on read, so the controller must not compare stored secret values to decide whether re-application is needed. Instead the operator computes a fingerprint (salted SHA-256) of the resolved secret material it applied and stores only that fingerprint in status. A change in the referenced Secret changes the fingerprint and triggers re-application. This also delivers rotation support.

5.  **Region is not part of the credentials API.** The AWS credentials structure carries only `access-key`, `secret-key`, and `session-token`. AWS region for S3 is resolved by MarkLogic through its own configuration/environment, not through this endpoint. The spec exposes an optional informational `region` field but documents that region wiring for S3 forests/backups is an environment concern, not something this endpoint sets.

6.  **AWS temporary credentials (`session-token`) expire.** If users supply STS session tokens, they are short-lived. v1 accepts an optional `sessionToken` for completeness but recommends IRSA/instance-role (keyless) for temporary-credential scenarios so the operator is not responsible for continuously rotating expiring tokens.

7.  **CSI abstractions are out of scope for v1.** Mounting object storage as a filesystem via a CSI driver is a fundamentally different integration than MarkLogic-native S3/Azure access and is not required to satisfy the acceptance criteria. It is recorded as a future research item, consistent with the requirement's own note.

8.  **Ordering matters.** Credentials must be applied only after the bootstrap host is initialized and security is established, and before object-storage-dependent workloads (such as scheduled backups) run. The reconcile is gated on cluster readiness.

### Accepted Scope Summary

v1 delivers declarative, GitOps-friendly, Kubernetes-Secret-backed **credential configuration** for AWS S3 and Azure Blob, applied cluster-wide against the bootstrap host, with rotation support, secret-safe status/observability, and AWS keyless (IRSA/instance-role) support. It does **not** create object-storage forests, does not implement CSI mounting, and defers Azure managed identity.

## Background: MarkLogic Object Storage Credentials

### Credential Model

MarkLogic reaches external object storage using credentials stored as a cluster security setting. These are set through the Management API:

```http
PUT /manage/v2/credentials/properties?type=aws
Content-Type: application/json

{
  "access-key": "AWS-ACCESS-KEY",
  "secret-key": "AWS-SECRET-KEY",
  "session-token": "OPTIONAL-STS-TOKEN"
}
```

```http
PUT /manage/v2/credentials/properties?type=azure
Content-Type: application/json

{
  "storage-account": "AZURE-STORAGE-ACCOUNT",
  "storage-key": "AZURE-STORAGE-KEY"
}
```

Returns `201 Created` on success, `400` on a malformed payload, and `401` when the caller lacks privileges.

The `type` query parameter selects the provider (`aws` is the default when omitted). The two provider credential sets are independent: configuring AWS does not affect Azure and vice versa.

### Required Privileges

`PUT /manage/v2/credentials/properties` requires one of:

1.  the `manage-admin` **and** `security` roles, or
2.  the `manage` and `manage-admin` privileges together with `credentials-set-aws` and/or `credentials-set-azure`.

Because this exceeds the `manage-admin`-only privilege used by the dynamic-host feature, object storage credential configuration runs under the admin-capable bootstrap credential in v1.

### Keyless (Workload Identity) Access

When the AWS credential set is empty, MarkLogic uses the standard AWS credential provider chain. On EKS with IRSA, or on EC2 with an instance profile, this lets MarkLogic access S3 without any static keys. In this mode the operator does not write access/secret keys; it ensures the MarkLogic pods run under a service account annotated for the target IAM role (a deployment prerequisite the user provides) and records that the cluster is configured for instance-role auth.

Azure keyless access via managed identity is version-dependent and is deferred; v1 uses storage-account + storage-key for Azure.

## Requirements

### Functional Requirements

#### Object Storage Declaration

Users must be able to declare AWS and/or Azure object storage access through the cluster spec.

Acceptance criteria:

1.  The operator supports `spec.objectStorage.aws` and `spec.objectStorage.azure`, each independently optional.
2.  Each provider block references a Kubernetes Secret by name for its credential material (except in keyless mode).
3.  No secret value is ever accepted as an inline spec field.
4.  Declaring only one provider configures only that provider and leaves the other untouched.

#### Credential Application

The operator must apply the declared configuration automatically during reconciliation.

Acceptance criteria:

1.  Once the cluster is initialized and secured, the operator applies each declared provider's credentials to the bootstrap host via the Management API.
2.  Application is idempotent: an unchanged configuration does not trigger repeated writes.
3.  The operator resolves credential material from the referenced Secret at apply time.
4.  For AWS `instanceRole` mode, the operator does not write static keys and records that keyless auth is in effect.

#### Credential Rotation

The operator must support rotating credentials through GitOps.

Acceptance criteria:

1.  When the referenced Secret's credential material changes, the operator detects the change and re-applies the affected provider.
2.  Detection uses a fingerprint of the applied material, never a read-back of the masked secret from MarkLogic.
3.  Rotation does not require editing the `MarklogicCluster` spec when only the Secret contents change.

#### Status and Observability

The operator must clearly report configuration outcome.

Acceptance criteria:

1.  `MarklogicCluster.status.objectStorage` exposes a per-provider phase (for example `Pending`, `Applied`, `Failed`).
2.  Status includes a machine-readable reason and human-readable message on failure.
3.  Status records the fingerprint and last-applied time of the successfully applied material, and never the material itself.
4.  Kubernetes events are emitted for apply success, apply failure, and rotation.
5.  Controller logs record each transition with structured fields and never log secret values.

### Non-Functional Requirements

#### Security

1.  Secret values must never appear in the spec, status, events, or logs.
2.  Only fingerprints (salted SHA-256) and Secret references are persisted in status.
3.  The operator reads referenced Secrets with least-privilege RBAC scoped to the operator's namespace access model.
4.  The admin-capable credential used for credential application is the existing bootstrap credential; an optional dedicated `manage-admin` + `security` credential is documented as a hardening path.

#### Reliability

1.  A failure to configure one provider must not block configuration of the other.
2.  A transient Management API failure results in a retriable state, not a terminal one.
3.  Applying object storage credentials must not disrupt the running cluster or its data.

#### Platform Compatibility

1.  The feature works on any Kubernetes distribution supported by the operator.
2.  AWS keyless mode targets EKS (IRSA) and EC2 instance profiles.
3.  Azure uses storage-account + storage-key across supported distributions; managed identity is deferred.

### Scope and Non-Goals

#### In Scope

1.  Declarative AWS S3 and Azure Blob credential configuration via `spec.objectStorage`.
2.  Kubernetes-Secret-backed credential material and AWS keyless (IRSA/instance-role) mode.
3.  Idempotent, cluster-wide application against the bootstrap host.
4.  Rotation via Secret change detection using fingerprints.
5.  Per-provider status, events, and secret-safe logging.

#### Out of Scope

1.  Creating or managing object-storage-backed forests (fast/large data directories, journaling, replicas).
2.  CSI-based object storage mounting.
3.  Azure managed identity (deferred pending MarkLogic support confirmation).
4.  Continuous rotation of expiring AWS STS session tokens.
5.  Region/endpoint provisioning for S3 (an environment concern outside the credentials API).

## API and User Experience Contract

### Trigger Model

Users configure object storage by adding a `spec.objectStorage` block to `MarklogicCluster` and creating the referenced Secret(s). The operator applies the configuration during reconciliation after the cluster is ready. Rotation is triggered by editing the referenced Secret; removing a provider block leaves previously applied credentials in place (v1 does not clear credentials on removal — see Validation Rules).

Example (both providers, secret-backed):

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: my-cluster
spec:
  auth:
    secretName: ml-admin-auth
  image: progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6
  objectStorage:
    aws:
      authType: secret
      secretName: ml-s3-credentials
      region: us-east-1
    azure:
      authType: secret
      secretName: ml-azure-credentials
  markLogicGroups:
    - name: dnode
      replicas: 3
      isBootstrap: true
      groupConfig:
        name: Default
---
apiVersion: v1
kind: Secret
metadata:
  name: ml-s3-credentials
type: Opaque
stringData:
  accessKey: AKIA...
  secretKey: wJalr...
  # sessionToken: optional STS token
---
apiVersion: v1
kind: Secret
metadata:
  name: ml-azure-credentials
type: Opaque
stringData:
  storageAccount: mystorageacct
  storageKey: base64key==
```

Example (AWS keyless via IRSA):

```yaml
spec:
  serviceAccountName: marklogic-workload   # annotated for the target IAM role
  objectStorage:
    aws:
      authType: instanceRole
      region: us-east-1
```

### Spec Fields

#### Additions to `MarklogicCluster.spec`

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `objectStorage` | ObjectStorageConfig | No | Cluster-wide object storage access configuration | unset (no configuration applied) |

#### `objectStorage` (ObjectStorageConfig)

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `aws` | AWSObjectStorage | No | AWS S3 credential configuration | unset |
| `azure` | AzureObjectStorage | No | Azure Blob credential configuration | unset |

#### `objectStorage.aws` (AWSObjectStorage)

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `authType` | enum (`secret`, `instanceRole`) | No | How AWS credentials are provided | `secret` |
| `secretName` | string | Conditional | Name of the Secret holding `accessKey`, `secretKey`, and optional `sessionToken`. Required when `authType=secret` | unset |
| `region` | string | No | Informational AWS region; documented as an environment concern, not written to the credentials API | unset |

#### `objectStorage.azure` (AzureObjectStorage)

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `authType` | enum (`secret`) | No | How Azure credentials are provided (`managedIdentity` reserved, not implemented in v1) | `secret` |
| `secretName` | string | Conditional | Name of the Secret holding `storageAccount` and `storageKey`. Required when `authType=secret` | unset |

#### Referenced Secret Key Contract

| Provider | Secret keys | Notes |
|---|---|---|
| AWS | `accessKey`, `secretKey`, optional `sessionToken` | All keys omitted is invalid for `authType=secret` |
| Azure | `storageAccount`, `storageKey` | Both required for `authType=secret` |

### Validation Rules

1.  When `objectStorage.aws.authType=secret`, `objectStorage.aws.secretName` is required.
2.  When `objectStorage.azure.authType=secret`, `objectStorage.azure.secretName` is required.
3.  `objectStorage.aws.authType=instanceRole` must not set `secretName`.
4.  `objectStorage.azure.authType` accepts only `secret` in v1; `managedIdentity` is rejected with an explanatory message until implemented.
5.  Referenced Secrets must exist and contain the required keys at apply time; a missing Secret or key results in a `Failed` provider status with a machine-readable reason, not a controller crash.
6.  Removing a provider block from the spec does not clear previously applied MarkLogic credentials in v1; users who need to revoke credentials do so directly (documented limitation).

### Validation Strategy

1.  **CEL rules on the CRD** enforce the structural invariants that fit cleanly: `secretName` required when `authType=secret`, `secretName` forbidden when `authType=instanceRole`, and the Azure `authType` enum restriction.
2.  **Reconcile-time validation in the `MarklogicCluster` controller** covers checks that depend on live state: Secret existence and key presence, bootstrap reachability and readiness, and Management API apply results. Violations are surfaced through `status.objectStorage` and events; no validating admission webhook is part of v1.

## Status Contract

### Source of Truth

Because object storage credentials are a cluster-wide MarkLogic setting, their status lives on `MarklogicCluster.status.objectStorage`, not on any `MarklogicGroup`. This is the opposite ownership choice from the dynamic-host feature (whose per-group workload is owned by `MarklogicGroup`), and it is deliberate: there is exactly one cluster-wide credential set per provider.

### Status Object

`status.objectStorage` contains one entry per configured provider.

| Field | Type | Set By | Description | Notes |
|---|---|---|---|---|
| `aws.phase` | enum | Operator | `Pending`, `Applied`, `Failed`, `Disabled` | Primary progress indicator for AWS |
| `aws.reason` | enum | Operator | Machine-readable reason on failure | e.g. `SecretNotFound`, `SecretKeyMissing`, `ManagementAPIError`, `BootstrapNotReady` |
| `aws.message` | string | Operator | Human-readable summary | Never contains secret values |
| `aws.appliedFingerprint` | string | Operator | Salted SHA-256 of the applied material | Never the material itself; empty in `instanceRole` mode |
| `aws.authType` | string | Operator | `secret` or `instanceRole` | Mirrors the applied mode |
| `aws.lastAppliedTime` | timestamp | Operator | When the current material was applied | |
| `azure.phase` | enum | Operator | `Pending`, `Applied`, `Failed`, `Disabled` | Primary progress indicator for Azure |
| `azure.reason` | enum | Operator | Machine-readable reason on failure | Same reason vocabulary as AWS |
| `azure.message` | string | Operator | Human-readable summary | Never contains secret values |
| `azure.appliedFingerprint` | string | Operator | Salted SHA-256 of the applied material | Never the material itself |
| `azure.lastAppliedTime` | timestamp | Operator | When the current material was applied | |

The cluster resource may also mirror an aggregate condition (for example `ObjectStorageReady`) into `status.conditions`, but the per-provider block above is authoritative.

## Controller Workflow

1.  **Gate on readiness.** The `MarklogicCluster` controller only attempts object storage configuration after the cluster is initialized and the bootstrap host is reachable and secured.
2.  **Resolve material.** For each declared provider in `secret` mode, read the referenced Secret and extract the required keys. Missing Secret/keys → set provider `phase=Failed` with a specific `reason` and emit an event; continue with the other provider.
3.  **Compute fingerprint.** Compute a salted SHA-256 over the resolved material. If it equals `status.<provider>.appliedFingerprint`, the provider is already up to date; skip the write.
4.  **Apply.** Call the corresponding Management API operation (`EnsureAWSCredentials` / `EnsureAzureCredentials`) against the bootstrap host using the admin-capable credential. On `201`, set `phase=Applied`, update `appliedFingerprint` and `lastAppliedTime`, and emit a success event.
5.  **Keyless mode.** For AWS `instanceRole`, skip the credential write, set `phase=Applied` with `authType=instanceRole` and an empty fingerprint, and record that keyless auth is in effect.
6.  **Failure handling.** Management API errors set `phase=Failed` with `reason=ManagementAPIError` and are retriable on the next reconcile; the message never includes request/response bodies that could carry secret material.
7.  **Independence.** AWS and Azure are reconciled independently so one provider's failure never blocks the other.

## Task Breakdown

Tasks are grouped by area and ordered to allow incremental, testable delivery.

### 1. API and CRD

- [ ] Add `ObjectStorageConfig`, `AWSObjectStorage`, and `AzureObjectStorage` types to `api/v1/common_types.go` (or a new `objectstorage_types.go`), with kubebuilder validation markers and enums.
- [ ] Add `ObjectStorage *ObjectStorageConfig` to `MarklogicClusterSpec` in `api/v1/marklogiccluster_types.go`.
- [ ] Add `ObjectStorageStatus` (per-provider) and wire it into `MarklogicClusterStatus`.
- [ ] Add CEL `XValidation` rules: `secretName` required when `authType=secret`, forbidden when `authType=instanceRole`; Azure `authType` restricted to `secret` in v1.
- [ ] Run `make generate manifests` to regenerate deepcopy and CRD YAML; verify `config/crd/bases` and `zz_generated.deepcopy.go`.

### 2. Management Client (`pkg/mlmanage`)

- [ ] Add `EnsureAWSCredentials(ctx, AWSCredentials) error` and `EnsureAzureCredentials(ctx, AzureCredentials) error` to the `Client` interface.
- [ ] Add `AWSCredentials` (`AccessKey`, `SecretKey`, `SessionToken`) and `AzureCredentials` (`StorageAccount`, `StorageKey`) config structs.
- [ ] Implement `PUT /manage/v2/credentials/properties?type=aws|azure` following the existing `doJSON` idempotent `Ensure*` pattern, expecting `201 Created`.
- [ ] Add `BuildAWSCredentialsPayload` / `BuildAzureCredentialsPayload` with validation (reject empty required fields) mirroring `BuildOAuthExternalSecurityPayload`.
- [ ] Ensure error strings never include request/response bodies that may contain secrets.

### 3. Secret Handling and Fingerprinting

- [ ] Add a helper to resolve provider material from a referenced Secret (`accessKey`/`secretKey`/`sessionToken`, `storageAccount`/`storageKey`) with clear missing-key errors.
- [ ] Add a salted SHA-256 fingerprint helper over resolved material; keep the salt internal and never persist raw material.
- [ ] Add unit tests confirming fingerprints change when any material field changes and that raw values are never returned in strings/logs.

### 4. Controller Reconciliation (`internal/controller` / `pkg/k8sutil`)

- [ ] Add an object storage reconcile step to the `MarklogicCluster` controller, gated on cluster readiness and bootstrap reachability.
- [ ] Implement per-provider flow: resolve → fingerprint → skip-if-unchanged → apply → update status → emit event.
- [ ] Implement AWS `instanceRole` keyless path (no write; record mode).
- [ ] Reconcile AWS and Azure independently so a failure in one does not block the other.
- [ ] Emit Kubernetes events for apply-success, apply-failure, and rotation.
- [ ] Add structured, secret-safe logging for each transition.

### 5. Status and Events

- [ ] Populate `status.objectStorage.<provider>` (phase, reason, message, appliedFingerprint, authType, lastAppliedTime).
- [ ] Optionally mirror an aggregate `ObjectStorageReady` condition into `status.conditions`.
- [ ] Verify no secret material reaches status, events, or logs (assert in tests).

### 6. Helm Chart

- [ ] Add `objectStorage` values to `charts/marklogic-operator-kubernetes/values.yaml` with documented defaults (disabled).
- [ ] Thread the values through the relevant templates so `spec.objectStorage` is rendered when configured.
- [ ] Update chart README/values documentation.

### 7. Samples and Docs

- [ ] Add a sample under `config/samples` demonstrating secret-backed AWS + Azure and an IRSA keyless variant.
- [ ] Keep this spec (`docs/spec/Object Storage Configuration.md`) as the reference and cross-link it from `docs` where object storage is mentioned.

### 8. Tests

- [ ] Unit tests: payload builders, secret resolution, fingerprint behavior, and validation.
- [ ] Client tests: `EnsureAWSCredentials` / `EnsureAzureCredentials` against a stub server asserting method, path, `type` query, and body shape (mirroring `TestEnsureOAuthExternalSecurityCreatesConfiguration`).
- [ ] Controller tests: readiness gating, skip-if-unchanged, rotation on secret change, independent per-provider failure, and secret-safe status/logs.
- [ ] Integration test (under `test/integration`) applying credentials against a running MarkLogic and verifying via the Management API that the provider is configured (without asserting secret values).
- [ ] CRD validation tests for the CEL rules.

### 9. Follow-Ups (not in v1)

- [ ] Azure managed identity support pending MarkLogic version confirmation.
- [ ] Optional dedicated least-privilege (`manage-admin` + `security`) credential for credential application.
- [ ] Research CSI-based object storage mounting as an alternative integration.
- [ ] Optional credential revocation when a provider block is removed from the spec.
