# MarkLogic Operator Least-Privilege PoC

This harness tests the MarkLogic-side role and user design from the `MarkLogic Operator Least Privilege` Confluence page against the current Rancher Desktop Kubernetes context. It supports MLE-32337 and keeps Kubernetes namespace scope separate from MarkLogic authorization.

## Preconditions

- Current kubeconfig context is `rancher-desktop`.
- Kubernetes 1.30 or later, Helm 3, `curl`, `jq`, and `openssl` are installed.
- Rancher Desktop has at least 6 GiB memory and 20 GiB free disk.
- A default dynamic StorageClass exists.
- Public images `progressofficial/marklogic-operator-kubernetes:1.3.1` and `progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6` are reachable.

The published operator and baseline MarkLogic images do not advertise `linux/arm64`. Setup builds the operator locally for arm64. The baseline MarkLogic image is pinned to its verified amd64 manifest digest and runs through Rancher Desktop's Rosetta/binfmt support, which must be enabled.

An in-place switch from this baseline to an ARM MarkLogic image is not supported because existing forest labels record the `x86_64` architecture. Use fresh persistent storage or a supported backup/restore migration when changing architecture.

The test creates namespaces `ml-lp-operator` and `ml-lp-test`. Change names through the environment variables defined in `common.sh` only when required.

## Run

From the repository root:

```bash
bash test/poc/least-privilege/setup.sh
bash test/poc/least-privilege/verify.sh
```

Verification replaces the MarkLogic pod using the operator credential and confirms that the bootstrap Secret remains available for recovery. The PoC never deletes the bootstrap admin Secret.

## Expected Role

The `marklogic-operator` role inherits `manage-admin`, `pki`, and `admin-ui-user`. Static joins require `admin-ui` for `POST /admin/v1/cluster-config`; MarkLogic silently omits a direct assignment of this protected privilege, so the built-in `admin-ui-user` role is required. The role also explicitly grants:

| Privilege | Action URI |
| --- | --- |
| `create-user` | `http://marklogic.com/xdmp/privileges/create-user` |
| `xdmp:remove-dynamic-hosts` | `http://marklogic.com/xdmp/privileges/remove-dynamic-hosts` |
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

Do not commit evidence without reviewing it for environment-specific or sensitive content.

## Limitations

- Rancher Desktop does not prove multi-node availability or failure-domain behavior.
- The base test does not invoke TLS, OAuth, or dynamic-host code paths.
- The current product code has a separate dynamic-host credential path. Validate that path independently before claiming complete removal of admin usage.
- The PoC provisions the MarkLogic role and user externally. Automatic `EnsureOperatorRole` and operator-user reconciliation remain follow-on product work.

## Cleanup

```bash
bash test/poc/least-privilege/cleanup.sh
DELETE_PVCS=true DELETE_NAMESPACES=true bash test/poc/least-privilege/cleanup.sh
```

PVC and namespace deletion require explicit flags. CRDs are retained and reported.
