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

Keyless (workload-identity) authentication is **not available**. v1 is Kubernetes-Secret-backed static credentials for both providers, and this is a product constraint rather than a scoping choice:

1.  **AWS S3 via IRSA is not supported by MarkLogic.** The earlier assumption — that empty credentials cause MarkLogic to fall back to the standard AWS credential provider chain — is incorrect. MarkLogic documents its own three-step order of precedence (Security database → environment variables → IAM Role), which contains no web-identity-token step, and IRSA works *only* through a projected service-account token exchanged via STS `AssumeRoleWithWebIdentity`. The IAM Role step is additionally gated on `MARKLOGIC_AWS_ROLE`, which MarkLogic ignores when EC2 configuration is disabled via `MARKLOGIC_EC2_HOST=0` — precisely what the rootless image this operator pins sets at build time. Verified against MarkLogic 12 documentation and the `marklogic/marklogic-docker` image contents; see `docs/spec/[STEPS] Object Storage.md`, Session 6.
2.  **Azure Blob via managed identity** is version-dependent and undocumented for this endpoint, so v1 supports Azure through storage-account + storage-key and flags managed identity as a follow-up pending MarkLogic support confirmation.

Neither exclusion blocks the epic's acceptance criteria, which require Kubernetes-native credential handling — satisfied by Secrets — rather than keyless identity specifically.

## Requirement Review

Before the design, the following observations refine the original business requirement. They are recorded here so the accepted scope is explicit.

### Confirmed and Aligned

1.  Modeling credentials in the cluster spec and applying them during reconciliation matches how MarkLogic actually stores object storage credentials — cluster-wide, via the Management API. The requirement's intent ("configure at cluster creation time") is directly achievable.
2.  Kubernetes-native secret handling (Secrets and, where supported, workload identity) is the correct mechanism and matches the operator's existing `auth.secretName` conventions.
3.  Declarative, GitOps-compatible configuration is naturally satisfied because the spec references Secrets by name rather than embedding values.

### Issues and Clarifications

1.  **Credentials are cluster-wide, not per-group.** The Management API endpoint `PUT /manage/v2/credentials/properties` sets one AWS credential set and one Azure credential set for the whole cluster. The feature must therefore live on `MarklogicCluster` and be reconciled against a single host, not fanned out per `MarklogicGroup`. Modeling it per group would misrepresent MarkLogic behavior. Confirmed on a two-host cluster (STEPS Session 7): a write on one host is immediately readable from the other, and a `DELETE` clears it cluster-wide.

    v1 reconciles against the **bootstrap host** specifically. Note that this is a convention, not a constraint — Session 7 showed a non-bootstrap host accepts the write and propagates it just as well. Targeting the bootstrap keeps this operation consistent with the other `Ensure*` security operations and keeps behavior predictable; falling back to another host when the bootstrap is down is recorded as a possible hardening, not v1 behavior.

2.  **"Supported forest storage scenarios" must be scoped down.** MarkLogic's stable, well-defined surface for object storage is *credential configuration*. The operator directly configuring object-storage-backed forests (fast/large data directories, journaling, replica forests) involves version- and scenario-specific constraints and is error-prone to automate generically. v1 therefore scopes the operator to **credential configuration** — which is the prerequisite that unblocks backups and any supported forest-on-object-storage usage the administrator then configures — and does **not** create object-storage forests on the user's behalf. This keeps the acceptance criterion "supports configuring storage access for backups and supported forest storage scenarios" satisfied at the *access* layer while avoiding unsupported automation.

3.  **Required privileges differ from the dynamic-host feature.** `PUT /manage/v2/credentials/properties` requires the `manage-admin` **and** `security` roles (or the `credentials-set-aws` / `credentials-set-azure` privileges). The `manage-admin`-only user introduced for dynamic hosts is therefore **not sufficient** on its own — confirmed against a live instance, which returns `403` for that user. Both sufficient combinations were also confirmed working, and the `credentials-set-*` privileges are **scoped per provider**, so a deployment can grant only the providers it actually configures.

    **Decision (v1):** apply object storage credentials using the existing admin-capable bootstrap credential. A dedicated least-privilege credential is *practical* — the granular `manage` + `manage-admin` + `credentials-set-{aws,azure}` combination works without the broad `security` role — but provisioning and lifecycle-managing an extra MarkLogic user is additional surface that does not change what the operator can do, since the operator already holds the bootstrap admin credential for other reconcile steps. It is therefore documented as an optional hardening path rather than v1 behavior. See `docs/spec/[STEPS] Object Storage.md`, Session 3.

4.  **Drift detection cannot rely on reading the secret back.** The Management API does not return secret material in a comparable form. `GET` returns `secret-key` as an opaque blob encrypted with MarkLogic's internal credentials key, and **every write re-encrypts** — re-applying the identical `secret-key` produces a different blob. (The blob is stable across repeated *reads* of the same stored value; the non-determinism is in the write path.) Comparing the stored value against the intended value is therefore impossible, not merely inadvisable. Instead the operator computes a fingerprint (salted SHA-256) of the resolved secret material it applied and stores only that fingerprint in status. A change in the referenced Secret changes the fingerprint and triggers re-application. This also delivers rotation support. Note that `access-key` and `session-token` are returned in **plaintext** by `GET`; only `secret-key` is encrypted.

5.  **Region is not part of the credentials API.** The AWS credentials structure carries only `access-key`, `secret-key`, and `session-token`. AWS region for S3 is resolved by MarkLogic through its own configuration/environment, not through this endpoint. The spec exposes an optional informational `region` field but documents that region wiring for S3 forests/backups is an environment concern, not something this endpoint sets.

6.  **AWS temporary credentials (`session-token`) are excluded from v1.** MarkLogic's credentials structure accepts an optional `session-token` for STS credentials, but the operator does not expose it. Three findings compound: the token is short-lived, the operator only re-applies when the referenced Secret changes (so an expired token stays expired until something external rewrites the Secret), and MarkLogic returns it in **plaintext** on read. The keyless mode that would normally cover this scenario does not exist (see Compatibility). Supporting the field would therefore mean shipping an option that works for a few hours and leaks in the meantime. **v1 accepts long-lived IAM user keys only.** Deployments that require STS credentials should drive Secret updates through an external mechanism and can revisit this once a rotation story exists.

7.  **CSI abstractions are out of scope for v1.** Mounting object storage as a filesystem via a CSI driver is a fundamentally different integration than MarkLogic-native S3/Azure access and is not required to satisfy the acceptance criteria. It is recorded as a future research item, consistent with the requirement's own note.

8.  **Ordering matters.** Credentials must be applied only after the bootstrap host is initialized and security is established, and before object-storage-dependent workloads (such as scheduled backups) run. The reconcile is gated on cluster readiness.

9.  **MarkLogic-side re-application is already safe.** Re-applying identical credential material returns `204` with no error, and applying a provider's credentials does not disturb the other provider's set. The operator's skip-if-unchanged fingerprint check is therefore a **cost optimization** (avoiding a needless write and a needless rotation event on every reconcile), not a correctness requirement. A fingerprint bug that causes a redundant re-apply degrades noise, not data.

10. **"Never configured" is externally detectable.** Before any value has been set for a provider, `GET /manage/v2/credentials/properties?type=<provider>` returns `{"<provider>": null}`. This gives the controller a way to distinguish "MarkLogic has never had credentials for this provider" from "credentials exist but the operator does not know their fingerprint" — relevant after a status loss or an operator upgrade. It cannot, however, tell the operator *which* material is configured (see #4), so it supplements the fingerprint rather than replacing it.

### Validation Status

The assumptions below were validated by the "Object Storage Credentials Research and Prototype" story (`docs/spec/[JIRA]Ojbect Storage.md`). **Evidence, per-session detail, and the full findings matrix live in `docs/spec/[STEPS] Object Storage.md`**, which is the authoritative record for this table.

All evidence to date comes from a single MarkLogic image (`progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6`) running as a **single self-initialized host** in local Docker. Findings are therefore scoped to that version and topology until re-confirmed more broadly.

| Assumption | Status | Notes |
|---|---|---|
| Management API response codes, payload shape, and `GET` read-back behavior | **Confirmed — corrected** | STEPS Session 2. Success is `204`, not `201`; `type` must be in the request body, not the query string; `secret-key` is returned as non-deterministic ciphertext and `session-token` in plaintext. Corrections applied throughout this spec. |
| Minimum required privileges (`manage-admin` + `security` vs. individual credential privileges) | **Confirmed** | STEPS Session 3. Both combinations work; `manage-admin` alone returns `403` (not `401`); `credentials-set-*` is scoped per provider. v1 decision recorded in Requirement Review #3. |
| Secret-backed static credential flow for both providers | **Confirmed** | STEPS Sessions 2–3. Both providers apply successfully; redundant re-apply is accepted without error. |
| Fingerprinting (salted SHA-256) as a safe, sufficient idempotency/rotation mechanism | **Confirmed and implemented** | STEPS Session 2, Test 4 proves read-back comparison is impossible, so fingerprinting is the only viable option. Implemented and tested in `pkg/objectstorage/fingerprint.go` (HMAC-SHA256, salt from `metadata.uid`, length-prefixed canonical encoding); salt stability, field ordering, and optional-field behavior are all covered by unit tests. |
| Secret changes reliably trigger reconciliation | **Confirmed and implemented** | A `Watches` on `v1.Secret` is wired into the `MarklogicCluster` controller with no RBAC or cache changes required. Required an explicit `case *corev1.Secret:` in the controller's `WithEventFilter` predicate, whose `UpdateFunc` otherwise drops updates by default and would have silently swallowed every rotation event. See `internal/controller/marklogiccluster_secret_watch_test.go`. |
| Per-provider status and failure-reason model | **Confirmed — expanded** | STEPS Session 2–3 error shapes drove the Failure Reasons table in the Status Contract, notably splitting `401` from `403`. |
| Credentials-only vs. backup/forest configuration boundary | **Confirmed** | Resolved in "Issues and Clarifications" #2 above; v1 is credential configuration only, and the forest/backup/region/endpoint behaviors in Out of Scope are unvalidated as well as unbuilt. |
| Cluster-wide (not per-host) credential propagation | **Confirmed** | STEPS Session 7, on a real two-host cluster. Writes on the bootstrap are visible on the non-bootstrap host for both providers, `DELETE` propagates, and provider independence holds across hosts. Also observed: any host accepts the write, so targeting the bootstrap is a deliberate convention rather than a MarkLogic requirement. |
| AWS IRSA / instance-role keyless access | **`No-Go` — verified impossible** | STEPS Session 6. MarkLogic does not use the AWS SDK credential provider chain, has no web-identity step, and its IAM-role path is disabled by the `MARKLOGIC_EC2_HOST=0` set in the operator's own image. Ruled out on evidence, not deferred for lack of it. |
| Azure managed identity | **Not attempted — deferred** | Requires Azure infrastructure. Out of scope for v1 regardless; see Out of Scope. |
| CSI volume abstractions | **Confirmed deferred** | Recorded as a future research item, not a v1 blocker. |
| Credential revocation via `DELETE` | **Confirmed working — deliberately deferred** | STEPS Session 4. Technically trivial; deferred on API-design grounds, see Validation Rules #6. |

### Accepted Scope Summary

v1 delivers declarative, GitOps-friendly, Kubernetes-Secret-backed **credential configuration** for AWS S3 and Azure Blob, applied cluster-wide against the bootstrap host, with rotation support and secret-safe status/observability. It is **Secrets-only for both providers**: AWS keyless (IRSA) is not supported by MarkLogic as the earlier design assumed, and Azure managed identity remains deferred. It does **not** create object-storage forests and does not implement CSI mounting.

## Background: MarkLogic Object Storage Credentials

### Credential Model

MarkLogic reaches external object storage using credentials stored as a cluster security setting. These are set through the Management API:

```http
PUT /manage/v2/credentials/properties
Content-Type: application/json

{
  "type": "aws",
  "access-key": "AWS-ACCESS-KEY",
  "secret-key": "AWS-SECRET-KEY"
}
```

MarkLogic also accepts an optional `session-token` field for STS credentials. The operator supports it as an **opt-in pass-through**: if the referenced Secret's `sessionToken` key is absent, the operator omits the field exactly as before; if present, the operator includes it in the applied payload and in the fingerprint, so rotation (Requirement Review #4) still detects when an external process rewrites the Secret with a fresh token before expiry. See Security NFR #5 for the accepted plaintext-exposure tradeoff.

```http
PUT /manage/v2/credentials/properties
Content-Type: application/json

{
  "type": "azure",
  "storage-account": "AZURE-STORAGE-ACCOUNT",
  "storage-key": "AZURE-STORAGE-KEY"
}
```

Returns `204 No Content` on success, `400` on a malformed payload, `401` when the caller is unauthenticated or supplies invalid credentials, and `403` when an authenticated caller lacks the required privileges.

**The provider is selected by the `type` field inside the JSON body, not by a `?type=` query-string parameter.** The query string is accepted but has no effect on how the server interprets the payload: an Azure-shaped body sent with `?type=azure` and no body `type` field fails with `400 MANAGE-INVALIDPAYLOAD`. AWS-shaped bodies happen to succeed without an explicit `type` only because `aws` is the server-side default. Payload builders must therefore always emit a `type` field. See `docs/spec/[STEPS] Object Storage.md`, Session 2.

The two provider credential sets are independent: configuring AWS does not affect Azure and vice versa.

### Required Privileges

`PUT /manage/v2/credentials/properties` requires one of:

1.  the `manage-admin` **and** `security` roles, or
2.  the `manage` and `manage-admin` privileges together with `credentials-set-aws` and/or `credentials-set-azure`.

Both combinations are confirmed working against a live instance, and the `credentials-set-*` privileges are scoped per provider — a caller holding only `credentials-set-aws` receives `403` when configuring Azure. An authenticated-but-under-privileged caller receives `403 Forbidden`, not `401`; `401` is reserved for missing or invalid credentials.

Because this exceeds the `manage-admin`-only privilege used by the dynamic-host feature, object storage credential configuration runs under the admin-capable bootstrap credential in v1.

### Credential Resolution Order (and why keyless is unavailable)

MarkLogic resolves AWS credentials in its own documented order of precedence — **not** the AWS SDK credential provider chain:

1.  Credentials configured in the MarkLogic Security database (what this feature sets).
2.  Environment variables.
3.  IAM Role.

The IAM Role step is EC2-oriented and conditional: it applies only when `MARKLOGIC_AWS_ROLE` is set, which MarkLogic populates from EC2 instance metadata, and which it ignores entirely when EC2 configuration is disabled with `MARKLOGIC_EC2_HOST=0`. The rootless image the operator pins sets `MARKLOGIC_EC2_HOST=0` in its Dockerfile, so the IAM Role step is inert in this deployment model.

This rules out **IRSA specifically**, independently of the above: IRSA authenticates by exchanging a projected service-account token through STS `AssumeRoleWithWebIdentity`, and MarkLogic's resolution order has no web-identity step. Neither the product documentation nor the container image references `AWS_ROLE_ARN` or `AWS_WEB_IDENTITY_TOKEN_FILE`.

A node-level variant — overriding `MARKLOGIC_EC2_HOST=1` so MarkLogic picks up the node's instance-profile role from IMDS — is theoretically reachable but is **not** pursued: it grants pod-level access to the node's role (so every pod on the node shares it), depends on IMDS access that EKS commonly blocks via a hop limit of 1, and re-enables container-inappropriate EC2 bootstrap behavior the image authors disabled deliberately. It is a security downgrade relative to a scoped Kubernetes Secret, not an upgrade.

Azure keyless access via managed identity is likewise deferred; v1 uses storage-account + storage-key for Azure.

## Requirements

### Functional Requirements

#### Object Storage Declaration

Users must be able to declare AWS and/or Azure object storage access through the cluster spec.

Acceptance criteria:

1.  The operator supports `spec.objectStorage.aws` and `spec.objectStorage.azure`, each independently optional.
2.  Each provider block references a Kubernetes Secret by name for its credential material.
3.  No secret value is ever accepted as an inline spec field.
4.  Declaring only one provider configures only that provider and leaves the other untouched.

#### Credential Application

The operator must apply the declared configuration automatically during reconciliation.

Acceptance criteria:

1.  Once the cluster is initialized and secured, the operator applies each declared provider's credentials to the bootstrap host via the Management API.
2.  Application is idempotent: an unchanged configuration does not trigger repeated writes. MarkLogic itself accepts a redundant re-apply without error, so this is an optimization rather than a safety property (Requirement Review #9).
3.  The operator resolves credential material from the referenced Secret at apply time.

#### Credential Rotation

The operator must support rotating credentials through GitOps.

Acceptance criteria:

1.  When the referenced Secret's credential material changes, the operator detects the change and re-applies the affected provider.
2.  Detection uses a fingerprint of the applied material, never a read-back from MarkLogic — which is impossible, since the stored `secret-key` is returned as non-deterministic ciphertext (Requirement Review #4).
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
4.  The admin-capable credential used for credential application is the existing bootstrap credential; an optional dedicated credential is documented as a hardening path. Two combinations are confirmed sufficient: the `manage-admin` + `security` roles, or the granular `manage` + `manage-admin` + `credentials-set-aws` and/or `credentials-set-azure` privileges. The granular form is the tighter option because it avoids the broad `security` role **and** is scoped per provider — a deployment that only uses S3 can grant `credentials-set-aws` alone, and a caller holding only that privilege is refused (`403`) when attempting to configure Azure.
5.  **MarkLogic returns `session-token` in plaintext** from `GET /manage/v2/credentials/properties`, unlike `secret-key` which it stores encrypted. Any caller with sufficient Management API privilege can read it back, and the operator cannot mask it. `sessionToken` is therefore an **opt-in** field: the operator never sends it unless the user's Secret supplies it, so a user who does not need STS credentials sees identical behavior (and identical risk) to today. A user who does supply it accepts the plaintext-exposure tradeoff explicitly, in return for STS support; keeping the token refreshed before expiry remains the user's responsibility via the existing Secret-rotation path.

#### Reliability

1.  A failure to configure one provider must not block configuration of the other.
2.  A transient Management API failure results in a retriable state, not a terminal one.
3.  Applying object storage credentials must not disrupt the running cluster or its data.

#### Platform Compatibility

1.  The feature works on any Kubernetes distribution supported by the operator.
2.  Secrets-backed credentials work identically on every supported distribution; v1 has no distribution-specific credential path. There is no EKS-specific keyless mode, because MarkLogic does not support IRSA (see Compatibility).
3.  Azure uses storage-account + storage-key across supported distributions; managed identity is deferred.

### Scope and Non-Goals

#### In Scope

1.  Declarative AWS S3 and Azure Blob credential configuration via `spec.objectStorage`.
2.  Kubernetes-Secret-backed credential material for both providers — the whole of v1's credential surface.
3.  Idempotent, cluster-wide application against the bootstrap host.
4.  Rotation via Secret change detection using fingerprints.
5.  Per-provider status, events, and secret-safe logging.

#### Out of Scope

1.  Creating or managing object-storage-backed forests (fast/large data directories, journaling, replicas). Forest, backup, region, and endpoint behaviors were **not validated** by the research story and are excluded on that basis as well as by design.
2.  CSI-based object storage mounting.
3.  AWS keyless access via IRSA / instance profile — not supported by MarkLogic as described; see Compatibility.
4.  Azure managed identity (deferred pending MarkLogic support confirmation).
5.  Automatic refresh of AWS STS session tokens — `sessionToken` is accepted as an opt-in pass-through field (see Background and Security NFR #5), but the operator does not obtain, renew, or track its expiry; keeping it current is the user's responsibility via the referenced Secret.
6.  Region/endpoint provisioning for S3 (an environment concern outside the credentials API).
7.  Revoking credentials when a provider block is removed (see Validation Rules #6).

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

Example (AWS keyless via IRSA) — **not supported**; MarkLogic has no IRSA support (see Compatibility). Shown only to document the reserved shape, should MarkLogic add it:

```yaml
spec:
  serviceAccountName: marklogic-workload   # annotated for the target IAM role
  objectStorage:
    aws:
      authType: instanceRole   # rejected by validation
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
| `authType` | enum (`secret`) | No | How AWS credentials are provided (`instanceRole` reserved, rejected in v1) | `secret` |
| `secretName` | string | Conditional | Name of the Secret holding `accessKey` and `secretKey`. Required when `authType=secret` | unset |
| `region` | string | No | Informational AWS region; documented as an environment concern, not written to the credentials API | unset |

#### `objectStorage.azure` (AzureObjectStorage)

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `authType` | enum (`secret`) | No | How Azure credentials are provided (`managedIdentity` reserved, not implemented in v1) | `secret` |
| `secretName` | string | Conditional | Name of the Secret holding `storageAccount` and `storageKey`. Required when `authType=secret` | unset |

#### Referenced Secret Key Contract

| Provider | Secret keys | Notes |
|---|---|---|
| AWS | `accessKey`, `secretKey`, `sessionToken` | `accessKey`/`secretKey` required for `authType=secret`. `sessionToken` is **optional**: omitted when absent, applied and fingerprinted when present (opt-in STS support, see Security NFR #5) |
| Azure | `storageAccount`, `storageKey` | Both required for `authType=secret` |

### Validation Rules

1.  When `objectStorage.aws.authType=secret`, `objectStorage.aws.secretName` is required.
2.  When `objectStorage.azure.authType=secret`, `objectStorage.azure.secretName` is required.
3.  `objectStorage.aws.authType` accepts only `secret` in v1; `instanceRole` is reserved and rejected with an explanatory message until keyless support is implemented. Reserving rather than omitting the value keeps the follow-up additive: enabling it later relaxes a validation rule instead of changing the enum's meaning.
4.  `objectStorage.azure.authType` accepts only `secret` in v1; `managedIdentity` is rejected with an explanatory message until implemented.
5.  Referenced Secrets must exist and contain the required keys at apply time; a missing Secret or key results in a `Failed` provider status with a machine-readable reason, not a controller crash.
6.  Removing a provider block from the spec does not clear previously applied MarkLogic credentials in v1; the provider moves to `phase=Disabled` and the credentials remain active in MarkLogic. Users who need to revoke credentials do so directly (documented limitation).

    **Decision (v1): revocation stays out of scope, despite being cheap to implement.** `DELETE /manage/v2/credentials/properties?type=<provider>` was confirmed working and returns the provider to the unconfigured `null` state, so the barrier is not technical. It is that spec removal is ambiguous: it may mean "revoke these credentials" or "stop managing this, leave it alone" — and the two readings differ destructively. A GitOps overlay that temporarily drops the block, or a user migrating credential management out of the operator, would silently break every running backup if removal implied revocation. An explicit opt-in signal is the right shape for this, which makes it an API-design question rather than a one-line API call; it is deferred to Follow-Ups on those grounds, not on effort.

### Validation Strategy

1.  **CEL rules on the CRD** enforce the structural invariants that fit cleanly: `secretName` required when `authType=secret`, and the `authType` enum restrictions for both providers (`secret` only in v1).
2.  **Reconcile-time validation in the `MarklogicCluster` controller** covers checks that depend on live state: Secret existence and key presence, bootstrap reachability and readiness, and Management API apply results. Violations are surfaced through `status.objectStorage` and events; no validating admission webhook is part of v1.

## Status Contract

### Source of Truth

Because object storage credentials are a cluster-wide MarkLogic setting, their status lives on `MarklogicCluster.status.objectStorage`, not on any `MarklogicGroup`. This is the opposite ownership choice from the dynamic-host feature (whose per-group workload is owned by `MarklogicGroup`), and it is deliberate: there is exactly one cluster-wide credential set per provider.

### Status Object

`status.objectStorage` contains one entry per configured provider.

| Field | Type | Set By | Description | Notes |
|---|---|---|---|---|
| `aws.phase` | enum | Operator | `Pending`, `Applied`, `Failed`, `Disabled` | Primary progress indicator for AWS |
| `aws.reason` | enum | Operator | Machine-readable reason on failure | From the vocabulary below; empty when `phase=Applied` |
| `aws.message` | string | Operator | Human-readable summary | Never contains secret values |
| `aws.appliedFingerprint` | string | Operator | Salted SHA-256 of the applied material | Never the material itself |
| `aws.authType` | string | Operator | `secret` | Mirrors the applied mode; reserved for future keyless modes |
| `aws.lastAppliedTime` | timestamp | Operator | When the current material was applied | |
| `azure.phase` | enum | Operator | `Pending`, `Applied`, `Failed`, `Disabled` | Primary progress indicator for Azure |
| `azure.reason` | enum | Operator | Machine-readable reason on failure | Same reason vocabulary as AWS |
| `azure.message` | string | Operator | Human-readable summary | Never contains secret values |
| `azure.appliedFingerprint` | string | Operator | Salted SHA-256 of the applied material | Never the material itself |
| `azure.lastAppliedTime` | timestamp | Operator | When the current material was applied | |

### Phase Semantics

| Phase | Meaning |
|---|---|
| `Pending` | The provider is declared but not yet applied — typically the cluster or bootstrap host is not ready yet. Retriable by definition. |
| `Applied` | The material identified by `appliedFingerprint` was accepted by MarkLogic (`204`). |
| `Failed` | The last attempt failed; see `reason` and `message`. Whether it is retriable depends on the reason (see below). |
| `Disabled` | The provider is not declared in the spec. Because v1 does not revoke on removal (Validation Rules #6), `Disabled` means "not managed by the operator", **not** "not configured in MarkLogic" — credentials applied by an earlier spec revision may still be active. |

### Failure Reasons

The Management API distinguishes failure modes that require different operator responses, so they must not be collapsed into a single `ManagementAPIError`. Errors are returned as `{"errorResponse":{"statusCode","status","messageCode","message"}}`.

| Reason | Trigger | Retriable | Operator action |
|---|---|---|---|
| `BootstrapNotReady` | Cluster/bootstrap host not yet initialized or reachable | Yes | Requeue; expected during startup |
| `SecretNotFound` | Referenced Secret does not exist | Yes | Requeue; resolves when the user creates the Secret |
| `SecretKeyMissing` | Secret exists but a required key is absent or empty | Yes | Requeue; names the missing key (not its value) |
| `InvalidPayload` | `400` — malformed request, e.g. a missing body `type` field | No | Operator bug; do not hot-loop. Log and surface loudly |
| `AuthenticationFailed` | `401` — the bootstrap credential is missing or invalid | Yes | Requeue; likely a stale admin Secret |
| `InsufficientPrivilege` | `403` — authenticated but lacking `credentials-set-*` / `security` | No | Requeue slowly; requires an administrator to grant privileges |
| `ManagementAPIUnreachable` | Network error, timeout, or `5xx` | Yes | Requeue with backoff |
| `ManagementAPIError` | Any other unexpected response | Yes | Catch-all; requeue with backoff |

Distinguishing `401` from `403` matters operationally: `401` points at the operator's admin Secret, while `403` points at MarkLogic role configuration. Collapsing them would make the most common privilege misconfiguration (Requirement Review #3) undiagnosable from status alone.

The cluster resource may also mirror an aggregate condition (for example `ObjectStorageReady`) into `status.conditions`, but the per-provider block above is authoritative.

## Fingerprinting

`appliedFingerprint` lets the operator answer "has the referenced Secret changed since I last applied it?" without storing credential material and without reading it back from MarkLogic (which is impossible — see Requirement Review #4).

### Why the hash is salted

`status` is readable by any principal with `get` on the `MarklogicCluster` and is stored in etcd. An unsalted `SHA-256` of credential material would act as a verification oracle: an observer could test candidate values offline and confirm a guess. That is a real risk rather than a theoretical one for the lower-entropy fields — `storageAccount` is a short human-chosen name and `accessKey` has a fixed `AKIA`-prefixed shape. Salting also prevents cross-cluster correlation: without it, two clusters sharing credentials would publish identical fingerprints, disclosing that fact to anyone able to read both.

### Salt source

The salt is derived from `MarklogicCluster.metadata.uid`.

| Property | Behavior |
|---|---|
| Stable across controller restarts | Yes — the UID lives on the CR, not in controller memory |
| Stable across controller replicas | Yes — no per-process or per-node state |
| Unique per cluster | Yes — defeats cross-cluster correlation |
| Additional resources required | None |
| Changes when | The `MarklogicCluster` is deleted and recreated |

The rejected alternatives: a **build-time constant** is shared across every installation, so it provides no correlation protection and invalidates every stored fingerprint on operator upgrade, causing a simultaneous re-apply across all managed clusters. A **generated Secret** works but introduces a resource to create, grant RBAC for, and back up, whose loss silently triggers the same mass re-apply.

The UID's one failure mode is benign: recreating the CR changes the salt, so the first reconcile re-applies credentials. That is correct behavior, since a recreated CR has no valid claim about what a previous cluster had applied.

### Computation

The fingerprint is computed over the resolved material in canonical field order, so logically identical credentials always produce the same value. Only the resulting digest is persisted; raw material never enters status, events, logs, or error strings.

## Controller Workflow

1.  **Gate on readiness.** The `MarklogicCluster` controller only attempts object storage configuration after the cluster is initialized and the bootstrap host is reachable and secured.
2.  **Resolve material.** For each declared provider in `secret` mode, read the referenced Secret and extract the required keys. Missing Secret/keys → set provider `phase=Failed` with a specific `reason` and emit an event; continue with the other provider.
3.  **Compute fingerprint.** Compute a salted SHA-256 over the resolved material. If it equals `status.<provider>.appliedFingerprint`, the provider is already up to date; skip the write.
4.  **Apply.** Call the corresponding Management API operation (`EnsureAWSCredentials` / `EnsureAzureCredentials`) against the bootstrap host using the admin-capable credential. On `204`, set `phase=Applied`, update `appliedFingerprint` and `lastAppliedTime`, and emit a success event.
5.  **Unsupported auth modes.** `authType` values reserved but not implemented in v1 (`instanceRole` for AWS, `managedIdentity` for Azure) are rejected by CRD validation, so the controller never sees them.
6.  **Failure handling.** Failures set `phase=Failed` with the specific `reason` from the Failure Reasons table and are requeued according to that table's retriability column. Error messages are constructed from the HTTP status code plus MarkLogic's `errorResponse.messageCode` and `errorResponse.message` fields only — **never** from the request body, and never by echoing the raw response body wholesale. Observed error responses do not reflect submitted credential values back to the caller, but the operator must not depend on that: treating the response body as untrusted keeps the guarantee intact if MarkLogic's error text changes.
7.  **Independence.** AWS and Azure are reconciled independently so one provider's failure never blocks the other.

## Task Breakdown

Tasks are grouped by area and ordered to allow incremental, testable delivery.

### 1. API and CRD

- [x] Add `ObjectStorageConfig`, `AWSObjectStorage`, and `AzureObjectStorage` types in `api/v1/objectstorage_types.go`, with kubebuilder validation markers and enums.
- [x] Add `ObjectStorage *ObjectStorageConfig` to `MarklogicClusterSpec` in `api/v1/marklogiccluster_types.go`.
- [x] Add `ObjectStorageStatus` (per-provider) and wire it into `MarklogicClusterStatus`, including the `ObjectStoragePhase` and `ObjectStorageFailureReason` enums.
- [x] Add CEL `XValidation` rules: `secretName` required when `authType=secret`; `authType` restricted to `secret` for both providers in v1, with explanatory messages for the reserved `instanceRole` / `managedIdentity` values.
- [x] Run `make generate manifests` to regenerate deepcopy and CRD YAML; verify `config/crd/bases` and `zz_generated.deepcopy.go`.
- [x] Run `make helm` as well — the chart ships its own copy of the CRDs in `charts/marklogic-operator-kubernetes/templates/`, which `make manifests` does **not** update. Skipping it leaves Helm installs on a stale schema that silently drops `spec.objectStorage`.

### 2. Management Client (`pkg/mlmanage`)

- [x] Add `EnsureAWSCredentials(ctx, AWSCredentials) error` and `EnsureAzureCredentials(ctx, AzureCredentials) error` to the `Client` interface.
- [x] Add `AWSCredentials` (`AccessKey`, `SecretKey`, optional `SessionToken`) and `AzureCredentials` (`StorageAccount`, `StorageKey`) config structs. `SessionToken` is opt-in pass-through — see Security NFR #5.
- [x] Implement `PUT /manage/v2/credentials/properties` with the provider selected by a `"type": "aws"|"azure"` field in the request body, following the existing `doJSON` idempotent `Ensure*` pattern, expecting `204 No Content`. Implemented in `pkg/mlmanage/credentials.go`.
- [x] Add `BuildAWSCredentialsPayload` / `BuildAzureCredentialsPayload` with validation (reject empty required fields) mirroring `BuildOAuthExternalSecurityPayload`; both must emit the `type` field, since Azure fails with `400` without it.
- [x] Ensure error strings never include request/response bodies that may contain secrets; build messages from the HTTP status code plus `errorResponse.messageCode` / `errorResponse.message` only. Implemented as `CredentialsError`, discarding `doJSON`'s own body-embedding error.
- [x] Map HTTP status codes to the Status Contract's failure reasons (`400`→`InvalidPayload`, `401`→`AuthenticationFailed`, `403`→`InsufficientPrivilege`, transport/`5xx`→`ManagementAPIUnreachable`). Implemented in `pkg/k8sutil/objectstorage_reconcile.go` rather than `pkg/mlmanage`, so the client package stays free of an `api/v1` import.

### 3. Secret Handling and Fingerprinting

- [x] Add a helper to resolve provider material from a referenced Secret (`accessKey`/`secretKey`, `storageAccount`/`storageKey`) with clear missing-key errors that name the key but never its value.
- [x] Derive the fingerprint salt from `MarklogicCluster.metadata.uid` (see Fingerprinting above); no separate salt Secret is provisioned.
- [x] Add a salted SHA-256 fingerprint helper over resolved material with canonical field ordering; never persist raw material. Implemented in `pkg/objectstorage/fingerprint.go` as HMAC-SHA256 with length-prefixed encoding.
- [x] Add unit tests confirming the fingerprint changes when *any* material field changes individually, that two clusters with identical credentials produce different fingerprints, and that raw values are never returned in strings/logs.

### 4. Controller Reconciliation (`internal/controller` / `pkg/k8sutil`)

- [x] Add an object storage reconcile step to the `MarklogicCluster` controller, gated on cluster readiness and bootstrap reachability.
- [x] Implement per-provider flow: resolve → fingerprint → skip-if-unchanged → apply → update status → emit event.
- [x] Add a `Watches` on `v1.Secret` mapping referencing `MarklogicCluster`s back into the reconcile queue, so a Secret edit triggers rotation without waiting for resync. No RBAC or cache changes were required (Secrets are already cached and already granted `watch`). **Note:** the controller's `WithEventFilter` predicate is global and defaults to dropping updates, so an explicit `case *corev1.Secret:` is required or the watch never fires on rotation.
- [x] Extend `clusterReferencesSecret` to include `spec.objectStorage.<provider>.secretName` via `ObjectStorageConfig.ReferencedSecretNames()`, so rotating a provider Secret wakes the controller.
- [x] Reconcile AWS and Azure independently so a failure in one does not block the other.
- [x] Requeue per the retriability column of the Status Contract's Failure Reasons table, so non-retriable misconfigurations do not hot-loop.
- [x] Emit Kubernetes events for apply-success and apply-failure.
- [x] Add structured, secret-safe logging for each transition.

### 5. Status and Events

- [x] Populate `status.objectStorage.<provider>` (phase, reason, message, appliedFingerprint, authType, lastAppliedTime).
- [ ] Optionally mirror an aggregate `ObjectStorageReady` condition into `status.conditions`.
- [x] Verify no secret material reaches status, events, or logs (assert in tests).

### 6. Helm Chart

- [x] **No `objectStorage` values are needed.** The original task assumed the chart renders cluster resources; it does not. `charts/marklogic-operator-kubernetes` installs only the operator Deployment, RBAC, ServiceAccount, Service, and the two CRDs — it never renders a `MarklogicCluster`. Users create that resource themselves (see `config/samples`), so adding `objectStorage` values would imply a capability the chart does not have.
- [x] The chart's only object storage responsibility is shipping the updated CRD schema, regenerated with `make helm`.

### 7. Samples and Docs

- [x] Add a sample under `config/samples` demonstrating secret-backed AWS + Azure (`object-storage.yaml`), registered in `config/samples/kustomization.yaml`.
- [ ] Keep this spec (`docs/spec/[SPEC]Object Storage.md`) as the reference and cross-link it from `docs` where object storage is mentioned.

### 8. Tests

- [x] Unit tests: payload builders, secret resolution, fingerprint behavior, and validation.
- [x] Client tests: `EnsureAWSCredentials` / `EnsureAzureCredentials` against a stub server asserting method, path, and body shape including the `type` field (mirroring `TestEnsureOAuthExternalSecurityCreatesConfiguration`), plus status-code handling and an assertion that credential material never reaches the error string.
- [x] Controller tests: readiness gating, skip-if-unchanged, rotation on secret change, independent per-provider failure, and secret-safe status/logs.
- [x] Integration test applying credentials against a running MarkLogic and verifying via the Management API that the provider is configured (without asserting secret values). Implemented as `pkg/mlmanage/credentials_live_test.go`, gated on `ML_MANAGE_ENDPOINT` so it skips by default. It lives in `pkg/mlmanage` rather than `test/integration` because it needs the client's digest authentication for the verification read — the Manage app server rejects basic auth — and because `test/integration` is Kubernetes-based and not wired into any make target.
- [x] CRD validation tests for the CEL rules, run against an envtest API server (`internal/controller/objectstorage_crd_validation_test.go`). CEL only executes inside a real API server, so these cannot be unit-tested. The test skips cleanly when envtest assets are unavailable.

### 9. Follow-Ups (not in v1)

- [ ] Azure managed identity support pending MarkLogic version confirmation.
- [ ] Optional dedicated least-privilege credential for credential application — either the `manage-admin` + `security` roles or, preferably, the per-provider `manage` + `manage-admin` + `credentials-set-{aws,azure}` privileges (both confirmed working; see Requirement Review #3).
- [ ] Fall back to a non-bootstrap host when the bootstrap is unreachable, instead of parking the provider in `BootstrapNotReady`. Confirmed possible (STEPS Session 7): any host accepts the credential write. Deferred because it widens the failure surface and diverges from the other `Ensure*` operations.
- [ ] Research CSI-based object storage mounting as an alternative integration.
- [ ] Credential revocation when a provider block is removed from the spec. The API side is trivial (`DELETE` with a `Content-Type` header, returns `204`); the open question is the opt-in signal that disambiguates "revoke" from "stop managing" — see Validation Rules #6.
