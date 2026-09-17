# Implement Declarative Object Storage Credentials for MarkLogic Clusters

**Issue type:** Story  
**Epic:** Object Storage

## User Story

As a platform engineer, I want to configure AWS S3 and Azure Blob credentials through the MarkLogic cluster specification and Kubernetes Secrets, so that credential application and rotation are automated and compatible with GitOps.

## Description

Implement the credential configuration feature defined by the functional spec and validated by the object storage research story. Users declare either or both providers in `MarklogicCluster.spec.objectStorage`, referencing Secrets in the cluster's namespace. The operator resolves the credentials and applies them as a cluster-wide security setting through the bootstrap host's Management API after initialization and security prerequisites are satisfied.

The implementation must support independent provider reconciliation, idempotent application, Secret-driven rotation, and actionable status without exposing credential values. AWS temporary credentials are supported through an optional externally refreshed session token.

This story delivers credential configuration. An `Applied` status confirms that MarkLogic accepted the credentials; it does not confirm cloud authorization, token validity, or successful backups. Backup and forest configuration remain administrator responsibilities.

## Acceptance Criteria

### 1. Declarative API and validation

- `MarklogicCluster.spec.objectStorage` supports independently optional `aws` and `azure` blocks, including both providers on the same cluster.
- Each provider supports `authType: secret`, defaults to that mode, and requires `secretName`. Credential values cannot be supplied inline in the CR.
- CRD validation rejects AWS `instanceRole` and Azure `managedIdentity` with explanatory messages.
- AWS `region` is optional and informational; it does not change MarkLogic's region or endpoint settings.
- Clusters without `objectStorage` retain their existing behavior and require no provider Secrets.

### 2. Secret resolution

- Provider Secrets are resolved only in the MarklogicCluster's namespace using the following contract:

| Provider | Required keys | Optional keys |
|---|---|---|
| AWS | `accessKey`, `secretKey` | `sessionToken` |
| Azure | `storageAccount`, `storageKey` | None |

- Missing Secrets and absent or blank required keys produce actionable provider failure status without exposing values or crashing reconciliation.
- A supplied nonempty AWS `sessionToken` is included in the applied material and fingerprint. An absent or empty token is omitted from the request.
- The operator does not obtain or renew temporary credentials or track token expiry. Users must refresh the referenced Secret through an external credential source.

### 3. Cluster-wide credential application

- After bootstrap readiness and admin-credential prerequisites are satisfied, the operator applies each declared provider through `PUT /manage/v2/credentials/properties` using the existing admin-capable bootstrap credential and configured Management API transport.
- Every request selects the provider with `type: aws` or `type: azure` in the JSON body. HTTP `204 No Content` is treated as success.
- Credentials are applied once per provider for the cluster, rather than once per MarklogicGroup or pod.
- An AWS-specific resolution or application failure does not prevent Azure reconciliation, and vice versa. Shared bootstrap prerequisite failures may leave both providers pending.

### 4. Idempotency and rotation

- Compute an HMAC-SHA256 fingerprint using the cluster UID as the public salt and a canonical, length-prefixed encoding of the provider and resolved credential fields.
- When a provider is already `Applied` and its fingerprint is unchanged, skip the credential write and preserve `lastAppliedTime`.
- Watch referenced Secrets so credential data changes trigger reconciliation without a CR edit. Metadata-only Secret changes do not require credential reapplication.
- Changing any credential field, including adding, changing, or removing `sessionToken`, changes the fingerprint and reapplies the affected provider.
- Fingerprints remain stable across controller restarts for the same cluster and material. Different cluster UIDs produce different fingerprints for identical material.
- Document that fingerprints detect changes to desired Secret material; they do not detect out-of-band changes in MarkLogic. A public UID salt does not prevent offline guessing of complete candidate credentials.

### 5. Status, failures, and observability

- Report each provider through `status.objectStorage.<provider>` with `Pending`, `Applied`, `Failed`, or `Disabled` phase, plus applicable `reason`, `message`, `authType`, `appliedFingerprint`, and `lastAppliedTime` fields.
- Populate fingerprint and application time on successful application. These fields are not a retained history after failure or removal.
- Apply the following failure and retry contract:

| Reason | Trigger | Retry behavior |
|---|---|---|
| `BootstrapNotReady` | Bootstrap discovery, admin-Secret resolution, or host-readiness probe fails | Retry after 10 seconds |
| `SecretNotFound` | Provider Secret is missing or cannot be read | Retry after 30 seconds |
| `SecretKeyMissing` | Required provider key is absent or blank | Retry after 30 seconds |
| `InvalidPayload` | Credential PUT returns HTTP 400 | No failure-driven periodic retry; retry on a subsequent reconcile trigger |
| `AuthenticationFailed` | Credential PUT returns HTTP 401 | Retry after 30 seconds |
| `InsufficientPrivilege` | Credential PUT returns HTTP 403 | No failure-driven periodic retry; fix privileges and trigger reconciliation |
| `ManagementAPIUnreachable` | Credential PUT encounters a transport error or HTTP 5xx | Retry after 30 seconds |
| `ManagementAPIError` | Other unexpected credential application failure | Retry after 30 seconds |

- Emit secret-safe events and structured logs for credential application success and failure. Rotation uses the same `ObjectStorageApplied` success event as initial application.
- A status persistence failure is returned to the controller rather than silently treated as success.
- Document that failures in the earlier readiness probe, including authentication failures, appear as `BootstrapNotReady` before credential PUT is attempted. Cluster readiness alone is not an object-storage readiness signal.

### 6. Credential confidentiality

- Never include credential values in CR fields, status, events, logs, or displayed errors.
- Discard raw Management API error bodies and structured remote `message` and `messageCode` fields because either may reflect submitted secrets. Report HTTP status and locally constructed context only; display generic transport failures.
- Verification tooling must not print raw credential GET responses, including plaintext AWS session tokens. Secret creation and rotation examples must avoid exposing real values in command arguments or checked-in files.

### 7. Removal behavior

- Removing one provider while retaining the other sets the removed provider to `Disabled`.
- Removing all provider configuration clears `status.objectStorage`.
- Neither action revokes credentials already stored in MarkLogic. Document that `Disabled` means unmanaged, not revoked.

### 8. Packaging and documentation

- Regenerate API deepcopy code and CRDs, including the Helm chart's separate CRD copy, so both installation paths accept the feature.
- Do not add cluster-level object-storage values to the operator Helm chart, which does not render MarklogicCluster resources.
- Provide a sample and walkthrough covering both providers, Secret-only rotation, failure diagnosis, removal behavior, and optional session-token handling.
- Document required Management API privileges, the bootstrap credential used, and the distinction between stored credentials and verified cloud access.

## Implementation Tasks

- Add or complete the API types, validation markers, generated schemas, and per-provider status types.
- Implement provider payload builders and secret-safe Management API client methods in `pkg/mlmanage`.
- Implement Secret resolution, fingerprinting, independent provider reconciliation, status persistence, and retry handling in `pkg/objectstorage` and `pkg/k8sutil`.
- Wire Secret watches and predicates into the MarklogicCluster controller, including provider Secret references.
- Update samples, the functional spec, demo scripts, and Helm CRD packaging.
- Reuse existing research and implementation code where it satisfies this contract; close remaining gaps and supply verification evidence.

## Verification / Definition of Done

- Unit and stub-server tests cover payload shapes, optional token handling, required-field validation, fingerprints, unchanged-material skips, rotation, provider independence, removal, status persistence failures, and retry mapping.
- Secret mapper and predicate tests demonstrate that referenced Secret data changes enqueue the correct clusters without cross-namespace matches.
- Regression tests prove that reflected credentials in both raw and structured error responses do not reach displayed errors, status, events, or logs.
- CRD validation tests run against an envtest API server and cover valid provider combinations, defaults, required Secret references, and rejected auth modes.
- Live integration evidence against the pinned supported MarkLogic image covers credential application for both providers, repeated application, and rotation with secret-safe verification. Record the tested image and environment; skipped live tests are not acceptance evidence.
- Demonstrate the controller flow on Kubernetes: apply both providers, update only a Secret, observe the affected fingerprint change, and confirm a provider-specific failure leaves the other provider applied.
- Relevant automated checks pass, generated artifacts are consistent, and documentation reflects the implemented behavior. Live cloud backup execution is outside this story's acceptance scope.

## Out of Scope

- AWS IRSA/instance-role authentication and Azure managed/workload identity.
- Automatic STS credential issuance, renewal, or expiry monitoring.
- Backup scheduling, object-storage-backed forest creation, CSI integration, and region/endpoint provisioning.
- Credential revocation on spec removal or cloud-side IAM key lifecycle management.
- Detection or repair of out-of-band credential changes in MarkLogic.
- A dedicated least-privilege MarkLogic user, non-bootstrap-host fallback, or an aggregate `ObjectStorageReady` condition.

## References

- [Object Storage epic](%5BEPIC%5DObject%20Storage.md)
- [Functional specification](%5BSPEC%5DObject%20Storage.md)
- [Research and prototype story](%5BJIRA%5DOjbect%20Storage.md)
- [Research evidence and findings](%5BSTEPS%5D%20Object%20Storage.md)
- [Demo walkthrough](%5BDEMO%5D%20Object%20Storage.md)
