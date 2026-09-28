# Least-Privilege PoC Results

## Environment

| Item | Value |
| --- | --- |
| Date | 2026-09-28T09:05:43Z |
| Git commit | `4923ee3a1b18641c043e584be20e34936d63f794` |
| Kubernetes context | `rancher-desktop` |
| Kubernetes version | Client `v1.35.0`, server `v1.35.0+k3s3` |
| Node architecture | `linux/arm64` |
| MarkLogic image digest | `sha256:5bbef4b49d737e5d9c1136bcd77ef52a9a3649a125cc4ff8ee1d908daefed56d` (`linux/amd64`, Rosetta) |
| Operator image | `marklogic-operator-kubernetes:ml-lp-arm64-readinessfix` |
| StorageClass | `local-path` (`rancher.io/local-path`) |
| Helm release | `ml-lp-operator`, chart `marklogic-operator-kubernetes-1.3.1` |
| Rancher Desktop | Apple Virtualization with Rosetta enabled |

## ARM Upgrade Attempt

On 2026-09-28, the live cluster image was changed to `progressofficial/marklogic-db:12.1.0-ubi9-rootless-arm-2.3.0` while retaining the existing PVC. The native ARM container started successfully as `aarch64`, but MarkLogic could not mount the forests created by the previous AMD64 image.

- Result: **Failed; cross-architecture in-place upgrade is not supported.**
- Error: `XDMP-LABELBADARCH: Bad architecture in label: x86_64 instead of aarch64`.
- Scope: all existing forests, including Security, Schemas, Documents, Modules, and App-Services.
- Data safety: the original PVC remained bound and unchanged.
- Recovery: the live cluster was rolled back to the pinned AMD64 digest.

## Acceptance Results

| MLE-32337 criterion | Result | Evidence |
| --- | --- | --- |
| PoC completed from the linked findings | Pass | `evidence/results.tsv` |
| Required operator role and user assignment verified | Pass | `evidence/marklogic-role-redacted.json`, `evidence/marklogic-user-redacted.json` |
| Correct least-privilege setup confirmed | Pass for static one-node and three-node operation | Positive API checks, denied escalation, pod replacement, operator-credential reconciliation, and two successful joins |
| Wiki findings used as source of truth | Pass with one required addition | `role.json`; static joins require the built-in `admin-ui-user` role |

## Three-Node Scale Result

The static group was increased from one replica to three while the live cluster referenced `least-privilege-operator`.

- Result: **Pass.** StatefulSet `node` reached 3/3 Ready and MarklogicGroup `node` reported `Ready=True`.
- MarkLogic's operator-authenticated `/manage/v2/hosts?view=status&format=json` response reported all three FQDNs.
- Both joining nodes logged `joined group Default` and `cluster configuration completed`; after replacement, both took the `host has already joined the cluster` path.
- All three pods mount only `least-privilege-operator`; `least-privilege-admin` remains retained and unmounted.
- All PVCs remained Bound with unchanged UIDs: `datadir-node-0` `d41d2ed7-b66a-4fce-bef8-7c8413cf78ad`, `datadir-node-1` `f74f56a5-c51f-4c66-bab4-d9751bc9a15c`, and `datadir-node-2` `94935f86-98e4-4fab-9c60-02dafbbb1bd3`.
- `POST /admin/v1/cluster-config` requires `admin-ui`. MarkLogic silently omitted a direct assignment of this protected privilege, so the custom role must inherit the built-in `admin-ui-user` role.
- The static readiness probe now accepts an authenticated HTTP response from port 7997 after the wrapper creates `/tmp/marklogic_ready`; previously, `curl -f` treated HTTP 401 as an unavailable server.

## Findings

- The operator ServiceAccount can manage pods in `ml-lp-test` but cannot list nodes or Secrets in `default`.
- The `marklogic-operator` role inherits `manage-admin`, `pki`, and `admin-ui-user` and contains all four wiki-defined execute privileges.
- The `least-privilege-operator` user carries `marklogic-operator` and does not directly carry `admin` or `security`.
- Management root, group, and host reads returned HTTP 200 with the operator identity.
- Creating a user assigned to `admin` was denied with HTTP 403.
- MarkLogic restarted successfully with a new pod UID while mounting only `least-privilege-operator`.
- The bootstrap Kubernetes Secret is retained for recovery and is not referenced by the reconciled StatefulSet.
- The MarkLogic pod is Ready after replacement using `least-privilege-operator`.
- Recent operator logs contain no authorization or authentication errors.
- The published amd64 MarkLogic image crashed under QEMU on Apple Silicon. Enabling Rancher Desktop Rosetta resolved the runtime issue; setup now checks this prerequisite.

## Limitations

- The three-node static join is validated locally; production HA and failure-domain behavior are not covered.
- TLS, OAuth, and dynamic-host results must be reported separately when not exercised.
- Kubernetes namespace RBAC and MarkLogic authorization are independent controls.
- The proof provisions the role and user externally; production reconciliation code for automatic role/user creation remains follow-on work.
