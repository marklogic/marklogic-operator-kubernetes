# S3 backup and restore example

Status: **Live validated on EKS with AWS S3 on September 22, 2026** using static
AWS credentials and the image recorded below. STS and other environments remain unverified.
Scenario: `backup-s3`. Test: `TestS3BackupRestore`.
Purpose: Teaching example for future MarkLogic feature integration tests.

This example connects the new Secret-backed object-storage credential support to
an observable use case. `status.objectStorage.aws.phase: Applied` means the
operator configured credentials; it does not prove that the credentials can
access S3 or that a backup is usable. This suite tests the rest of that path.
Requirements owner, maintainer, and live validation reviewer: TBD with the team.

## Use this as a future feature example

The executable entry point is [`TestS3BackupRestore`](backup_test.go), registered
as `backup-s3` in the [scenario catalog](../../scenarios/catalog.json). It combines
the operator configuration contract with observable server behavior: applying a
storage credential, writing a full backup to S3, and reading that backup during
restore. The changed document before restore makes the final assertion distinguish
a successful restore from data that simply remained in the database.

| File | Responsibility to reuse when adding a feature test |
| --- | --- |
| [`config.go`](config.go) | Validate explicit environment settings and keep the destination inside a dedicated test prefix; normalize credentials consistently with the operator. |
| [`fixture.go`](fixture.go) | Build the isolated TLS cluster, provider Secret, and client Pod; let the operator apply the storage credential. |
| [`backup_test.go`](backup_test.go) | Check the live gate before setup, create a run, sequence named cases, stop dependent cases after failure, and record evidence. |
| [`queries.go`](queries.go) | Keep server operations separate from orchestration and pass inputs as external variables. |
| [`assertions.go`](assertions.go) | Require the current credential fingerprint, exact forest coverage, completed jobs, and a backup path owned by the run. |
| [`contracts_test.go`](contracts_test.go) | Check configuration, fixtures, parsing, and failure handling locally without Kubernetes or AWS. |

For a new feature, follow the [contribution guide](../../CONTRIBUTING.md) and
[scenario template](../../SCENARIO_TEMPLATE.md): define its observable acceptance
criteria, choose a unique test name and gate, register the scenario, and reuse
`testutil.NewRun`, `run.ApplyObjects`, `run.Stage`, and `run.Case`. Keep the new
feature's operations and assertions in its own suite; share fixture builders
when more than one suite needs them.

Future storage cases could cover credential rotation, incremental backup, or
Azure restore. Each needs its own inputs, expected results, external-artifact
cleanup policy, and live evidence. Those cases are not implemented here. The
[live validation record](#live-validation-record) covers this example's full S3
backup and same-cluster restore only.

## Cases

| Case | Required result |
| --- | --- |
| `credentials_applied` | The generated cluster references the run's AWS Secret and reports `Applied` with the fingerprint for its UID and exact credential material, including an optional STS token. |
| `seed_and_verify` | The isolated cluster's Documents database contains this run ID and the original fixture value. |
| `backup_completed` | Native full backup completes for every expected forest; the actual backup directory stays within this run's unique S3 prefix. |
| `change_and_verify` | The document is changed and the new value is observed before restore. |
| `restore_original_data` | Restore completes for every expected forest and the original document value/run ID returns. |

HTTP errors, missing or multiple eval results, malformed responses, missing job
IDs, failed/canceled/unknown jobs, stale credential status, missing forests, and
paths outside the run prefix fail the test. Later cases stop after a failure.
No authentication failure is treated as a successful negative test.

The suite restores only Documents on its own newly provisioned MarkLogic cluster.
It does not adopt an existing database or namespace. It uses two TLS MarkLogic
nodes from the shared fixture plus one curl Pod; it requires no Keycloak or OAuth
setup. HAProxy is disabled. Backup, status polling, and restore use the bootstrap
host directly, keeping the same job coordinator. Control queries use Schemas so
restoring Documents cannot take their evaluation database offline.

## Prerequisites and run

Before running live, install a CRD and operator built with `spec.objectStorage`
support. Merging code locally does not update the cluster. The operator must watch
new test namespaces. This suite never installs or upgrades it.

The shared MarkLogic preflight requires Kubernetes discovery, namespace lifecycle,
Pod create/get/list/watch/exec/log, events/PVC listing, Service/ConfigMap/Secret/
Deployment get/list/watch/create/patch, MarklogicCluster get/create/patch,
StatefulSet get/list/watch, PV get, and readable operator/StorageClass resources.
See [shared preflight](../testutil/preflight.go) for the executable contract.
The fixture uses two 10Gi Delete-reclaim PVCs and needs compatible capacity and
image pull access for the supplied MarkLogic image and `curlimages/curl:8.12.1`.

Supply an **existing AWS S3 bucket** and a dedicated test prefix reachable from
MarkLogic. Credentials need list/read/write and any multipart-upload/KMS permissions
required by that bucket's policy. The operator does not configure S3 region,
endpoint, proxy, or encryption settings. A private S3-compatible service needs
separate endpoint/DNS validation and is not covered by this example.

| Variable | Purpose |
| --- | --- |
| `INTEGRATION_CONTEXT` | Explicit Kubernetes context. |
| `INTEGRATION_OPERATOR_NAMESPACE` / `INTEGRATION_OPERATOR_DEPLOYMENT` | Installed object-storage-capable operator. |
| `MARKLOGIC_IMAGE` | Image supporting native S3 backup/restore; record the actual version during live validation. |
| `INTEGRATION_BACKUP_S3_URI` | `s3://bucket/dedicated-test-prefix`; bucket root, signed URLs, and path traversal are rejected. Path segments use letters, numbers, underscores, or hyphens. |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | Export from your approved credential source. Do not commit values. |
| `AWS_SESSION_TOKEN` | Optional externally refreshed STS credential; it must remain valid for the whole run. |
| `INTEGRATION_STORAGE_CLASS` | Optional explicit Delete-reclaim class, otherwise shared preflight selects the single default. |
| `INTEGRATION_RESULTS_DIR` | Optional local JSON/JUnit/diagnostic results directory. |

```sh
# First run local checks; these disable live gates and need no AWS credentials.
make integration-describe SCENARIO=backup-s3
make integration-check
make integration-test-local

# Export AWS credentials from your credential source before the live command.
INTEGRATION_CONTEXT=<context> \
INTEGRATION_OPERATOR_NAMESPACE=<namespace> \
INTEGRATION_OPERATOR_DEPLOYMENT=<deployment> \
MARKLOGIC_IMAGE=<image> \
INTEGRATION_BACKUP_S3_URI=s3://<bucket>/<dedicated-test-prefix> \
make integration-test SCENARIO=backup-s3
```

Direct Go invocation also checks `INTEGRATION_BACKUP_S3=true` before any setup.
The runner enables only this registered live suite. The default suite timeout is
45 minutes; readiness, credential application, and backup/restore jobs each have
bounded waits. Temporary credentials must leave enough time for the whole run.

The in-cluster client mounts a generated admin credential file and verifies TLS
with the test CA. Cloud credentials go only into the namespaced provider Secret;
the test does not call the Management API to install them itself. That distinction
ensures the case exercises the operator's credential reconciliation.

## Evidence and cleanup

The run appends its random run ID to the supplied S3 prefix and asks MarkLogic to
create the backup directory. It records `s3-backup-prefix`, and after a successful
backup the actual `s3-backup` path, in `run.json.retainedArtifacts`. The prefix
entry records the intended destination; a failed run may have no objects there.
Backup directory preparation and native S3 behavior passed in the environment
recorded below. Other images and bucket policies require their own validation.

**S3 objects are intentionally retained as evidence.** Namespace/PV cleanup does
not delete them. Use an agreed test-bucket lifecycle policy, or after reviewing
results list and remove only the exact run prefix from the report:

```sh
aws s3 ls 's3://<bucket>/<test-prefix>/<run-id>/' --recursive
aws s3 rm 's3://<bucket>/<test-prefix>/<run-id>/' --recursive
```

Do not substitute the bucket root or shared test prefix. On a versioned or
retention-protected bucket, deletion/lifecycle must also account for noncurrent
versions and retention rules. The suite makes no claim of cloud cleanup.
`INTEGRATION_RETAIN_NAMESPACE=true` independently retains Kubernetes resources
for inspection. Reports still record namespace cleanup separately from retained
external evidence.

## Scope and validation evidence

The example covers one full native S3 backup and same-cluster restore of known
content. It does not cover Azure Blob, backup scheduling, incremental backups,
credential rotation/expiry during a job, cross-cluster disaster recovery, operator
reinstallation, or region/encryption customization. No new bucket, IAM identity,
cloud cluster, or cloud policy is created by this suite.

Local tests cover destination validation, credential fingerprint freshness,
fixture isolation, credential mounting, multipart eval parsing, and completion
checks. The catalog-driven test verifies that this live suite skips before
setup when disabled. None of these replace live backup evidence.

For each live run, record the source commit and working changes, exact
command without secrets, EKS/Kubernetes/operator/MarkLogic versions, both image
IDs, all case outcomes, the report directory, S3 prefix, and namespace/PV cleanup.
AKS/Azure coverage must be recorded separately; it is not inferred from EKS/S3.

## Live validation record

On September 22, 2026, `TestS3BackupRestore` passed all five cases on
`pzhou@marklogic-tiered-poc.us-west-2.eksctl.io`: credential application, original
data verification, full S3 backup, changed-data verification, and restoration of
the original run ID and value. JUnit reports **5 tests, 0 failures, 0 skips**.
No test-code changes were needed for this run.

| Setting | Observed value |
| --- | --- |
| Source | `7a51a4ce43f46bcef1aaa91fe0e1a9749ffdb51c`; working tree contained only the two README walkthrough edits when the test started. |
| Kubernetes | `v1.32.13-eks-bca9cf6` |
| Operator | `308453789681.dkr.ecr.us-west-2.amazonaws.com/marklogic-tiered-poc/marklogic-operator@sha256:221eb6c1d702bd3822e58ea08ba0ff4a0c34757b93a8e043a5438cec3845005d` |
| MarkLogic, both nodes | `progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6`, digest `sha256:5bbef4b49d737e5d9c1136bcd77ef52a9a3649a125cc4ff8ee1d908daefed56d` |
| Client | `curlimages/curl:8.12.1`, digest `sha256:94e9e444bcba979c2ea12e27ae39bee4cd10bc7041a472c4727a558e213744e6` |
| Storage | `gp2`, Delete reclaim policy |
| Credentials | Resolved from the local AWS default profile; static access/secret keys, no session token. Values were passed through the child process environment and not printed. |
| Run ID | `b8c8f8c8-9dd8-4b0f-b150-0b4336c1e50d` |
| Namespace | `backup-s3-ggq6c`; cleanup completed, namespace absent and no PVs still reference it. |
| Local report directory | `test/test_results/integration/run-2691958607/` (ignored by Git), containing `run.json` and `junit.xml`. |

The existing operator was used without installing or upgrading it. With AWS
credentials loaded into the environment, the command was equivalent to:

```sh
INTEGRATION_CONTEXT=pzhou@marklogic-tiered-poc.us-west-2.eksctl.io \
INTEGRATION_OPERATOR_NAMESPACE=marklogic-operator-system \
INTEGRATION_OPERATOR_DEPLOYMENT=marklogic-operator-controller-manager \
MARKLOGIC_IMAGE=progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6 \
INTEGRATION_BACKUP_S3_URI=s3://marklogic-tiered-poc-308453789681-usw2/integration-backup \
INTEGRATION_RETAIN_NAMESPACE=false MARKLOGIC_OAUTH_RETAIN_NAMESPACE=false \
make integration-test SCENARIO=backup-s3
```

S3 listing independently confirmed the backup's `BackupTag.txt`, Documents forest
files, and configuration files under the retained directory:

```text
s3://marklogic-tiered-poc-308453789681-usw2/integration-backup/b8c8f8c8-9dd8-4b0f-b150-0b4336c1e50d/20260922-1253388045800
```

S3 objects remain intentionally retained. Namespace and Kubernetes PV deletion
were checked; provider-side EBS deletion was not independently checked. This run
does not establish STS rotation/expiry, other MarkLogic images, or AKS/Azure coverage.

The implementation follows the MarkLogic APIs for
[eval](https://docs.marklogic.com/REST/POST/v1/eval),
[full backup](https://docs.marklogic.com/xdmp:database-backup),
[backup status](https://docs.marklogic.com/xdmp:database-backup-status), and
[restore](https://docs.marklogic.com/xdmp:database-restore).
