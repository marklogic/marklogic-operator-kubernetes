# Object Storage Credentials Research and Prototype

As an operator engineer, I want to research and prototype how the MarkLogic Management API and Kubernetes-native identity mechanisms work together for object storage credentials, so that the team can commit to a design that is technically validated before building the full feature.

## Description

The functional spec assumes a specific integration model (cluster-wide credentials via `PUT /manage/v2/credentials/properties`, fingerprint-based drift detection, AWS IRSA keyless support). This story validates those assumptions against a real MarkLogic cluster and records findings that unblock implementation stories. The research must distinguish documented behavior from behavior observed on the MarkLogic image versions supported by the operator.

## Acceptance Criteria

### Must Achieve

1. Confirm against a live MarkLogic instance that `PUT /manage/v2/credentials/properties?type=aws` and `PUT /manage/v2/credentials/properties?type=azure` behave as documented, including response codes, request/response payload shape, provider independence, and masking behavior when the configuration is read back with `GET`.

2. Confirm the minimum authorization required independently for AWS and Azure: `manage-admin` + `security` versus the applicable individual credential privileges. Document whether a dedicated least-privilege MarkLogic user is practical for v1 or must be deferred.

3. Validate the complete Secret-backed static credential flow for both providers: resolve the expected Kubernetes Secret keys, apply the credentials through the Management API, verify the resulting configuration without exposing secret values, and confirm that unchanged material can be safely treated as idempotent.

4. Validate the fingerprinting approach (salted SHA-256 over resolved Secret material) as a safe and sufficient mechanism for idempotency and rotation detection, including stable salt handling across controller restarts, canonical field ordering, optional-field behavior, changes to every credential field, and confirmation that raw values never appear in status, events, logs, or errors. Confirm separately how Secret changes trigger reconciliation; a fingerprint alone is not a watch mechanism.

5. Resolve the supported-usage boundary from the epic: document whether v1 configures cluster-wide credentials only, or also configures backups or object-storage-backed forests. Explicitly record which forest, backup, region, and endpoint behaviors are out of scope if they are not validated.

6. Produce a findings matrix in this spec or a linked document. For each research question, record the test environment and MarkLogic image version, documented behavior, observed evidence, conclusion, and resulting v1 scope decision. Link the findings from the functional design spec's `Requirement Review` section.

7. Validate the status/condition model for reporting per-provider object storage configuration outcome: confirm the fields needed to indicate success or failure per provider, that failure reasons are specific enough to be actionable, and that no secret material appears in status output.

### Nice to Have (deferred beyond a Secrets-only v1)

8. Prototype AWS keyless access (IRSA on EKS) end-to-end using a documented supported MarkLogic image: a MarkLogic pod running under an IAM-role-annotated service account successfully accesses an S3 bucket with no static keys in the Kubernetes Secret or Management API configuration. Record the EKS and IAM prerequisites. Not required if v1 scope is Secrets-only.

9. Investigate Azure managed identity and Azure Workload Identity support across the MarkLogic image versions supported by the operator. Produce an explicit `Go`, `No-Go`, or `Defer` recommendation for v1, including the tested versions and reason for the recommendation. Not required if v1 scope is Secrets-only.

10. Research and document whether Kubernetes CSI abstractions (for example, mounting object storage as a volume) are a viable alternative or complementary integration path. State explicitly whether CSI is in scope for v1 or remains a follow-up and ensure that the research does not block the MarkLogic-native credential path.

No production code is required to ship from this story; throwaway/POC code is acceptable and should not block on full test coverage.
