# JIRA Stories: Object Storage Configuration via Operator

Reference design: [Object Storage Configuration.md](./Object%20Storage%20Configuration.md)

---

## Epic

**Epic: Object Storage Configuration via Operator**

Enable customers to declaratively configure AWS S3 and Azure Blob object storage credentials as part of the MarkLogic Kubernetes Operator cluster spec, so clusters are usable for backups and supported object-storage scenarios immediately after deployment, without manual post-deployment steps.

---

## Story 1 (Research / POC): Validate MarkLogic Object Storage Credential Integration Approach

**Type:** Spike / Research
**Priority:** Highest (blocks all downstream stories)

**Story:**
As an operator engineer, I want to research and prototype how the MarkLogic Management API and Kubernetes-native identity mechanisms work together for object storage credentials, so that the team can commit to a design that is technically validated before building the full feature.

**Description:**
The functional spec assumes a specific integration model (cluster-wide credentials via `PUT /manage/v2/credentials/properties`, fingerprint-based drift detection, AWS IRSA keyless support). This story validates those assumptions against a real MarkLogic cluster and records findings that unblock implementation stories.

**Acceptance Criteria:**
1. Confirm against a live MarkLogic instance that `PUT /manage/v2/credentials/properties?type=aws|azure` behaves as documented (response codes, payload shape, masking behavior on `GET`).
2. Confirm the minimum privileges actually required (`manage-admin` + `security` vs. individual `credentials-set-aws` / `credentials-set-azure` privileges) and document whether a dedicated least-privilege MarkLogic user is practical for v1 or must be deferred.
3. Prototype AWS keyless access (IRSA on EKS) end-to-end: MarkLogic pod running under an IAM-role-annotated service account successfully accesses an S3 bucket with no static keys configured.
4. Investigate Azure managed identity support across the MarkLogic image versions the operator supports; produce a clear go/no-go recommendation for v1.
5. Validate the fingerprinting approach (salted SHA-256 over resolved Secret material) as a safe, sufficient mechanism for idempotency and rotation detection, given that the Management API masks credentials on read.
6. Research and document whether Kubernetes CSI abstractions (e.g., mounting object storage as a volume) are a viable alternative or complementary integration path, per the original requirement's open question.
7. Produce a short findings write-up (in this spec or a linked doc) confirming or adjusting the scope decisions already recorded in the "Requirement Review" section of the design spec.
8. No production code is required to ship from this story; throwaway/POC code is acceptable and should not block on full test coverage.

---

## Story 2: API & CRD Schema for `spec.objectStorage`

**Type:** Story

**Story:**
As a MarkLogic administrator, I want to declare AWS S3 and/or Azure Blob credential configuration in my `MarklogicCluster` spec, so that I can configure object storage access declaratively as part of my GitOps workflow.

**Acceptance Criteria:**
1. `MarklogicClusterSpec` supports an optional `objectStorage` field with independent `aws` and `azure` sub-blocks.
2. `aws` supports `authType` (`secret` | `instanceRole`), `secretName`, and an informational `region`.
3. `azure` supports `authType` (`secret`, with `managedIdentity` reserved/rejected in v1) and `secretName`.
4. No field accepts inline secret material; only Secret references are accepted.
5. CEL validation rules enforce: `secretName` required when `authType=secret`; `secretName` forbidden when `authType=instanceRole`; Azure `authType` restricted to `secret` in v1.
6. `MarklogicClusterStatus` includes a new `objectStorage` status block (see Story 6) with generated deepcopy code.
7. `make generate manifests` runs cleanly and CRD YAML under `config/crd/bases` reflects the new schema.
8. CRD validation unit/envtest coverage for the new CEL rules (valid and invalid combinations).

---

## Story 3: Management Client Support for Credential APIs

**Type:** Story

**Story:**
As the operator, I want a management-client method to apply AWS and Azure object storage credentials to a MarkLogic cluster via the Management API, so that credential configuration can be automated during reconciliation.

**Acceptance Criteria:**
1. `mlmanage.Client` interface gains `EnsureAWSCredentials(ctx, AWSCredentials) error` and `EnsureAzureCredentials(ctx, AzureCredentials) error`.
2. Implementation calls `PUT /manage/v2/credentials/properties?type=aws` / `?type=azure` with the correct JSON payload shape and expects `201 Created`.
3. Payload builders (`BuildAWSCredentialsPayload`, `BuildAzureCredentialsPayload`) validate required fields and reject incomplete input, mirroring the existing `BuildOAuthExternalSecurityPayload` pattern.
4. No request or response body containing credential material is ever included in returned error strings.
5. Unit tests cover successful apply, validation failures, and non-2xx Management API responses using a stub HTTP server (mirroring `TestEnsureOAuthExternalSecurityCreatesConfiguration`).

---

## Story 4: Secret Resolution and Credential Fingerprinting

**Type:** Story

**Story:**
As the operator, I want to safely resolve object storage credential material from referenced Kubernetes Secrets and compute a non-reversible fingerprint of that material, so that I can detect configuration drift and support rotation without ever persisting or logging secret values.

**Acceptance Criteria:**
1. A helper resolves AWS material (`accessKey`, `secretKey`, optional `sessionToken`) and Azure material (`storageAccount`, `storageKey`) from a referenced Secret, returning a clear, specific error when the Secret or a required key is missing.
2. A helper computes a salted SHA-256 fingerprint over resolved material; the salt/material is never returned by any exported function other than the fingerprint itself.
3. Fingerprint changes whenever any material field changes, and is stable when material is unchanged.
4. Unit tests assert no raw secret values appear in any error string, log field, or returned struct other than the in-memory material used for the API call.

---

## Story 5: Cluster Controller Reconciliation for Object Storage

**Type:** Story

**Story:**
As a platform engineer, I want the MarkLogic Operator to automatically apply my declared object storage configuration during cluster reconciliation, so that credentials are configured without any manual post-deployment step.

**Acceptance Criteria:**
1. The `MarklogicCluster` controller reconciles `spec.objectStorage` only after the cluster is initialized and the bootstrap host is reachable and secured.
2. For each declared provider (`secret` mode): resolve material → compute fingerprint → skip if unchanged → apply via the management client → update status.
3. AWS `instanceRole` mode skips the credential write and records that keyless auth is in effect.
4. AWS and Azure are reconciled independently; a failure in one does not block or fail the other.
5. Management API failures result in a retriable state (requeue), not a terminal/crash condition.
6. Reconciliation does not disrupt the running cluster, existing groups, or data.
7. Controller tests cover: first-time apply, no-op on unchanged config, rotation after Secret change, missing Secret/key handling, and independent per-provider failure.

---

## Story 6: Status, Conditions, and Events for Object Storage

**Type:** Story

**Story:**
As a MarkLogic administrator, I want the operator to clearly report whether my object storage configuration succeeded or failed, so that I can verify and troubleshoot the setup without inspecting MarkLogic directly.

**Acceptance Criteria:**
1. `MarklogicCluster.status.objectStorage.aws` and `.azure` each expose `phase` (`Pending`/`Applied`/`Failed`/`Disabled`), `reason`, `message`, `appliedFingerprint`, `authType`, and `lastAppliedTime`.
2. Failure reasons are machine-readable (e.g., `SecretNotFound`, `SecretKeyMissing`, `ManagementAPIError`, `BootstrapNotReady`).
3. An aggregate condition (e.g., `ObjectStorageReady`) is optionally mirrored into `status.conditions`.
4. Kubernetes events are emitted for apply success, apply failure, and rotation.
5. Controller logs record each phase transition with structured fields and never include secret material.
6. Tests assert that no secret value ever appears in `status`, events, or logs.

---

## Story 7: Helm Chart Support for Object Storage Configuration

**Type:** Story

**Story:**
As a DevOps engineer deploying the operator via Helm, I want to configure object storage settings through Helm values, so that my chart-based deployments stay consistent with my GitOps workflow.

**Acceptance Criteria:**
1. `charts/marklogic-operator-kubernetes/values.yaml` exposes `objectStorage` values (disabled by default) mirroring the CRD schema.
2. Templates render `spec.objectStorage` on the `MarklogicCluster` resource when configured.
3. Chart README/values documentation is updated to describe the new fields.
4. Helm template/lint tests cover default (disabled) and configured (AWS/Azure/both) scenarios.

---

## Story 8: Samples and Documentation

**Type:** Story

**Story:**
As a new user of the operator, I want ready-to-use sample manifests and documentation for object storage configuration, so that I can quickly and correctly configure AWS/Azure access for my cluster.

**Acceptance Criteria:**
1. A sample under `config/samples` demonstrates secret-backed AWS + Azure configuration.
2. A sample demonstrates AWS keyless (IRSA) configuration.
3. `docs/spec/Object Storage Configuration.md` remains the canonical reference and is cross-linked from relevant docs (e.g., README or other docs referencing backups/storage).

---

## Story 9: End-to-End and Integration Test Coverage

**Type:** Story

**Story:**
As the operator maintainers, we want end-to-end and integration test coverage for object storage configuration, so that regressions in credential application, rotation, and failure handling are caught before release.

**Acceptance Crieria:**
1. Integration test applies AWS and/or Azure credentials against a running MarkLogic cluster and verifies (via the Management API) that the provider is configured, without asserting on secret values.
2. Integration test verifies rotation: updating the referenced Secret results in re-application and an updated fingerprint/`lastAppliedTime`.
3. Integration/e2e test verifies independent provider failure handling (e.g., invalid Azure secret does not block AWS from applying).
4. Test suite is wired into existing CI (`test/integration`, `test/e2e` as appropriate) following current project conventions.

---

## Backlog / Future Stories (Out of Scope for v1)

These are explicitly deferred per the design spec's "Follow-Ups" section and should be created as separate backlog items, not scheduled into the v1 delivery:

1. **Azure Managed Identity Support** — Add keyless Azure Blob access once MarkLogic version support is confirmed (depends on Story 1 findings).
2. **Dedicated Least-Privilege Credential for Object Storage** — Support an optional `manage-admin` + `security` (or `credentials-set-*` privilege) MarkLogic user for credential application, instead of reusing the bootstrap admin credential.
3. **CSI-Based Object Storage Mounting Research** — Deeper investigation into whether CSI volume abstractions are a viable complementary or alternative integration path.
4. **Credential Revocation on Spec Removal** — Optionally clear MarkLogic-side credentials when a provider block is removed from `spec.objectStorage`, instead of leaving previously applied credentials in place.
