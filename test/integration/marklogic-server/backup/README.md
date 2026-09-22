# S3 backup and restore example

Status: Implemented with local contract tests; **live EKS/S3 validation pending**.
Scenario: `backup-s3`. Test: `TestS3BackupRestore`.

This example connects the new Secret-backed object-storage credential support to
an observable use case. `status.objectStorage.aws.phase: Applied` means the
operator configured credentials; it does not prove that the credentials can
access S3 or that a backup is usable. This suite tests the rest of that path.
Requirements owner, maintainer, and live validation reviewer: TBD with the team.

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
Backup directory preparation, cloud policies, and native S3 behavior still need
live validation; local contracts do not establish that they work on a given image.

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

For the first live run, record the source commit and working changes, exact
command without secrets, EKS/Kubernetes/operator/MarkLogic versions, both image
IDs, all case outcomes, the report directory, S3 prefix, and namespace/PV cleanup.
AKS/Azure coverage must be recorded separately; it is not inferred from EKS/S3.

The implementation follows the MarkLogic APIs for
[eval](https://docs.marklogic.com/REST/POST/v1/eval),
[full backup](https://docs.marklogic.com/xdmp:database-backup),
[backup status](https://docs.marklogic.com/xdmp:database-backup-status), and
[restore](https://docs.marklogic.com/xdmp:database-restore).
