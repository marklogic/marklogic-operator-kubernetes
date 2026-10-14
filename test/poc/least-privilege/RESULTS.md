# Least-Privilege PoC Results

## Environment

| Item | Value |
| --- | --- |
| Date | 2026-09-29 |
| Kubernetes context | `rancher-desktop` |
| Kubernetes version | Client `v1.35.0`, server `v1.35.0+k3s3` |
| MarkLogic image | `progressofficial/marklogic-db:12.1.0-ubi9-rootless-2.3.0` |
| Operator image | `marklogic-operator-kubernetes:ml-lp-tls-dynamic-fix-v4` |
| StorageClass | `local-path` (`rancher.io/local-path`) |
| Helm release | `ml-lp-operator`, chart `marklogic-operator-kubernetes-1.3.1` |

## Acceptance Results

| MLE-32337 criterion | Result | Evidence |
| --- | --- | --- |
| PoC completed from the linked findings | Pass | `evidence/results.tsv` |
| Required operator role and user assignment verified | Pass | `evidence/marklogic-role-redacted.json`, `evidence/marklogic-user-redacted.json` |
| Correct least-privilege setup confirmed | Pass for static and dynamic operation with TLS | Positive API checks, denied escalation, pod replacement, token-based dynamic joins, scale-up, and scale-down |
| Wiki findings used as source of truth | Pass with one required addition | `role.json`; static joins require the built-in `admin-ui-user` role |

## Three-Node Scale Result

The static group was increased from one replica to three while the live cluster referenced `least-privilege-operator`.

- Result: **Pass.** StatefulSet `node` reached 3/3 Ready and MarklogicGroup `node` reported `Ready=True`.
- MarkLogic's operator-authenticated `/manage/v2/hosts?view=status&format=json` response reported all three FQDNs.
- Both joining nodes logged `joined group Default` and `cluster configuration completed`; after replacement, both took the `host has already joined the cluster` path.
- All three pods mount only `least-privilege-operator`; `least-privilege-admin` remains retained and unmounted.
- All PVCs remained Bound with unchanged UIDs: `datadir-node-0` `d40bae77-113f-4a46-b54d-5e71076e6aba`, `datadir-node-1` `b0d23c81-6517-432f-99c4-4fa6a4e9e2c6`, and `datadir-node-2` `565915e4-e770-469c-9acf-8823be4f578f`.
- `POST /admin/v1/cluster-config` requires `admin-ui`. MarkLogic silently omitted a direct assignment of this protected privilege, so the custom role must inherit the built-in `admin-ui-user` role.
- The static readiness probe now accepts an authenticated HTTP response from port 7997 after the wrapper creates `/tmp/marklogic_ready`; previously, `curl -f` treated HTTP 401 as an unavailable server.

## TLS and Dynamic Result

- Self-signed TLS is enabled on the default application servers. Authenticated HTTPS returned 200 and plaintext HTTP returned 403.
- Dynamic group `dynamic` reached `Idle` with one joined host and no PVC.
- Scale-up reached `Idle/2`; scale-down removed `dynamic-1` and returned to `Idle/1`.
- Dynamic lifecycle operations completed while the live cluster and all workload templates referenced `least-privilege-operator`.
- MarkLogic 12.1 requires `admin-issue-dynamic-host-token` in addition to the previously defined role permissions.
- Full evidence is recorded in `evidence/tls-dynamic-results.tsv`, `evidence/dynamic-group-redacted.json`, and `evidence/dynamic-events.txt`.

## Findings

- The operator ServiceAccount can manage pods in `ml-lp-test` but cannot list nodes or Secrets in `default`.
- The `marklogic-operator` role inherits `manage-admin`, `pki`, and `admin-ui-user` and contains the five verified execute privileges.
- The `least-privilege-operator` user carries `marklogic-operator` and does not directly carry `admin` or `security`.
- Management root, group, and host reads returned HTTP 200 with the operator identity.
- Creating a user assigned to `admin` was denied with HTTP 403.
- MarkLogic restarted successfully with a new pod UID while mounting only `least-privilege-operator`.
- The bootstrap Kubernetes Secret is retained for recovery and is not referenced by the reconciled StatefulSet.
- The MarkLogic pod is Ready after replacement using `least-privilege-operator`.
- Recent operator logs contain no authorization or authentication errors.

## Limitations

- The three-node static join is validated locally; production HA and failure-domain behavior are not covered.
- OAuth remains untested.
- Kubernetes namespace RBAC and MarkLogic authorization are independent controls.
- The proof provisions the role and user externally; production reconciliation code for automatic role/user creation remains follow-on work.
