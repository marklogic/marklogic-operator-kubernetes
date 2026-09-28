# MLE-32337 Least-Privilege PoC Change Report

## Purpose

This document consolidates the work completed for the MarkLogic Operator least-privilege proof of concept (PoC). It records the implementation, defects found, code changes, validation performed, and remaining limitations.

The PoC demonstrates that a MarkLogic cluster can be bootstrapped with an administrator credential and then operated with a dedicated least-privilege credential while retaining the administrator Secret for recovery.

## Scope

The PoC covered:

- MarkLogic-side role and user permissions required by the Operator.
- Kubernetes Secret handoff from bootstrap admin to operator user.
- Static cluster creation and joining at three replicas.
- Pod replacement using only the operator Secret.
- Kubernetes RBAC boundaries.
- Join completion and host-online detection.
- Readiness behavior after MarkLogic security is enabled.
- Clean teardown and installation from scratch.
- Manual MarkLogic image upgrade validation.

The PoC did not validate production high availability, TLS, OAuth, dynamic-host operations, or automatic product reconciliation of the MarkLogic role and user.

## Environment

| Item | Value |
| --- | --- |
| Date | 2026-09-28 |
| Kubernetes | Client `v1.35.0`, server `v1.35.0` |
| StorageClass | `local-path` |
| Operator namespace | `ml-lp-operator` |
| Test namespace | `ml-lp-test` |
| MarklogicCluster | `least-privilege` |
| MarklogicGroup / StatefulSet | `node` |
| Replica count | 3 |
| Operator image | Locally built image using the PoC values |
| Initial MarkLogic image | `progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6` |
| Upgraded MarkLogic image | `progressofficial/marklogic-db:12.1.0-ubi9-rootless-2.3.0` |

## PoC Artifacts Added

The PoC harness is under `test/poc/least-privilege/`.

| File | Purpose |
| --- | --- |
| `cluster.yaml` | Declares the three-node MarklogicCluster used by the PoC. |
| `role.json` | Defines the least-privilege MarkLogic role. |
| `setup.sh` | Installs the Operator and cluster, provisions the role/user, creates the operator Secret, and performs credential handoff. |
| `verify.sh` | Runs positive and negative authorization checks and captures redacted evidence. |
| `common.sh` | Provides context checks, Secret access, waits, Manage API helpers, and port-forward lifecycle handling. |
| `cleanup.sh` | Removes PoC resources, with explicit flags for PVC and namespace deletion. |
| `values.yaml` | Configures the locally built Operator image. |
| `README.md` | Documents prerequisites, execution, expected permissions, evidence, limitations, and cleanup. |
| `RESULTS.md` | Records the environment and acceptance results. |
| `.gitignore` | Excludes generated or sensitive local evidence as appropriate. |

Generated evidence is written under `test/poc/least-privilege/evidence/` and must be reviewed before it is committed. Evidence includes the result matrix, redacted role/user/cluster data, Kubernetes RBAC inventory, cluster-scoped exceptions, and redacted Operator logs.

## Least-Privilege Design

Two Kubernetes Secrets are used:

- `least-privilege-admin`: bootstrap administrator credential, retained for recovery.
- `least-privilege-operator`: dedicated runtime credential used by the reconciled workload.

The MarkLogic user `least-privilege-operator` receives only the custom `marklogic-operator` role. It does not directly receive `admin` or `security`.

The `marklogic-operator` role inherits:

- `manage-admin`
- `pki`
- `admin-ui-user`

It also receives these execute privileges:

| Privilege | Action URI |
| --- | --- |
| `create-user` | `http://marklogic.com/xdmp/privileges/create-user` |
| `xdmp:remove-dynamic-hosts` | `http://marklogic.com/xdmp/privileges/remove-dynamic-hosts` |
| `xdmp:eval` | `http://marklogic.com/xdmp/privileges/xdmp-eval` |
| `create-external-security` | `http://marklogic.com/xdmp/privileges/create-external-security` |

### Permission discovered during the PoC

Static joining calls `POST /admin/v1/cluster-config`, which requires the protected `admin-ui` privilege. Assigning `admin-ui` directly to the custom role was silently discarded by MarkLogic. Inheriting the built-in `admin-ui-user` role was therefore required.

Without it, joining returned HTTP 403 with `SEC-NOADMIN`. The returned HTML error body was then incorrectly treated as `/tmp/cluster.zip`, causing the local apply request to fail with `XDMP-INVZIP`.

## Setup and Credential Handoff

The setup workflow performs the following operations:

1. Verifies the `rancher-desktop` context, default StorageClass, and required tools.
2. Creates `ml-lp-operator` and `ml-lp-test` when needed.
3. Builds the Operator and installs it with Helm.
4. Creates `least-privilege-admin` only when it does not already exist.
5. Applies the MarklogicCluster and waits for bootstrap readiness.
6. Creates or updates the `marklogic-operator` role.
7. Creates or updates the `least-privilege-operator` user.
8. Creates `least-privilege-operator` as a Kubernetes Secret.
9. Patches the live MarklogicCluster to reference the operator Secret.
10. Retains the bootstrap admin Secret for recovery.

The StatefulSet uses the `OnDelete` update strategy. A template change alone does not rotate existing pods. During validation, pods were replaced individually and their controller revision hashes were checked to prove that all workloads adopted the operator Secret.

## Product Code Changes

### Static join completion

`pkg/k8sutil/scripts/cluster-config.sh` was changed to make static joining deterministic:

- Added `ensure_join_restart` to detect the MarkLogic PID change after applying cluster configuration.
- Added an explicit MarkLogic restart when the expected automatic restart does not occur.
- Added `join_check` to query the bootstrap Manage API until the joining FQDN reports `<online>true</online>`.
- Replaced the unreliable post-join localhost timestamp assumption.
- Required both restart completion and actual online status before logging join success.

This distinction matters because a host can be registered in MarkLogic but still be offline or disconnected.

### Readiness probe

`pkg/k8sutil/statefulset.go` changed the static readiness command from:

```text
test -f /tmp/marklogic_ready && curl -s -f http://localhost:7997/
```

to:

```text
test -f /tmp/marklogic_ready && curl -s -o /dev/null http://localhost:7997/
```

After security initialization, port 7997 can return HTTP 401 while MarkLogic is reachable and healthy. `curl -f` interpreted that authenticated response as a failure and kept joined pods unready. The revised probe still requires the wrapper completion marker and successful HTTP connectivity but does not require a 2xx response.

## Validation Performed

### Authorization

The PoC verified that:

- The custom role contains all expected inherited roles and explicit privileges.
- The operator user has the custom role and no direct `admin` or `security` role.
- Management root, group, and host reads return HTTP 200 with operator credentials.
- An attempted creation of a user assigned to `admin` is rejected with HTTP 403.
- Recent Operator logs contain no authentication or authorization failures after handoff.

### Kubernetes RBAC

The Operator ServiceAccount could manage the required resources in `ml-lp-test`. It could not list nodes or Secrets in `default`. Kubernetes namespace authorization and MarkLogic authorization were treated as independent controls.

### Three-node static cluster

The cluster was scaled and recreated with three replicas:

- StatefulSet `node` reached 3/3 Ready.
- MarklogicGroup `node` reported Ready.
- Joiners logged restart detection, online confirmation, `joined group Default`, and `cluster configuration completed`.
- Replacement pods followed the already-joined path correctly.
- The Manage API reported `total-hosts=3` and `total-hosts-offline=0`.
- All pods used `least-privilege-operator` after explicit `OnDelete` rotation.
- `least-privilege-admin` remained present and unmounted.

### Clean reinstall

A clean test removed the Helm release, MarklogicCluster, PoC namespaces, PVCs, MarkLogic CRDs, and release-owned cluster RBAC. The PoC was then installed from scratch.

During the first setup pass, joining pods transiently received HTTP 401 while bootstrap security initialization was settling. Rerunning setup after all three pods became Ready completed successfully. The final clean cluster reported all three hosts online.

The clean-install PVC UIDs used for subsequent safety checks were:

| PVC | UID |
| --- | --- |
| `datadir-node-0` | `d40bae77-113f-4a46-b54d-5e71076e6aba` |
| `datadir-node-1` | `b0d23c81-6517-432f-99c4-4fa6a4e9e2c6` |
| `datadir-node-2` | `565915e4-e770-469c-9acf-8823be4f578f` |

### Manual MarkLogic upgrade

The MarkLogic cluster was manually upgraded from:

```text
progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6
```

to:

```text
progressofficial/marklogic-db:12.1.0-ubi9-rootless-2.3.0
```

The manual upgrade completed without observed issues. The resulting three-node cluster was healthy, with all three pods Ready and the Manage API reporting three total hosts and zero offline hosts.

## Current Source and Runtime State

At the time this report was written, `cluster.yaml` declares:

```yaml
image: progressofficial/marklogic-db:12.1.0-ubi9-rootless-2.3.0
auth:
  secretName: least-privilege-admin
```

This file is the bootstrap declaration. `setup.sh` subsequently patches the live custom resource to `least-privilege-operator`.

The current live custom resource matches the source image and has three healthy nodes. It currently references `least-privilege-admin`, not `least-privilege-operator`, because the bootstrap declaration was applied after the earlier handoff. Both Secrets are retained, but the runtime credential handoff is not active in this latest state. Re-running the handoff step and rotating `OnDelete` pods is required before claiming that the current runtime uses only the least-privilege Secret.

## Defects and Lessons Learned

- A Kubernetes Ready pod is not sufficient evidence that all MarkLogic hosts are connected.
- Validation must include the Manage API `total-hosts-offline` value.
- Host registration does not imply host-online status.
- Static join completion must detect the actual MarkLogic restart and online state.
- Protected MarkLogic privileges may not persist when assigned directly.
- `OnDelete` StatefulSets require deliberate pod rotation and revision verification.
- HTTP 401 can prove service reachability after security is enabled and must not automatically fail the readiness probe.
- Unique local Operator tags are needed with `IfNotPresent` to avoid testing a stale cached image.
- Bootstrap credential retention provides a necessary recovery path without requiring workloads to continue using administrator credentials.

## Acceptance Outcome

The least-privilege objective passed for static one-node and three-node operation during the completed handoff validation:

- The dedicated operator identity can perform required reconciliation and static join operations.
- Unauthorized admin escalation is denied.
- Runtime workloads can use only the operator Secret.
- The bootstrap admin Secret remains available for controlled recovery.
- Three hosts can join and report online with the corrected join and readiness logic.
- The manual upgrade from MarkLogic `12.0.3-ubi9-rootless-2.2.6` to `12.1.0-ubi9-rootless-2.3.0` completed without observed issues.

The current live custom resource uses the bootstrap admin Secret. This does not invalidate the completed least-privilege validation, but it means the latest runtime is not presently in the final handoff state.

## Remaining Work

- Implement product-level reconciliation for automatic creation and maintenance of the MarkLogic role and operator user.
- Reapply the operator-Secret handoff to the current cluster and rotate all `OnDelete` pods, then verify their revision and mounted Secret.
- Validate dynamic-host workflows and their separate credential path.
- Validate TLS and OAuth configurations.
- Validate production failure domains and multi-node availability outside Rancher Desktop.
- Add focused automated tests for join restart detection, online-host checks, and authenticated readiness responses.

## Commands

From the repository root:

```bash
bash test/poc/least-privilege/setup.sh
bash test/poc/least-privilege/verify.sh
```

Standard cleanup retains PVCs and namespaces unless explicitly requested:

```bash
bash test/poc/least-privilege/cleanup.sh
```

Full destructive PoC cleanup:

```bash
DELETE_PVCS=true DELETE_NAMESPACES=true bash test/poc/least-privilege/cleanup.sh
```
