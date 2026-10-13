# Operator Credentials: Flow, Status, and Events

This guide explains how the Operator creates and uses the MarkLogic identity `marklogic-kubernetes-operator`, and how to troubleshoot its credential handoff.

## Credential and Role Model

The Operator maintains two separate MarkLogic identities:

- **Bootstrap admin**: used to initialize MarkLogic, call Admin API endpoints, create or repair the operator role and user, and recover the operator identity. Keep this Secret available after handoff.
- **Operator user**: used for Management API operations after the Operator verifies the credentials. Its fixed username is `marklogic-kubernetes-operator`.

The Operator first creates or uses the configured admin Secret. Unless `spec.auth.operatorSecretName` is set, it also creates `<MarklogicCluster-name>-operator` with `username` and `password` keys. The generated password is 32 characters. A user-provided operator Secret must contain a non-empty `password`; the Operator still uses the fixed username.

The MarkLogic role is named `marklogic-operator` and has description `Dedicated role for the MarkLogic Kubernetes Operator`. It is created if absent and updated if it already exists. The `marklogic-kubernetes-operator` user is likewise created or updated, assigned this role, and given the description `Dedicated service account for the MarkLogic Kubernetes Operator`.

The role inherits `manage-admin`, `pki`, and `admin-ui-user`. It also has these execute privileges:

| Privilege | Action URI |
| --- | --- |
| `create-user` | `http://marklogic.com/xdmp/privileges/create-user` |
| `xdmp:remove-dynamic-hosts` | `http://marklogic.com/xdmp/privileges/remove-dynamic-hosts` |
| `admin-issue-dynamic-host-token` | `http://marklogic.com/xdmp/privileges/admin/issue-dynamic-host-token` |
| `xdmp:eval` | `http://marklogic.com/xdmp/privileges/xdmp-eval` |
| `create-external-security` | `http://marklogic.com/xdmp/privileges/create-external-security` |

**Permission scope:** because the role inherits `manage-admin`, this role has broad Management API permissions; it is not a narrowly least-privileged role. The bootstrap admin Secret is also intentionally retained as a recovery path.

## Handoff Flow

1. The MarklogicCluster reconciliation creates or adopts the bootstrap admin Secret and creates the generated operator Secret unless a user-provided operator Secret is configured.
2. The MarkLogic pods start with the bootstrap admin identity. It is required for initial setup and `/admin/v1` operations, including initialization and joining a cluster. The operator user cannot replace admin for these bootstrap endpoints because it is provisioned only after MarkLogic security is initialized.
3. The MarklogicGroup controller reads the operator Secret and checks Management API access to the bootstrap host using `marklogic-kubernetes-operator`.
4. If operator authentication is rejected, the controller tries the retained bootstrap admin credentials. When the bootstrap host is online, admin credentials create or repair the `marklogic-operator` role and operator user. The controller requeues and verifies the operator login on a later reconciliation.
5. Once operator authentication succeeds and the bootstrap host is online, the controller records the active Secret in `.status.credentialSecretName` and sets the `OperatorCredentialsReady` condition to True.
6. After handoff, the operator Secret is used for intended `/manage/v2` operations, including dynamic group reconciliation. The admin Secret remains mounted and continues to be used for `/admin/v1`, identity repair, and recovery.

The `OperatorCredentialsReady` condition describes the credential handoff, not overall MarklogicGroup or StatefulSet readiness. A group without a MarklogicCluster owner is skipped by this credential reconciliation and will not receive this handoff condition.

## Condition and Event Reference

The condition is stored in `.status.conditions` with type `OperatorCredentialsReady`. Its `status`, `reason`, and `message` describe the latest observed credential state. A False condition causes reconciliation to retry after approximately five seconds. Successful activation records the active Secret name in `.status.credentialSecretName` and requeues promptly.

| Condition status | Reason | Event | Meaning and next check |
| --- | --- | --- | --- |
| False | `OperatorSecretUnavailable` | Warning | The operator Secret could not be read or has no password. Check `spec.auth.operatorSecretName` or the generated `<cluster>-operator` Secret, namespace, and `password` key. |
| False | `BootstrapHostNotReady` | Warning | The bootstrap host is not online. Check the MarkLogic pod, startup logs, and host readiness. |
| False | `BootstrapHostNotFound` | Warning | The Management API did not report the configured bootstrap host. Check `spec.bootstrapHost`, DNS/service naming, and MarkLogic cluster membership. |
| False | `OperatorManagementAPIFailed` | Warning | Operator credentials could not be validated due to a non-authentication Management API error. Check bootstrap-host availability, network access, TLS configuration, and the condition message. |
| False | `BootstrapAdminSecretUnavailable` | Warning | Operator authentication failed and the bootstrap admin Secret could not be read. Check the configured admin Secret and its `username` and `password` keys. |
| False | `BootstrapAdminAccessFailed` | Warning | Operator authentication failed and bootstrap admin could not access the Management API. Check admin credentials, host availability, and Management API access. |
| False | `OperatorIdentityReconciliationFailed` | Warning | Admin credentials could not create or repair the role/user. Check the admin Secret and MarkLogic Management API response in the condition message. |
| False | `OperatorIdentityReconciled` | Normal | Admin credentials repaired the identity. This is progress; the controller is waiting for the next reconciliation to verify operator authentication. If it persists, inspect the operator Secret, user password, and role assignment. |
| True | `OperatorCredentialsActive` | Normal | The operator user authenticated successfully and `.status.credentialSecretName` identifies the active Secret. |

Warning events are emitted for failure reasons. `OperatorIdentityReconciled` is a Normal progress event even though the condition remains False until authentication is verified. `OperatorCredentialsActive` is a Normal event. Events are emitted when the condition status or reason changes, not on every retry; the condition message may still be updated with newer details for the same reason.

## Troubleshooting Commands

Show the MarklogicGroup condition and recent events:

```sh
kubectl describe marklogicgroup <group-name> --namespace <namespace>
```

Inspect the complete status, including the active credential Secret name:

```sh
kubectl get marklogicgroup <group-name> --namespace <namespace> -o yaml
```

Check that the expected operator Secret exists and has the required keys without printing their values:

```sh
kubectl describe secret <cluster-name>-operator --namespace <namespace>
```

If using `spec.auth.operatorSecretName`, substitute that Secret's name. Keep the bootstrap admin Secret available; removing it eliminates the Operator's automatic identity-recovery path.
