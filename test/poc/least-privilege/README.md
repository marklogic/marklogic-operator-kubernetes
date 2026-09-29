# MarkLogic Operator Least-Privilege PoC

This harness tests the MarkLogic-side role and user design from the `MarkLogic Operator Least Privilege` Confluence page against the current Rancher Desktop Kubernetes context. It supports MLE-32337 and keeps Kubernetes namespace scope separate from MarkLogic authorization.

## Preconditions

- Current kubeconfig context is `rancher-desktop`.
- Kubernetes 1.30 or later, Helm 3, `curl`, `jq`, and `openssl` are installed.
- Rancher Desktop has at least 6 GiB memory and 20 GiB free disk.
- A default dynamic StorageClass exists.
- Public image `progressofficial/marklogic-db:12.1.0-ubi9-rootless-2.3.0` is reachable.
- The Operator image is built locally from the current source.

The test creates namespaces `ml-lp-operator` and `ml-lp-test`. Change names through the environment variables defined in `common.sh` only when required.

## Run

From the repository root:

```bash
bash test/poc/least-privilege/setup.sh
bash test/poc/least-privilege/verify.sh
bash test/poc/least-privilege/verify-tls-dynamic.sh
```

Verification replaces the static MarkLogic pods using the operator credential, validates self-signed TLS, scales the ephemeral dynamic group from one to two replicas and back to one, and confirms that the bootstrap Secret remains available for recovery. The PoC never deletes the bootstrap admin Secret.

See [TLS-DYNAMIC-POC.md](TLS-DYNAMIC-POC.md) for the complete TLS and dynamic-node design, defects, fixes, execution flow, and validation results.

## Expected Role

The `marklogic-operator` role inherits `manage-admin`, `pki`, and `admin-ui-user`. Static joins require `admin-ui` for `POST /admin/v1/cluster-config`; MarkLogic silently omits a direct assignment of this protected privilege, so the built-in `admin-ui-user` role is required. The role also explicitly grants:

| Privilege | Action URI |
| --- | --- |
| `create-user` | `http://marklogic.com/xdmp/privileges/create-user` |
| `xdmp:remove-dynamic-hosts` | `http://marklogic.com/xdmp/privileges/remove-dynamic-hosts` |
| `admin-issue-dynamic-host-token` | `http://marklogic.com/xdmp/privileges/admin/issue-dynamic-host-token` |
| `xdmp:eval` | `http://marklogic.com/xdmp/privileges/xdmp-eval` |
| `create-external-security` | `http://marklogic.com/xdmp/privileges/create-external-security` |

The operator user must not directly carry `admin` or `security`. The verification also attempts to create an admin-role user and requires MarkLogic to reject that escalation.

## Evidence

The verifier writes only redacted evidence under `test/poc/least-privilege/evidence/`:

- `results.tsv`: pass/fail matrix.
- `marklogic-role-redacted.json`: effective operator role definition.
- `marklogic-user-redacted.json`: effective operator user assignment.
- `cluster-redacted.yaml`: resulting custom resource.
- `kubernetes-rbac.yaml`: namespaced Role and RoleBinding inventory.
- `cluster-rbac-exceptions.yaml`: cluster-scoped exceptions, normally the read-only StorageClass role.
- `operator-redacted.log`: recent controller log with credential-related values redacted.
- `tls-dynamic-results.tsv`: TLS and dynamic-host pass/fail matrix.
- `dynamic-group-redacted.json`: final one-replica dynamic lifecycle status.
- `dynamic-events.txt`: dynamic-group Kubernetes events.

Do not commit evidence without reviewing it for environment-specific or sensitive content.

## Limitations

- Rancher Desktop does not prove multi-node availability or failure-domain behavior.
- OAuth is not covered by this PoC.
- The PoC provisions the MarkLogic role and user externally. Automatic `EnsureOperatorRole` and operator-user reconciliation remain follow-on product work.

## Cleanup

```bash
bash test/poc/least-privilege/cleanup.sh
DELETE_PVCS=true DELETE_NAMESPACES=true bash test/poc/least-privilege/cleanup.sh
```

PVC and namespace deletion require explicit flags. CRDs are retained and reported.
