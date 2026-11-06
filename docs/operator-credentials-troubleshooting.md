# Operator Credentials: Flow, Status, and Events

This guide explains how the Operator creates and uses the MarkLogic identity `marklogic-kubernetes-operator`, and how to troubleshoot its credential handoff.

## Credential and Role Model

The Operator maintains two separate MarkLogic identities:

- **Bootstrap admin**: used to initialize MarkLogic, call bootstrap Admin API endpoints, and initially create the operator role and user. Healthy reconciliation does not require this Secret.
- **Operator user**: used for Management API operations after the Operator verifies the credentials. Its fixed username is `marklogic-kubernetes-operator`.

The Operator first creates or uses the configured admin Secret. Unless `spec.auth.operatorSecretName` is set, it also creates `<MarklogicCluster-name>-operator` with a `password` key. The generated password is 32 characters. A user-provided operator Secret must contain a non-empty `password`; the Operator always uses the fixed username.

The MarkLogic role is named `marklogic-kubernetes-operator` and has description `Dedicated role for the MarkLogic Kubernetes Operator`. During initial provisioning, it is created if absent and updated if it already exists. The `marklogic-kubernetes-operator` user is likewise initially created or updated, assigned this role, and given the description `Dedicated service account for the MarkLogic Kubernetes Operator`. After credential handoff, the Operator does not reconcile direct changes to this user or role.

The role inherits `manage-admin`, `pki`, and `admin-ui-user`. It also has these execute privileges:

| Privilege | Action URI |
| --- | --- |
| `create-user` | `http://marklogic.com/xdmp/privileges/create-user` |
| `xdmp:remove-dynamic-hosts` | `http://marklogic.com/xdmp/privileges/remove-dynamic-hosts` |
| `admin-issue-dynamic-host-token` | `http://marklogic.com/xdmp/privileges/admin/issue-dynamic-host-token` |
| `xdmp:eval` | `http://marklogic.com/xdmp/privileges/xdmp-eval` |
| `create-external-security` | `http://marklogic.com/xdmp/privileges/create-external-security` |
| `credentials-set-aws` | `http://marklogic.com/xdmp/privileges/credentials-set-aws` |
| `credentials-set-azure` | `http://marklogic.com/xdmp/privileges/credentials-set-azure` |

**Permission scope:** because the role inherits `manage-admin`, this role has broad Management API permissions; it is not a narrowly least-privileged role. The bootstrap admin Secret is retained for MarkLogic initialization, not as a steady-state recovery credential.

## Credential Ownership Matrix

| Operation | Identity | Reason |
| --- | --- | --- |
| MarkLogic initialization, security initialization, and `/admin/v1` cluster joining | Bootstrap admin | These operations run before the operator user exists or require Admin API access. |
| Initial operator role and user creation | Bootstrap admin | Provisions the dedicated identity after MarkLogic security is available. |
| Healthy Management API reconciliation | Operator user | Avoids a steady-state dependency on the bootstrap admin Secret. |
| TLS certificate templates, certificate insertion, PKI eval, and App Server TLS settings after handoff | Operator user | Uses the inherited `pki`, `manage-admin`, and `xdmp:eval` permissions. |
| Dynamic-host status, group configuration, token issuance, initialization, membership, and removal | Operator user | Uses one operator-authenticated client for the complete dynamic lifecycle. |
| CA import, OAuth external-security creation, and OAuth App Server creation | Operator user | Uses `pki`, Management API permissions, and `create-external-security`. |
| Rejected active operator credentials | None | Reports `OperatorAuthenticationRejected`; align the Secret with the password configured in MarkLogic. |

## Trusted CA TLS

For named certificates, set `spec.tls.caSecretName` to a Secret containing the CA bundle under `cacert.pem`. The controller loads that bundle for operator-user validation and dynamic-host Management API calls, and MarkLogic pods use the same bundle for HTTPS calls. Certificate verification remains enabled on this path; an absent Secret or `cacert.pem` key blocks reconciliation with an explicit status message.

When TLS is enabled without `caSecretName`, the self-signed bootstrap compatibility flow continues to skip certificate verification until the generated CA can be discovered. Production named-certificate deployments should always provide `caSecretName`.

## Handoff Flow

1. The MarklogicCluster reconciliation creates or adopts the bootstrap admin Secret and creates the generated operator Secret unless a user-provided operator Secret is configured.
2. The MarkLogic pods start with the bootstrap admin identity. It is required for initial setup and `/admin/v1` operations, including initialization and joining a cluster. The operator user cannot replace admin for these bootstrap endpoints because it is provisioned only after MarkLogic security is initialized.
3. The MarklogicGroup controller reads only the `password` key from the operator Secret and checks Management API access to the bootstrap host using the fixed username `marklogic-kubernetes-operator`.
4. If initial operator authentication is rejected before handoff, the controller tries the bootstrap admin credentials. When the bootstrap host is online, admin credentials create the `marklogic-kubernetes-operator` role and operator user. Once credentials have been active, rejection does not trigger admin fallback or mutate the MarkLogic identity.
5. Once operator authentication succeeds and the bootstrap host is online, the controller records the active Secret in `.status.credentialSecretName` and sets the `OperatorCredentialsReady` condition to True.
6. After handoff, the operator Secret is used for intended `/manage/v2` operations, including dynamic group reconciliation. Healthy reconciliation does not require the admin Secret. The admin Secret remains available only for bootstrap `/admin/v1` initialization operations.

The `OperatorCredentialsReady` condition describes the credential handoff, not overall MarklogicGroup or StatefulSet readiness. A group without a MarklogicCluster owner is skipped by this credential reconciliation and will not receive this handoff condition.

## Condition and Event Reference

The condition is stored in `.status.conditions` with type `OperatorCredentialsReady`. Its `status`, `reason`, and `message` describe the latest observed credential state. A False condition causes reconciliation to retry after approximately five seconds. Successful activation records the active Secret name in `.status.credentialSecretName` and requeues promptly.

| Condition status | Reason | Event | Meaning and next check |
| --- | --- | --- | --- |
| False | `OperatorSecretUnavailable` | Warning | The operator Secret could not be read or has no password. Check `spec.auth.operatorSecretName` or the generated `<cluster>-operator` Secret, namespace, and `password` key. |
| False | `OperatorTLSConfigurationInvalid` | Warning | The configured trusted CA Secret could not be read or does not contain `cacert.pem`. Check `spec.tls.caSecretName` and the Secret data. |
| False | `BootstrapHostNotReady` | Warning | The bootstrap host is not online. Check the MarkLogic pod, startup logs, and host readiness. |
| False | `BootstrapHostNotFound` | Warning | The Management API did not report the configured bootstrap host. Check `spec.bootstrapHost`, DNS/service naming, and MarkLogic cluster membership. |
| False | `OperatorManagementAPIFailed` | Warning | Operator credentials could not be validated due to a non-authentication Management API error. Check bootstrap-host availability, network access, TLS configuration, and the condition message. |
| False | `OperatorAuthenticationRejected` | Warning | MarkLogic rejected credentials from the active operator Secret. The Operator will not modify the MarkLogic identity or use admin fallback; update the Secret to match the password configured in MarkLogic. |
| False | `BootstrapAdminSecretUnavailable` | Warning | Operator authentication failed and the bootstrap admin Secret could not be read. Check the configured admin Secret and its `username` and `password` keys. |
| False | `BootstrapAdminAccessFailed` | Warning | Operator authentication failed and bootstrap admin could not access the Management API. Check admin credentials, host availability, and Management API access. |
| False | `OperatorIdentityReconciliationFailed` | Warning | Admin credentials could not create the initial role/user. Check the admin Secret and MarkLogic Management API response in the condition message. |
| False | `OperatorIdentityReconciled` | Normal | Admin credentials created the initial identity. This is progress; the controller is waiting for the next reconciliation to verify operator authentication. |
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

If using `spec.auth.operatorSecretName`, substitute that Secret's name. Keep the bootstrap admin Secret available until initial operator identity provisioning completes.
