# Functional Spec: MarkLogic Operator Dynamic Host Scaling

## Introduction

### Overview

This specification defines the requirements, API contract, and controller workflow for enabling MarkLogic dynamic host scaling in the MarkLogic Operator for Kubernetes.

The feature is modeled as an extension of the existing `markLogicGroups` API, not as a separate top-level control plane. A dynamic host pool is declared as a `markLogicGroups[]` entry with `isDynamic: true`. The `MarklogicCluster` controller continues to fan out cluster intent into child `MarklogicGroup` resources, and the `MarklogicGroup` controller owns the lifecycle of the corresponding StatefulSet, Services, Pods, and dynamic host status.

This design preserves the current operator architecture while automating the MarkLogic Dynamic Host Token lifecycle: group configuration, token issuance, host join, host removal, and restart recovery.

### Goals

1.  Allow users to scale evaluator-only host pools up and down by changing `markLogicGroups[i].replicas`.
2.  Automate the MarkLogic dynamic host join and remove lifecycle using the Dynamic Host Token API.
3.  Preserve the existing `MarklogicCluster -> MarklogicGroup -> workload` ownership model.
4.  Provide a clear, queryable status model for each dynamic group.
5.  Recover correctly when operator restarts or MarkLogic restarts clear dynamic host membership.
6.  Ensure dynamic evaluator capacity can participate in HAProxy traffic distribution when HAProxy is enabled and the group is configured for HAProxy via the existing per-group `haproxy` field (`HAProxyGroup`).
7.  Leave room for multiple dynamic pools and future autoscaling extensions without introducing a second API model.

### Compatibility

Dynamic host support requires a MarkLogic Server image whose version implements the Dynamic Host Token API (`POST /manage/v2/clusters/{cluster-name}/dynamic-host-token` and `DELETE /manage/v2/clusters/{cluster-name}/dynamic-hosts`). The minimum supported MarkLogic version for dynamic groups is **MarkLogic 12.0**. Operators should refuse to mark `status.dynamic.dynamicHostsEnabled=true` if the bootstrap cluster reports a lower version (transition to `Failed` with `InvalidConfiguration`; the `message` identifies the version mismatch).

## Background: MarkLogic Dynamic Hosts

### Dynamic Host Concept

MarkLogic supports a dynamic host mode where evaluator-only hosts can be added to and removed from a cluster at runtime using a time-limited token.

A dynamic host is:

1.  An evaluator-only node (e-node). It does not host forests or act as a persistent data host. Local host state may be ephemeral (`EmptyDir`) or retained on a PVC, but that retained state must not be used for forest-bearing persistence.
2.  Joined to a MarkLogic group that has `allow-dynamic-hosts` enabled.
3.  Joined using a time-limited dynamic host token issued by the cluster.
4.  Permanently removable through the Management API.
5.  Permanently removed from the cluster when the MarkLogic cluster is restarted.

Dynamic hosts do not participate in quorum and are intended for burst evaluation capacity. They can be temporarily disabled and later re-enabled until they are permanently removed.

### Dynamic Host Token Lifecycle

For the operator-managed path, the dynamic host join flow has three phases after the dynamic host Pod is created and MarkLogic is started locally:

1.  **Enable dynamic hosts on the target group.** The bootstrap cluster must have `allow-dynamic-hosts: true` enabled on the target group, and API token authentication enabled on the Admin App Server configuration used by the join host in that cluster.
2.  **Obtain a token.** The operator requests a time-limited token from the bootstrap cluster's Management API for a specific host FQDN and group.
3.  **Join using the token.** The joining host receives the token on its own `/admin/v1/init` endpoint, and MarkLogic completes the join server-side.

This condenses the full product procedure documented by MarkLogic. In the product documentation, MarkLogic Server must already be installed on each dynamic host machine before token generation and join; in the Kubernetes operator design, that prerequisite is represented by the container image and Pod startup.

### Management API Calls

The following REST calls form the dynamic host lifecycle. In production, the operator authenticates with the credentials referenced by `spec.auth`.


#### Enable Dynamic Hosts on a Group

```http
PUT /manage/v2/groups/{group-name}/properties
Content-Type: application/json

{"allow-dynamic-hosts": true}
```
Returns: `204 No Content` if successful.


#### Enable API Token Authentication on Admin App Server

```http
PUT /manage/v2/servers/Admin/properties?group-id={group-name}
Content-Type: application/json

{"API-token-authentication": true}
```
Returns: `204 No Content` if successful.

The official product guidance also requires that the join host be in the same cluster as the group enabled for dynamic hosts.

#### Obtain a Dynamic Host Token

```http
POST /manage/v2/clusters/{cluster-name}/dynamic-host-token
Content-Type: application/json

{
  "dynamic-host-token": {
    "group": "{group-name}",
    "host": "{joining-host-fqdn}",
    "port": 8001,
    "duration": "PT15M",
    "comment": "operator-managed"
  }
}
```

Returns: `201 Created` with the token in the response body.

#### Join a Dynamic Host

Executed on the joining host, not the bootstrap cluster:

```http
POST http://{joining-host}:8001/admin/v1/init
Content-Type: application/xml

<init xmlns="http://marklogic.com/manage">
  <dynamic-host-token>{token}</dynamic-host-token>
</init>
```

#### Remove Dynamic Hosts

Executed on the bootstrap cluster:

```http
DELETE /manage/v2/clusters/{cluster-name}/dynamic-hosts
Content-Type: application/xml

<dynamic-hosts>
  <dynamic-host>{host-id-1}</dynamic-host>
  <dynamic-host>{host-id-2}</dynamic-host>
</dynamic-hosts>
```

### Authentication and Least-Privilege Model

The operator has two different authentication concerns, and they should not be treated as requiring the same privilege level.

1.  **Static bootstrap and static host join remain admin-oriented.** The existing static workflow still talks directly to the Admin API on port `8001`, including `/admin/v1/instance-admin` for bootstrap security initialization and `/admin/v1/cluster-config` for static host join. That path continues to require the MarkLogic `admin` user.
2.  **Dynamic host join is different.** The joining dynamic host uses a time-limited token on its own `/admin/v1/init` endpoint. That join step does not need an admin user on the joining host; the privileged operations happen on the bootstrap cluster when the operator enables dynamic hosts, enables API token authentication, issues the token, and removes dynamic hosts.
3.  **Dynamic-host reconciliation must therefore use the `manage-admin` role instead of `admin`.** The Management API operations used by this feature (`PUT /manage/v2/groups/{name}/properties`, `PUT /manage/v2/servers/Admin/properties`, `POST /manage/v2/clusters/{name}/dynamic-host-token`, and `DELETE /manage/v2/clusters/{name}/dynamic-hosts`) are documented by MarkLogic as accepting the `manage-admin` role rather than requiring the full `admin` role.
4.  **Least-privilege requirement for this feature.** The operator should create and use a dedicated MarkLogic user for dynamic-host reconciliation whose role is `manage-admin`. Reusing the bootstrap admin user for steady-state dynamic-host reconciliation does not satisfy the least-privilege goal of this feature.
5.  **Credential split by purpose is the design target.** Static bootstrap and static host join continue to use the admin-oriented credential, while dynamic-host reconciliation should use a separate `manage-admin` credential. The exact implementation can be either operator-managed user creation or a dedicated secret supplied for the dynamic-host path, but the specification requirement is that dynamic-host operations do not run as admin.

### Kubernetes and Operator Model

Dynamic hosts are still represented as operator-managed groups.

1.  A dynamic pool is declared as a `markLogicGroups[]` entry.
2.  The `MarklogicCluster` controller creates or updates a child `MarklogicGroup` resource for that entry.
3.  The `MarklogicGroup` controller owns the StatefulSet, Services, Pod-level cleanup, and dynamic host status.
4.  Dynamic groups use a StatefulSet so hostnames remain deterministic. By default they do not use the standard MarkLogic data PVC, but users may explicitly enable PVC-backed datadir persistence for a dynamic group. Auxiliary PVCs may also be attached for non-forest purposes such as audit or log retention.

Dynamic host Pods, the StatefulSet, and the headless Service must carry the stable label `app.kubernetes.io/component=dynamic-host` so operations, tests, and dashboards can target them consistently.
For dynamic groups, this label must be used in the StatefulSet selector from first creation. The existing static-group selector value `app.kubernetes.io/component=database` is not valid for dynamic groups and cannot be corrected in place after a StatefulSet is created.

## Requirements

### Functional Requirements

#### Dynamic Group Declaration

Users must be able to declare a dynamic evaluator-only pool through the existing `markLogicGroups` array.

Acceptance criteria:

1.  The operator supports `markLogicGroups[].isDynamic: true`.
2.  A dynamic group is reconciled into a dedicated child `MarklogicGroup` resource, StatefulSet, and Services.
3.  A dynamic group uses `EmptyDir` for the main MarkLogic datadir by default, and the operator must not inherit cluster-level persistence defaults for that path when the group does not set `persistence.enabled` explicitly.
4.  A user may explicitly set `persistence.enabled: true` on a dynamic group to back the main MarkLogic datadir with a PVC.
5.  Multiple dynamic groups are allowed in the same cluster, as long as each group name is unique.

#### HAProxy Integration

When HAProxy is enabled, dynamic groups should be able to receive the traffic classes that benefit from burst evaluator capacity.

Acceptance criteria:

1.  If cluster-level HAProxy is enabled and a dynamic group is configured to participate in HAProxy, the generated HAProxy backend configuration includes that group's Pods for the HAProxy app server or TCP ports resolved for that group.
2.  In v1, the primary HAProxy use case for dynamic groups is evaluator or application traffic, such as App-Services or other query-facing app servers that can benefit from burst capacity.
3.  Admin and Manage traffic should continue to target static groups by default. Dynamic groups are not required to participate in those control-plane endpoints in v1.
4.  Excluding a dynamic group from HAProxy must not affect that group's dynamic-host join, remove, or restart-recovery lifecycle.

#### Scale-Up Workflow

The operator must automate adding new dynamic hosts to the MarkLogic cluster.

Acceptance criteria:

1.  When a dynamic group's `replicas` increases, the operator scales that group's StatefulSet.
2.  For each new Pod that reaches `Running`, the operator obtains a token from the bootstrap host and joins the Pod to the MarkLogic cluster.
3.  The operator reports per-host join status on the dynamic `MarklogicGroup` resource.
4.  A failed join does not block other new hosts from joining.

#### Scale-Down Workflow

The operator must automate safe removal of dynamic hosts from the MarkLogic cluster.

Acceptance criteria:

1.  When a dynamic group's `replicas` decreases, the operator evaluates the highest-ordinal hosts and applies storage-mode-specific scale-down behavior before scaling down the StatefulSet.
2.  For dynamic hosts using the default `EmptyDir` datadir, the operator calls the MarkLogic Dynamic Host Remove API before Pod termination.
3.  For dynamic hosts using PVC-backed datadir persistence, the operator does not deregister the host during ordinary replica scale-down.
4.  The operator reports per-host removal or retention status on the dynamic `MarklogicGroup` resource.
5.  For `EmptyDir`-backed hosts, Pod termination only occurs after successful deregistration. If deregistration cannot be completed, the group remains `Degraded` with `reason=RemoveFailed` and the StatefulSet is not scaled down further.

#### Scale to Zero

Dynamic groups must support scaling to zero replicas.

Acceptance criteria:

1.  Setting a dynamic group's `replicas: 0` scales the group's StatefulSet to zero. For `EmptyDir`-backed dynamic hosts, the operator removes all dynamic hosts from MarkLogic first. For PVC-backed dynamic hosts, ordinary scale-to-zero preserves the registered hosts.
2.  At least one static bootstrap group must exist in the cluster spec for any dynamic group to be valid.
3.  At reconcile time, scale-up is rejected if no static bootstrap group with at least one running replica is available.

#### Restart Recovery

The operator must recover when MarkLogic restart behavior clears dynamic membership.

Acceptance criteria:

1.  If a MarkLogic cluster restart removes dynamic hosts from membership, the operator detects the mismatch between desired replicas, Pods, and registered dynamic hosts.
2.  For `EmptyDir`-backed dynamic hosts, the operator reissues tokens and rejoins the existing Pods instead of requiring users to recreate the group.
3.  For PVC-backed dynamic hosts, the operator clears the retained dynamic-host datadir state before rejoin so the Pod can join as a fresh dynamic host again.
4.  When clearing PVC-backed dynamic-host state for restart recovery, the operator should preserve the log folder if possible, but successful rejoin takes precedence over log retention.
5.  Restart recovery is safe after operator restarts and MarkLogic restarts.

#### Status and Observability

The operator must provide transparent, user-visible progress reporting.

Acceptance criteria:

1.  Each dynamic `MarklogicGroup` exposes the current scaling phase.
2.  Each dynamic `MarklogicGroup` exposes per-host join and remove state.
3.  Kubernetes events are emitted for major lifecycle transitions and errors.
4.  Controller logs record each state transition with structured fields.

### Non-Functional Requirements

#### Data Integrity

1.  Dynamic hosts must never host forests. When `persistence.enabled=true`, the PVC may retain local MarkLogic host state, but the host still must not be treated as a persistent forest-bearing data host.
2.  Scale-down must follow the resolved datadir storage mode: `EmptyDir`-backed hosts are deregistered before Pod termination, while PVC-backed hosts are retained in MarkLogic during ordinary replica scale-down.
3.  Dynamic group logic must not modify or delete resources belonging to static groups.

#### Availability

1.  Scale-up and scale-down of dynamic groups must not disrupt the static MarkLogic cluster.
2.  A failed or partially completed scaling operation must not leave the cluster in an inconsistent state.
3.  The bootstrap cluster's health must be verified before attempting scale-up or restart recovery.

#### Platform Compatibility

1.  The feature supports any Kubernetes distribution supported by the operator.
2.  No storage class or CSI driver requirements are introduced for the default dynamic-host path, because dynamic groups use `EmptyDir` unless `persistence.enabled=true` is explicitly set. If main datadir persistence or auxiliary PVCs are configured, the usual storage class and CSI driver requirements apply to those volumes.

### Scope and Non-Goals

#### In Scope

1.  Declarative dynamic group management through `spec.markLogicGroups[]`.
2.  Token-based join and API-based remove lifecycle.
3.  Per-group status, events, and logs for observability.
4.  Recovery from interrupted scaling and from MarkLogic restart removal of dynamic hosts.
5.  Multiple dynamic pools in the same cluster.
6.  Optional PVC-backed persistence for the main dynamic-host datadir when explicitly configured.
7.  Optional auxiliary PVCs for non-forest purposes such as audit or log retention when explicitly configured.

#### Out of Scope

1.  Schedule-based autoscaling in v1.
2.  Metrics-based autoscaling in v1.
3.  Dynamic hosts with persistent forests.
4.  Making the bootstrap group itself dynamic.
5.  Replacing the existing static group initialization workflow.

## API and User Experience Contract

### Trigger Model

Users manage dynamic capacity by editing entries in `spec.markLogicGroups`.

Static groups keep their current behavior. A group becomes dynamic when `isDynamic: true` is set. Users scale a dynamic group by changing that group's `replicas` field. Users remove a dynamic group entirely by removing the entry from `markLogicGroups` after scaling it to zero, or by allowing the operator to scale it to zero as part of deletion handling.

`MarklogicCluster` remains the user-authored source of truth. Child `MarklogicGroup` resources are controller-managed projections of that intent. Users should not edit propagated dynamic-group fields on child resources directly, because the `MarklogicCluster` controller may overwrite them on the next reconcile.

Example:

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: my-cluster
spec:
  auth:
    secretName: ml-admin-auth
  markLogicGroups:
    - name: dnode
      replicas: 3
      isBootstrap: true
      groupConfig:
        name: Default

    - name: enode-burst
      replicas: 2
      isDynamic: true
      groupConfig:
        name: DynamicEval
      persistence:
        enabled: false
      resources:
        requests:
          cpu: "1"
          memory: "4Gi"
        limits:
          cpu: "2"
          memory: "8Gi"
      dynamic:
        tokenDuration: "PT15M"
```

### Spec Fields

#### Additions to `markLogicGroups[]`

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `isDynamic` | boolean | No | Declares the group as a MarkLogic dynamic host pool | `false` |
| `dynamic` | DynamicGroupConfig | No | Dynamic-host-specific settings. May be omitted when `isDynamic=true`; defaults apply. Must not be set when `isDynamic=false` | defaults applied |

#### Additions to `MarklogicGroup.spec` (controller-populated child resource)

| Field | Type | Required | Description | Population |
|---|---|---|---|---|
| `isDynamic` | boolean | No | Mirrors the resolved dynamic/static mode of the parent group entry | Set by the `MarklogicCluster` controller for dynamic groups |
| `dynamic` | DynamicGroupConfig | No | Mirrors the resolved dynamic-host settings used by the child controller | Set by the `MarklogicCluster` controller for dynamic groups |

#### `markLogicGroups[].dynamic`

| Field | Type | Required | Description | Default |
|---|---|---|---|---|
| `tokenDuration` | string | No | ISO 8601 duration used when requesting dynamic host tokens | `"PT15M"` |

#### Reused Existing Fields

The dynamic-host workflow does not introduce new fields for the following concerns; the existing group-level fields are reused unchanged:

| Concern | Existing field | Notes |
|---|---|---|
| Bootstrap host FQDN | `MarklogicGroup.spec.bootstrapHost` | Populated by the `MarklogicCluster` controller from the bootstrap group's headless service. |
| Kubernetes cluster DNS domain | `MarklogicGroup.spec.clusterDomain` | Defaults to `cluster.local`. Used to compute Pod FQDNs for token requests. |
| StatefulSet update strategy | `MarklogicGroup.spec.updateStrategy` | Cluster controller sets this to `RollingUpdate` for dynamic groups; users do not set it through `markLogicGroups[]`. |
| Main datadir persistence | `markLogicGroups[].persistence` | Reused unchanged. If omitted on a dynamic group, the operator defaults it to `enabled=false` and uses `EmptyDir` for `/var/opt/MarkLogic`. If explicitly set to `enabled=true`, the dynamic host uses PVC-backed datadir storage and ordinary scale-down retains the registered host. |
| HAProxy participation | `markLogicGroups[].haproxy` (`HAProxyGroup`) | Existing per-group HAProxy override. Dynamic groups opt into HAProxy traffic by setting `haproxy.enabled=true` and listing the relevant `appServers` / `tcpPorts`. |
| TLS | `markLogicGroups[].tls` | Reused unchanged. See [TLS](#tls-and-management-api-transport) for transport implications. |
| Auxiliary PVCs and mounts | `markLogicGroups[].additionalVolumeClaimTemplates` and `markLogicGroups[].additionalVolumeMounts` | Existing fields may be used on dynamic groups for auxiliary storage such as audit or log retention. They must not be used to persist forests or replace the standard MarkLogic datadir persistence model. |

### Effective Behavior Rules

Dynamic groups reuse the existing group schema for image, resources, affinity, node selection, probes, TLS, and log collection. The operator applies the following mode-specific behavior:

1.  `replicas` means desired dynamic host count and may be `0`.
2.  Dynamic groups use a StatefulSet and headless Service like static groups.
3.  If `markLogicGroups[].persistence` is omitted, the main MarkLogic datadir defaults to `EmptyDir` for dynamic groups. Dynamic groups do not inherit cluster-level persistent storage defaults for `/var/opt/MarkLogic`.
4.  If a user explicitly sets `markLogicGroups[].persistence.enabled=true` on a dynamic group, the operator uses PVC-backed storage for `/var/opt/MarkLogic` on that group.
5.  The `MarklogicCluster` controller must default the child `MarklogicGroup` to `persistence.enabled=false` only when the dynamic group did not specify persistence explicitly. If the user sets `persistence.enabled=true`, the controller preserves that choice on the child resource. Auxiliary volumes or `additionalVolumeClaimTemplates` for non-forest uses such as audit or log retention remain allowed.
6.  Dynamic groups use the dynamic-host join and remove lifecycle instead of the existing static cluster join flow. For ordinary replica scale-down, `EmptyDir`-backed hosts are deregistered from MarkLogic, while PVC-backed hosts are retained.
7.  Dynamic groups default to `RollingUpdate` strategy. Since dynamic hosts are stateless and ephemeral by default, and PVC-backed dynamic hosts still remain evaluator-only, `RollingUpdate` allows the operator to apply image or resource changes without requiring manual pod deletion.
8.  The `MarklogicCluster` controller writes `RollingUpdate` into the existing `MarklogicGroup.spec.updateStrategy` field when `isDynamic=true`. No new user-facing `updateStrategy` field is added to `markLogicGroups[]` in v1, and overriding `updateStrategy` for a dynamic group by editing the child `MarklogicGroup` is unsupported because that child field is controller-managed.

### Validation Rules

1.  `isDynamic=true` requires `isBootstrap=false`.
2.  `dynamic` may be omitted when `isDynamic=true`; if omitted, defaults apply. If `dynamic` is present, `isDynamic` must be `true`.
3.  `dynamic.tokenDuration` must be a valid ISO 8601 duration string.
4.  `isDynamic` is immutable after a group is created.
5.  A dynamic group's `groupConfig.name` must remain immutable while that group has registered dynamic hosts.
6.  When `isDynamic=true`, `persistence.enabled` may be `false` or `true`. If omitted, it defaults to `false` for that dynamic group.
7.  `isDynamic=true` allows `additionalVolumeClaimTemplates` only for auxiliary storage such as audit or log retention; they must not be used to persist forests or replace the standard MarkLogic datadir persistence model beyond the explicit `persistence` setting for `/var/opt/MarkLogic`.
8.  At least one static bootstrap group must exist in the cluster spec when any dynamic group is defined.
9.  Group names and `groupConfig.name` values must remain unique across static and dynamic groups.

### Validation Strategy

Validation rules above are enforced through two mechanisms in v1:

1.  **CEL rules on the CRD** for structural and field-level invariants that fit CRD validation cleanly. In v1 this includes rules such as `dynamic` requiring `isDynamic=true`, `tokenDuration` being a valid ISO 8601 duration, and any cross-entry checks that can be expressed maintainably with the current CEL-based CRD validation model.
2.  **Reconcile-time validation in the controllers** for checks that either depend on live bootstrap-cluster state or are not enforced robustly by the current CEL rules. The `MarklogicCluster` controller is responsible for surfacing any remaining cross-entry configuration violations that are not blocked by CEL. The `MarklogicGroup` controller is responsible for checks that depend on live bootstrap state or multi-field storage semantics, including bootstrap reachability, bootstrap health, bootstrap MarkLogic version compatibility, and ensuring any auxiliary PVCs or mounts on a dynamic group are not used to persist forests or replace the standard MarkLogic datadir.

No validating admission webhook is part of v1 for this feature. As a result, invalid specifications that are not rejected by CEL are not blocked at create or update time. Instead, the responsible controller surfaces the violation through `status` and events and transitions the affected group to `Failed` with `InvalidConfiguration` (the `message` field identifies the specific rule that was violated, e.g., missing bootstrap group or reuse of a forest-bearing MarkLogic group).

## Status Contract

### Source of Truth

Detailed dynamic host lifecycle state is reported on the child `MarklogicGroup` resource, not on `MarklogicCluster`.

Rationale:

1.  The `MarklogicGroup` controller owns the StatefulSet, Services, Pods, and pod-level cleanup.
2.  The current operator already treats group resources as the ownership boundary for workload status.
3.  Multiple dynamic groups require independent status, not one cluster-wide singleton status object.

The cluster resource may mirror high-level aggregate health into `status.conditions`, but `MarklogicGroup.status.dynamic` is authoritative.

For static groups, the existing `status.conditions`, `status.stage`, and `status.markLogicGroupStatus` fields continue to behave as they do today. For dynamic groups, `status.dynamic` is the authoritative lifecycle state. The legacy `stage` and `markLogicGroupStatus` fields may still be populated for compatibility with existing tooling, but they are not the source of truth for dynamic scaling progress.

### `MarklogicGroup.status.dynamic`

| Field | Type | Required | Description |
|---|---|---|---|
| `phase` | enum | Yes | Current lifecycle phase |
| `desiredReplicas` | integer | Yes | Desired number of dynamic hosts from spec |
| `localReadyReplicas` | integer | Yes | Number of Pods passing the configured readiness probe (locally ready, may or may not be joined) |
| `readyReplicas` | integer | Yes | Number of hosts that are both locally ready and registered in MarkLogic. Always `<= localReadyReplicas`. |
| `message` | string | No | Human-readable summary |
| `reason` | enum | No | Machine-readable reason for degraded or failed state |
| `dynamicHostsEnabled` | boolean | Yes | Whether dynamic-host settings have been applied to the MarkLogic group (`allow-dynamic-hosts` and API token authentication) |
| `hosts` | []HostStatus | No | Per-host join and remove state |
| `lastTransitionTime` | timestamp | No | Time of most recent phase change |

### `HostStatus`

| Field | Type | Description |
|---|---|---|
| `podName` | string | Kubernetes Pod name |
| `hostname` | string | FQDN registered in MarkLogic |
| `hostId` | string | MarkLogic host ID used for removal |
| `state` | enum | `Pending`, `Joining`, `Joined`, `Retained`, `Removing`, `Removed`, `Failed` |
| `message` | string | Human-readable detail |
| `lastUpdated` | timestamp | Time of last state change |

### Phase Values

The phase is a coarse lifecycle indicator. Finer-grained context (scale direction, restart recovery, specific blocking condition) is carried in `reason`, `message`, and per-host `hosts[].state`.

| Value | Meaning |
|---|---|
| `Pending` | Initial phase before the first reconcile completes, or while the operator is preparing the group for dynamic hosts (waiting for bootstrap readiness or applying group configuration) |
| `Reconciling` | The operator is actively converging the group toward desired state: joining new hosts, deregistering excess hosts, or rejoining Pods after a MarkLogic restart. The specific sub-activity is surfaced through `message`, `reason`, and `hosts[].state` |
| `Idle` | Group is at desired replica count and all required hosts are joined |
| `Degraded` | Group is not at desired state and either the operation is blocked on a transient condition (e.g., bootstrap unreachable, remove API failing) or one or more hosts have permanently failed while others are healthy. The `reason` field distinguishes the specific cause |
| `Failed` | The operation requires manual intervention (permanent error, retry budget exhausted, unsupported configuration) |
| `Deleting` | The dynamic group is being deleted; scale-to-zero / deregistration is in progress before finalizer removal |

### Reason Values

The `reason` enum carries the actionable category of a non-`Idle` state. Finer-grained detail (which API call failed, which sub-step, which host) is carried in `message` and `hosts[].message`.

| Value | Meaning |
|---|---|
| `BootstrapNotReady` | Bootstrap cluster is unreachable or reachable but not healthy enough to accept dynamic hosts (covers network failure, Management API unavailability, and bootstrap hosts not yet `online`) |
| `GroupConfigFailed` | Creating or configuring the MarkLogic group for dynamic hosts failed (e.g., enabling `allow-dynamic-hosts` or API token authentication) |
| `JoinFailed` | Joining one or more hosts failed (covers token request rejection, token expiry before join, and `/admin/v1/init` failures) |
| `RemoveFailed` | Deregistering one or more hosts from MarkLogic failed, or an orphaned host entry could not be cleaned up |
| `PodStartupTimeout` | One or more Pods did not reach `Running` and local readiness in time |
| `ClusterRestartDetected` | MarkLogic restart cleared dynamic membership and the operator is rejoining existing Pods |
| `InvalidConfiguration` | Dynamic group configuration is invalid or unsupported (covers missing bootstrap group, MarkLogic version below the minimum supported version, StatefulSet sync errors caused by invalid spec, and reuse of a MarkLogic group that already has forests) |
| `RetryBudgetExhausted` | Maximum retries per host or per operation reached without success; the affected operation has been escalated to `Failed` |

### Phase Transitions

The following table defines the allowed phase transitions. Any transition not listed is invalid.

Definitions:

- An *active phase* is `Reconciling` or `Idle` — a phase whose target is determined by reconciling spec, Kubernetes, and MarkLogic state.
- A *transient* condition is a transport error, 5xx response, timeout, or any error explicitly classified as transient in the failure matrix.
- A *permanent* condition is an authentication, authorization, or schema-validation error from MarkLogic, or exhaustion of the retry budget.

| From | To | Trigger |
|---|---|---|
| _(initial)_ | `Pending` | Dynamic group first reconciled, before any state has been observed |
| `Pending` | `Reconciling` | Bootstrap reachable and healthy; group configuration is being applied or hosts are being joined |
| `Pending` | `Idle` | Group configuration succeeded and desired replicas = 0 |
| `Pending` | `Degraded` | Bootstrap not ready, or transient configuration error |
| `Pending` | `Failed` | Permanent configuration error or invalid configuration (including unsupported MarkLogic version) |
| `Reconciling` | `Idle` | Desired replica count reached and all required hosts joined |
| `Reconciling` | `Degraded` | Transient error, or some hosts joined while one or more permanently failed |
| `Reconciling` | `Failed` | Permanent error (auth, rejected join, retry budget exhausted) |
| `Idle` | `Reconciling` | Desired replicas changed, or MarkLogic membership lost for existing Pods (restart recovery) |
| `Degraded` | `Reconciling` | Transient condition clears, or user changes replicas to drive further reconciliation |
| `Degraded` | `Idle` | Failed hosts resolved (removed or rejoined) and group is at desired state |
| `Degraded` | `Failed` | Retry budget exhausted (`RetryBudgetExhausted`) or the underlying error is reclassified as permanent |
| `Failed` | `Degraded` | User intervention partially clears the blocking condition; the operator resumes retrying |
| `Failed` | `Reconciling` | User fixes the blocking condition and the group can resume converging toward desired state |
| _(any)_ | `Deleting` | A deletion timestamp is set on the child `MarklogicGroup` |
| `Deleting` | _(removed)_ | All dynamic hosts deregistered (or skipped due to full-cluster teardown) and finalizers removed |

## User Experience

### Initiating Scale-Up

Example JSON patch, assuming the dynamic group is the second item in `markLogicGroups`:

```bash
kubectl patch marklogiccluster my-cluster --type=json \
  -p='[{"op":"replace","path":"/spec/markLogicGroups/1/replicas","value":5}]'
```

### Initiating Scale-Down

```bash
kubectl patch marklogiccluster my-cluster --type=json \
  -p='[{"op":"replace","path":"/spec/markLogicGroups/1/replicas","value":2}]'
```

### Monitoring Progress

```bash
# Check dynamic group status
kubectl get marklogicgroup enode-burst -o jsonpath='{.status.dynamic}'

# Watch group events
kubectl describe marklogicgroup enode-burst

# Check dynamic host pods
kubectl get pods -l app.kubernetes.io/component=dynamic-host,app.kubernetes.io/instance=enode-burst
```

## Architectural Decisions

### Extend `markLogicGroups` Instead of Adding Top-Level `dynamicHost`

#### Decision

Dynamic host pools are modeled as `markLogicGroups[]` entries with `isDynamic: true` and a `dynamic` configuration block.

#### Rationale

1.  **Matches the current operator architecture.** `MarklogicCluster` already reconciles cluster intent into child `MarklogicGroup` resources.
2.  **Keeps ownership boundaries intact.** `MarklogicGroup` already owns StatefulSets and Services.
3.  **Avoids a second reconciliation model.** A top-level `dynamicHost` feature would force the cluster controller to own Pods, finalizers, and per-host status directly.
4.  **Reduces schema duplication.** Dynamic groups reuse the existing group-level image, resources, placement, probes, TLS, and logging fields.
5.  **Supports multiple pools naturally.** Multiple dynamic groups do not require an API redesign.

#### Tradeoffs

1.  Group-mode validation is more complex because some fields are only valid for static groups.
2.  The group controller must branch between static and dynamic reconcile paths.

### StatefulSet for Dynamic Groups

Dynamic groups use a StatefulSet despite not using the standard MarkLogic data persistence model.

Rationale:

1.  StatefulSets provide deterministic FQDNs required by the token API.
2.  StatefulSets scale down in ordinal order, which makes deregistration deterministic.
3.  The existing operator already manages MarkLogic workloads through StatefulSets.
4.  Allowing optional auxiliary PVCs for logs or audit retention does not change the need for stable pod identity.

### Default Datadir Storage for Dynamic Hosts

#### Decision

Dynamic groups use `EmptyDir` for the main MarkLogic datadir mounted at `/var/opt/MarkLogic` by default. Users may explicitly set `persistence.enabled=true` on a dynamic group to back that path with a PVC. The default remains `EmptyDir`; PVC-backed persistence is opt-in.

#### Rationale

1.  `EmptyDir` is the safer default because it keeps the local host state disposable and aligns with the original dynamic-host model.
2.  Some users still need the option to retain local host state across Pod recreation or scale-down for a specific ordinal, so the existing `persistence` field should remain available on dynamic groups.
3.  The storage mode affects lifecycle behavior: `EmptyDir`-backed hosts should be treated as disposable and deregistered on ordinary scale-down, while PVC-backed hosts can be retained.
4.  Retention requirements for logs or audit data remain narrower than preserving the full datadir, so auxiliary PVCs are still appropriate when only those artifacts need durability.

#### Tradeoffs

1.  **Pros:** `EmptyDir` avoids a default storage-class dependency for the main datadir and keeps default recovery semantics simple.
2.  **Pros:** PVC-backed persistence gives users an explicit escape hatch when they need retained local host state for a dynamic ordinal.
3.  **Pros:** scale-down behavior can be aligned with storage intent: disposable `EmptyDir` hosts are removed, retained PVC-backed hosts are preserved.
4.  **Cons:** PVC-backed dynamic hosts make lifecycle and reconciliation logic more complex because the operator must distinguish retained hosts from orphans and must sanitize retained datadir state before restart-driven rejoin.
5.  **Cons:** any local state under `/var/opt/MarkLogic` is still lost for the default `EmptyDir` path when the Pod is deleted and recreated.
6.  **Cons:** if a workload requires durable forests or full data-host semantics, it is still not a fit for the dynamic-host model and should be represented as a static persistent group instead.

#### PVC-Backed Restart Recovery Handling

PVC-backed dynamic hosts are only retained across ordinary replica scale-down while their MarkLogic host registration remains valid. A whole-cluster restart is different: once MarkLogic clears dynamic-host membership, the PVC-backed Pod must not reuse the retained local dynamic-host state as-is.

Required handling in this design:

1.  If a PVC-backed dynamic host loses cluster membership because of a MarkLogic cluster restart, the operator must treat rejoin as a fresh dynamic-host join.
2.  Before issuing a new token join for that PVC-backed host, the operator must clear the retained dynamic-host data in the mounted volume so stale local MarkLogic state does not interfere with rejoin.
3.  The operator should preserve the log folder in that volume if possible, but correctness of restart recovery takes precedence over log retention.
4.  This cleanup requirement is specific to restart-driven rejoin after membership loss. It does not change the ordinary replica scale-down rule that PVC-backed hosts may remain registered while their Pods are scaled down.

#### `EmptyDir` Capacity and Limit Handling

`EmptyDir` does not provide a fixed Kubernetes-wide capacity by itself. For the default disk-backed mode, the practical limit is the node's available ephemeral storage unless a `sizeLimit` is configured on the volume. For `medium: Memory`, the practical limit is node memory unless a `sizeLimit` is configured.

Required handling in this design:

1.  The default `EmptyDir` path is best-effort scratch storage for local dynamic-host state, not guaranteed durable capacity.
2.  Implementations should allow an `EmptyDir.sizeLimit` to be configured on the generated volume so users can cap per-Pod usage when they stay on the default storage mode.
3.  Implementations should set container `ephemeral-storage` requests and limits consistently with the expected `EmptyDir` usage so scheduling and eviction behavior are more predictable.
4.  If `EmptyDir` consumption exceeds the configured `sizeLimit`, or if the node runs out of ephemeral storage, Kubernetes may evict the Pod.
5.  An eviction or Pod restart that loses `EmptyDir` state is handled the same way as any other replacement of an `EmptyDir`-backed dynamic host: the old host entry is treated as disposable, the operator removes or repairs stale membership as needed, and the replacement Pod follows the normal token-join flow.
6.  If a workload requires predictable larger local capacity or retained local state across Pod recreation, the user should set `persistence.enabled=true` instead of relying on the default `EmptyDir` behavior.

### Auxiliary PVCs for Dynamic Groups

#### Decision

Dynamic groups may use `additionalVolumeClaimTemplates` and matching mounts for auxiliary storage such as audit or log retention. The existing `persistence` field may also be used to make the main dynamic-host datadir PVC-backed, but neither mechanism may be used to make the host a forest-bearing persistent data host.

#### Rationale

1.  Dynamic hosts remain evaluator-only and should not become persistent data hosts.
2.  The operator's existing `persistence` field already controls the main MarkLogic datadir, so that is the right knob when the user explicitly wants a PVC-backed dynamic host datadir.
3.  Audit and operational logs are still a distinct concern from main datadir persistence. If only those artifacts need durability, a dedicated auxiliary PVC is a cleaner fit than enabling full datadir persistence.
4.  The operator already supports external log shipping; auxiliary PVCs provide an additional option when local retention is required by policy.

#### Tradeoffs

1.  Auxiliary PVCs introduce storage-class and lifecycle dependencies for otherwise ephemeral dynamic hosts.
2.  The controller must validate that auxiliary mounts are not used to replace the standard datadir or otherwise make the host a forest-bearing stateful node outside the explicit `persistence` setting.
3.  Users must manage log-retention sizing and cleanup separately from the dynamic host lifecycle.

### Status Ownership on `MarklogicGroup`

Detailed dynamic status belongs on the child `MarklogicGroup` resource.

Rationale:

1.  The group controller owns the concrete workload and pod lifecycle.
2.  Multiple dynamic pools require independent status.
3.  This fits the existing controller split in the repository.

### Controller-Owned Dynamic Reconciliation

#### Decision

Dynamic-host reconciliation logic belongs in the `MarklogicGroup` controller, not in Pod startup scripts such as `cluster-init-wrapper.sh` or `cluster-config.sh`.

Pod startup scripts for dynamic groups are limited to the following responsibilities:

1.  Start the local MarkLogic process.
2.  Satisfy the configured readiness probe (for example, accept connections on port `8001`, or, if the existing script-based probe is reused, create `/tmp/marklogic_ready` once the equivalent local condition holds).
3.  Branch on `MARKLOGIC_DYNAMIC_HOST=true` before any existing host-role detection (including the `*-0` bootstrap check) so the static `join_cluster()` / `/admin/v1/cluster-config` path is skipped. See [Dynamic Pod Init Path](#dynamic-pod-init-path) for the full contract.

Anything beyond the above — in particular any MarkLogic Management API call, group creation, token handling, or cluster-membership mutation — is out of scope for Pod startup scripts.

#### Rationale

1.  **Dynamic reconciliation is cluster-scoped, not Pod-local.** Group creation, token issuance, host removal, and restart recovery all depend on desired state, observed Kubernetes state, and observed MarkLogic state across more than one Pod.
2.  **The controller owns the source of truth needed for convergence.** The `MarklogicGroup` controller already reconciles the StatefulSet, Services, Pod finalizers, and `status.dynamic`, so it is the correct place to compare desired replicas, actual Pods, and registered hosts.
3.  **Leader election must gate external mutations.** Only the elected controller replica should issue tokens, submit `/admin/v1/init` joins, and call the remove API. Pod startup scripts run on every replica independently and cannot observe leader-election state, so they cannot safely gate Management API mutations.
4.  **Recovery requires persisted operator state and re-observation.** Operator restart recovery and MarkLogic restart recovery both require rebuilding intent from Kubernetes and Management API state, which is controller behavior rather than entrypoint-script behavior.
5.  **Scripts should remain deterministic and host-local.** Keeping `cluster-init-wrapper.sh` and `cluster-config.sh` limited to local startup behavior reduces race conditions and prevents Pod-local logic from mutating shared cluster state.

#### Tradeoffs

1.  The controller implementation becomes more complex because it must own concrete additions the rest of this spec commits to: a Pod watch, the two dynamic finalizers, three-way reconciliation, the token lifecycle, and per-host retry accounting.
2.  The existing script path still needs a dynamic-mode guard (`MARKLOGIC_DYNAMIC_HOST=true`, checked before the `*-0` hostname detection) so dynamic Pods do not accidentally execute the static cluster join flow.
3.  Some bootstrap-time behavior that is simple in shell for static groups must be reimplemented in Go for the dynamic path.
4.  Upgrade ordering matters more than for static groups: operator and image versions that disagree on the dynamic-mode contract (for example an older image without the `MARKLOGIC_DYNAMIC_HOST` branch, or a newer image paired with an operator that does not yet drive token joins) can strand Pods in a locally-ready but unjoined state. See [Operator Upgrade Behavior](#operator-upgrade-behavior).

### Explicit Restart Recovery

MarkLogic removes dynamic hosts from cluster membership on restart. The spec treats this as a first-class recovery path.

Rationale:

1.  Dynamic host loss on restart is normal product behavior, not an exceptional failure.
2.  Users should not need to recreate group entries after a restart.
3.  Recovery must rejoin existing Pods when possible instead of destroying and recreating them unnecessarily.

## Detailed Workflow

### Overview

The operator compares three sources of truth for each dynamic group:

1.  Desired state from `markLogicGroups[i].replicas`.
2.  Kubernetes state from the child `MarklogicGroup` StatefulSet and Pods.
3.  MarkLogic state from the list of registered dynamic hosts in the corresponding MarkLogic group.

### Mid-Operation Spec Changes

On every reconcile, the controller re-derives intent from `spec.replicas` and the three-way reconciliation table. If `replicas` changes while a previous operation is in progress, the controller adjusts direction on the next reconcile loop without waiting for the prior operation to complete.

Examples:

1.  If scale-up from 2→5 is at 3 joined and `replicas` changes to 3, the controller stops joining new hosts and transitions to `Idle`.
2.  If `replicas` changes to 1 during scale-up from 2→5, the controller stays in `Reconciling` and switches to deregistering the excess hosts.
3.  If `replicas` increases during an in-progress scale-down, the controller stays in `Reconciling` and resumes joining new hosts after completing deregistration of any hosts that have already started the remove process.

The controller must never leave a host in a half-joined or half-removed state. A join that has already started (token issued) should complete or fail before the controller changes direction for that specific host. A remove that has already been submitted to the Management API should complete before the host is considered available for rejoin.

### Controller Responsibilities

#### `MarklogicCluster` Controller

The cluster controller continues to:

1.  Validate `markLogicGroups` entries, including dynamic-group rules.
2.  Materialize one child `MarklogicGroup` resource per entry.
3.  Propagate `isDynamic` and `dynamic` into the child `MarklogicGroup.spec` for dynamic groups.
4.  Default child persistence to `enabled=false` for dynamic groups only when the group did not set persistence explicitly, while preserving an explicit `persistence.enabled=true` choice.
5.  Avoid direct ownership of StatefulSets, Services, Pods, and dynamic host status.

#### `MarklogicGroup` Controller

The group controller must:

1.  Keep the existing reconcile path for static groups.
2.  Use a dynamic reconcile path when `spec.isDynamic=true`.
3.  Reconcile Services, StatefulSet, pod finalizers, group finalizers, group configuration, join and remove lifecycle, and `status.dynamic`.
4.  Use a MarkLogic Management API client that authenticates with the credentials referenced by `spec.auth` and targets `spec.bootstrapHost` (the existing field already populated by the cluster controller from the bootstrap group's headless service). The group controller reads the MarkLogic cluster name from its configured environment variable before issuing cluster-scoped token or remove requests.
5.  When the leader-election lease changes, only the leader replica issues tokens, executes joins, and submits remove calls. Non-leader replicas may continue to observe state but must not mutate MarkLogic.

### TLS and Management API Transport

When `spec.tls.enableOnDefaultAppServers=true` (or the equivalent group-level TLS setting), the operator's Management API client must use HTTPS for all bootstrap-cluster calls and for the join POST to each dynamic Pod. Behavior:

1.  The client trusts the CA certificate referenced by the existing TLS configuration on the bootstrap group. If a custom CA is configured, the operator mounts and uses that CA.
2.  When TLS is enabled, the join URL becomes `https://{joining-host}:8001/admin/v1/init` (the secured Admin port) instead of `http://{joining-host}:8001/admin/v1/init`. The token request URL likewise becomes `https://{bootstrap-host}:8002/...`.
3.  Server name verification uses the Pod FQDN computed from StatefulSet name, ordinal, namespace, and `spec.clusterDomain`. Certificates issued by the operator-managed CA must include this SAN.
4.  Plain-text fallback is not permitted when TLS is enabled.

### One-Time Group Configuration

Before the first dynamic host can join, the operator must ensure the target MarkLogic group exists and is configured for dynamic hosts.

Actions:

1.  If the MarkLogic group does not exist, create it with `POST /manage/v2/groups`.
2.  If the MarkLogic group already exists, verify it has no forests attached. If forests exist, transition to `Failed` with `InvalidConfiguration` and emit a warning event. Dynamic groups must not reuse a MarkLogic group that holds persistent data.
3.  Enable `allow-dynamic-hosts` on the target group.
4.  Enable `API-token-authentication` on the Admin App Server for the target group.
5.  Set `status.dynamic.dynamicHostsEnabled=true`.

Failure behavior:

1.  If bootstrap is unreachable or not healthy, transition to `Degraded` with `BootstrapNotReady`.
2.  If the API call fails with a transient transport error, timeout, or 5xx, transition to `Degraded` with `GroupConfigFailed` and retry with backoff.
3.  If the API call is rejected because of credentials, privileges, or invalid configuration, transition to `Failed` with `GroupConfigFailed`.

Note: transient blocked conditions use `Degraded` with a `reason` such as `BootstrapNotReady` or `GroupConfigFailed`.

Bootstrap-readiness criteria:

A bootstrap cluster is considered "ready enough" to accept dynamic-host configuration and joins when *all* of the following are true:

1.  `GET /manage/v2/hosts?format=json` returns 200 and lists at least one host.
2.  Every host listed by the bootstrap is in `online` status (per the host status response).
3.  The bootstrap MarkLogic version satisfies the [minimum supported version](#compatibility).

If the host-list check fails, the controller transitions to `Degraded` with `BootstrapNotReady` and retries with backoff. If the version check fails, the controller transitions to `Failed` with `InvalidConfiguration` (the `message` identifies the version mismatch) and does not retry until user action changes the bootstrap state.

The controller, not the Pod bootstrap script, owns dynamic group creation and configuration.

In this repository, that means the `MarklogicGroup` controller's dynamic reconcile path performs the bootstrap-cluster Management API mutations. Pod startup scripts are limited to local MarkLogic startup and must not create groups or apply dynamic-host settings.

All configuration calls (`PUT /manage/v2/groups/{name}/properties`, `PUT /manage/v2/servers/Admin/properties`) are idempotent. The controller may re-execute them on restart without checking `status.dynamic.dynamicHostsEnabled` first. The status field is an optimization to skip unnecessary API calls, not a correctness gate.

### Dynamic Pod Init Path

Dynamic group Pods must not execute the existing static cluster join flow.

Required behavior:

1.  Dynamic group Pods start MarkLogic locally and wait for the operator-managed token join.
2.  The existing container entrypoint may remain `cluster-init-wrapper.sh`, but the dynamic-mode path it invokes must skip the existing static `join_cluster()` path and must not call `/admin/v1/cluster-config`.
3.  The StatefulSet template for dynamic groups must inject the environment variable `MARKLOGIC_DYNAMIC_HOST=true` into the MarkLogic container. This is the authoritative mode switch for the existing `cluster-init-wrapper.sh -> cluster-config.sh` path; the script logic must branch on this variable before any other host-role detection (including the existing `*-0` hostname check). A separate entrypoint binary is not required.
4.  The pod init path for dynamic groups must only prepare the local MarkLogic process to accept the later `/admin/v1/init` token POST from the controller.
5.  The preferred approach is for dynamic groups to override the readiness probe to use a `TCPSocket` check on port `8001`, because dynamic hosts should become locally ready before they join cluster membership. If the existing script-based readiness probe is reused instead, the dynamic init path must not create `/tmp/marklogic_ready` until the local readiness condition equivalent to that probe is satisfied.
6.  Local readiness in the dynamic workflow is defined by the configured readiness probe for that group. With the preferred `TCPSocket` readiness probe, local readiness means the MarkLogic Admin process is accepting connections on port `8001`.
7.  The existing liveness probe (`TCPSocket` on port `8001`) is appropriate for dynamic hosts. It checks that the local MarkLogic process is alive without requiring cluster membership. A dynamic pod that has lost membership (e.g., after MarkLogic restart) should remain alive so restart recovery can rejoin it.

Implementation note: the existing `cluster-init-wrapper.sh` currently invokes `cluster-config.sh`, and `cluster-config.sh` uses `[[ "${HOSTNAME}" == *-0 ]]` to detect bootstrap hosts. A dynamic group's pod-0 (e.g., `enode-burst-0`) matches this pattern. The `MARKLOGIC_DYNAMIC_HOST=true` guard must therefore be checked before the `-0` suffix test and before any static bootstrap or join logic runs, or dynamic pods must use an entirely separate entrypoint that does not include the bootstrap detection logic.

### Dynamic Scale-Up Workflow

Entry criteria:

1.  `spec.isDynamic=true`.
2.  Desired replicas > joined hosts.
3.  Bootstrap cluster is reachable and healthy.
4.  Group configuration has been applied.

Phase: `Reconciling` (scale-up sub-activity; `message` indicates scale-up)

#### Step 1: Reconcile the StatefulSet

1.  Update the dynamic group's StatefulSet replica count to the desired value.
2.  Ensure dynamic labels are applied to the StatefulSet, Pods, and Services, including `app.kubernetes.io/component=dynamic-host` in the selector and pod template labels.
3.  Wait for new Pods to reach `Running` and local readiness. Local readiness means satisfying the readiness probe configured for the dynamic group. With the preferred `TCPSocket` readiness probe, this means the MarkLogic Admin process on the Pod is accepting connections on port `8001`; it does not imply the host has already joined the cluster.
4.  Add the controller-owned finalizer `marklogic.progress.com/dynamic-host-cleanup` to each dynamic Pod so the operator can decide during deletion handling whether deregistration is required before the Pod is removed.

#### Step 2: Join Each Unjoined Pod

For each Pod that exists but is not yet joined:

The leader-elected controller replica performs token requests and `/admin/v1/init` joins after local Pod readiness is observed. Pods do not self-join. The controller processes unjoined pods sequentially — one token request and join per reconcile iteration, or one at a time within a single reconcile. This avoids token expiry from batch-requesting tokens for many pods. Implementations may parallelize if they can guarantee each token is used within its validity window, but sequential processing is the recommended default.

For PVC-backed ordinals returning after an earlier scale-down, the controller must first check whether that ordinal's host is still registered in MarkLogic. If the retained host entry still exists and the Pod has resumed successfully with its PVC-backed local state, the controller marks the host `Joined` and skips token issuance for that Pod. This optimization applies only when MarkLogic still recognizes that retained host. It does not apply to restart recovery after cluster membership has been cleared.

1.  Compute the Pod FQDN from StatefulSet name, ordinal, namespace, and cluster domain.
2.  Request a dynamic host token from the bootstrap cluster for that FQDN.
3.  POST the token to the Pod's `/admin/v1/init` endpoint.
4.  Verify the host appears in the MarkLogic Management API.
5.  Record the returned `hostId` in `status.dynamic.hosts[]`. On rejoin (restart recovery or repeat join after deregistration), overwrite any previous `hostId` for that Pod, since MarkLogic assigns a new host ID on each join.
6.  Update the host state to `Joined`.

#### Step 3: Update Group Status

1.  Increment `readyReplicas` as each host joins.
2.  When all desired hosts are joined, transition to `Idle`.

Failure behavior:

1.  Retry token request failures caused by transport errors or 5xx responses.
2.  Treat 401 or 403 token request failures as group-wide auth failures and transition to `Failed` with `JoinFailed` (the `message` field identifies the token-request step).
3.  Retry network-related join failures with backoff.
4.  Mark a host `Failed` with `JoinFailed` if MarkLogic rejects the join after retries.
5.  If a token expires before join completes, request a new token and retry without marking the host `Failed`; the transient condition is reflected in `message` rather than in `reason`.
6.  If at least one host is joined and one or more hosts are permanently failed, transition to `Degraded`.

### Dynamic Scale-Down Workflow

Entry criteria:

1.  Desired replicas < number of currently joined hosts.
2.  Bootstrap cluster is reachable.

Phase: `Reconciling` (scale-down sub-activity; `message` indicates scale-down)

#### Step 1: Identify Hosts to Remove

1.  Compute the highest ordinal Pods above the desired replica count.
2.  Resolve the corresponding `hostId` values from `status.dynamic.hosts[]`.
3.  Determine the resolved main datadir storage mode for those ordinals from the group's effective `persistence.enabled` setting.

Note: Scale-down removes only the highest-ordinal hosts. If a lower-ordinal host (e.g., pod-2 in a 5-replica group) is in `Failed` state, it persists until the user scales below that ordinal or manually intervenes. The `Degraded` phase remains active as long as any `Failed` host exists in the group.

#### Step 2: Apply Storage-Mode-Specific Removal Behavior

The leader-elected controller replica branches on the resolved storage mode.

For `EmptyDir`-backed hosts:

1.  Call `DELETE /manage/v2/clusters/{cluster-name}/dynamic-hosts` with the host IDs to remove.
2.  Wait for a successful response.
3.  Update each host state to `Removed`.
4.  Remove the dynamic cleanup finalizer from successfully deregistered Pods.

For PVC-backed hosts:

1.  Do not call the dynamic-host remove API during ordinary replica scale-down.
2.  Preserve the host's `hostId` and MarkLogic registration.
3.  Update each retained host state to `Retained`.
4.  Remove the dynamic cleanup finalizer from the Pods being scaled down so Kubernetes can terminate them.

#### Step 3: Scale Down the StatefulSet

1.  Reduce the StatefulSet replica count to the desired value.
2.  Allow Kubernetes to delete Pods whose finalizers have been removed.
3.  Remove deleted `EmptyDir`-backed hosts from `status.dynamic.hosts[]`.
4.  Keep PVC-backed retained hosts in `status.dynamic.hosts[]` with state `Retained`.

#### Step 4: Update Group Status

1.  Decrement `readyReplicas` for the ordinals that are no longer running.
2.  When the StatefulSet and the intended storage-mode-specific MarkLogic membership both match desired state, transition to `Idle`.

Failure behavior:

1.  If bootstrap becomes unreachable or unhealthy, transition to `Degraded` with `BootstrapNotReady` and do not scale down the StatefulSet.
2.  If the remove API fails for an `EmptyDir`-backed host, transition to `Degraded` with `RemoveFailed` and retry later.
3.  If Pod deletion is delayed after deregistration, continue waiting and emit a warning event if timeout thresholds are exceeded.

### Restart Recovery Workflow

Phase: `Reconciling` (restart-recovery sub-activity; `reason=ClusterRestartDetected`)

Restart recovery is entered when the operator detects that one or more dynamic Pods still exist but the corresponding hosts are no longer registered in MarkLogic.

Typical triggers:

1.  Bootstrap cluster restart or other MarkLogic restart that clears dynamic host membership.
2.  Operator restart followed by reconciliation against changed MarkLogic state.

Actions:

1.  Query MarkLogic for the current dynamic hosts in the target group using `GET /manage/v2/hosts?group-id={group-name}&format=json` (or the equivalent `/manage/v2/groups/{name}/hosts` form supported by the target MarkLogic version).
2.  Compare the result with desired replicas and actual Pods.
3.  For each `EmptyDir`-backed Pod that exists but is no longer registered, request a new token and rejoin it.
4.  For each PVC-backed Pod that exists but is no longer registered, clear the retained dynamic-host datadir state on the volume, preserving the log folder if possible, then request a new token and rejoin it.
5.  Rebuild `status.dynamic.hosts[]` from observed MarkLogic state and successful rejoins.
6.  Return to `Idle` when desired replicas and registered hosts match again.

Host-to-Pod mapping: when reconstructing state from the Management API response, the operator maps a registered MarkLogic hostname back to its Pod by taking the leftmost DNS label of the FQDN. For example, `enode-burst-3.enode-burst.marklogic.svc.cluster.local` maps to Pod `enode-burst-3`. `status.dynamic.hosts[]` remains the primary source of `hostId` values; the Management API query is the recovery fallback when persisted status is missing or stale.

Pod replacement with lost local state is treated as an orphan-recovery case, not as a healthy retained membership. If a dynamic Pod is recreated and the replacement Pod reuses the same StatefulSet ordinal and DNS name but has lost its local MarkLogic state (for example because the previous Pod's `EmptyDir` was deleted or the Pod was evicted under ephemeral-storage pressure), any stale MarkLogic host entry associated with the previous incarnation must be removed before the replacement Pod is considered joined. The replacement Pod must then follow the normal unjoined-Pod path and receive a new token and new `hostId`. If the ordinal is PVC-backed and the same PVC is reattached while MarkLogic still recognizes the retained host, the controller may reuse that retained state. If MarkLogic membership has been cleared, such as after a whole-cluster restart, the controller must clear the retained dynamic-host datadir state before issuing a fresh token join, while preserving the log folder if possible.

Failure behavior:

1.  If bootstrap is unavailable, transition to `Degraded` with `BootstrapNotReady` (use `ClusterRestartDetected` instead when the mismatch itself is the signal that recovery is needed rather than a transport failure).
2.  If some Pods rejoin and some fail, remain in `Degraded` with `JoinFailed`.
3.  If no Pods can be rejoined because of permanent auth or configuration errors, transition to `Failed`.

### Dynamic Group Deletion

When a dynamic group entry is removed from `spec.markLogicGroups`:

1.  The child `MarklogicGroup` must carry the finalizer `marklogic.progress.com/dynamic-group-cleanup` so it is not garbage-collected before host deregistration completes.
2.  The operator performs explicit cleanup of all registered dynamic hosts before finalizer removal. Unlike ordinary replica scale-down, this deletion cleanup applies even to PVC-backed retained hosts.
3.  After all dynamic hosts are deregistered, the group controller removes the `marklogic.progress.com/dynamic-group-cleanup` finalizer and the child `MarklogicGroup` resource is deleted.
4.  The owned StatefulSet and Services are garbage-collected.

If bootstrap infrastructure is already gone because the entire cluster is being deleted, the operator may skip deregistration and proceed with cleanup. In that case the controller must still remove the `marklogic.progress.com/dynamic-host-cleanup` finalizer from each Pod before removing `marklogic.progress.com/dynamic-group-cleanup` from the `MarklogicGroup`, so Pod garbage collection is not blocked.

Implementation note: because this workflow uses a Pod-scoped finalizer, the `MarklogicGroup` controller must watch and reconcile owned Pods in addition to StatefulSets and Services.

### Finalizers

The dynamic-host workflow uses two finalizers:

| Finalizer | Owner | Purpose | Removed when |
|---|---|---|---|
| `marklogic.progress.com/dynamic-host-cleanup` | Pod | Blocks Pod deletion until the controller has applied the required storage-mode-specific cleanup decision for that Pod | After the Management API confirms removal for `EmptyDir`-backed hosts, after the controller intentionally releases the Pod for PVC-backed ordinary scale-down, or during full-cluster teardown when the bootstrap is gone |
| `marklogic.progress.com/dynamic-group-cleanup` | `MarklogicGroup` | Blocks group deletion until all dynamic hosts have been explicitly cleaned up and Pod finalizers removed | After explicit deletion cleanup deregisters all remaining dynamic hosts, including PVC-backed retained hosts (or is skipped for full-cluster teardown) |

Because one finalizer is Pod-scoped, this design requires the `MarklogicGroup` controller to observe owned Pods and react to Pod deletion timestamps and finalizer state, not just StatefulSet replica changes.

## Failure Handling and Recovery

### Recovery Principles

1.  **Never leave unintended orphaned MarkLogic hosts.** If an `EmptyDir`-backed Pod disappears without successful deregistration, the operator must remove the orphaned MarkLogic host entry on the next reconcile. For PVC-backed dynamic hosts, a missing Pod may instead represent an intentionally retained host after ordinary scale-down, so the operator must first distinguish retained membership from a true orphan.
2.  **Fail per host when possible.** One host failing to join should not block other hosts from joining.
3.  **Derive state from the system.** Persisted status is helpful, but Kubernetes Pods and MarkLogic membership are authoritative.
4.  **Gate scale-down on the resolved storage mode.** An `EmptyDir`-backed dynamic Pod must not be deleted before its MarkLogic host entry is removed, except during full-cluster teardown when bootstrap no longer exists. A PVC-backed dynamic Pod may be deleted during ordinary replica scale-down without deregistering the retained host entry.
5.  **Treat restart recovery as normal.** Rejoin logic after MarkLogic restart is part of steady-state reconciliation.

### Failure Matrix

The matrix groups scenarios that share the same phase and reason outcome. Direction-specific effects, such as whether scale-up or scale-down is blocked, remain defined in the workflow sections above.

| Scenario class | Expected Behavior | Phase | Reason |
|---|---|---|---|
| Bootstrap unavailable or not ready during active reconciliation | Retry with backoff and block joins or removals until bootstrap is healthy. | `Degraded` | `BootstrapNotReady` |
| Group configuration API transient failure (`transport`, timeout, or `5xx`) | Retry with backoff. | `Degraded` | `GroupConfigFailed` |
| Group configuration API permanent rejection (`auth`, permission, or validation) | Stop retrying and require user action. | `Failed` | `GroupConfigFailed` |
| Token expires before join completes | Request a new token and retry; treat as transient and surface detail in `message`. | `Reconciling` | — |
| Join step transient transport failure | Retry the affected host with backoff. | `Degraded` | `JoinFailed` |
| Join step permanent rejection or auth failure | Fail the affected host, or fail the group if the error blocks all joins. | `Degraded` or `Failed` | `JoinFailed` |
| Pod does not reach `Running` and local readiness before timeout | Fail the affected host after timeout. | `Degraded` | `PodStartupTimeout` |
| Deregistration API or orphan cleanup failure for `EmptyDir`-backed scale-down or explicit group cleanup | Retry and do not advance that cleanup path until removal succeeds. | `Degraded` | `RemoveFailed` |
| MarkLogic membership is lost while Pods still exist | Detect the mismatch and rejoin existing Pods. | `Reconciling` | `ClusterRestartDetected` |
| Invalid or unsupported configuration | Stop automatic retry and require user action. Examples include missing bootstrap group, unsupported MarkLogic version, invalid StatefulSet spec, or reuse of a MarkLogic group that already has forests. | `Failed` | `InvalidConfiguration` |
| Per-host or per-operation retry budget exhausted | Stop retrying for that host or operation. | `Failed` | `RetryBudgetExhausted` |

Operator restart during in-progress reconciliation is handled by rebuilding state from Pods and MarkLogic membership and then continuing reconciliation; see [Restart Recovery Workflow](#restart-recovery-workflow). Full-cluster teardown after bootstrap loss is handled by the deletion and finalizer rules; see [Dynamic Group Deletion](#dynamic-group-deletion) and [Finalizers](#finalizers).

### Three-Way Reconciliation Logic

On every reconcile of a dynamic group, the operator must compare:

1.  **Desired state:** group `spec.replicas`.
2.  **Kubernetes state:** actual Pods in the group's StatefulSet.
3.  **MarkLogic state:** hosts registered in the corresponding MarkLogic group.

| Pods exist | MarkLogic host registered | Action |
|---|---|---|
| Yes | Yes | Host is healthy. No action needed. |
| Yes | No | Pod exists but is not joined. Join or rejoin it. |
| No | Yes | If the group uses `EmptyDir`, this is an orphaned MarkLogic host entry and it should be removed. If the group uses PVC-backed datadir persistence and the ordinal is intentionally scaled down, retain the host entry instead. |
| No | No | Clean state. No action needed. |

The orphan-removal case (`Pods: No`, `MarkLogic: Yes`) always applies to `EmptyDir`-backed dynamic hosts. For PVC-backed dynamic hosts, `Pods: No`, `MarkLogic: Yes` may represent an intentionally retained host after ordinary replica scale-down; that case is not treated as an orphan unless the dynamic group is being deleted or the retained host can no longer be matched to an intended ordinal.

If a replacement Pod reappears with the same ordinal after the previous Pod was deleted, the operator must not treat the retained MarkLogic host entry as proof that the replacement Pod is already joined. A reused ordinal or hostname is not sufficient to preserve identity across Pod recreation when local state was stored on `EmptyDir`; the old MarkLogic host entry belongs to the previous Pod incarnation and must be removed or replaced through the normal reconcile flow.

### Retry Defaults

The following values are configurable implementation defaults, not fixed protocol requirements.

| Parameter | Default |
|---|---|
| Initial backoff interval | 10 seconds |
| Maximum backoff interval | 5 minutes |
| Maximum retries per host | 10 |
| Token request timeout | 30 seconds |
| Join call timeout | 2 minutes |
| Pod startup timeout | 5 minutes |
| Deregistration API timeout | 2 minutes |

## Observability

### Kubernetes Events

Events are emitted on the dynamic `MarklogicGroup` resource.

Events are intentionally sparse and group-oriented. They are meant to summarize lifecycle transitions, blocking conditions, and operation completion for `kubectl describe`. Per-host success details, retry attempts, token-expiry retries, and orphan detection details belong in controller logs and `status.dynamic`, not in repeated Kubernetes events.

Warning events should be emitted when the group first enters a blocked or failed condition, and again only when the condition meaningfully changes. Retry loops should be logged, not emitted as a new warning event on every attempt.

| Event Type | Reason | When Emitted |
|---|---|---|
| `Normal` | `DynamicHostsConfigured` | Dynamic host settings have been enabled on the MarkLogic group |
| `Normal` | `DynamicHostScaleStarted` | A scale-up or scale-down reconcile begins; direction is included in the event `message` |
| `Normal` | `DynamicHostRestartRecoveryStarted` | Restart recovery begins |
| `Warning` | `BootstrapNotReady` | The group first becomes blocked because the bootstrap cluster is unreachable or not healthy enough to proceed |
| `Warning` | `GroupConfigFailed` | Group configuration enters a degraded or failed state |
| `Warning` | `JoinFailed` | Join processing enters a degraded or failed state |
| `Warning` | `RemoveFailed` | Deregistration or orphan cleanup enters a degraded state |
| `Warning` | `InvalidConfiguration` | The spec or live bootstrap state is invalid or unsupported and requires user action |
| `Warning` | `RetryBudgetExhausted` | Retry budget is exhausted for a host or operation |
| `Normal` | `DynamicHostScaleComplete` | Scale-up or scale-down finishes and the group reaches the desired replica count; direction is included in the event `message` |
| `Normal` | `DynamicHostRestartRecoveryComplete` | Restart recovery finished and group is back to `Idle` |

The event reasons above intentionally align with the group-level status reasons where possible. The operator should prefer richer structured logs over additional event reasons for transient retries and per-host progress details.

### Operator Logs

Controller logs are the detailed diagnostic surface for dynamic-host reconciliation. They should be structured and include correlation fields sufficient to identify the target namespace, cluster, group, current phase, and, when applicable, the Pod, hostname, or host ID being processed. Replica-count fields are recommended when logging scale decisions or convergence progress.

Dynamic host tokens MUST be redacted from operator logs and Kubernetes events. The token value is sensitive (it grants cluster-join authority for its validity window) and must never appear in any persisted artifact. Implementations should log only token metadata (host FQDN, requested duration, expiration time) and never the token string.

### Metrics

No new dynamic-host-specific Prometheus metrics are part of v1. For v1, observability is provided through `status.dynamic`, Kubernetes events, and controller logs.

### Status Conditions

The child `MarklogicGroup.status.conditions` array carries one additional condition type for dynamic groups in v1, in addition to the existing static-group conditions:

| Type | Meaning |
|---|---|
| `DynamicHostsReady` | `True` when `phase` is `Idle` and `readyReplicas == desiredReplicas`; `False` when the group is dynamic but not yet converged; `Unknown` before the first successful observation of dynamic status |

`DynamicHostsEnabled` and bootstrap reachability remain part of the authoritative `status.dynamic` contract (`dynamicHostsEnabled`, `phase`, `reason`, and `message`) and are not duplicated as separate condition types in v1.

The parent `MarklogicCluster.status.conditions` may expose an aggregate `DynamicHostsReady` condition across all dynamic groups. If exposed, it is `True` only when every dynamic group reports `DynamicHostsReady=True`, `False` when any dynamic group reports `DynamicHostsReady=False`, and omitted when the cluster has no dynamic groups.

## Required Infrastructure Permissions

### RBAC

The existing ClusterRole already grants the core resource and observation permissions the dynamic-host workflow depends on: create/delete/get/list/patch/update/watch for Pods, Services, StatefulSets, Secrets, and ConfigMaps, plus status-subresource updates for `MarklogicCluster` and `MarklogicGroup`.

Those existing Pod permissions are required for the dynamic path because the `MarklogicGroup` controller must watch owned Pods, inspect deletion timestamps, and patch Pod finalizers during deregistration and cleanup. The only additional RBAC requirement specific to the dynamic-host workflow is Kubernetes event creation and update:

```yaml
- apiGroups:
  - ""
  - events.k8s.io
  resources:
  - events
  verbs:
  - create
  - patch
  - update
```

### Controller Watches

The `MarklogicGroup` controller must observe Pods owned by dynamic groups because Pod deletion timestamps, Pod-scoped finalizers, and orphan-recovery state are part of the workflow. This is a controller watch change, not a new resource kind. The dynamic reconcile path cannot rely on StatefulSet replica changes alone.

### Network Access

The operator Pod requires network access to:

1.  The bootstrap host Management API on port `8002` (or `8002` over TLS when enabled).
2.  Dynamic host Admin API on port `8001`.

This is a network policy concern, not an RBAC concern.

If `networkPolicy.enabled=true`, the configured `MarklogicCluster.spec.networkPolicy` must permit the dynamic-host traffic required by this workflow. In v1, the operator does not synthesize dynamic-host-specific allow rules; it reconciles the user-supplied NetworkPolicy spec.

At minimum, the configured policy must allow:

1.  Operator Pod → dynamic Pod port `8001` for the token join POST.
2.  Operator Pod → bootstrap Pod port `8002` (for Management API calls).
3.  Dynamic Pod ↔ bootstrap Pod traffic required for MarkLogic inter-host communication after join.

## Testing Strategy

The current repository has two automated test layers: controller-runtime `envtest` tests under `internal/controller` and end-to-end tests under `test/e2e`. There are no standalone isolated unit-test packages for this feature today. The dynamic-host implementation should introduce focused unit tests alongside the `envtest` layer for any pure helper logic it adds, and it should add dynamic-host-specific end-to-end coverage to the existing E2E suite.

The MarkLogic Management API is not available in `envtest`. Controller tests must exercise the dynamic reconcile path against a fake or in-process stub of the Management API client so that token issuance, join, remove, host listing, and error injection can be driven deterministically. Real Management API behavior should be covered by the dynamic-host-specific E2E additions listed below.

### Isolated Logic Tests

If the dynamic-host implementation introduces reusable helper functions or pure reconciliation decision logic, add focused unit tests for:

1.  Validation and configuration helpers: `isDynamic`, `dynamic`, bootstrap requirements, dynamic-group persistence defaults and explicit PVC enablement, duplicate group names, immutability of `isDynamic` and `groupConfig.name`, and ISO 8601 parsing for `dynamic.tokenDuration`.
2.  Identity and transport helpers: Pod FQDN computation and Management API URL construction for plain and TLS modes (bootstrap `8002`, join `8001`, correct scheme).
3.  Core reconciliation decisions: the three-way reconciliation table (desired replicas × Pods × MarkLogic membership), including the orphan-removal case when `replicas=0` and the StatefulSet is already scaled down.
4.  Lifecycle and failure mapping: allowed phase transitions, host state transitions, and failure classification from transient vs. permanent errors to the correct `phase` and `reason` per the [Failure Matrix](#failure-matrix).
5.  Retry and recovery logic: retry/backoff computation, per-host retry-budget accounting, and restart-recovery rebuild of `status.dynamic.hosts[]` from observed MarkLogic state.
6.  Token redaction helper used by log and event emission paths.

### Controller / `envtest` Tests

This is the primary non-E2E test layer in the current repository. These tests run the reconcilers against an `envtest` API server and a stubbed Management API client, and verify the resulting Kubernetes resources, finalizers, status, conditions, and events.

1.  **Static group unchanged:** static `MarklogicGroup` reconcile path is unaffected by the dynamic code paths.
2.  **Dynamic group materialization and workload shape:** a dynamic `markLogicGroups` entry produces a child `MarklogicGroup` with `spec.isDynamic=true`, propagated `dynamic` config, `updateStrategy=RollingUpdate`, dynamic labels/selectors, and `MARKLOGIC_DYNAMIC_HOST=true`. When group-level persistence is omitted, the child defaults to `persistence.enabled=false` even if cluster-level persistence defaults are enabled. When group-level `persistence.enabled=true` is explicitly set, the child preserves that PVC-backed datadir choice.
3.  **Persistence modes, auxiliary PVCs, and validation rules:** a dynamic group with `additionalVolumeClaimTemplates` for audit/log retention reconciles successfully; a dynamic group with explicit main-datadir `persistence.enabled=true` also reconciles successfully; and unsupported configurations are limited to cases that still violate the documented rules, such as reuse of a forest-bearing MarkLogic group.
4.  **Bootstrap configuration path:** controller creates the MarkLogic group when absent, enables `allow-dynamic-hosts` and `API-token-authentication`, sets `status.dynamic.dynamicHostsEnabled=true`, and treats those calls as idempotent on re-reconcile.
5.  **Bootstrap gating:** controller defers group configuration and joins until bootstrap readiness checks succeed; unsupported MarkLogic version transitions to `Failed` with `InvalidConfiguration`.
6.  **Happy path reconciliation:** scale-up, scale-down, and scale-to-zero converge correctly, including sequential token issuance, correct `hosts[]` / `hostId` tracking, `EmptyDir`-backed deregistration before StatefulSet scale-down, PVC-backed retained-host behavior during ordinary scale-down, and `phase` returning to `Idle` when `readyReplicas == desiredReplicas`.
7.  **Representative failure and retry paths:** partial join failure, bootstrap unreachable, token-expiry retry, and orphan MarkLogic host cleanup produce the expected `phase`, `reason`, and retry behavior.
8.  **Deletion and finalizer flow:** Pod finalizers block premature deletion, parent-spec removal triggers dynamic-group cleanup, full-cluster teardown skips the remove API when bootstrap is gone, and Pod-watch events requeue the owning `MarklogicGroup`.
9.  **Restart and direction-change recovery:** restart recovery rebuilds `status.dynamic.hosts[]`, rejoins existing Pods, overwrites prior `hostId` values on rejoin, and converges correctly when `replicas` changes mid-operation.
10. **Multiple-pool isolation and leader-election gating:** multiple dynamic groups reconcile independently, and non-leader controller replicas observe state but do not issue tokens, execute joins, or call the remove API.
11. **Integration surfaces at controller level:** HAProxy backend generation includes dynamic pods only when configured, and TLS transport uses `https://` for bootstrap (`8002`) and join (`8001`) URLs without plain-text fallback.
12. **Observability contract:** `DynamicHostsReady` condition, events from the [Kubernetes Events](#kubernetes-events) table, and token redaction in logs/events/status all behave as specified.

### End-to-End Tests (required additions)

The repository already has a general E2E suite under `test/e2e`, but it does not yet include dynamic-host-specific end-to-end coverage. The cases below describe the dynamic-host acceptance tests that should be added for this feature.

1.  **Dynamic capacity lifecycle:** scale up from `0` to `N`, scale back down to `0`, and reverse direction mid-operation while verifying the correct storage-mode-specific behavior: deregistration for `EmptyDir`-backed hosts, retained membership for PVC-backed hosts during ordinary scale-down, Pod cleanup, and convergence to the final desired count.
2.  **Restart and pod-loss recovery:** restart MarkLogic, then separately delete or kill a dynamic host Pod, and verify rejoin, finalizer behavior, operator cleanup, and steady-state recovery. Coverage must include both `EmptyDir`-backed hosts and PVC-backed hosts, with the PVC-backed path verifying datadir cleanup before rejoin and preservation of the log folder when possible.
3.  **Isolation across groups:** verify dynamic scaling does not disrupt static groups, and verify multiple dynamic pools scale independently under concurrent changes.
4.  **TLS end-to-end:** dynamic group with TLS enabled uses HTTPS Management API and join calls successfully.
5.  **HAProxy end-to-end:** dynamic group with `haproxy.enabled=true` receives traffic on the configured app server.
6.  **Deletion paths:** remove a dynamic group entry from `spec.markLogicGroups` and verify full cleanup; separately verify full-cluster deletion does not block on the remove API when bootstrap is already gone.
7.  **Auxiliary PVC scenario:** verify an audit/log-retention PVC attaches and retains data while forests remain absent.

## Rollout and Compatibility

### Backward Compatibility

1.  Existing CRs without `isDynamic` continue to behave exactly as they do today.
2.  No top-level `dynamicHost` field is introduced.
3.  The feature is additive to `markLogicGroups[]`.
4.  Existing static-group reconciliation remains unchanged.
5.  Existing static-only clusters do not require manifest rewrites or data migration to adopt an operator version that includes dynamic-group support.

### CRD Evolution

Cluster-facing spec additions:

1.  `markLogicGroups[].isDynamic`
2.  `markLogicGroups[].dynamic`

Child-resource spec additions:

3.  `MarklogicGroup.spec.isDynamic`
4.  `MarklogicGroup.spec.dynamic`

The child-resource additions are controller-populated projections, not a second user-facing configuration surface.

Status additions:

1.  `MarklogicGroup.status.dynamic`

All changes are additive. The parent `MarklogicCluster` additions are optional and user-authored. The child `MarklogicGroup` additions are optional and are populated only for dynamic groups.

### Operator Upgrade Behavior

1.  Upgrading the operator does not create dynamic resources unless a group entry explicitly sets `isDynamic: true`.
2.  If an upgrade occurs while a dynamic operation is in progress, the new operator version must rebuild state from desired replicas, Kubernetes Pods, and MarkLogic membership, then resume safely.
3.  Pre-upgrade child `MarklogicGroup` resources remain valid. The cluster controller populates the new child dynamic fields only for dynamic groups when they reconcile; static child resources do not require mutation.

### Future Extensions

Schedule-based and metrics-based scaling are out of scope for v1 (see [Scope and Non-Goals](#scope-and-non-goals)). If they are added later, the recommended model is a **separate scaler CR**, owned by its own controller and shipped independently of the MarkLogic Operator. The scaler computes a desired replica count from its inputs (schedules, metrics, min/max) and **patches `spec.markLogicGroups[i].replicas`** on the parent `MarklogicCluster`. The MarkLogic Operator remains a reconciler of desired state: it does not read scaler policy, does not write back to its own spec, and does not arbitrate between scaling signals.

Illustrative shape of such a scaler CR (not part of v1):

```yaml
# ILLUSTRATIVE — owned by a separate scaler project, not by the MarkLogic Operator.
apiVersion: scaling.marklogic.progress.com/v1alpha1
kind: MarklogicDynamicScaler
metadata:
  name: enode-burst-scaler
spec:
  targetRef:
    kind: MarklogicCluster
    name: production
    groupName: enode-burst        # which markLogicGroups[] entry to scale
  minReplicas: 2
  maxReplicas: 20
  schedule:
    - cron: "0 8 * * 1-5"
      replicas: 10
  metrics:
    - type: cpu
      averageUtilization: 70
```

This keeps responsibilities clean:

1.  The scaler decides **how many** dynamic hosts are needed.
2.  The MarkLogic Operator decides **how** to safely realize that count through its join, remove, and restart-recovery lifecycle.

Why a separate CR rather than nesting scaling policy under `markLogicGroups[i].dynamic`:

1.  **Single writer per field.** Keeping the scaler CR separate means `MarklogicCluster.spec.markLogicGroups[i].replicas` has exactly one writer (the scaler) and the rest of that object has one writer (the user or GitOps). This matches the pattern Kubernetes already uses for HPA vs. Deployment and avoids write-write contention between the MarkLogic Operator and a scaler that both edit the same CR.
2.  **Clear schema ownership.** The MarkLogic Operator's CRD only defines fields it actually implements. Scaling policy lives on a CRD owned by the scaler project and versions independently.
3.  **Pluggable.** Users who prefer HPA, KEDA `ScaledObject`, or a custom scheduler can point those at `spec.markLogicGroups[i].replicas` directly without installing the MarkLogic scaler CRD or carrying unused fields under `dynamic`.
4.  **Minimal surface in v1.** The only dynamic-host field v1 defines under `dynamic` is `tokenDuration` (see [`markLogicGroups[].dynamic`](#marklogicgroupsdynamic)). Nothing in the MarkLogic Operator CRD has to change when or if a scaler project ships later.

## Open Questions

### When Audit Log Persistence is Enabled, Should Dynamic Hosts Get PVCs?

## Appendix and Examples

### Terms and Definitions

| Term | Meaning |
|---|---|
| Dynamic host | An evaluator-only MarkLogic host that joins and leaves the cluster via the Dynamic Host Token API |
| Dynamic group | A `markLogicGroups[]` entry with `isDynamic=true` managed through the dynamic host lifecycle |
| Static group | A traditional group managed through the existing cluster join workflow |
| Bootstrap cluster | The static hosts that serve as the authority for Management API calls and token issuance |
| Dynamic host token | A time-limited token that authorizes a host to join the cluster dynamically |
| Host ID | A MarkLogic-internal identifier used for host removal |
| Three-way reconciliation | Comparing desired replicas, Kubernetes Pods, and MarkLogic membership |

### Example: Full Cluster with a Dynamic Group

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: production
  namespace: marklogic
spec:
  image: "progressofficial/marklogic-db:12.0.0-ubi9-rootless-2.2.2"
  auth:
    secretName: ml-admin-auth
  persistence:
    enabled: true
    size: "100Gi"
  markLogicGroups:
    - name: dnode
      replicas: 3
      isBootstrap: true
      groupConfig:
        name: Default

    - name: enode-burst
      replicas: 5
      isDynamic: true
      groupConfig:
        name: DynamicEval
      persistence:
        enabled: false
      resources:
        requests:
          cpu: "2"
          memory: "8Gi"
        limits:
          cpu: "4"
          memory: "16Gi"
      dynamic:
        tokenDuration: "PT15M"
```

The controller reads the MarkLogic cluster name from its configured environment variable when it needs to call cluster-scoped endpoints. `persistence.enabled: false` is shown explicitly for clarity, but dynamic groups remain non-persistent even when the cluster-level default is persistent.

### Example: Observed Child `MarklogicGroup` Projection

The following snippet is controller-generated state derived from the parent `MarklogicCluster`; users should not edit these projected fields directly.

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicGroup
metadata:
  name: enode-burst
spec:
  name: enode-burst
  replicas: 5
  persistence:
    enabled: false
  updateStrategy: RollingUpdate
  isDynamic: true
  dynamic:
    tokenDuration: "PT15M"
```

### Example: Scale-Up Status on `MarklogicGroup`

```yaml
status:
  dynamic:
    phase: Reconciling
    desiredReplicas: 5
    localReadyReplicas: 5
    readyReplicas: 3
    dynamicHostsEnabled: true
    message: "Joining host 4 of 5"
    hosts:
      - podName: enode-burst-0
        hostname: enode-burst-0.enode-burst.marklogic.svc.cluster.local
        hostId: "14271932171933168288"
        state: Joined
        lastUpdated: "2026-04-06T10:01:00Z"
      - podName: enode-burst-1
        hostname: enode-burst-1.enode-burst.marklogic.svc.cluster.local
        hostId: "14271932171933168289"
        state: Joined
        lastUpdated: "2026-04-06T10:01:30Z"
      - podName: enode-burst-2
        hostname: enode-burst-2.enode-burst.marklogic.svc.cluster.local
        hostId: "14271932171933168290"
        state: Joined
        lastUpdated: "2026-04-06T10:02:00Z"
      - podName: enode-burst-3
        hostname: enode-burst-3.enode-burst.marklogic.svc.cluster.local
        state: Joining
        lastUpdated: "2026-04-06T10:02:15Z"
      - podName: enode-burst-4
        hostname: enode-burst-4.enode-burst.marklogic.svc.cluster.local
        state: Pending
        lastUpdated: "2026-04-06T10:02:15Z"
    lastTransitionTime: "2026-04-06T10:00:00Z"
```

### Example: Idle Status After Restart Recovery

```yaml
status:
  dynamic:
    phase: Idle
    desiredReplicas: 2
    localReadyReplicas: 2
    readyReplicas: 2
    dynamicHostsEnabled: true
    message: "Dynamic group recovered after MarkLogic restart and is at desired replica count"
    hosts:
      - podName: enode-burst-0
        hostname: enode-burst-0.enode-burst.marklogic.svc.cluster.local
        hostId: "14271932171933168301"
        state: Joined
        lastUpdated: "2026-04-06T11:01:00Z"
      - podName: enode-burst-1
        hostname: enode-burst-1.enode-burst.marklogic.svc.cluster.local
        hostId: "14271932171933168302"
        state: Joined
        lastUpdated: "2026-04-06T11:01:30Z"
    lastTransitionTime: "2026-04-06T11:02:00Z"
```
