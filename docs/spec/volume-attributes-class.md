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

Qualification establishes tested support, not a runtime allowlist. A configuration with a served supported API, readable class and StorageClass, and matching driver names may proceed even if that provider/version has not been qualified. There is no portable Kubernetes flag proving VAC provisioning or live-modification support. The operator will not inspect sidecar image versions, call CSI directly, create probe volumes, or interpret provider-specific parameter values to infer support. Kubernetes and the CSI components validate and apply those parameters. Confirmed API absence or a missing referenced class permits new provisioning without VAC, with actionable warnings; runtime CSI failures do not trigger automatic default-profile substitution or destructive recovery.

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
2. Create new scale-up PVCs with each template's validated provisioning class observed at creation, including a non-empty class after live changes or an unset field after opt-out. If the VAC API is unavailable or the referenced class is not found, omit VAC only from that template's new claims and continue provisioning with StorageClass/driver defaults. Preserve the desired selection, report the fallback and remediation, and retain safeguards for other validation failures.
3. Select primary and explicitly declared additional claims through template identity, active ordinals, and verified workload association. Never patch unrelated claims or externally supplied PVCs merely referenced through `additionalVolumes`.
4. Patch mismatched bound PVC fields directly, idempotently, with bounded conflict retries and fresh reads. Observe unbound claims without attempting immutable reference changes.
5. Suppress VAC-only immutable claim-template drift without concealing unrelated persistence changes.

#### Capability and Progress Reporting

1. Use cached API discovery; check class existence and StorageClass provisioner/CSI driver compatibility where possible.
2. Report absent APIs, disabled features, missing classes, unsupported drivers, and modification errors through conditions, warnings, and Events.
3. Continue initial provisioning and scale-up without VAC on confirmed API absence (`VACUnsupported`) or a missing class (`VACNotFound`), independently for each template. Emit a transition-only Warning event and log message, and keep a non-success condition explaining fallback and suggesting prerequisite repair, class-name correction, or opt-out. Authorization failures, discovery timeouts, unreadable/unresolved storage configuration, and driver mismatches remain gated and retryable as appropriate; they are not evidence that VAC is absent. Do not block solely because CSI capabilities, parameter acceptance, or provider qualification have not been established; these are not portable preflight checks. Continue existing workloads, required consumer scheduling, and unrelated reconciliation.
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

Expose the group observation under `MarklogicGroup.status.storage.volumeAttributesClass`. This schema is implemented independently of resize bookkeeping. The cluster uses the same path for its summary and a `groups` array of child observations.

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
| `pvcs[].requestedClassName` | Optional string | Saved PVC reference; not proof of CSI application. |
| `pvcs[].uid` | Optional string | Observed PVC identity; absent for a missing claim. |
| `pvcs[].state`, `reason`, `message` | String | Per-PVC state and actionable diagnostics. |
| `pvcs[].modifyVolumeError` | Optional boolean | Current-target native `ModifyVolumeError=True` observed during retryable modification; retained if progress becomes diagnostically stalled. Omitted when false. |
| `pvcs[].lastProgressTime` | Timestamp | Last change in the observed target, identity, classes, state, or diagnostic; preserved across unchanged observations and restarts. |

Cluster `groups` entries contain `name`, `observedGeneration`, `state`, `reason`, and `message`. Cluster `observedGeneration` refers to the cluster generation; each group's value refers to its observed child generation.

Desired/current values are read-only diagnostic fields, not two configurable native PVC fields. Retry timestamps or last-progress timestamps may be stored separately if needed for restart-safe backoff/stall detection. Do not add resize markers, filesystem checkpoints, or restart metadata.

#### Unused Precreated PVCs

`MarklogicGroup.status.storage.unusedPVCs` reports potentially unused operator-precreated claims independently of VAC completion. Each record includes `name`, `uid`, `templateName`, optional native `phase`, `volumeName`, `volumeAttributesClassName`, `reason`, `message`, and a stable `firstObservedTime`. Records are sorted by claim name. The parent cluster does not aggregate this inventory; inspect the affected group. Unused candidates do not alter `StorageAttributesSynced` or count toward VAC completion.

The observer uses the existing `marklogic.progress.com/storage-group-uid` annotation as precreation provenance, verifies any group/StatefulSet owners and the live managed StatefulSet, and requires a canonical declared-template/ordinal name. Cached indexes select only this group's marked claims and Pods referencing each candidate, including Pending or terminating Pods; candidate claim identity and lifecycle are verified through uncached reads. This avoids namespace-wide API lists. Pod-reference absence is an eventually consistent cache observation, not an atomic proof of non-use. Observation runs on group handler exits, including failed scale-up or resize paths. PVC and consuming-Pod events drive updates, with 30-second fallback polling only while unused candidates or unresolved observation errors remain. Active marked claims alone do not keep reporting polling alive. Reporting does not mutate PVCs, Pods, StatefulSets, or storage settings.

| Reason | Meaning |
| --- | --- |
| `ScaleUpPending` | A claim exists for a desired ordinal that is not active in the live StatefulSet, such as after precreation succeeded but the replica update failed. Keep it for retry/reuse. |
| `OutsideReplicaRange` | A marked claim has no observed Pod reference and is outside both desired and live replica ranges, such as after cancelled scale-up or later scale-down. It may be deliberately retained storage containing data. |

While candidates exist, `StorageClaimsInUse=False` with reason `UnusedPrecreatedPVCs` points to this inventory. The operator emits a transition-only `UnusedPrecreatedPVC` Warning Event and WARNING-prefixed log for new or meaningfully changed candidates. Repeated observations do not rewrite status or repeat Events. Bound or assigned claims explicitly warn that deletion may delete backing storage. An observation failure sets `StorageClaimsInUse=Unknown` with reason `PrecreatedPVCObservationFailed`, preserves the prior inventory, and emits a deduplicated warning rather than assuming claims are unused or resolved.

Successful reuse, a newly observed Pod reference, or claim removal clears the corresponding inventory records; when the last candidate disappears, the condition is removed and a Normal `UnusedPrecreatedPVCsResolved` Event is emitted. Disabling VAC management does not remove outstanding unused-claim reports. Updates use fresh reads, optimistic status patches, conflict retries, and UID/generation fencing, preserving resize, VAC, and unrelated status.

```bash
kubectl get marklogicgroup <group> -n <namespace> \
  -o jsonpath='{.status.storage.unusedPVCs}'
kubectl describe marklogicgroup <group> -n <namespace>
```

This is reporting, not an automatic cleanup policy or proof that a volume is empty. Review current Pod references, desired replicas, claim identity, StatefulSet retention policy, and PV reclaim policy before any manual deletion. In particular, do not delete a bound claim merely because VAC or scale-up failed. Claims are retained and reused on retry; no new delete permissions are required.

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

The only PVC spec field patched for live VAC changes is `spec.volumeAttributesClassName`, leaving Kubernetes and the CSI driver to apply the change. Dispatch also records a `marklogic.progress.com/storage-operation-id` metadata token for restart-safe fencing. Capacity and unrelated settings remain unchanged. Existing StatefulSet templates retain their historical class because those templates are immutable. VAC-only changes will not replace workloads or claims, or restart Pods. Conflicts will trigger fresh reads before retrying.

### Initial Provisioning and Scale-Up

New StatefulSets will use the validated class for each volume, except that confirmed API absence or a missing class omits VAC from the affected generated template with an actionable warning. During scale-up, matching effective provisioning and historical classes allow normal StatefulSet provisioning. For a mismatch, the operator will create missing new-replica claims with current settings before increasing replicas, omitting the class after opt-out or this warned fallback. Required claims must be created or verified for reuse; failures block only that scale action. Retries preserve partial progress, starting ordinals, and retention. The operator will not wait for binding because `WaitForFirstConsumer` may require the new Pod to schedule first. Existing claims retain their creation-time references, even if those references are unavailable; fallback does not clear or replace them.

### Changes Before Binding

A claim records the class selected at creation. If A changes to B before binding, the operator will preserve the claim, report `PendingClassMismatch`, and allow its Pod to schedule. Once bound, it will converge to the latest valid target. An original request that cannot provision will produce `ProvisioningBlocked` and require prerequisite repair or administrator recovery, not automatic claim replacement. Scheduling delays alone are not VAC failures.

### Claim Ownership and Reuse

The operator will verify the group-controlled StatefulSet, template/ordinal association, claim identity, owner references, and compatible storage settings before mutation or reuse. Reuse reads the claim uncached and checks StorageClass, normalized filesystem/block mode, required access modes, and sufficient storage. Bound claims with reported capacity must have sufficient applied capacity, not merely an outstanding larger request. Names and metadata alone are insufficient. Legacy ownerless claims also require a verified managed Pod reference and fresh identity check. Verified retained claims will be reused without changing retention, then converge when active and bound. Ambiguous or incompatible claims block the affected action with a diagnostic; they are not automatically replaced or deleted.

### Coordination With Other Storage Operations

Conflicting VAC, resize, scale-up preparation, and workload-replacement actions will be serialized per group using a shared operation record in status. It will retain the operation, target generation, PVC identities, and dispatch phase for restart recovery, protected by resource-version checks. Resize and VAC keep separate targets and progress. Observation, necessary scheduling, and unrelated work continue; binding waits do not hold the dispatch guard. Initial limits are one in-flight PVC per group and four operator-wide.

### Platform Validation

The operator will discover the served API, validate the requested class, and compare its driver with the StorageClass provisioner. GA is preferred; beta is used only when GA is absent. Confirmed API absence or a missing class permits initial provisioning and scale-up without VAC on the affected new template/claim, with warnings and fix suggestions. Unreadable or unresolved storage configuration and driver mismatches still gate affected actions; transient lookup failures remain retryable rather than being treated as absence. Passing validation permits the selected class without a provider qualification allowlist or proof of CSI capability. Provider-specific parameters remain opaque to the operator. Kubernetes/CSI performs provisioning and modification; runtime CSI errors do not trigger fallback. Desired CR selections remain unchanged so bound claims can converge after prerequisites are repaired. Qualification separately establishes tested provisioning and live-modification support. Existing workloads, required scheduling, unconfigured volumes, and unrelated groups continue. Administrators maintain the profiles; the operator references them.

### Progress and Completion

PVC watches will drive `status.storage.volumeAttributesClass` and group/cluster conditions. Completion requires native Kubernetes status confirming that every active managed PVC has applied its latest target for the observed generation. API writes, Pod readiness, stale observations, or zero targets cannot establish success. Events will reflect meaningful transitions; Kubernetes/CSI owns provider retries.

Initial internal defaults are 30-second fallback observation, five-minute discovery refresh, and jittered API backoff from one to 60 seconds. After 30 minutes without progress, `Stalled` is diagnostic, not terminal or destructive. These limits require provider qualification.

## Detailed Workflow

### Resolve Configuration

Read the latest group configuration and resolve the desired class for each primary and additional PVC template. Exclude the primary template when persistence is disabled. Validate non-empty selections against the served API, class reference, and StorageClass driver. Do not require provider qualification or prevalidate opaque CSI parameters. Record lookup/association failures and skip affected actions while continuing unrelated work; unconfigured templates do not require VAC validation.

### Prepare New Claims

For a new StatefulSet, generate templates with their validated classes. For API-unavailable or class-not-found fallback, omit the affected class from generated copies only, keeping healthy templates and CR selections unchanged. For scale-up, preserve existing immutable templates and create missing new-replica claims where the effective provisioning class differs from the historical template, including an omitted class after opt-out or fallback. Use the shared operation guard for conflicting actions and verify claim ownership before reuse. Increase replicas after all required claims are prepared, without waiting for binding or holding the guard across that wait. Preparation failures block the affected scale action.

### Update Bound Claims

Find active claims and verify their workload association. Under the shared operation guard, patch only the VAC field on bound PVCs whose reference differs from a valid non-empty target. Leave unbound claims unchanged and observe their binding. Re-read and revalidate after conflicts. Confirm that the saved field retains the requested class; report a dropped field instead of repeatedly patching it. When the reference already matches, observe progress without another write.

### Observe and Report

Read native PVC modification status and correlate it with each template's latest target. Update per-PVC progress, the group condition, and the parent cluster condition using the Status Contract, preserving unrelated status. Publish Events only for meaningful transitions and skip unchanged status writes. Continue through PVC watches and fallback requeues; after a restart, resume from current resources and the stored operation record.

## Failure Handling and Recovery

| Failure | Required behavior |
| --- | --- |
| Supported API absent, including feature configuration that makes it unavailable | Continue initial provisioning and scale-up without VAC on new claims. Preserve desired selections and existing claims/templates. Publish `VACUnsupported` with Warning event/log and suggest enabling the supported API/feature configuration or upgrading Kubernetes, verifying CSI support, or opting out. Progress remains non-success; no live VAC write is attempted without validation. |
| Provider/version not qualified or CSI capability/parameter acceptance not prevalidated | Proceed when API/reference/driver checks pass. Observe actual provisioning/modification results and report failures; lack of qualification alone does not block the request or establish success. |
| Referenced class missing | Continue new provisioning without that template's VAC, publish `VACNotFound` with Warning event/log, and suggest creating the class with the correct driverName, correcting the name, or opting out. Preserve healthy template classes and the desired CR selection. Existing unbound claims retain their original references; repair their original prerequisites rather than clearing or replacing them. |
| Known driver mismatch | Gate the affected explicit request and report the specific validation failure; do not treat it as unsupported/missing or provision a default-profile substitute. |
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

For a current-target native `ModifyVolumeError=True`, retain `InProgress` and the provider reason/message with `StorageAttributesSynced=False`. Emit a `VolumeAttributesClassModifyError` Warning on the group when the error first appears, reappears after observed recovery, or changes reason, PVC identity, or desired target. Persist the error flag to deduplicate across reconciles and restarts. Message-only changes and ordinary `ModifyingVolume=True` do not trigger warnings; errors associated with a previous target do not warn against the new selection. A current-target `Infeasible` remains `Failed` and uses the existing failure Event rather than an additional modification-error Warning.

The group controller watches managed PVC status changes through PVC-to-group mapping and verifies workload association during reconciliation. The parent controller reacts to child storage observations, VAC conditions, generation/spec, ownership, and lifecycle changes. Unrelated child status and parent status-only updates do not trigger aggregation loops.

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
4. Confirmed API absence and class-not-found permit initial provisioning and scale-up without VAC, with actionable Warning events/logs and non-success conditions. Validate healthy templates independently, preserve desired selections and existing claims, and recover live convergence after remediation. Discovery/access failures and driver mismatches remain gated rather than triggering fallback. Passing checks permits unqualified providers and opaque parameters without claiming CSI support or successful application. Existing workloads and unrelated reconciliation continue.
5. Both Helm scope modes with least-privilege PVC/VAC permissions; safe group watch mapping and parent aggregation.
6. Concurrent resize/VAC requests preserve capacity validation, field isolation, status and workload lifecycle semantics.
7. Real API-server rejection of unbound VAC reference changes on qualified beta/GA lanes; A-to-B supersession before binding, missing original class remediation, and consumer scheduling without immutable patch loops or claim deletion.
8. Scale-up after opt-out, confirmed API absence, or class-not-found with a non-empty historical template precreates affected new claims with the field absent before increasing replicas. Retained/precreated claims remain unchanged. Safeguard failures block only the affected scale action and remain observable; fallback must not bypass ownership or PVC creation errors.
9. VAC-aware external-provisioner creates the intended initial profile; external-resizer/CSI modifies a bound volume independently. Verify complete and omitted-parameter profiles against documented driver defaults.

Use envtest to prove API-server schema rejection and reconciliation, not CSI completion. Use a VAC-capable integration/E2E lane to verify provider-visible profile changes and non-disruption; retain an unsupported-cluster lane. Review existing Kubernetes test pins rather than assuming they expose VAC.

### Acceptance

The requirement is complete only when the primary persistence field and independent additional-template fields govern creation-time selection for new initial/scale-up claims, warned API-absent/class-missing fallback, post-binding convergence of superseded or reused claims, in-place live changes, validation, and correctly correlated observable completion without VAC-driven workload replacement. Opt-out and fallback must not leak a historical template class into affected newly created scale-up claims. Fallback uses StorageClass/driver defaults and is never reported as successful VAC application. One template's configuration must never become another template's target. Evidence for shared coordination, ownership safeguards, non-disruption, and both CSI paths is required. Partial implementation must not be advertised as fulfilling the whole design.

## Implementation and Qualification Gates

1. Implement live PVC modification, validation with scoped warned provisioning fallback, ownership verification, the shared durable operation guard, native-status watches, and group/cluster aggregation according to the selected decisions. Initial provisioning and scale-up POC behavior alone do not complete the feature.
2. Qualify conditional precreation against a real API server and CSI driver: least-privilege PVC `create`, retention, partial failures, concurrent updates, restart recovery, non-zero ordinals, and `WaitForFirstConsumer` without a binding barrier.
3. Prove creation-time selection followed by post-binding convergence and verify that unrepairable unbound requests remain nondestructive and actionable. Publish the administrator prerequisite-repair/data-recovery procedure; do not introduce automatic replacement.
4. Qualify the Kubernetes 1.32 beta target and exact EKS/AKS/GKE/OpenShift configurations before advertising support. Verify provisioning and live modification separately, parameter defaults, warned unsupported/missing fallback versus other invalid requests, and the initial concurrency/timing limits.
5. Test legacy association, simultaneous resize/VAC/scaling requests, conflicts/restarts, non-disruption, per-template target correlation, and parent aggregation. Publish the selected API semantics and controlled migration guidance with the release evidence.

The sections above describe the full feature design. The implementation below covers initial/scale-up class selection, API discovery, class/driver validation, VAC reference updates on verified bound claims, convergence after binding, dedicated group/cluster progress reporting, and durable VAC/resize exclusion. Broader scale-up/workload-replacement coordination and operator-wide concurrency limits remain separate implementation work.

## Day-1 POC

### Implemented Behavior

- Discover `storage.k8s.io/v1` VolumeAttributesClasses first and try `v1beta1` only when GA is absent. Cache positive and negative discovery for five minutes; authorization and connection failures are not treated as API absence.
- Validate every non-empty primary/additional selection using uncached API reads. Require the VAC's `driverName` to equal its StorageClass provisioner. An omitted StorageClass uses Kubernetes default-selection ordering; no resolvable default or an explicit empty StorageClass blocks the selected request because driver compatibility cannot be established.
- Publish the generation-aware group condition `StorageAttributesValidated`: `True` means these checks passed, `False` records known unsupported/invalid configuration, and `Unknown` records discovery, authorization, or other lookup failures. It does not mean provisioning or modification completed. Emit Events on validation transitions, without rewriting unchanged conditions.
- Recheck selected profiles every 30 seconds, with discovery refreshed after cache expiry or evidence of an API mapping/resource-endpoint change. Object-level missing-class, authorization, and timeout errors retain valid discovery cache entries. Confirmed API absence (`VACUnsupported`) or a missing class (`VACNotFound`) permits initial provisioning and scale-up with the affected new VAC field omitted. Preserve desired CR selections and healthy template classes. Keep a non-success validation/progress condition and emit transition-only Warning events/logs explaining use of StorageClass/driver defaults and suggesting prerequisite repair, class correction/creation, or opt-out. Other validation failures remain gated; remediation needs no additional CR edit.
- Skip the native field on unsupported clusters: omit `spec.volumeAttributesClassName` entirely, rather than sending an empty string or null, from new primary/additional claim templates and precreated scale-up PVCs. Do not issue live VAC patches without successful API validation. For existing StatefulSets, preserve the complete immutable claim-template array, including historical VAC references and server-populated metadata/defaults, and send unrelated workload or replica changes as resource-version guarded merge patches. Unchanged historical VAC fields therefore stay out of outgoing requests without clearing stored references or the desired CR selection. Discovery authorization/network failures are not evidence of unsupported APIs.
- Use `spec.persistence.volumeAttributesClassName` for the primary claim on a cluster or standalone group.
- Cluster-owned groups inherit the entire cluster persistence object unless they supply a complete replacement. A replacement's omitted/empty class does not inherit the cluster class.
- Use each `additionalVolumeClaimTemplates[].spec.volumeAttributesClassName` for an independent additional-volume class. Group lists inherit or replace as a whole, independently of primary persistence.
- A selected non-empty class is copied into the primary `datadir` PVC template.
- Kubernetes uses that template when initially creating the group's data PVCs.
- Existing StatefulSets retain their complete live immutable claim templates; changing the selection does not update those templates.
- On scale-up, compare desired and historical classes by template name. For every mismatch, precreate that template's missing new-ordinal claims with its current class before increasing replicas. An empty selection omits the VAC field.
- Precreated claims use the current persistence settings and template annotations, workload selector labels, and a `marklogic.progress.com/storage-group-uid` identity annotation. They do not gain a deletion-causing owner reference.
- Verified retained or previously precreated claims are reused without changing their VAC during precreation, including while Pending. Active bound claims subsequently converge to their own template's latest non-empty selection.
- Claim creation or verification errors prevent replica increase. Precreated claims do not have to bind first, so `WaitForFirstConsumer` can schedule the new Pods.
- Without an effective class, the operator leaves the PVC field unset; ordinary StorageClass/driver defaults apply.
- Additional templates preserve their own fields and profiles, never the primary class; empty references are omitted on generated copies. Existing StatefulSets retain historical VAC values for all templates. Pod templates remain unchanged by VAC-only updates.
- Persistence-disabled groups receive no primary claim, but declared additional templates still participate in provisioning and scale-up.
- Native and Helm CRDs accept the field at cluster, embedded-group, and standalone-group scopes.
- Discover active claims by declared primary/additional template name and StatefulSet ordinal range, intersected with the desired replica count. Respect non-zero starting ordinals; skip inactive retained claims, missing claims, unbound claims, and terminating claims. Do not select arbitrary claims referenced by `additionalVolumes`.
- Patch only `spec.volumeAttributesClassName` within the spec of verified Bound claims, plus the operation fence metadata token. Use uncached reads, resource-version optimistic locking, bounded conflict retries, fresh target/generation checks, and a readback confirming the saved field. Preserve capacity, unrelated annotations, owner references, binding, claim identity, and workload templates. Nil/empty selections do not clear an existing class or revert provider settings.
- Require a verified live group-controlled StatefulSet and claim identity. Accept matching group UID annotations or verified group/StatefulSet owners; an otherwise ownerless legacy claim also requires its active StatefulSet-controlled Pod to reference that exact template volume. Reject foreign annotations/owners. Verify the PV's claim UID/name/namespace binding and compare its actual CSI driver with the claim's StorageClass provisioner and requested VAC driver.
- Leave an unbound claim's creation-time class unchanged even when the desired class changes. PVC status watches and 30-second fallback reconciliation reconsider it after binding; submit only the latest valid non-empty target once the original class application is no longer pending.
- Submit at most one new live VAC request per group pass, and wait while an active managed claim has a native Pending/InProgress/unknown operation or an unapplied request. A native Infeasible result for the current requested target permits a corrected target; stale terminal status does not complete a newer request. API submission emits `VolumeAttributesClassRequested`, not a completion condition.
- Keep the existing resize-first handler order. VAC dispatch first acquires `status.storage.operation` with an ID, PVC name, and PVC UID through optimistic group-status CAS. Resize acquisition uses the same group resource version and refuses an outstanding VAC record; VAC refuses active resize status. Resize eligibility reads target PVCs uncached, so informer lag cannot hide pending VAC work. A VAC record remains until native work settles. After interrupted dispatch, recovery uses a resource-version-guarded metadata fence before releasing an unsubmitted request, invalidating delayed writes without clearing VAC. Recovery continues after opt-out and does not hold a lock across pending claim binding.
- Observe VAC progress read-only on every group handler exit, including resize waits and workload/dispatch errors. Report desired/requested/applied classes for each selected primary/additional template in stable PVC-name order. Include missing claims in the desired ordinal range, preserve binding mismatches as Pending, and exclude opted-out templates and inactive scale-down ordinals. Observation never patches PVCs or restarts Pods.
- Publish `status.storage.volumeAttributesClass` and the generation-aware `StorageAttributesSynced` condition independently of `StorageAttributesValidated`. Completion requires a verified bound CSI PV, matching desired/requested/applied classes, and no active native modification or error. Native Infeasible is Failed only for the latest target; stale target results never complete or fail a newer selection. Preserve provider reason/message details, distinguish ambiguous writes from definitive rejection or dropped fields, and report unsupported/unknown states as Unknown.
- Preserve progress timestamps across unchanged observations and process restarts. After 30 minutes without change, Pending/InProgress becomes diagnostically Stalled; continue watching and allow fresh progress to clear it. Aggregate in priority order Failed, Unknown, Stalled, InProgress, Pending, Completed. Zero desired replicas produce Unknown/NoActivePVCs, never vacuous completion.
- Aggregate only configured, verified cluster-owned groups, including additional-only groups. Missing/unverified children, selections not yet matching the latest cluster spec, and missing/stale generation-aware child observations are Unknown. Completion additionally requires current per-PVC evidence for every configured template and replica. Expose stable group-name-ordered details and `StorageAttributesSynced` on the parent; child storage-status watches and 30-second fallback reconciliation drive updates.
- Write VAC status through fresh-read, optimistic status-subresource patches with bounded conflict retries and UID/generation fencing. Preserve unrelated conditions, resize status, and dynamic-host status; skip unchanged writes and emit completion/failure/unknown/stall Events only on meaningful transitions. Opt-out removes only active VAC observations/conditions and does not clear applied PVC references. Native provider diagnostics remain in dedicated progress even if Kubernetes Condition format/length limits require a shorter summary.

### Prerequisites and Limits

- Deploy an operator image built from this checkout and install the updated CRDs. Existing release images do not contain this POC.
- The API server must retain the PVC VAC field, with the relevant API and feature configuration enabled.
- Use an existing VAC and a compatible CSI StorageClass, driver, and external-provisioner.
- Beta support on Kubernetes 1.32 and specific provider configurations still require qualification.
- API discovery and matching driver names do not prove CSI or sidecar capabilities, feature-field retention, successful provisioning, or live-modification support. Only confirmed API absence or a missing referenced class triggers warned fallback for new provisioning. CSI runtime errors, permission failures, lookup timeouts, and driver mismatches are not fallback signals. Defaults do not guarantee the requested performance profile; progress remains non-success until the desired class is actually applied.
- Install the updated native or Helm RBAC: PVC `create`/`patch`, cluster-scoped VAC `get`, and cluster-scoped PV `get` are required in both operator scope modes. No PV or VAC write/delete permission is added.
- CSI/external-resizer applies a submitted live VAC request asynchronously. Matching the PVC spec, a successful PATCH, or `VolumeAttributesClassRequested` is not proof that provider settings changed.
- Preexisting claims without a matching group UID annotation or verified group/StatefulSet owner reference still block precreation-based scale-up. Active ownerless claims can be verified through their managed Pod for live convergence; inactive ownerless retained claims are not adopted or modified.
- Durable VAC/resize dispatch exclusion is implemented. A unified guard for scale-up/workload replacement and an operator-wide concurrency limit remain unimplemented. Dedicated per-PVC/group/cluster progress is implemented; `StorageAttributesValidated` describes preflight checks, while `StorageAttributesSynced` describes observed CSI application. Kubernetes PVC status remains the source of provider progress/errors.
- Fake-client tests verify selection, mutation safety, retries, and event mapping, not real API-server admission or successful CSI provisioning/modification. Provider qualification is still required before claiming tested platform support.
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