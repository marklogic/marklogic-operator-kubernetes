# Design Spec: MarkLogic Operator Volume Attributes Management

## Introduction

### Overview

This specification defines one end-to-end requirement: administrators can declaratively select and change a Kubernetes `VolumeAttributesClass` (VAC) for MarkLogic data volumes, from initial provisioning through live reconciliation and observable completion, without operator-caused workload disruption.

The API, provisioning, in-place updates, compatibility handling, and observability are parts of the same controller capability, not separate feature contracts. This document follows the structure of the [volume-resize specification](volume%20resize.md) while distinguishing performance modification from capacity expansion.

### Goal

1. Select storage performance profiles for primary and additional persistent volumes through cluster defaults and group overrides, independently per claim template.
2. Create new primary and additional PVCs, including scale-up claims, with each template's effective class observed at claim creation; reconcile later changes after binding.
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

1. Expose optional `volumeAttributesClassName` as `*string` under the existing `spec.persistence` on clusters, standalone groups, and embedded per-group overrides.
2. Preserve existing whole-object persistence overrides: a group without `persistence` inherits cluster persistence, including VAC; a supplied group object replaces it without field merging.
3. Allow an independent `spec.volumeAttributesClassName` in each existing `additionalVolumeClaimTemplates` entry. Preserve existing whole-list group override semantics; primary persistence does not supply a default to additional templates.
4. Validate primary class references as RFC 1123 subdomain names of at most 253 characters; allow nil and empty. Additional references use the existing native PVC schema and Kubernetes PVC admission validation. Normalize nil/empty selections to an omitted field in generated PVCs.
5. Regenerate deepcopy, native CRDs, and Helm CRDs, and publish API documentation.

#### Provisioning and Live Reconciliation

1. Populate the primary `datadir` template from `persistence.volumeAttributesClassName` and each additional template from its own `spec.volumeAttributesClassName`; leave the field unset for unconfigured templates.
2. Create new scale-up PVCs with each template's validated provisioning class observed at creation, including a non-empty class after live changes or an unset field after opt-out. Block the affected provisioning action when an explicitly requested class cannot be honored; do not substitute driver defaults.
3. Select primary and explicitly declared additional claims through template identity, active ordinals, and verified workload association. Never patch unrelated claims or externally supplied PVCs merely referenced through `additionalVolumes`.
4. Patch mismatched bound PVC fields directly, idempotently, with bounded conflict retries and fresh reads. Observe unbound claims without attempting immutable reference changes.
5. Suppress VAC-only immutable claim-template drift without concealing unrelated persistence changes.

#### Capability and Progress Reporting

1. Use cached API discovery; check class existence and StorageClass provisioner/CSI driver compatibility where possible.
2. Report absent APIs, disabled features, missing classes, unsupported drivers, and modification errors through conditions, warnings, and Events.
3. When an explicit non-empty VAC is unsupported, missing, incompatible, or cannot be validated, block affected new provisioning and report a non-success condition. Continue unconfigured provisioning, existing workloads, required consumer scheduling, and unrelated reconciliation; never silently drop an explicit request.
4. Watch managed PVCs and expose desired/current class and normalized progress under `status.storage`.
5. Set `StorageAttributesSynced=True` only when every targeted primary or additional PVC has applied its own current desired class. Emit dispatch/completion/failure Events only on transitions.

### Non-Functional Requirements

#### Data Integrity and Availability

A VAC-only update must preserve StatefulSet UID, PVC UID, Pod UID, restart counts, and Pod template/hash. It must not delete PVCs/PVs, recreate StatefulSets, roll Pods, or invoke filesystem-resize recovery. Provider-specific latency, temporary performance effects, and unrelated Pod failures remain environmental limitations; the operator does not promise they cannot occur.

#### Recovery and Backward Compatibility

Reconstruct progress from the latest CR and PVC state after restart. Preserve partial progress, use bounded API retries, and avoid writing status or emitting Events on unchanged observations. Unconfigured resources retain their prior behavior. Feature errors must not stop Services, ConfigMaps, normal workload reconciliation, or existing resize/dynamic-host handling.

### Scope and Non-Goals

In scope: active operator-managed primary claims and declared additional claim templates, independent per-template profiles, initial provisioning, scale-up, live VAC changes, compatibility checks, and group/cluster observability.

Out of scope: adding/removing/renaming claim templates on existing StatefulSets, modification of externally supplied PVCs, non-PVC volumes, StorageClass migration, capacity resizing, automatic provider rollback, VAC resource administration, and new user-configurable scheduling/strategy fields. A Fluent Bit-specific claim is eligible only when explicitly declared as an additional template; logs sharing datadir naturally share that volume's performance profile.

Retained claims outside the active ordinal range are not modified; reactivated claims become targets again. Reactivation reuses existing storage, not new provisioning: bound claims converge to the latest managed class, while unbound claims follow the binding recovery rules. Opt-out leaves reused claims unchanged. The controller must report missing active claims rather than silently excluding them from completion.

## API and User Experience Contract

### Trigger Model and Spec Fields

The administrator updates the CR's per-template class references. Cluster reconciliation resolves persistence and additional templates into each child `MarklogicGroup`; group reconciliation owns PVC changes and detailed status. A standalone group uses its own configuration directly.

| Field | Type | Meaning |
| --- | --- | --- |
| `MarklogicCluster.spec.persistence.volumeAttributesClassName` | Optional `*string` | Primary volume selection within cluster persistence. |
| `MarklogicCluster.spec.markLogicGroups[i].persistence.volumeAttributesClassName` | Optional `*string` | Primary selection within a complete per-group persistence override. |
| `MarklogicGroup.spec.persistence.volumeAttributesClassName` | Optional `*string` | Standalone primary selection or resolved child value. |
| `spec.additionalVolumeClaimTemplates[i].spec.volumeAttributesClassName` | Optional `*string` | Independent selection for this additional template on clusters or standalone groups. |
| `MarklogicCluster.spec.markLogicGroups[i].additionalVolumeClaimTemplates[j].spec.volumeAttributesClassName` | Optional `*string` | Selection within the replacement per-group additional-template list. |

VAC configuration belongs to the existing persistence/PVC definitions. `status.storage` is the selected observation location for the full implementation, not a second configuration API or an already implemented POC field.

#### API Placement Decision

Keep the primary field at `spec.persistence.volumeAttributesClassName`. The existing [Persistence type](../../api/v1/common_types.go#L40) requires `size` and includes provisioning settings and resize strategy. The [group parameter resolver](../../pkg/k8sutil/marklogicServer.go#L310) replaces the complete persistence object when a group supplies an override; it does not merge individual fields.

This placement keeps the existing override contract: users repeat enabled/size/StorageClass and any other required settings in a profile-only group override. Group overrides replace complete objects or lists. Additional volumes use native PVC fields. Capacity remains managed by the resize feature.

| Embedded group value | Effective class |
| --- | --- |
| Omitted `persistence` | Inherit the complete cluster persistence object, including its class. |
| Complete `persistence` with non-empty class | Use this group's persistence settings and primary class. |
| Complete `persistence` with omitted/nil/empty class | No primary VAC selection; do not inherit the cluster class. |

Removing the entire group persistence override resumes cluster inheritance. Removing only its class leaves the override in place and stops primary VAC management. `persistence: {}` remains invalid because `size` is required. Existing manifests without VAC remain valid; there is no class default.

Here, a **PVC template (PersistentVolumeClaim template)** is a blueprint for a separate persistent volume claim per Pod replica, defining settings such as size, StorageClass, access modes, and VAC. The operator generates the primary `datadir` PVC template from `persistence` and takes additional PVC templates from entries in `additionalVolumeClaimTemplates`. These definitions populate the StatefulSet's `spec.volumeClaimTemplates`. They are not Helm templates, Pod templates, or VolumeAttributesClass resources.

For example, an additional PVC template named `logsdir` for StatefulSet `dnode` with replicas at ordinals 0 and 1 defines claims named `logsdir-dnode-0` and `logsdir-dnode-1`. The template is the reusable definition; those PVCs are the individual storage requests created from it.

The `additionalVolumeClaimTemplates` list is inherited in full from the cluster when the group omits that field. If the group supplies `additionalVolumeClaimTemplates`, its entire PVC-template list replaces the cluster's list, including each PVC template's class; entries are not merged by name. An omitted/nil/empty additional class means no management for that PVC template, never inheritance from the primary or another PVC template. Removing an entry is not an opt-out operation: existing StatefulSet PVC-template add/remove/rename restrictions still apply.

For each PVC template, nil/empty class means no live VAC management; newly created claims omit the field, with a provisioning safeguard when the historical StatefulSet PVC template differs. Optional non-nullable schema fields prune explicit null to absence and decode to nil.

Kubebuilder markers must enforce the RFC 1123 pattern and `MaxLength=253` at all three scopes. Native and Helm schemas must remain identical; existing manifests remain valid.

```yaml
spec:
  persistence:
    enabled: true
    size: 20Gi
    storageClassName: gp3
    volumeAttributesClassName: ml-gp3-standard
  additionalVolumeClaimTemplates:
  - metadata:
      name: logsdir
    spec:
      accessModes: [ReadWriteOnce]
      storageClassName: gp3
      volumeAttributesClassName: ml-gp3-logs
      resources:
        requests:
          storage: 10Gi
  additionalVolumeMounts:
  - name: logsdir
    mountPath: /var/opt/MarkLogic-logs
  markLogicGroups:
  - name: dnode
    replicas: 2
    isBootstrap: true
    # Inherits primary ml-gp3-standard and additional logsdir ml-gp3-logs.
  - name: query
    persistence:
      enabled: true
      size: 20Gi
      storageClassName: gp3
      volumeAttributesClassName: ml-gp3-premium
```

The query group still inherits the additional logs template independently of its primary persistence override. The referenced classes and their driver-specific parameters are managed externally. Persistence-disabled groups create no primary claim but still provision and manage declared additional claim templates. `additionalVolumes` entries using external PVC references are not additional claim templates and are not VAC targets.

### Status Contract

Expose the group observation under `MarklogicGroup.status.storage.volumeAttributesClass`. This is the selected public schema for the full implementation, not an already implemented POC field or resize bookkeeping reused under another name.

| Field | Type | Meaning |
| --- | --- | --- |
| `observedGeneration` | Integer | Group generation represented by the observation. |
| `state` | Enum | `Pending`, `InProgress`, `Completed`, `Failed`, `Stalled`, or `Unknown`. |
| `reason`, `message` | String | Aggregate diagnostic summary. |
| `pvcs` | Array | Stable name-ordered records for active target PVCs, including missing claims. |
| `pvcs[].name` | String | PVC identity within the group namespace. |
| `pvcs[].templateName` | String | Primary `datadir` or declared additional template identity. |
| `pvcs[].desiredClassName` | String | Latest effective class for this PVC's template; different templates may have different targets. |
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

The parent cluster aggregates managed groups with at least one configured eligible template, including groups whose primary persistence is disabled but whose additional templates are configured. Missing or stale child status is Unknown, not success. Unconfigured/opted-out templates are excluded independently. With zero active targets, use a non-success `NoActivePVCs` observation rather than claiming a provider operation completed.

### Removal and Manual Intervention

When a template's effective class becomes nil/empty, stop live VAC management for that template and leave its existing PVC class fields untouched. Once a class has been applied, Kubernetes forbids clearing the reference to nil/empty; opt-out is not provider rollback or cancellation. Removing an entire group persistence or additional-template list override resumes cluster inheritance for that object/list, not field-level merging.

New claims created after opt-out must omit the field even if the immutable historical StatefulSet template still contains a class. Continue the provisioning safeguard described below for that template mismatch. Remove that template's active modification observation when no provisioning safeguard error remains; otherwise retain a non-success diagnostic/Event without attempting live modification. Other configured templates continue to participate in aggregation. Already-created or retained claims keep their previous reference, including when binding is still pending.

Administrators observe group/cluster status, describe the CR and PVCs, and fix missing classes, permissions, driver configuration, or infeasible requests. Retry recoverable errors after remediation. A new class selection supersedes the desired target immediately, but an unbound claim cannot receive it until binding completes. No separate imperative operation or automatic PVC migration is required.

## Architectural Decisions

This section describes the full implementation approach. Current implementation scope is recorded separately in the Day-1 POC section.

### Configuration and Inheritance

The primary volume will use `spec.persistence.volumeAttributesClassName`; additional volumes will independently use `additionalVolumeClaimTemplates[].spec.volumeAttributesClassName`. A PVC template defines one claim per Pod replica. Groups inherit the cluster's persistence object and template list unless they supply a complete replacement. An omitted or empty class within that override leaves the template unmanaged. Opting out stops future changes but leaves existing PVC references and settings intact. Additional volumes remain eligible when primary persistence is disabled.

### Existing Volume Changes

The operator will patch only `spec.volumeAttributesClassName` on verified bound PVCs, leaving Kubernetes and the CSI driver to apply the change. Capacity and unrelated settings remain unchanged. Existing StatefulSet templates retain their historical class because those templates are immutable. VAC-only changes will not replace workloads or claims, or restart Pods. Conflicts will trigger fresh reads before retrying.

### Initial Provisioning and Scale-Up

New StatefulSets will use the validated class for each volume. During scale-up, matching desired and historical classes allow normal StatefulSet provisioning. For a mismatch, the operator will create missing new-replica claims with current settings before increasing replicas, omitting the class after opt-out. Required claims must be created or verified for reuse; failures block only that scale action. Retries preserve partial progress, starting ordinals, and retention. The operator will not wait for binding because `WaitForFirstConsumer` may require the new Pod to schedule first.

### Changes Before Binding

A claim records the class selected at creation. If A changes to B before binding, the operator will preserve the claim, report `PendingClassMismatch`, and allow its Pod to schedule. Once bound, it will converge to the latest valid target. An original request that cannot provision will produce `ProvisioningBlocked` and require prerequisite repair or administrator recovery, not automatic claim replacement. Scheduling delays alone are not VAC failures.

### Claim Ownership and Reuse

The operator will verify the group-controlled StatefulSet, template/ordinal association, claim identity, owner references, and compatible storage settings before mutation or reuse. Names and metadata alone are insufficient. Legacy ownerless claims also require a verified managed Pod reference and fresh identity check. Verified retained claims will be reused without changing retention, then converge when active and bound. Ambiguous claims block the affected action; inactive, external, undeclared, and terminating claims remain untouched.

### Coordination With Other Storage Operations

Conflicting VAC, resize, scale-up preparation, and workload-replacement actions will be serialized per group using a shared operation record in status. It will retain the operation, target generation, PVC identities, and dispatch phase for restart recovery, protected by resource-version checks. Resize and VAC keep separate targets and progress. Observation, necessary scheduling, and unrelated work continue; binding waits do not hold the dispatch guard. Initial limits are one in-flight PVC per group and four operator-wide.

### Platform Validation

The operator will validate the requested class, StorageClass/CSI driver association, and qualified platform capability. GA is preferred; qualified beta is used only when GA is absent. Provisioning and live modification require separate qualification. Invalid, unsupported, or unverifiable explicit requests will block affected new provisioning or scale-up rather than silently use driver defaults. Existing workloads, required scheduling, unconfigured volumes, and unrelated groups continue. Transient validation failures remain retryable. Administrators maintain the profiles; the operator references them.

### Progress and Completion

PVC watches will drive `status.storage.volumeAttributesClass` and group/cluster conditions. Completion requires native Kubernetes status confirming that every active managed PVC has applied its latest target for the observed generation. API writes, Pod readiness, stale observations, or zero targets cannot establish success. Events will reflect meaningful transitions; Kubernetes/CSI owns provider retries.

Initial internal defaults are 30-second fallback observation, five-minute discovery refresh, and jittered API backoff from one to 60 seconds. After 30 minutes without progress, `Stalled` is diagnostic, not terminal or destructive. These limits require provider qualification.

## Detailed Workflow

### Resolve Configuration

Read the latest group configuration and resolve the desired class for each primary and additional PVC template. Exclude the primary template when persistence is disabled. Validate non-empty selections against the class, driver, and platform requirements. Record validation failures and skip affected actions while continuing unrelated work; unconfigured templates do not require VAC validation.

### Prepare New Claims

For a new StatefulSet, generate templates with their validated classes. For scale-up, preserve existing templates and create missing new-replica claims only where the desired class differs from the historical template. Omit the class after opt-out. Use the shared operation guard for conflicting actions and verify claim ownership before reuse. Increase replicas after all required claims are prepared, without waiting for binding or holding the guard across that wait. Preparation failures block the affected scale action.

### Update Bound Claims

Find active claims and verify their workload association. Under the shared operation guard, patch only the VAC field on bound PVCs whose reference differs from a valid non-empty target. Leave unbound claims unchanged and observe their binding. Re-read and revalidate after conflicts. Confirm that the saved field retains the requested class; report a dropped field instead of repeatedly patching it. When the reference already matches, observe progress without another write.

### Observe and Report

Read native PVC modification status and correlate it with each template's latest target. Update per-PVC progress, the group condition, and the parent cluster condition using the Status Contract, preserving unrelated status. Publish Events only for meaningful transitions and skip unchanged status writes. Continue through PVC watches and fallback requeues; after a restart, resume from current resources and the stored operation record.

## Failure Handling and Recovery

| Failure | Required behavior |
| --- | --- |
| API absent, feature disabled, or capability unqualified for an explicit request | Block affected new workload/claim creation or scale-up, retain existing claims/workloads, and report unsupported/unknown. Continue unconfigured provisioning, required consumer scheduling, and unrelated work; do not silently omit the requested field. |
| Class missing or known driver mismatch | Block the affected explicit request and report the specific validation failure; do not provision a default-profile substitute. Existing unbound claims retain their original references; diagnose their original provisioning prerequisites and re-evaluate after remediation. |
| Superseded target on an unbound claim | Report `PendingClassMismatch`, permit required consumer scheduling, and apply the latest managed target only after binding; no immutable reference patch or claim replacement. |
| Original unbound request cannot provision | Report `ProvisioningBlocked` with original-class diagnostics; repair original prerequisites where possible, otherwise require administrator intervention without destructive automatic recovery. |
| Opt-out with conflicting historical template | Precreate missing claims with no VAC; if the safeguard fails, block only the affected scale action and retain its diagnostic/Event. Do not clear existing PVC references. |
| Permission denied or discovery timeout | Report authorization/validation-unknown, not class-not-found/API-absent. Retry with bounded backoff and block affected explicit new provisioning rather than substituting defaults; continue unrelated reconciliation and required scheduling. |
| Conflict/throttling/transient API failure | Fresh-read conflict handling and capped jittered backoff; preserve partial progress. |
| Native `Infeasible` | Failed for that target; require a feasible class/environment change, no destructive fallback. |
| Provider modification error/cooldown | Preserve native reason/message and keep retryable progress observable; no automatic rollback or Pod restart. |
| Stalled or unknown progress | Report non-success and continue observation; elapsed time alone is not proof of unsupported CSI. |
| Unsafe ownership/name collision | No mutation/adoption; identify the claim and required administrator action. |
| Controller restart or superseded target | Re-read latest CR/PVCs; resume idempotently and do not publish stale target success. |

The operator retries its own transient API operations, not CSI RPCs. Once a bound PVC spec holds the desired class, external-resizer/driver retries own provider progress; observe their status without redundant patches. Native `Infeasible` requires a corrected feasible target or prerequisites, not an operator retry budget pretending to repair invalid parameters.

Discovery and early compatibility checks cannot guarantee that every driver will bind a requested claim. If unsupported behavior emerges during binding/modification, report the real failure; never delete/recreate the claim to hide it.

Use the initial internal timing and concurrency limits selected in Architectural Decisions, then qualify them against provider constraints. Reuse capped backoff mechanics, not resize's terminal retry budget for potentially long CSI modifications. Observation must not terminate permanently just because a provider is slow.

## Required Infrastructure Permissions

| Resource | Required verbs | Scope |
| --- | --- | --- |
| `persistentvolumeclaims` | `get`, `list`, `watch`, `patch` | Watched namespaces; ClusterRole for cluster-scoped installation. |
| `volumeattributesclasses` | `get`, `list`, `watch` | Cluster-scoped in both installation modes. |
| `persistentvolumeclaims` precreation | `create` | Watched namespaces; required by the selected conditional-precreation design. |

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
| `claimResizeStatusCAS()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L913-L938) | Reuse resource-version compare-and-set concept for the shared durable operation guard. | Existing guard checks only active resize status; it is not a reusable VAC/resize lock without extension. |
| `computeResizeRetryBaseDelaySeconds()` and `jitteredResizeRetryDelaySeconds()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1157-L1219) | Extract/reuse capped exponential backoff and jitter mathematics. | Keep separate VAC policy/timing. Do not import `scheduleRetryOrFail()`'s resize terminal budget or stop CSI observation. |
| `emitResizeEvent()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L940-L945) and `result.RequeueSoon()` in [result_helper.go](../../pkg/result/result_helper.go#L67) | Reuse recorder/context infrastructure and requeue utility. | VAC-specific reasons and transition deduplication; the recorder wrapper itself does not deduplicate. |
| `discoverPrimaryPVCs()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1532-L1604) | Adapt deterministic naming, stable ordering, missing/binding observations, and declared additional-template enumeration. | Current implementation uses individual `Get`s, quantity targets, and zero-based ordinals. Add per-template class targets, starting ordinals, ownership checks, and watch mapping. |
| `validateStorageClassExpansionAllowed()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1741-L1771) | Reuse deduplicated StorageClass lookup and authorization-error reporting pattern. | Validate provisioner/class driver instead of `allowVolumeExpansion`; VAC discovery/reads and CSI diagnostics are new. Expansion permission is not a VAC prerequisite. |
| `recalculatePVCProgress()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1122-L1140) | Reuse per-PVC aggregation approach. | Replace filesystem checkpoint counts with native class/target/state completion; implement new group/cluster conditions. |
| `generatePVCTemplate()` / `generateStatefulSetsParams()` in [statefulset.go](../../pkg/k8sutil/statefulset.go#L408-L435) | Reuse primary claim construction and persistence gating; deep-copy additional templates. | Resolve primary VAC from persistence and additional VACs from their own native fields; normalize empty references without mutating the CR. Preserve existing template VACs by name. |
| Resize tests in [volume_resize_validation_test.go](../../pkg/k8sutil/volume_resize_validation_test.go) | Reuse fake-client, recorder, partial-progress/retry test patterns. | Add discovery, native modification state, field isolation, and UID/non-disruption assertions. Existing resize tests do not prove VAC support. |

### Logic That Must Not Be Reused

1. `syncStatefulSetPVCTemplates()` in [volume_resize_validation.go](../../pkg/k8sutil/volume_resize_validation.go#L1241-L1290) orphan-deletes the StatefulSet. `processStatefulSetSynchronization()` drives its recovery/recreation. Both violate the VAC-only no-recreation contract.
2. `processPodRestarts()`, filesystem checkpoint classification, readiness-after-remount, and resize outcome health gates are capacity-expansion recovery, not VAC modification semantics.
3. Quantity comparisons, shrink rejection, `allowVolumeExpansion` checks, sequential resize strategy, and deferred capacity snapshots do not govern VAC class changes.
4. The resize handler's early-return workflow must not be copied as a feature-error barrier. Integrate VAC outcomes so unsupported/failed modification does not stop ordinary reconciliation.

The existing [group handler](../../pkg/k8sutil/handler.go#L39-L46) returns immediately when a resize result reports `Completed()`. `result.RequeueSoon()` also reports `Completed()=true` in [result_helper.go](../../pkg/result/result_helper.go#L36-L45): this means the reconcile step returned a result, not that the storage operation succeeded. VAC waiting must accumulate a requeue deadline and continue safe ordinary reconciliation rather than copying that early-return pattern.

New logic is required for VAC discovery/version handling, ownership-safe data selection and watches, driver validation, native modification-status interpretation, immutable VAC template preservation, selected scale-up precreation, the shared operation guard, and parent condition aggregation.

## Testing Strategy

### Unit and Schema Tests

1. Cluster/standalone/embedded-group API scopes; omitted persistence, whole-object overrides, explicit empty classes, whole-list additional inheritance/override, and no primary-to-additional VAC inheritance.
2. RFC 1123 boundary/invalid names; generated deepcopy independence; matching native/Helm schemas and optional-field behavior.
3. Independent primary/additional classes on new StatefulSets, omitted/empty additional classes, additional provisioning with disabled primary persistence, template deep-copy isolation, and unchanged Pod templates.
4. Fake discovery for GA, beta, API absence, permission failure, cache refresh and dropped-field handling.
5. Class existence/provisioner checks, ownership collision/legacy claim selection, idempotency, optimistic conflicts and bounded retries.
6. Native `Pending`, `InProgress`, `Infeasible`, condition errors, mixed states, stale targets, missing claims, no-active-target behavior and transition-only Events.
7. Pending PVC phase versus native modification Pending; no reference patches on unbound claims, `PendingClassMismatch`/`ProvisioningBlocked`, and independent provisioning/modification qualification.

### Controller and Integration Tests

1. Initial provisioning and scale-up with different effective classes across primary and multiple additional templates, including after live modification and with primary persistence disabled.
2. Precreation races, non-zero ordinals, `WaitForFirstConsumer`, retained/reactivated claims, and crash recovery before replica increase.
3. Direct PVC mutation with unchanged StatefulSet/PVC/Pod UIDs, Pod template hash and restart counts; repeated reconciliation without immutable-field errors.
4. Unsupported/unverifiable explicit VAC requests block affected new provisioning without default-profile substitution; unconfigured resources, existing workloads, required scheduling, and unrelated reconciliation continue. Failed/stalled modification remains observable and conflicting resize actions wait on the shared guard.
5. Both Helm scope modes with least-privilege PVC/VAC permissions; safe group watch mapping and parent aggregation.
6. Concurrent resize/VAC requests preserve capacity validation, field isolation, status and workload lifecycle semantics.
7. Real API-server rejection of unbound VAC reference changes on qualified beta/GA lanes; A-to-B supersession before binding, missing original class remediation, and consumer scheduling without immutable patch loops or claim deletion.
8. Scale-up after opt-out with a non-empty historical template creates new claims with the field absent; unsupported explicit selections block affected scale-up instead. Retained/precreated claims remain unchanged when unmanaged. Safeguard failures block only the affected scale action and remain observable.
9. VAC-aware external-provisioner creates the intended initial profile; external-resizer/CSI modifies a bound volume independently. Verify complete and omitted-parameter profiles against documented driver defaults.

Use envtest to prove API-server schema rejection and reconciliation, not CSI completion. Use a VAC-capable integration/E2E lane to verify provider-visible profile changes and non-disruption; retain an unsupported-cluster lane. Review existing Kubernetes test pins rather than assuming they expose VAC.

### Acceptance

The requirement is complete only when the primary persistence field and independent additional-template fields govern creation-time selection for new initial/scale-up claims, post-binding convergence of superseded or reused claims, in-place live changes, explicit-request validation, and correctly correlated observable completion without VAC-driven workload replacement. Opt-out must not leak a historical template class into newly created scale-up claims, and an unsupported explicit request must not produce a default-profile substitute. One template's configuration must never become another template's target. Evidence for the selected creation-time semantics, shared coordination, ownership safeguards, non-disruption, and both CSI paths is required. Partial implementation must not be advertised as fulfilling the whole design.

## Implementation and Qualification Gates

1. Implement live PVC modification, strict explicit-request validation, ownership verification, the shared durable operation guard, native-status watches, and group/cluster aggregation according to the selected decisions. Initial provisioning and scale-up POC behavior alone do not complete the feature.
2. Qualify conditional precreation against a real API server and CSI driver: least-privilege PVC `create`, retention, partial failures, concurrent updates, restart recovery, non-zero ordinals, and `WaitForFirstConsumer` without a binding barrier.
3. Prove creation-time selection followed by post-binding convergence and verify that unrepairable unbound requests remain nondestructive and actionable. Publish the administrator prerequisite-repair/data-recovery procedure; do not introduce automatic replacement.
4. Qualify the Kubernetes 1.32 beta target and exact EKS/AKS/GKE/OpenShift configurations before advertising support. Verify provisioning and live modification separately, parameter defaults, strict handling of explicit invalid requests, and the initial concurrency/timing limits.
5. Test legacy association, simultaneous resize/VAC/scaling requests, conflicts/restarts, non-disruption, per-template target correlation, and parent aggregation. Publish the selected API semantics and controlled migration guidance with the release evidence.

The sections above describe the full feature design. The POC below implements initial class selection and creation-time selection for new scale-up claims; it does not implement the remaining workflows or change volume-resize behavior.

## Day-1 POC

### Implemented Behavior

- Use `spec.persistence.volumeAttributesClassName` for the primary claim on a cluster or standalone group.
- Cluster-owned groups inherit the entire cluster persistence object unless they supply a complete replacement. A replacement's omitted/empty class does not inherit the cluster class.
- Use each `additionalVolumeClaimTemplates[].spec.volumeAttributesClassName` for an independent additional-volume class. Group lists inherit or replace as a whole, independently of primary persistence.
- A selected non-empty class is copied into the primary `datadir` PVC template.
- Kubernetes uses that template when initially creating the group's data PVCs.
- Existing StatefulSets retain the live template's class; changing the selection does not update an immutable claim template.
- On scale-up, compare desired and historical classes by template name. For every mismatch, precreate that template's missing new-ordinal claims with its current class before increasing replicas. An empty selection omits the VAC field.
- Precreated claims use the current persistence settings and template annotations, workload selector labels, and a `marklogic.progress.com/storage-group-uid` identity annotation. They do not gain a deletion-causing owner reference.
- Existing active claims remain unchanged. Verified retained or previously precreated claims are reused without changing their VAC, including while Pending.
- Claim creation or verification errors prevent replica increase. Precreated claims do not have to bind first, so `WaitForFirstConsumer` can schedule the new Pods.
- Without an effective class, the operator leaves the PVC field unset; ordinary StorageClass/driver defaults apply.
- Additional templates preserve their own fields and profiles, never the primary class; empty references are omitted on generated copies. Existing StatefulSets retain historical VAC values for all templates. Pod templates remain unchanged by VAC-only updates.
- Persistence-disabled groups receive no primary claim, but declared additional templates still participate in provisioning and scale-up.
- Native and Helm CRDs accept the field at cluster, embedded-group, and standalone-group scopes.

### Prerequisites and Limits

- Deploy an operator image built from this checkout and install the updated CRDs. Existing release images do not contain this POC.
- The API server must retain the PVC VAC field, with the relevant API and feature configuration enabled.
- Use an existing VAC and a compatible CSI StorageClass, driver, and external-provisioner.
- Beta support on Kubernetes 1.32 and specific provider configurations still require qualification.
- This POC does not discover support, validate VAC existence/driver compatibility, or fall back automatically on unsupported clusters.
- Install the updated native or Helm RBAC, which grants PVC `create` permission for scale-up precreation.
- A changed class affects only newly created scale-up claims; it does not modify existing volumes.
- Preexisting claims without a matching group UID annotation or verified group/StatefulSet owner reference block precreation-based scale-up. Legacy ownerless retained-claim migration still requires a separate verified association mechanism.
- Live PVC modification, progress status, and parent aggregation remain unimplemented.
- Unit tests verify generated objects and schemas, not successful disk provisioning by a real CSI driver.
- Adding/removing/renaming additional templates on an existing StatefulSet is not supported by this POC; declare them before initial creation. Only their VAC selections gain the existing-template preservation/scale-up behavior.
- Existing POC CRs must move their selection into `persistence` and retain provisioning settings before applying the new schemas. Updating CRDs does not migrate stored objects automatically. Roll out the matching operator and schemas together through a controlled upgrade; do not assume the old operator understands the new field location.

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