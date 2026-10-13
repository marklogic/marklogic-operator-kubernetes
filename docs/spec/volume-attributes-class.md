# Design Spec: MarkLogic Operator Volume Attributes Management

## Introduction

### Overview

This specification defines one end-to-end requirement: administrators can declaratively select and change a Kubernetes `VolumeAttributesClass` (VAC) for MarkLogic data volumes, from initial provisioning through live reconciliation and observable completion, without operator-caused workload disruption.

The API, provisioning, in-place updates, compatibility handling, and observability are parts of the same controller capability, not separate feature contracts. This document follows the structure of the [volume-resize specification](volume%20resize.md) while distinguishing performance modification from capacity expansion.

### Goal

1. Select storage performance profiles through cluster defaults and group overrides.
2. Create new primary data PVCs, including scale-up claims, with the effective class observed at claim creation; reconcile later changes after binding.
3. Apply supported class changes to existing PVCs without recreating StatefulSets, PVCs, or Pods.
4. Report desired/current class, per-PVC progress, aggregate conditions, and actionable errors.
5. Preserve existing behavior when unconfigured and continue unrelated reconciliation when unsupported.

## Background: Kubernetes Volume Attributes Modification

A VAC is a cluster-scoped resource containing a CSI `driverName` and driver-specific parameters such as IOPS and throughput. Its driver name and parameters are immutable; administrators create a different class and update the reference to request another profile. The operator does not interpret provider parameters or create classes.

Initial dynamic provisioning requires a VAC-aware external-provisioner and compatible CSI driver/configuration. Live modification is a separate path: Kubernetes and a compatible external-resizer perform asynchronous modification through the CSI driver's `ControllerModifyVolume` capability. Qualification must establish both paths; expansion support alone does not establish modification support. Unlike resize, this operation does not require filesystem expansion or an operator-driven Pod remount.

Changing a VAC reference is not a generic parameter merge or rollback. Omitted parameters use defaults at provisioning and may use defaults during modification depending on the CSI driver. Administrators should use complete profiles for the attributes they intend to control and consult driver documentation; the operator does not fill missing parameters or promise to preserve previous values.

The design is grounded in the [Kubernetes VAC documentation](https://kubernetes.io/docs/concepts/storage/volume-attributes-classes/) and [Kubernetes v1.34 PVC update validation](https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/apis/core/validation/validation.go). The latter permits VAC reference changes only on bound claims. An unbound PVC's reference is fixed at creation; native modification `Pending` is not the same as PVC phase `Pending`. These restrictions must also be verified on the qualified 1.32 beta API-server lane.

### PVC Field Mapping

| Concept | Kubernetes field | Operator responsibility |
| --- | --- | --- |
| Requested class | `pvc.spec.volumeAttributesClassName` | The only native VAC field the operator writes. |
| Applied class | `pvc.status.currentVolumeAttributesClassName` | Read Kubernetes-owned status; never populate it directly. |
| In-flight target | `pvc.status.modifyVolumeStatus.targetVolumeAttributesClassName` | Correlate observations with the latest desired class. |
| Native progress | `pvc.status.modifyVolumeStatus.status` | Observe `Pending`, `InProgress`, and `Infeasible`. |
| Modification conditions | `pvc.status.conditions` | Observe `ModifyingVolume` and `ModifyVolumeError`, reason and message. |

Kubernetes has no native `Completed` or `Failed` modification enum. The operator derives these states from the requested/applied class and modification status. Matching the PVC spec alone is not evidence that the provider applied the change.

### Platform Compatibility

| Kubernetes version | Served VAC API | Qualification requirement |
| --- | --- | --- |
| 1.29–1.30 | `storage.k8s.io/v1alpha1` | Alpha; outside this design's supported API scope. |
| 1.31–1.33 | `storage.k8s.io/v1beta1` | Confirm served API and feature configuration. The requested 1.32 minimum requires qualification. |
| 1.34+ | `storage.k8s.io/v1` | GA API; provisioning and modification sidecar/CSI qualification still required. |

Prefer `v1`, falling back to `v1beta1`. Kubernetes version alone does not establish support. EKS, AKS, GKE, and OpenShift qualification must record distribution/version, served API, API-server and sidecar feature configuration, exact CSI driver, external-provisioner and external-resizer versions/configuration, and volume type. Prove VAC provisioning and live modification separately, including driver-specific omitted-parameter behavior. API discovery and matching driver names are necessary checks, not proof of sidecar or CSI capability. Do not silently raise the operator's existing minimum version or claim distribution support without testing.

## Requirements

### Functional Requirements

#### Declarative Storage Selection

1. Expose optional `volumeAttributesClassName` as `*string` under `spec.storage` on clusters, standalone groups, and embedded per-group overrides.
2. Resolve inheritance per field, independently of persistence overrides.
3. Validate RFC 1123 subdomain names of at most 253 characters; allow nil and empty.
4. Regenerate deepcopy, native CRDs, and Helm CRDs, and publish API documentation.

#### Provisioning and Live Reconciliation

1. Populate only primary `datadir` claim templates when creating a StatefulSet; leave the field unset when unconfigured.
2. Create new scale-up PVCs with the effective provisioning class observed at creation, including a non-empty class after live changes or an unset field after opt-out/unsupported fallback.
3. Select managed data claims through namespace/owner labels and verified workload association. Never patch auxiliary, Fluent Bit-specific, or unrelated claims.
4. Patch mismatched bound PVC fields directly, idempotently, with bounded conflict retries and fresh reads. Observe unbound claims without attempting immutable reference changes.
5. Suppress VAC-only immutable claim-template drift without concealing unrelated persistence changes.

#### Capability and Progress Reporting

1. Use cached API discovery; check class existence and StorageClass provisioner/CSI driver compatibility where possible.
2. Report absent APIs, disabled features, missing classes, unsupported drivers, and modification errors through conditions, warnings, and Events.
3. Omit VAC during provisioning when the API is unavailable; unrelated reconciliation must continue.
4. Watch managed PVCs and expose desired/current class and normalized progress under `status.storage`.
5. Set `StorageAttributesSynced=True` only when every targeted data PVC has applied the current desired class. Emit dispatch/completion/failure Events only on transitions.

### Non-Functional Requirements

#### Data Integrity and Availability

A VAC-only update must preserve StatefulSet UID, PVC UID, Pod UID, restart counts, and Pod template/hash. It must not delete PVCs/PVs, recreate StatefulSets, roll Pods, or invoke filesystem-resize recovery. Provider-specific latency, temporary performance effects, and unrelated Pod failures remain environmental limitations; the operator does not promise they cannot occur.

#### Recovery and Backward Compatibility

Reconstruct progress from the latest CR and PVC state after restart. Preserve partial progress, use bounded API retries, and avoid writing status or emitting Events on unchanged observations. Unconfigured resources retain their prior behavior. Feature errors must not stop Services, ConfigMaps, normal workload reconciliation, or existing resize/dynamic-host handling.

### Scope and Non-Goals

In scope: active operator-managed primary data claims, initial provisioning, scale-up, live changes, compatibility checks, and group/cluster observability.

Out of scope: auxiliary claim-template changes, Fluent Bit-specific storage, StorageClass migration, capacity resizing, automatic provider rollback, VAC resource administration, and new user-configurable scheduling/strategy fields. Fluent Bit logs sharing datadir naturally share that volume's performance profile.

Retained claims outside the active ordinal range are not modified; reactivated claims become targets again. Reactivation reuses existing storage, not new provisioning: bound claims converge to the latest managed class, while unbound claims follow the binding recovery rules. Opt-out leaves reused claims unchanged. The controller must report missing active claims rather than silently excluding them from completion.

## API and User Experience Contract

### Trigger Model and Spec Fields

The administrator updates the CR's class reference. Cluster reconciliation resolves the effective field into each child `MarklogicGroup`; group reconciliation owns PVC changes and detailed status. A standalone group uses its own field directly.

| Field | Type | Meaning |
| --- | --- | --- |
| `MarklogicCluster.spec.storage.volumeAttributesClassName` | Optional `*string` | Cluster default. |
| `MarklogicCluster.spec.markLogicGroups[i].storage.volumeAttributesClassName` | Optional `*string` | Per-group override. |
| `MarklogicGroup.spec.storage.volumeAttributesClassName` | Optional `*string` | Standalone selection or resolved child value. |

`storage` is separate from `persistence`: size, StorageClass, access modes, annotations, and existing persistence override semantics remain unchanged.

#### API Placement Decision

Keep the new field at `spec.storage.volumeAttributesClassName`, not under `persistence`. The existing [Persistence type](../../api/v1/common_types.go#L41) requires `size` and includes provisioning settings and resize strategy. The [group parameter resolver](../../pkg/k8sutil/marklogicServer.go#L373) replaces the complete persistence object when a group supplies an override; it does not merge individual fields.

A separate optional storage object lets a group select only a performance profile while inheriting persistence unchanged. Putting VAC under persistence would require users to repeat provisioning settings for a profile-only override or require new partial-merge semantics and changes to existing validation. Neither is part of this feature. A separate object does not imply that all persistence settings are immutable: capacity remains managed by the resize feature. Keep only one supported spec location; do not add a second alias under persistence. Status remains under `status.storage`.

| Embedded group value | Effective class |
| --- | --- |
| Omitted storage, `storage: {}`, or nil class | Inherit cluster value, even if persistence is overridden. |
| Non-empty class | Override cluster value. |
| Explicit empty string | Opt out; do not inherit. |

Nil/empty at cluster or standalone-group scope means no live VAC management; newly created claims omit the field, with a provisioning safeguard when the historical template differs. Removing a group override resumes inheritance, not necessarily opt-out. There is no default class. Optional non-nullable schema fields prune explicit null to absence and decode to nil.

Kubebuilder markers must enforce the RFC 1123 pattern and `MaxLength=253` at all three scopes. Native and Helm schemas must remain identical; existing manifests remain valid.

```yaml
spec:
  persistence:
    enabled: true
    size: 20Gi
    storageClassName: gp3
  storage:
    volumeAttributesClassName: ml-gp3-standard
  markLogicGroups:
  - name: dnode
    replicas: 2
    isBootstrap: true
    # Inherits ml-gp3-standard.
  - name: query
    storage:
      volumeAttributesClassName: ml-gp3-premium
```

The referenced classes and their driver-specific parameters are managed externally. Persistence-disabled groups create no primary claim and take no VAC action.

### Status Contract

Expose the group observation under `MarklogicGroup.status.storage.volumeAttributesClass`. This is a proposed public schema for implementation, not resize bookkeeping reused under another name.

| Field | Type | Meaning |
| --- | --- | --- |
| `observedGeneration` | Integer | Group generation represented by the observation. |
| `state` | Enum | `Pending`, `InProgress`, `Completed`, `Failed`, `Stalled`, or `Unknown`. |
| `reason`, `message` | String | Aggregate diagnostic summary. |
| `pvcs` | Array | Stable name-ordered records for active target PVCs, including missing claims. |
| `pvcs[].name` | String | PVC identity within the group namespace. |
| `pvcs[].desiredClassName` | String | Latest effective CR value. |
| `pvcs[].currentClassName` | Optional string | Kubernetes-reported applied value, absent when unavailable. |
| `pvcs[].state`, `reason`, `message` | String | Per-PVC state and actionable diagnostics. |

Desired/current values are read-only diagnostic fields, not two configurable native PVC fields. Retry timestamps or last-progress timestamps may be stored separately if needed for restart-safe backoff/stall detection. Do not add resize markers, filesystem checkpoints, or restart metadata.

#### State Mapping

| Observation for the latest desired class | Public state |
| --- | --- |
| Missing/unbound PVC, undispatched request, or native `Pending` | Pending. |
| Unbound PVC references an older class | Pending with `PendingClassMismatch`; no reference patch until bound. |
| Native `InProgress` or `ModifyingVolume=True` | InProgress. |
| PVC spec/current class both equal desired; no active modification or active error condition | Completed. |
| Native `Infeasible`, invalid class, or non-retryable rejected write | Failed. |
| `ModifyVolumeError=True` with a retryable/provider error | InProgress with error details; not automatically terminal. |
| Prolonged absence of progress | Stalled; do not infer unsupported CSI from time alone. |
| Unsupported API or unknown native state | Unknown; never success. |

Correlate native target with the latest desired class; old-target completion or failure cannot settle the new request. When target correlation is unavailable or ambiguous, keep the observation pending/unknown until fresh evidence resolves it.

#### Aggregate Condition

Set `StorageAttributesSynced` in `MarklogicGroup.status.conditions`, with `observedGeneration`, reason, message, and transition time. Aggregate True only when all active targets meet the completion predicate. Failure takes precedence, followed by unsupported/unknown, stalled, in-progress, and pending; preserve every per-PVC detail.

The parent cluster aggregates configured, persistence-enabled, managed groups. Missing or stale child status is Unknown, not success. Unconfigured/opted-out groups are excluded. With zero active targets, use a non-success `NoActivePVCs` observation rather than claiming a provider operation completed.

### Removal and Manual Intervention

When the effective class becomes nil/empty, stop live VAC management and leave all existing PVC class fields untouched. Once a class has been applied, Kubernetes forbids clearing the reference to nil/empty; opt-out is not provider rollback or cancellation. A removed embedded-group override still inherits a configured cluster value.

New claims created after opt-out must omit the field even if the immutable historical StatefulSet template still contains a class. Continue the provisioning safeguard described below for that template mismatch. Remove the active modification observation/condition when no provisioning safeguard error remains; otherwise retain a non-success diagnostic/Event without attempting live modification. Opted-out groups remain excluded from provider-completion aggregation. Already-created or retained claims keep their previous reference, including when binding is still pending.

Administrators observe group/cluster status, describe the CR and PVCs, and fix missing classes, permissions, driver configuration, or infeasible requests. Retry recoverable errors after remediation. A new class selection supersedes the desired target immediately, but an unbound claim cannot receive it until binding completes. No separate imperative operation or automatic PVC migration is required.

## Architectural Decisions

**Terms used below**

- **VAC:** A named storage performance profile, such as a particular IOPS or throughput setting.
- **PVC:** A Kubernetes request for storage. `datadir` is the primary MarkLogic data claim.
- **Bound PVC:** A storage request assigned to a volume. Binding alone does not prove a later performance change finished.
- **StatefulSet:** The workload that manages MarkLogic Pods and provides a template for creating their PVCs.
- **CSI driver:** The storage driver that provisions or modifies the actual volume.
- **CR:** The MarkLogic cluster or group configuration submitted by the administrator.

### Change Existing Volumes Safely

- Update the class reference on each eligible bound data PVC.
- Let the external-resizer and CSI driver perform the storage change.
- Observe their status; report success only when the requested class has been applied.
- Do not recreate the StatefulSet, PVCs, or Pods for a VAC-only change.
- Do not run filesystem-expansion checks, remount recovery, or Pod-restart steps from the resize feature.

**Keep the existing StatefulSet template unchanged**

- Kubernetes does not allow changing the VAC field in an existing StatefulSet's claim template.
- When generating an update, copy that template's existing VAC value into the proposed StatefulSet object.
- Do this before comparing or updating the object, including updates for unrelated settings.
- Ignoring the difference during comparison is not enough: `ReconcileStatefulset()` writes the full desired specification.
- Preserve only the existing template's VAC value. Do not hide other persistence changes.
- Let the resize controller handle capacity changes separately.

### Create New Volumes and Scale Up

**When creating a new StatefulSet**

- Put the selected, supported VAC in the primary `datadir` claim template.
- Leave auxiliary claim templates and the Pod specification unchanged.
- Omit the VAC field when no class is selected or support checks fail.
- Report failed support checks; do not silently claim the requested profile was applied.

**Why scale-up needs special handling**

- Example: existing PVCs have changed from class A to B, but the StatefulSet template still contains A.
- Increasing replicas normally would create the next PVC using A.
- Changing that PVC to B after binding would not meet the new-claim selection requirement.

**Proposed solution: create missing PVCs before increasing replicas**

1. Read the latest selected class and requested replica count.
2. If the old template has a different class, create the missing primary claims with the current supported selection.
3. Omit the field on those new claims after opt-out or unsupported fallback, even if the old template contains A.
4. Increase replicas only after the required claims exist safely. The StatefulSet then uses those claims.

**Safety rules for creating claims**

- Use the exact claim names and Pod numbers expected by the StatefulSet, including a non-zero starting number.
- Use the effective persistence settings: size, StorageClass, access modes, annotations, and ownership metadata.
- Coordinate size changes with resize validation; do not bypass it.
- If a claim already exists, re-read it and verify ownership and configuration.
- Never adopt or overwrite an unrelated claim just because its name matches.
- Reuse verified existing claims; they are not newly provisioned volumes.
- With `WaitForFirstConsumer`, create the claim and permit Pod scheduling without waiting for binding.
- This avoids a deadlock: storage provisioning may need the Pod's placement before it can bind the claim.
- Preserve retention rules and claims kept after scale-down. Do not introduce claim deletion.
- Re-read the CR before advancing. Resume safely after crashes or concurrent updates without creating duplicates.
- If claim creation or verification fails, stop that scale action, report why, and continue unrelated work.

**What happens if the selection changes during provisioning?**

- A PVC records the class selected when its creation request is prepared.
- The CSI driver may provision the actual volume later.
- If the PVC was created with A and the CR changes to B before binding, keep the PVC.
- Permit any required Pod scheduling and report `PendingClassMismatch` while waiting.
- After binding, request B and observe completion. The existence of the old claim is not success for B.
- This design cannot guarantee B at the actual provisioning instant despite concurrent CR changes.
- The team must agree to creation-time selection followed by post-binding updates before implementation.

**Decision still needed**

- PVC precreation requires approval and integration testing; it is not existing resize functionality.
- Do not recreate the StatefulSet before or after this process.
- If precreation is rejected, validate another non-disruptive way to create new claims with the selected class.
- Creating them from the old template and changing them after binding is not an equivalent solution.

### Handle PVCs Waiting for Storage

- Do not change the VAC reference on an existing unbound PVC; Kubernetes rejects that change under the qualified API rules.
- Observe binding and report any difference from the latest selected class.
- A missing original class, driver problem, or disabled feature can prevent binding.
- Selecting a different class in the CR does not repair the existing unbound request.
- For a known binding failure, report `ProvisioningBlocked`, the original class, and the required corrective action.
- For unexplained delays, report Pending or Stalled; do not invent a permanent provider failure.
- Ask administrators to repair the original provisioning prerequisites where possible.
- Once bound, apply the latest non-empty managed class.
- Do not repeatedly submit rejected patches or delete/recreate the claim as a workaround.
- Opt-out leaves existing class references untouched; it does not cancel or roll back provider work.
- If the original request cannot be repaired, report that administrator intervention is required.
- Continue unrelated reconciliation. Do not block consumer scheduling solely because the target class changed.

### Select Only the Correct Data PVCs

- Find candidate claims in the group's namespace using workload and group labels.
- Verify association with the managed StatefulSet/Pod and the primary `datadir` template.
- Include only claims for the active Pod-number range.
- Do not modify auxiliary, unrelated, or inactive retained claims.
- Add consistent data-role and owner-identity labels to new primary claims.
- Labels and expected names help discovery but are not sufficient proof of ownership.
- Support older claims without the new labels using verified workload associations and existing labels.
- Do not require every claim to have a direct MarklogicGroup owner reference; retention rules may omit it.
- If ownership is unclear, emit a warning and make no change.
- Verify retained claims when they become active again; neither ignore them nor blindly adopt them.

### Coordinate Performance Changes and Resizing

- Keep VAC reconciliation and status separate from the resize workflow.
- Resize manages capacity; VAC manages the performance profile.
- Patch only the PVC's VAC field. Do not overwrite capacity or unrelated settings.
- Detect concurrent updates rather than overwriting them.
- On a conflict, re-read the PVC and CR, revalidate the action, and retry within a bounded limit.
- A newer class selection becomes the desired target; do not finish an old target before recognizing the new one.
- Do not copy resize's rule of retaining an earlier capacity target and deferring newer requests.
- Coordinate VAC updates and claim creation with resize, scaling, and workload replacement for the same group.
- Delay conflicting VAC actions, but continue safe observation and ordinary reconciliation.
- A failed or slow VAC operation must not stop unrelated Services, ConfigMaps, or safe workload updates.
- The existing `claimResizeStatusCAS()` guard protects resize initialization only; it is not a shared resize/VAC lock.
- Design an explicit shared coordination mechanism before implementation.

## Detailed Workflow

### Resolve and Validate

1. Resolve the effective storage field into the group and establish current generation/replicas.
2. For a persistence-disabled group, take no VAC action. For an unconfigured group, perform no live VAC writes, but retain any historical-template provisioning safeguard and its failure diagnostics.
3. Discover a served VAC API (`v1` preferred, beta fallback), with cached results, periodic refresh, and invalidation after API errors/upgrades.
4. Read the requested class and verify `driverName` against StorageClass provisioner where available. A bound PV's CSI driver may provide an additional check only when existing read permissions permit it; do not introduce broad PV access solely for optional validation.
5. Distinguish discovery failure, permission denial, absent class, disabled/dropped field, and driver mismatch. API existence does not prove CSI support; use separately qualified provisioning/modification configurations and native observations, not driver-name matching as a capability guarantee.

### Provision or Apply

1. Generate a new StatefulSet's primary template with the eligible resolved class, or without the field when unconfigured/unsupported.
2. Before scale-up, ensure required missing primary claims are created safely with the effective provisioning class, including an unset field after opt-out/fallback. A claim creation/ownership error may block that scale action, not unrelated resources.
3. List/verify active managed primary claims. Track missing claims and pending binding explicitly.
4. For an existing mismatched bound PVC and a managed non-empty target, issue a field-scoped optimistic patch to `spec.volumeAttributesClassName`. For unbound claims, follow binding recovery instead; never patch the reference. On conflict, re-read the PVC and CR, recheck ownership/phase/target, and retry within a bound.
5. If the spec already equals desired, do not patch again; observe provider progress. Never treat a successful patch as completion.
6. Verify the write response/fresh read retains the class. Cache/report a dropped field instead of entering an endless patch loop.
7. Preserve the live VAC field in existing StatefulSet templates during all ordinary diff/update paths.

### Observe and Aggregate

1. Read current/target class, native modification state, and conditions from all active claims.
2. Normalize each record using the completion and target-correlation rules.
3. Aggregate group status/condition, then parent condition, without losing unrelated conditions or resize status.
4. Emit transition-only Events and structured logs; skip no-op status patches.
5. Watch managed PVC changes and requeue after a bounded interval as fallback. Resume from observed state after controller restart.

## Failure Handling and Recovery

| Failure | Required behavior |
| --- | --- |
| API absent or feature disabled | Omit field when creating new claims, including safeguarded scale-up against historical templates; retain existing claims, report unsupported, continue unrelated work. |
| Class missing or known driver mismatch | Skip VAC application and create missing claims without VAC where safely possible. Existing unbound claims cannot be retroactively changed; report their original provisioning failure and re-evaluate after remediation. |
| Superseded target on an unbound claim | Report `PendingClassMismatch`, permit required consumer scheduling, and apply the latest managed target only after binding; no immutable reference patch or claim replacement. |
| Original unbound request cannot provision | Report `ProvisioningBlocked` with original-class diagnostics; repair original prerequisites where possible, otherwise require administrator intervention without destructive automatic recovery. |
| Opt-out/fallback with conflicting historical template | Precreate missing claims with no VAC; if the safeguard fails, block only the affected scale action and retain its diagnostic/Event. Do not clear existing PVC references. |
| Permission denied | Report authorization error, not class-not-found; retry after permission repair. |
| Conflict/throttling/transient API failure | Fresh-read conflict handling and capped jittered backoff; preserve partial progress. |
| Native `Infeasible` | Failed for that target; require a feasible class/environment change, no destructive fallback. |
| Provider modification error/cooldown | Preserve native reason/message and keep retryable progress observable; no automatic rollback or Pod restart. |
| Stalled or unknown progress | Report non-success and continue observation; elapsed time alone is not proof of unsupported CSI. |
| Unsafe ownership/name collision | No mutation/adoption; identify the claim and required administrator action. |
| Controller restart or superseded target | Re-read latest CR/PVCs; resume idempotently and do not publish stale target success. |

The operator retries its own transient API operations, not CSI RPCs. Once a bound PVC spec holds the desired class, external-resizer/driver retries own provider progress; observe their status without redundant patches. Native `Infeasible` requires a corrected feasible target or prerequisites, not an operator retry budget pretending to repair invalid parameters.

Discovery and early compatibility checks cannot guarantee that every driver will bind a requested claim. If unsupported behavior emerges during binding/modification, report the real failure; never delete/recreate the claim to hide it.

Retry timing, concurrency bounds, discovery refresh, and stall thresholds must be chosen against provider constraints during implementation. Reuse capped backoff mechanics, not resize's terminal retry budget for potentially long CSI modifications. Observation must not terminate permanently just because a provider is slow.

## Required Infrastructure Permissions

| Resource | Required verbs | Scope |
| --- | --- | --- |
| `persistentvolumeclaims` | `get`, `list`, `watch`, `patch` | Watched namespaces; ClusterRole for cluster-scoped installation. |
| `volumeattributesclasses` | `get`, `list`, `watch` | Cluster-scoped in both installation modes. |
| `persistentvolumeclaims` precreation | `create` only if precreation is approved | Watched namespaces; explicit addition to the minimum mutation design. |

Preserve existing StorageClass, Event, and CR-status permissions. No VAC mutation verbs and no PVC delete permission are needed for this feature. Update Kubebuilder markers, generated RBAC, cluster- and namespace-scoped Helm templates, and generation/post-processing. A namespace Role cannot grant access to cluster-scoped VAC objects; use the existing cluster-reader role/binding pattern.

Administrators control class creation and CR update rights because performance profiles affect cost and performance.

## Observability

`status.storage` and `StorageAttributesSynced` are the queryable source of progress. Events provide concise lifecycle history; logs carry group, namespace, PVC, target class, native state, and actionable error without credentials.

Emit Normal dispatch/completion Events and Warning unsupported/missing/incompatible/infeasible/failed/stalled Events on the affected MarkLogic CR. Gate emission by meaningful target/state/reason transitions, not by every retry.

Add explicit PVC-to-group watch mapping using verified workload association. The current group controller watches StatefulSets, Services, and Pods, not PVCs; its default update predicate ignores other types. Both watch registration and PVC-status-aware predicates must change. Parent aggregation must react to relevant child storage status changes rather than relying only on child spec updates.

## Reuse of the Existing Volume Resize Implementation

The resize implementation provides useful primitives and design patterns, but its helpers are typed to resize and are not a ready-made VAC controller. Extract small common helpers only where they preserve existing resize behavior.

| Existing implementation | Reuse decision | Required adaptation or restriction |
| --- | --- | --- |
| `processResizeSubmission()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L311-L415) | Reuse read/compare/field-scoped `MergeFrom` patch pattern. | Replace quantity mutation with VAC reference; add optimistic locking, fresh-read conflict retries, target/ownership revalidation. Current code marks a patch error and retries through resize phases, not a conflict-specific loop. |
| `patchResizeStatus()` and `transitionResizePhase()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L881-L911) | Reuse fresh-read status-subresource patch and transition timestamp patterns. | New storage types, no-op detection, condition merging, and status conflict handling; do not write `VolumeResizeStatus`. |
| `claimResizeStatusCAS()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L913-L938) | Reuse resource-version compare-and-set concept if cross-feature fencing needs persistence. | Existing guard checks only active resize status; it is not a reusable VAC/resize lock without extension. |
| `computeResizeRetryBaseDelaySeconds()` and `jitteredResizeRetryDelaySeconds()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1157-L1219) | Extract/reuse capped exponential backoff and jitter mathematics. | Keep separate VAC policy/timing. Do not import `scheduleRetryOrFail()`'s resize terminal budget or stop CSI observation. |
| `emitResizeEvent()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L940-L945) and `result.RequeueSoon()` in [result_helper.go](../../pkg/result/result_helper.go#L67) | Reuse recorder/context infrastructure and requeue utility. | VAC-specific reasons and transition deduplication; the recorder wrapper itself does not deduplicate. |
| `discoverPrimaryPVCs()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1532-L1604) | Adapt deterministic naming, stable ordering, missing/binding observations. | Current implementation uses individual `Get`s, quantity targets, zero-based ordinals, and can include auxiliary templates. It is not ownership-label validation or the required data-only list/watch selector. |
| `validateStorageClassExpansionAllowed()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1741-L1771) | Reuse deduplicated StorageClass lookup and authorization-error reporting pattern. | Validate provisioner/class driver instead of `allowVolumeExpansion`; VAC discovery/reads and CSI diagnostics are new. Expansion permission is not a VAC prerequisite. |
| `recalculatePVCProgress()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1122-L1140) | Reuse per-PVC aggregation approach. | Replace filesystem checkpoint counts with native class/target/state completion; implement new group/cluster conditions. |
| `generatePVCTemplate()` / `generateStatefulSetsParams()` in [statefulset.go](../../pkg/k8sutil/statefulset.go#L408-L435) | Reuse primary claim construction and persistence gating. | Add eligible resolved VAC and primary ownership metadata; keep additional templates untouched. Separate generated-object creation from existing-template normalization. |
| Resize tests in [volume_resize_validation_test.go](../../pkg/k8sutil/volume_resize_validation_test.go) | Reuse fake-client, recorder, partial-progress/retry test patterns. | Add discovery, native modification state, field isolation, and UID/non-disruption assertions. Existing resize tests do not prove VAC support. |

### Logic That Must Not Be Reused

1. `syncStatefulSetPVCTemplates()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1241-L1290) orphan-deletes the StatefulSet. `processStatefulSetSynchronization()` drives its recovery/recreation. Both violate the VAC-only no-recreation contract.
2. `processPodRestarts()`, filesystem checkpoint classification, readiness-after-remount, and resize outcome health gates are capacity-expansion recovery, not VAC modification semantics.
3. Quantity comparisons, shrink rejection, `allowVolumeExpansion` checks, sequential resize strategy, and deferred capacity snapshots do not govern VAC class changes.
4. The resize handler's early-return workflow must not be copied as a feature-error barrier. Integrate VAC outcomes so unsupported/failed modification does not stop ordinary reconciliation.

The existing [group handler](../../pkg/k8sutil/handler.go#L39-L46) returns immediately when a resize result reports `Completed()`. `result.RequeueSoon()` also reports `Completed()=true` in [result_helper.go](../../pkg/result/result_helper.go#L36-L45): this means the reconcile step returned a result, not that the storage operation succeeded. VAC waiting must accumulate a requeue deadline and continue safe ordinary reconciliation rather than copying that early-return pattern.

New logic is required for VAC discovery/version handling, ownership-safe data selection and watches, driver validation, native modification-status interpretation, immutable VAC template preservation, approved scale-up precreation, and parent condition aggregation.

## Testing Strategy

### Unit and Schema Tests

1. Cluster/standalone/embedded-group API scopes; nil, empty storage, explicit empty, inheritance and override independent of persistence.
2. RFC 1123 boundary/invalid names; generated deepcopy independence; matching native/Helm schemas and optional-field behavior.
3. New StatefulSet data template with/without VAC, disabled persistence, unchanged auxiliaries/Fluent Bit claims, unchanged Pod template.
4. Fake discovery for GA, beta, API absence, permission failure, cache refresh and dropped-field handling.
5. Class existence/provisioner checks, ownership collision/legacy claim selection, idempotency, optimistic conflicts and bounded retries.
6. Native `Pending`, `InProgress`, `Infeasible`, condition errors, mixed states, stale targets, missing claims, no-active-target behavior and transition-only Events.
7. Pending PVC phase versus native modification Pending; no reference patches on unbound claims, `PendingClassMismatch`/`ProvisioningBlocked`, and independent provisioning/modification qualification.

### Controller and Integration Tests

1. Initial provisioning and scale-up with the currently effective class, including after live modification.
2. Precreation races, non-zero ordinals, `WaitForFirstConsumer`, retained/reactivated claims, and crash recovery before replica increase.
3. Direct PVC mutation with unchanged StatefulSet/PVC/Pod UIDs, Pod template hash and restart counts; repeated reconciliation without immutable-field errors.
4. Unsupported deployment continues without VAC; failed/stalled modification does not block unrelated resources or existing resize.
5. Both Helm scope modes with least-privilege PVC/VAC permissions; safe group watch mapping and parent aggregation.
6. Concurrent resize/VAC requests preserve capacity validation, field isolation, status and workload lifecycle semantics.
7. Real API-server rejection of unbound VAC reference changes on qualified beta/GA lanes; A-to-B supersession before binding, missing original class remediation, and consumer scheduling without immutable patch loops or claim deletion.
8. Scale-up after opt-out/unsupported fallback with a non-empty historical template creates new claims with the field absent; retained/precreated claims remain unchanged when unmanaged. Safeguard failures block only the affected scale action and remain observable.
9. VAC-aware external-provisioner creates the intended initial profile; external-resizer/CSI modifies a bound volume independently. Verify complete and omitted-parameter profiles against documented driver defaults.

Use envtest to prove API-server schema rejection and reconciliation, not CSI completion. Use a VAC-capable integration/E2E lane to verify provider-visible profile changes and non-disruption; retain an unsupported-cluster lane. Review existing Kubernetes test pins rather than assuming they expose VAC.

### Acceptance

The requirement is complete only when the same declarative field governs creation-time selection for new initial/scale-up claims, post-binding convergence of superseded or reused claims, in-place live changes, unsupported-environment behavior, and correctly correlated observable completion without VAC-driven workload replacement. Opt-out/fallback must not leak a historical template class into newly created scale-up claims. Approval of the creation-time semantics and evidence for both CSI paths are acceptance gates. Partial implementation must not be advertised as fulfilling the whole design.

## Open Design and Qualification Items

1. Verify the 1.32 beta minimum and exact EKS/AKS/GKE/OpenShift configurations, including separate external-provisioner/external-resizer/CSI versions, feature settings, and parameter defaults; do not replace the baseline with 1.34 without approval.
2. Approve and validate precreation or another non-disruptive creation-time class-selection mechanism, including transitions to unset, claim retention/ownership, unbound supersession/recovery, and the additional PVC `create` permission. Confirm the creation-time versus actual provisioning-instant requirement before implementation.
3. Finalize legacy ownership migration, cross-feature fencing, status schema, refresh/retry/concurrency/stall thresholds, and parent update predicates before implementation.

The sections above describe the full feature design. The Day-1 POC below implements only initial class selection; it does not implement the remaining workflows or change volume-resize behavior.

## Day-1 POC

### Implemented Behavior

- Use `spec.storage.volumeAttributesClassName` on a cluster or standalone group.
- Cluster-owned groups inherit the cluster class unless they select another class or explicitly opt out with `""`.
- Inheritance is independent of group persistence overrides.
- A selected non-empty class is copied into the primary `datadir` PVC template.
- Kubernetes uses that template when creating the group's new data PVCs.
- Without an effective class, the operator leaves the PVC field unset; ordinary StorageClass/driver defaults apply.
- Auxiliary templates and Pod templates remain unchanged. Persistence-disabled groups receive no primary claim.
- Native and Helm CRDs accept the field at cluster, embedded-group, and standalone-group scopes.

### Prerequisites and Limits

- Deploy an operator image built from this checkout and install the updated CRDs. Existing release images do not contain this POC.
- The API server must retain the PVC VAC field, with the relevant API and feature configuration enabled.
- Use an existing VAC and a compatible CSI StorageClass, driver, and external-provisioner.
- Beta support on Kubernetes 1.32 and specific provider configurations still require qualification.
- This POC does not discover support, validate VAC existence/driver compatibility, or fall back automatically on unsupported clusters.
- Do not change the class on an already deployed workload in this POC. Its claim template is immutable.
- Live PVC modification, post-change scale-up precreation, progress status, and parent aggregation remain unimplemented.
- Unit tests verify generated objects and schemas, not successful disk provisioning by a real CSI driver.

### Manual Verification

The [sample](../../config/samples/volume-attributes-class.yaml) contains two fresh clusters: one selecting `ml-standard` and one without a VAC selection.

1. Replace `csi-storage` with an existing compatible StorageClass and `ml-standard` with an existing VAC for that driver.
2. Install the updated CRDs and deploy the operator built from this checkout.
3. Create the sample namespace and deploy both clusters:

```sh
kubectl create namespace vac-poc
kubectl apply -f config/samples/volume-attributes-class.yaml
```

4. After the StatefulSets create their claims, inspect the three configured data PVCs:

```sh
kubectl get pvc -n vac-poc datadir-vac-dnode-0 datadir-vac-dnode-1 datadir-vac-enode-0 \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.volumeAttributesClassName}{"\n"}{end}'
```

Each line must show `ml-standard`. Inspect the unconfigured claim separately:

```sh
kubectl get pvc -n vac-poc datadir-vac-default-node-0 -o yaml
```

Its `spec` must not contain `volumeAttributesClassName`, unless external admission independently injects a value. Confirm all claims bind; if not, inspect PVC Events and CSI provisioning logs rather than treating the presence of the field as successful provisioning.