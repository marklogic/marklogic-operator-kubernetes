# Functional Spec: MarkLogic Operator Volume Resizing

## Introduction

### Overview

This specification defines the requirements, API contract, and controller workflow for enabling persistent volume resizing for MarkLogic clusters managed by the MarkLogic Operator for Kubernetes.

The feature allows an operator to increase the size of persistent storage used by a MarkLogic group through a declarative update to the custom resource. The operator reconciles the requested change by validating prerequisites, resizing the underlying PersistentVolumeClaims, determining whether filesystem expansion requires a Pod restart, synchronizing StatefulSet template state when necessary, and reporting progress through status, events, and logs.

### Goal

The goals of this feature are to:

1.  Allow operators to increase storage capacity for running MarkLogic deployments without manual PVC surgery.
    
2.  Support online filesystem expansion when the Kubernetes storage stack permits it.
    
3.  Provide a safe fallback path for storage environments that require Pod restart and StatefulSet template synchronization to complete a resize.
    
4.  Preserve data integrity and minimize service disruption throughout the operation.
    
5.  Expose a clear, queryable status model so users can monitor progress and recover from failures.
    

## Background: **Kubernetes Volume Expansion**

Kubernetes supports dynamic PVC expansion when all of the following are true:

1.  The target StorageClass has `allowVolumeExpansion: true`.
    
2.  The underlying storage driver supports controller-side volume expansion.
    
3.  The requested size is greater than the current requested size. Shrinking is not supported.
    

A resize request for a filesystem-backed volume typically has two stages:

1.  Volume expansion. The storage provider expands the underlying block device, and Kubernetes updates PVC capacity once the change is acknowledged.
    
2.  Filesystem expansion. The filesystem on the mounted volume is expanded to consume the new capacity.
    

Filesystem expansion behavior for filesystem-backed volumes depends on the storage stack:

1.  Online expansion. Some CSI drivers and filesystems support expansion while the pod remains running.
    
2.  Offline expansion. If online expansion is not supported, the PVC may report `FileSystemResizePending` or otherwise indicate that workload-side remount is still required. In that case, the workload must be restarted so kubelet can complete filesystem expansion during mount.
    

For this operator, filesystem expansion cannot be treated as a storage-only concern. The controller must evaluate requested size, observed PVC capacity, and resize-related PVC conditions together; it must not rely on `FileSystemResizePending` as the only completion signal.

### PVC Field Mapping

The following Kubernetes PVC fields are authoritative for resize logic:

|            Concept            |                 Kubernetes Field                  |                                     Notes                                      |
|-------------------------------|---------------------------------------------------|---------------------------------------------------------------------------------|
| Current actual volume size    | `pvc.status.capacity[storage]`                    | Reflects the size the storage backend has provisioned                           |
| Requested volume size         | `pvc.spec.resources.requests[storage]`            | The value the operator patches to trigger expansion                            |
| Filesystem resize pending     | `pvc.status.conditions` with type `FileSystemResizePending` and status `True` | Indicates block expansion is done but filesystem expansion requires pod remount |
| Resizing in progress          | `pvc.status.conditions` with type `Resizing` and status `True` | Some CSI drivers emit this while expansion is in flight                        |

The operator defines `currentSize` at operation start as the minimum of `pvc.status.capacity[storage]` across all target PVCs. This handles edge cases where a previous resize partially completed.

StatefulSet `volumeClaimTemplates` are treated as effectively immutable for this workflow. Live PVCs can be expanded independently of the historical template, which creates a temporary mismatch between live storage state and StatefulSet template state. The operator resolves that mismatch by deleting and recreating the StatefulSet using orphan propagation so Pods and PVCs remain preserved.

Orphan deletion preserves Pods; it does not restart them. As a result, StatefulSet delete/recreate is not itself sufficient to complete offline filesystem expansion. If remount-based filesystem completion is required, the operator must explicitly restart Pods after PVC expansion reaches the required checkpoint.

## Requirements

### Functional Requirements

#### **Operator-Driven Resize**

Users must be able to trigger a resize by increasing the requested persistence size in the MarkLogic custom resource.

Acceptance criteria:

1.  The operator detects an increase to the effective persistence size for a target MarkLogic group.
    
2.  The operator identifies the PVCs associated with that group and patches them to the requested size.
    
3.  The operator updates CR status throughout the lifecycle of the operation.
    
4.  The operator emits Kubernetes events for major lifecycle transitions.
    

#### **Pre-Resize Validation**

The operator must validate the request and environment before making changes.

Acceptance criteria:

1.  Requests that reduce size are rejected.
    
2.  The operator verifies that each target PVC exists and is `Bound` before issuing resize patches. If a target PVC is not yet `Bound`, the operator must not start resize; it should either wait in a recoverable state when binding is still expected or fail with `PVCNotBound` when the condition is not expected to resolve safely.
    
3.  The operator verifies that the associated StorageClass allows expansion.
    
4.  The operator fails safely and surfaces a reason if expansion is rejected by the Kubernetes API or storage provider.
    
5.  The operator rejects or stalls conflicting resize operations for the same target group.
    

#### **Status & Observability Reporting**

The operator must provide transparent, user-visible progress reporting.

Acceptance criteria:

1.  The CR status exposes the current resize phase.
    
2.  The CR status exposes terminal success or failure.
    
3.  The CR status includes a message and structured reason when the operation is stalled or failed.
    
4.  Kubernetes events are emitted for important milestones and errors.
    
5.  Controller logs record each state transition and actionable error.
    

### Non-Functional Requirements

#### **Data Integrity**

The operator must preserve persistent data throughout the resize workflow.

The operator guarantees:

1.  No destructive action is taken on PVCs or PVs as part of resize.
    
2.  StatefulSet delete/recreate uses orphaning semantics so the controller object can be replaced without deleting Pods or PVCs.
    
3.  Pods are terminated using normal Kubernetes graceful shutdown behavior.
    
4.  Completion is reported only after infrastructure reconciliation and health checks succeed.
    

#### **Availability**

The operator should prefer the least disruptive path available for the environment.

1.  Online expansion is preferred when the storage stack supports it.
    
2.  If offline filesystem expansion is required, the operator performs an explicit controlled Pod restart.
    
3.  StatefulSet delete/recreate is used to synchronize future `volumeClaimTemplates`, not as a substitute for Pod restart when remount is required.

4.  Temporary disruption may occur during the offline path and must be surfaced clearly in status and events.
    

#### **Platform Compatibility**

This feature supports Kubernetes environments whose storage classes and CSI drivers satisfy the documented expansion prerequisites.

Initial validation and testing targets:

1.  AWS EKS with the AWS EBS CSI driver.
    
2.  Azure AKS with the Azure Disk CSI driver.
    

Cloud-specific limitations such as rate limits, quota exhaustion, cooldown periods, or delayed backend expansion are treated as environmental constraints and surfaced through operator status and events.

### **Scope and Non-Goals**

#### In Scope

1.  Expansion of the primary persistence-backed PVC used by a MarkLogic group.
    
2.  Declarative resize triggered by increasing the requested storage size in the CR.
    
3.  Support for parallel and sequential PVC resize strategies.
    
4.  Status, events, and logs for end-to-end observability.
    
5.  Recovery from interrupted or partially completed resize operations.
    
6.  Explicit serialization of resize against other operations that mutate the same StatefulSet or Pod lifecycle.
    

#### **Out of Scope**

1.  Volume shrinking.
    
2.  StorageClass migration.
    
3.  Cross-cluster storage orchestration outside the target MarkLogic resource.
    
4.  Expansion of arbitrary `additionalVolumeClaimTemplates` in the first version, unless explicitly added in a later phase.
    
5.  Hard guarantees about zero service interruption in storage environments that require offline filesystem expansion.

6.  Concurrent execution of resize with scale, upgrade, or other StatefulSet-mutating operations in v1.
    

## API and User Experience Contract

### **Trigger Model**

Users trigger resize by modifying the effective persistence size on `MarklogicCluster`, either through `MarklogicCluster.spec.persistence.size` or a group-specific override such as `MarklogicCluster.spec.markLogicGroups[i].persistence.size`. The operator computes the effective desired size for each managed group and reconciles resize at the `MarklogicGroup` level, where the StatefulSet, Pods, and PVCs are managed.

  
Example:

```yaml
spec:
  persistence:
    enabled: true
    size: 50Gi          # Increased from 20Gi
    resizeStrategy: parallel  # Options: 'parallel' (default) or 'sequential'
```

The operator shall interpret an increase to the effective persistence size as a request to expand the persistent volumes associated with the target MarkLogic group.

The operator shall not treat an unchanged size as a new resize operation.

The operator shall reject or fail a request that attempts to reduce the requested size.

The operator shall allow only one active resize operation per target `MarklogicGroup`. If the desired size is increased again while an operation is already in progress, the controller snapshots the current active target size, completes or fails that operation deterministically, and then evaluates whether a subsequent resize is still required.

#### Trigger Scope

Resize is reconciled at the MarkLogic group level.

If persistence is configured at a higher level and inherited by a group, the operator computes the effective persistence size for that group and performs resize reconciliation against the group-owned PVCs.

Detailed resize execution state is owned by `MarklogicGroup.status.volumeResizeStatus`. Cluster-level resources may expose summary conditions, but the authoritative workflow state for an individual resize lives on the group-scoped resource.

### Spec Fields

|              Field              |  Type   |                           Description                            |                      Notes                       |
|---------------------------------|---------|------------------------------------------------------------------|--------------------------------------------------|
|    spec.persistence.enabled     | boolean | Indicates whether persistence is enabled for the target workload | Resize is valid only when persistence is enabled |
|      spec.persistence.size      | string  |                  Desired persistent volume size                  |      Increasing this field triggers resize       |
| spec.persistence.resizeStrategy |  enum   |          Strategy the operator uses when resizing PVCs           |               Default is parallel                |

#### Resize Strategy Values

|   Value    |                                                  Meaning                                                  |
|------------|-----------------------------------------------------------------------------------------------------------|
|  parallel  |                          Submit resize requests for all target PVCs concurrently                          |
| sequential | Resize one PVC at a time and wait for each PVC to reach the expected checkpoint before moving to the next |

### Resize Strategy Semantics

1.  If resizeStrategy is omitted, the operator defaults to parallel.
    
2.  If spec.persistence.size increases, the operator begins a new resize reconciliation.
    
3.  If spec.persistence.size is unchanged, no resize action is taken.
    
4.  If spec.persistence.size decreases, the request is rejected or marked failed with a machine-readable reason such as ShrinkNotSupported.
    
5.  Concurrent execution of two resize operations for the same target group is not allowed.

6.  If a new desired size is observed while a resize is active, the operator does not start a second overlapping workflow. It finishes or fails the active operation against its snapshotted target size and then evaluates whether another resize is required.

7.  Sequential mode must use a stable PVC ordering that is persisted in status so a restarted controller resumes on the same PVC rather than reordering the workflow.
    

### **Status Contract**

The operator shall expose resize progress under status.volumeResizeStatus.

#### **Status Object**

The operator reports the progress and outcome of a resize operation under status.volumeResizeStatus. This status object is the primary user-visible contract for observing the workflow, diagnosing failure, and determining whether manual intervention is required.

The minimum v1 public contract is intentionally smaller than the full recovery bookkeeping the controller may persist internally. The fields below are the recommended user-facing status fields for v1.

|           Field            |      Type       | Required |  Set By  |                                Description                                |                                                Notes                                                 |
|----------------------------|-----------------|----------|----------|---------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------|
|       `operationID`        |     string      |   Yes    | Operator | Unique identifier for the active resize operation                         | Used to correlate retries, restarts, and phase transitions                                          |
|   `observedGeneration`     |     integer     |   Yes    | Operator | Generation of the `MarklogicGroup` spec observed when the operation began | Prevents ambiguity across later spec updates                                                         |
|          `phase`           |      enum       |   Yes    | Operator | Current lifecycle phase of the resize workflow                            | Primary progress indicator                                                                           |
|         `message`          |     string      |    No    | Operator | Human-readable summary of the current state                               | Intended for direct user consumption                                                                 |
|          `reason`          |      enum       |    No    | Operator | Machine-readable reason for a stalled or failed operation                 | Populated when the workflow is degraded or failed                                                    |
|       `currentSize`        |     string      |   Yes    | Operator | Volume size in effect when the active resize operation began              | Example: `20Gi`                                                                                      |
|        `targetSize`        |     string      |   Yes    | Operator | Desired final volume size for the active operation                        | The controller snapshots the active target size at operation start                                   |
|     `deferredTargetSize`   |     string      |    No    | Operator | Newer desired size observed while the active operation is still running   | Present only when a later requested size is deferred behind the snapshotted active `targetSize`       |
| `deferredObservedGeneration` |  integer     |    No    | Operator | Generation where the deferred target was observed                         | Helps users distinguish the active operation from a pending follow-up resize                          |
|     `resizeStrategy`       |     string      |   Yes    | Operator | Strategy used for the active resize operation                             | Expected values: `parallel`, `sequential`                                                            |
|        `totalPvcs`         |     integer     |   Yes    | Operator | Total number of PVCs included in the current operation                    | Denominator for progress                                                                             |
|    `pvcsCheckpointed`      |     integer     |   Yes    | Operator | Number of PVCs that have reached the checkpoint required for advancement  | Replaces the ambiguous term `pvcsResized`                                                            |
|        `activePVC`         |     string      |    No    | Operator | Name of the PVC currently being processed                                 | Required in sequential mode                                                                          |
|       `pvcStatuses`        |      array      |   Yes    | Operator | Per-PVC resize state for all targeted PVCs                                | Required for crash recovery, partial progress reporting, and selective Pod restart                    |
|       `failedPVCs`         |      array      |    No    | Operator | Per-PVC failure details                                                   | Each entry includes name, reason, and message                                                        |
|        `warnings`          | array of string |    No    | Operator | Non-fatal issues encountered during the operation                         | Does not imply terminal failure                                                                      |
|       `retryCount`         |     integer     |    No    | Operator | Number of retries attempted for recoverable failures                      | Useful for debugging and support                                                                     |
|       `nextRetryTime`      |    timestamp    |    No    | Operator | Time at which the controller plans to retry a stalled operation           | Present only for retry-eligible stalled states                                                       |
|    `lastTransitionTime`    |    timestamp    |    No    | Operator | Time at which the phase most recently changed                             | Helps detect stuck operations                                                                        |
|      `firstStartedTime`    |    timestamp    |    No    | Operator | Time at which the current resize operation began                          | Used for duration measurement                                                                        |
|      `completionTime`      |    timestamp    |    No    | Operator | Time at which the operation reached a terminal state                      | Set when the phase becomes `Completed` or `Failed`                                                   |

#### **Recovery Metadata**

The operator may persist additional implementation-specific recovery metadata in status when needed for crash-safe resume. Recommended examples include `orderedTargetPVCs`, `podsRestartPending`, and the `statefulSetSync.*` fields used during StatefulSet delete/recreate recovery.

These fields help the controller resume in-flight workflows safely, but they are not part of the minimum v1 user-facing status contract.

#### **PVC Status Record**

Each entry in `pvcStatuses` SHOULD use the following shape.

|          Field          |  Type  |                                      Description                                      |
|-------------------------|--------|--------------------------------------------------------------------------------------|
|         `name`          | string | Name of the PVC                                                                      |
|        `podName`        | string | Pod associated with the PVC, when determinable                                       |
|     `requestedSize`     | string | Current requested PVC size observed by the operator                                  |
|    `observedCapacity`   | string | Current PVC status capacity observed by the operator                                 |
|         `state`         |  enum  | Per-PVC workflow state                                                               |
|    `checkpointType`     |  enum  | `OnlineComplete`, `OfflinePending`, `OfflineComplete`, or empty until checkpointed   |
|      `restartRequired`  | boolean | Whether this PVC requires its associated Pod to be restarted                         |
|       `lastReason`      | string | Most recent machine-readable reason for this PVC, if any                             |
|      `lastMessage`      | string | Most recent human-readable message for this PVC, if any                              |
|   `lastTransitionTime`  | timestamp | Time this PVC status most recently changed                                        |

Recommended `pvcStatuses[*].state` values are `Pending`, `ResizeSubmitted`, `WaitingForCheckpoint`, `Checkpointed`, `RestartPending`, `Restarted`, and `Failed`.

`OfflinePending` means the PVC reached the checkpoint through `FileSystemResizePending` and its associated Pod still requires restart before final verification can pass. After the required Pod restart completes and the filesystem-resize signal is gone, the controller updates the PVC entry to `checkpointType: OfflineComplete`, `state: Restarted`, and `restartRequired: false`. This preserves the historical fact that offline completion was required without leaving the PVC marked as still restart-pending.

#### **Phase Values**

The operator SHALL use the following phase values for status.volumeResizeStatus.phase.

|         Value         |                                          Meaning                                           |
|-----------------------|--------------------------------------------------------------------------------------------|
|        `Validating`         |            The operator is validating the request and environment prerequisites             |
|       `ResizingPVCs`        |                        The operator is submitting PVC resize requests                       |
|    `WaitingForPVCResize`    |     The operator is waiting for Kubernetes and the storage backend to reach the checkpoint  |
| `SynchronizingStatefulSet`  | The operator is capturing recovery metadata and reconciling the StatefulSet template state |
|       `RestartingPods`      |        The operator is restarting Pods to complete filesystem expansion when required        |
|      `WaitingForPodsReady`  |                  The operator is waiting for restarted or adopted Pods to become ready       |
|    `VerifyingResizeOutcome` | The operator is verifying PVC state, StatefulSet template state, Pod readiness, and MarkLogic health |
|         `Completed`         |                             The operation finished successfully                             |
|          `Stalled`          |     The operation is degraded and retryable, but cannot currently make progress safely      |
|          `Failed`           |                  The operation cannot be completed automatically for this attempt            |

#### **Reason Values**

The operator SHOULD populate status.volumeResizeStatus.reason with a machine-readable code when the resize is stalled or failed.

|           Value            |                                       Meaning                                       |
|----------------------------|-------------------------------------------------------------------------------------|
|        `ResizeFailed`         |                    The resize operation failed before meaningful progress was made                   |
|    `PartialResizeFailure`     |                                     Some PVCs succeeded and others failed                             |
|      `ResizeRateLimited`      |                              The provider or API server rate-limited the request                      |
|    `StorageQuotaExceeded`     |                      The underlying storage backend rejected the request for capacity reasons         |
|       `ResizeForbidden`       |                           The operator lacks permission to perform required API actions                |
|     `InvalidResizeRequest`    |                                        The requested resize is invalid                                |
| `StorageClassNotExpandable`   |                               The target StorageClass does not allow expansion                        |
|     `ShrinkNotSupported`      |                                  The request attempted to reduce volume size                          |
|         `PVCNotBound`         |                                  One or more target PVCs are not in `Bound` state                    |
|      `ConcurrentResize`       |                     A second execution path attempted to start while an operation was already active  |
|    `StatefulSetSyncFailed`    |                 StatefulSet synchronization could not be completed safely or in time                  |
|      `PodRecoveryFailed`      |                      One or more Pods failed to recover to the required healthy state                 |
| `TemplateUpdateInterrupted`   |                      The operator was interrupted during StatefulSet delete/recreate reconciliation   |
|  `MarkLogicHealthCheckFailed` |                Infrastructure reconciliation completed, but MarkLogic health verification failed      |
|           `Paused`            |                    The resize was paused by user annotation `marklogic.progress.com/resize-paused`    |
|    `MaxRetriesExceeded`       |                         The retry count exceeded the configured maximum, promoting `Stalled` to `Failed` |
| `MaxOperationTimeExceeded`    |                         The total operation time exceeded the configured maximum                      |

The reason enum is intentionally broader than the controller's internal error taxonomy. Phase-specific details such as delete-versus-recreate failure, or whether a failure was caused by timeout versus API rejection, should be surfaced in `message`, events, and logs rather than expanding the public reason set further.

#### **Failed PVC Record**

Each entry in `failedPVCs` SHOULD use the following shape.

|  Field  |  Type  |                 Description                 |
|---------|--------|---------------------------------------------|
|  `name`   | string |         Name of the PVC that failed         |
| `reason`  |  enum  | Machine-readable reason for the PVC failure |
| `message` | string |         Human-readable error detail         |

#### **Terminal and Waiting State Semantics**

The following semantics apply to terminal and recoverable waiting states:

|   Phase   |                                                Meaning                                                |
|-----------|-------------------------------------------------------------------------------------------------------|
| `Completed` | Terminal success. All required resize and reconciliation steps succeeded, the target Pods are healthy, and the StatefulSet template is synchronized |
|  `Stalled`  | Recoverable waiting state. The operation is degraded but retryable, and the controller may continue automatically after backoff or external remediation |
|  `Failed`   | Terminal failure. The active resize attempt cannot continue automatically and requires a new triggering change or explicit retry mechanism to proceed |

Example status snippets are provided in the appendix.

#### User Experience

##### Initiating a Resize

The user performs a resize by increasing the persistence size in the CR. No separate imperative command is required.

##### Monitoring Progress

The user can observe progress through the custom resource status, Kubernetes events, and operator logs.

Typical commands include:

1.  `kubectl get marklogicgroup <name> -o yaml`
    
2.  `kubectl describe marklogicgroup <name>`
    
3.  `kubectl get pvc`
    
4.  `kubectl describe pvc <pvc-name>`
    

The status object is the primary source of truth for current phase, reason, and completion state. Events provide a concise lifecycle trail. Logs provide controller-level debugging detail.

##### Manual Intervention

Some resize failures are environmental rather than logical. Examples include quota exhaustion, unsupported StorageClass expansion, provider throttling, or pods that do not return healthy after restart.

The operator must support a clear manual recovery model:

1.  If the operation is stalled, the status must identify why.
    
2.  Users must be able to resolve the underlying issue and allow reconciliation to continue.
    
3.  If the operator restarts mid-operation, it must recover from persisted status and continue or surface a safe stalled state rather than starting over blindly.

4.  If a newer desired size was submitted during an active operation, users must be able to determine whether that newer target is deferred or already being reconciled through `targetSize`, `deferredTargetSize`, and `deferredObservedGeneration`.
    

## Architectural Decisions

### PVC-First vs. StatefulSet-First

Volume resize in this operator has three related but distinct concerns:

1.  Live PVC expansion.
    
2.  Filesystem expansion completion, which may require Pod restart.
    
3.  StatefulSet template synchronization so future Pods use the new requested size.

Because the StatefulSet `volumeClaimTemplates` field is not safely mutable for this workflow, the operator must reconcile persistent storage and StatefulSet template state in a controlled sequence. The key architectural decision is whether to resize the live PVCs first or recreate the StatefulSet template first.

#### Method A: PVC-First

Workflow:

1.  Detect requested size increase.
    
2.  Validate the environment and target PVCs.
    
3.  Patch the live PVCs to the new size.
    
4.  Wait for all target PVCs to reach the required checkpoint.
    
5.  Delete the StatefulSet with orphan propagation.
    
6.  Recreate the StatefulSet with updated `volumeClaimTemplates`.
    
7.  Restart Pods if filesystem expansion still requires remount.
    
8.  Verify PVC state, Pods, and application health.

Pros:

1.  Storage validation happens before disruptive workload reconciliation.
    
2.  If the provider rejects the resize, the operator can fail early without modifying the StatefulSet.
    
3.  The rollback surface is minimal because the workload controller has not yet been moved to the new template.
    
4.  Kubernetes tolerates a live PVC being larger than the historical requested template during the transition window.

Cons:

1.  There is a temporary mismatch between actual PVC size and the StatefulSet template.
    
2.  The operator must track an intermediate state until template reconciliation is complete.
    
3.  The controller must model Pod restart explicitly; orphan deletion alone does not trigger remount.

##### Method B: StatefulSet-First

Workflow:

1.  Detect requested size increase.
    
2.  Delete and recreate the StatefulSet with the new template.
    
3.  Patch the live PVCs to match the new template.
    
4.  Restart Pods if required and reconcile health.

Pros:

1.  The workload template reflects the desired end state immediately.

Cons:

1.  A storage-provider failure after StatefulSet recreation leaves the workload definition ahead of actual storage state.
    
2.  Recovery requires rollback logic and another StatefulSet reconciliation step.
    
3.  The operator failure domain becomes larger because storage and workload mutations are coupled too early.
    
4.  Interrupted reconciliation is harder to recover safely.

#### Decision

The operator will use the PVC-first approach.

This approach minimizes the blast radius of failure, validates storage expansion before mutating workload controller state, and keeps rollback logic simple. It aligns better with controller best practices by reconciling persistent storage before reconciling future template state.

Note: If PVCs are successfully expanded but the StatefulSet delete/recreate then fails permanently, the system is in a "safe partial-completion" state. The PVCs are larger than the template, which is tolerable at runtime — the workload continues to function with expanded storage. The template synchronization can be retried independently. This intermediate state is acceptable and should be documented as such in status.

### Concurrency and Fencing

Resize must be treated as a single-writer, multi-step operation.

Required controller rules:

1.  Exactly one active resize operation is allowed per `MarklogicGroup`.
    
2.  The controller claims the operation by writing status using optimistic concurrency on the status subresource. Specifically, the initial status write that sets `operationID` must use a `resourceVersion`-based compare-and-swap. This prevents two controller replicas from both starting a resize if leader election fails momentarily.
    
3.  Resize is serialized against scale, image update, probe update, and any reconcile path that mutates the same StatefulSet or Pod lifecycle.
    
4.  Cluster deletion takes precedence over resize progression.
    
5.  If the desired size changes during an active resize, the new desired size is deferred rather than executed concurrently.

### Resize Strategy

The operator supports two execution strategies:

1.  `parallel`. All PVC resize requests are submitted concurrently.
    
2.  `sequential`. PVCs are resized one at a time.

#### Decision

1.  `parallel` is the default because it minimizes total elapsed time in healthy environments.
    
2.  `sequential` is supported for quota-sensitive or rate-limit-sensitive environments.

3.  `parallel` increases the risk of partial success and partial failure.

4.  `sequential` reduces the immediate blast radius of a PVC failure, but requires stable ordering and persisted active-PVC state to remain crash-safe.

## Detailed Workflow

### Overview

The operator uses a single high-level resize workflow for both `parallel` and `sequential` execution. The selected `resizeStrategy` affects how PVC resize requests are submitted and how progress is checkpointed. The later phases are shared, but Pod restart is modeled explicitly rather than being conflated with StatefulSet reconciliation.

The common phase model is:

1.  `Validating`
    
2.  `ResizingPVCs`
    
3.  `WaitingForPVCResize`
    
4.  `SynchronizingStatefulSet`
    
5.  `RestartingPods`, if required
    
6.  `WaitingForPodsReady`
    
7.  `VerifyingResizeOutcome`
    
8.  `Completed`

### Strategy Model

The operator supports two execution strategies in v1:

1.  `parallel`  
    The operator submits resize requests for all target PVCs during the `ResizingPVCs` phase, then waits for all target PVCs to reach the required checkpoint during `WaitingForPVCResize`.
    
2.  `sequential`  
    The operator submits a resize request for exactly one target PVC during `ResizingPVCs`, then waits for that PVC to reach the required checkpoint during `WaitingForPVCResize`. If additional PVCs remain, the workflow loops back to `ResizingPVCs` and repeats until all PVCs have been processed in stable persisted order.

The selected strategy does not change the later phases of the workflow. StatefulSet synchronization, Pod restart handling, and final verification behavior are identical for both strategies.

#### Phase: `Validating`

Purpose:  
Validate the resize request and the execution environment before making any changes to PVCs or workload resources.

Entry criteria:

1.  The effective requested size for the target MarkLogic group is greater than the current size.
    
2.  No conflicting resize operation is already active for the same target group.

Actions:

1.  Resolve the effective persistence configuration for the target group.
    
2.  Identify the PVCs owned by or associated with the group.
    
3.  Verify that persistence is enabled.
    
4.  Verify that the requested size is greater than the current effective size.
    
5.  Verify that all target PVCs are in `Bound` state.
    
6.  Verify that the relevant `StorageClass` allows expansion.
    
7.  Record the operation metadata in status, including `operationID`, `observedGeneration`, `currentSize`, `targetSize`, `totalPvcs`, `resizeStrategy`, and `firstStartedTime`.
    
8.  Build and persist the stable ordered PVC list that will be used for the full operation in both `parallel` and `sequential` modes.

9.  Verify that the StatefulSet `updateStrategy` is `OnDelete`. If `RollingUpdate` is set, the StatefulSet controller could independently roll Pods during the resize window, creating a race condition. In v1, the operator must reject resize when `updateStrategy` is not `OnDelete` and surface `InvalidResizeRequest` rather than mutating the StatefulSet strategy implicitly.

10. Emit a warning event if any target PersistentVolume has `reclaimPolicy: Delete`. This does not block the operation, but surfaces the risk that accidental PVC deletion by another actor would permanently destroy data.

Exit criteria:

1.  All target PVCs are identified.
    
2.  All validation checks required to begin resize have passed.

Failure behavior:

1.  If validation fails, the operator transitions to `Failed` or `Stalled` with an appropriate machine-readable reason.
    
2.  No PVC or StatefulSet mutation occurs if validation fails.

#### Phase: `ResizingPVCs`

Purpose:  
Submit PVC resize requests according to the selected strategy.

Shared actions:

1.  Determine which PVC or PVCs are eligible for resize submission in the current reconcile cycle.
    
2.  Patch the target PVC request size to the desired `targetSize`.
    
3.  Record progress in status.
    
4.  Never submit additional PVC patches if the operation is already in a degraded state.

Strategy-specific behavior:

For `parallel`:

1.  The operator submits resize requests for all target PVCs in scope.
    
2.  The operator may submit all PVC patch requests in a single reconcile cycle.
    
3.  The operator does not proceed to later phases until all target PVCs reach the required checkpoint.

For `sequential`:

1.  The operator submits a resize request for exactly one PVC at a time.
    
2.  The operator does not submit the next PVC resize request until the current PVC has reached the required checkpoint in `WaitingForPVCResize`.
    
3.  The currently active PVC must be persisted in status.

Exit criteria:

For `parallel`:

1.  Resize requests for all target PVCs have been accepted by the Kubernetes API.

For `sequential`:

1.  Resize request for the current PVC has been accepted by the Kubernetes API.

Failure behavior:

1.  If one or more PVC patch operations fail, the operator transitions to `Stalled` or `Failed`.
    
2.  Per-PVC failure information should be recorded in `failedPVCs` when applicable.

#### Phase: `WaitingForPVCResize`

Purpose:  
Wait for the storage system and Kubernetes control plane to advance PVCs to the checkpoint required for continuation.

Checkpoint definition:

A PVC is considered checkpointed for phase advancement when the resize request has been accepted and the PVC has reached the state required for the next workflow step. This may mean that block-level expansion is complete, or that the PVC has reached the state that proves workload-side action is still required.

Concretely, a PVC has reached the required checkpoint when one of the following is true:

1.  **Online expansion complete:** `pvc.status.capacity[storage]` >= `targetSize` AND the PVC does not have a condition of type `FileSystemResizePending` with status `True`. In this case, no Pod restart is needed for this PVC.

2.  **Offline expansion pending:** `pvc.spec.resources.requests[storage]` == `targetSize` AND the PVC has a condition of type `FileSystemResizePending` with status `True`. This confirms the block-level expansion has been acknowledged and workload-side remount is still required.

The operator must record per-PVC checkpoint type (online vs. offline) in `pvcStatuses` so the later restart-handling logic can determine whether any Pod restart is needed and which Pods are eligible for selective restart.

Shared actions:

1.  Observe PVC status.
    
2.  Detect progress toward the required checkpoint.
    
3.  Update `pvcsCheckpointed` and the corresponding `pvcStatuses` entries as PVCs reach the expected checkpoint.

Strategy-specific behavior:

For `parallel`:

1.  The operator waits until all target PVCs reach the required checkpoint.
    
2.  `pvcsCheckpointed` may increase by more than one between reconcile cycles.

For `sequential`:

1.  The operator waits until the current PVC reaches the required checkpoint.
    
2.  If additional PVCs remain, the operator transitions back to `ResizingPVCs` for the next PVC.
    
3.  `pvcsCheckpointed` increments one PVC at a time.

4.  `pvcsCheckpointed` must be monotonically increasing. If the observed count ever decreases (for example, because a previously checkpointed PVC regresses due to a storage backend anomaly), the operator must transition to `Stalled` with reason `PartialResizeFailure` rather than continuing the loop. This prevents infinite cycling between `ResizingPVCs` and `WaitingForPVCResize`.

Exit criteria:

For `parallel`:

1.  All target PVCs have reached the required checkpoint.

For `sequential`:

1.  If more PVCs remain, transition back to `ResizingPVCs`.
    
2.  If no PVCs remain, proceed to `SynchronizingStatefulSet`.

Failure behavior:

1.  If PVC progress stalls beyond an acceptable threshold, the operator transitions to `Stalled` or `Failed`.
    
2.  Provider rejections, quota issues, or rate limiting are surfaced through `reason`, `message`, and retry metadata.

#### Phase: `SynchronizingStatefulSet`

Purpose:  
Capture the recovery metadata needed for safe resume and reconcile the StatefulSet template to the new storage size.

Actions:

1.  Read the existing StatefulSet associated with the target group.
    
2.  Persist sufficient recovery metadata to resume or diagnose the workflow if the operator is interrupted.
    
3.  Record the original StatefulSet UID and enough status to distinguish delete-issued from delete-observed.
    
4.  Delete the StatefulSet using orphan propagation.
    
5.  Persist that delete has been issued before later steps can proceed.
    
6.  Re-check for existence of the original StatefulSet by name and UID until deletion is observed, while confirming that Pods and PVCs remain preserved.
    
7.  Create a new StatefulSet using the updated desired configuration.
    
8.  Ensure non-resize fields remain consistent with the current MarkLogic group spec.
    
9.  Record the recreated StatefulSet UID in status.

10. The recreated StatefulSet must use identical `.spec.selector` and Pod template labels as the original StatefulSet. If these differ, the StatefulSet will not adopt the orphaned Pods, leaving them permanently orphaned. The controller must derive these from the same label generation logic used for the original, not from an independently computed set.

Exit criteria:

1.  Recovery metadata has been captured successfully.
    
2.  The original StatefulSet object no longer exists.
    
3.  The new StatefulSet exists and reflects the desired storage size.

Failure behavior:

1.  If recovery metadata cannot be captured, the operator transitions to `Failed` or `Stalled`.
    
2.  The operator must not delete the StatefulSet unless backup requirements for recovery have been met.
    
3.  If orphan deletion fails, transition to `Failed` or `Stalled` with reason such as `StatefulSetSyncFailed`.
    
4.  If deletion does not complete in the expected interval, transition to `Stalled` with reason such as `StatefulSetSyncFailed`, with timeout detail recorded in `message`.
    
5.  If StatefulSet recreation fails, transition to `Failed` or `Stalled` with reason such as `StatefulSetSyncFailed`.

#### Phase: `RestartingPods`

Purpose:  
Perform controlled Pod restart if filesystem expansion requires remount-based completion.

Actions:

1.  Determine whether restart is required based on persisted status and observed PVC state.
    
2.  Restart only the Pods listed in `podsRestartPending`, whose associated PVCs required offline expansion. Pods whose PVCs completed expansion online do not need restart and should not be disrupted unnecessarily.
    
3.  Restart Pods in a controlled sequence.
    
4.  After each required Pod restart completes and the filesystem-resize signal is gone for the associated PVC, update that PVC status to `checkpointType: OfflineComplete`, `state: Restarted`, and `restartRequired: false`, and remove the Pod from `podsRestartPending`.

5.  Never assume orphan deletion or StatefulSet recreation has already performed the required restart.

Exit criteria:

1.  Required restart actions have completed successfully.
    
2.  Filesystem expansion has completed as required by the environment.

Failure behavior:

1.  If restarted Pods do not recover, transition to `Stalled` or `Failed`.

#### Phase: `WaitingForPodsReady`

Purpose:  
Wait for all expected Pods to return to healthy running state after restart or StatefulSet recreation.

Actions:

1.  Wait for expected Pods to become `Running`.
    
2.  Wait for expected Pods to become `Ready`.

Exit criteria:

1.  All expected Pods are healthy.

Failure behavior:

1.  If Pods fail to schedule or become ready, transition to `Stalled` with reason such as `PodRecoveryFailed`, with the specific readiness or scheduling detail recorded in `message`.

#### Phase: `VerifyingResizeOutcome`

Purpose:  
Verify that the end state is correct before marking the operation complete.

Actions:

1.  Verify that the requested PVC size equals the target size.
    
2.  Verify that observed PVC capacity has reached the target size.
    
3.  Verify that no remaining filesystem-expansion-required signal is present for targeted PVCs.
    
4.  Verify that the recreated StatefulSet template reflects the target size.
    
5.  Verify MarkLogic health checks, if defined by the operator.

Exit criteria:

1.  Storage state, StatefulSet template state, Pod readiness, and application health all reflect the desired end state.

Failure behavior:

1.  If infrastructure is healthy but MarkLogic validation fails, transition to `Stalled` with reason such as `MarkLogicHealthCheckFailed`.

#### Phase: `Completed`

Purpose:  
Mark the resize workflow as successfully completed.

Actions:

1.  Set terminal success status.
    
2.  Record `completionTime`.
    
3.  Emit completion event.
    
4.  Preserve final status for user visibility and auditability.

Exit criteria:

1.  The target size has been reconciled successfully.
    
2.  PVCs, StatefulSet template state, Pod health, and application health are all consistent with the desired end state.

### State Transition Notes

The main state transition difference between the two strategies is the behavior between `ResizingPVCs` and `WaitingForPVCResize`.

For `parallel`:

1.  `Validating` -> `ResizingPVCs`
    
2.  `ResizingPVCs` -> `WaitingForPVCResize`
    
3.  `WaitingForPVCResize` -> `SynchronizingStatefulSet` once all PVCs reach the required checkpoint
    
4.  Later phases proceed linearly through StatefulSet synchronization, restart if required, readiness waiting, and final verification

For `sequential`:

1.  `Validating` -> `ResizingPVCs`
    
2.  `ResizingPVCs` -> `WaitingForPVCResize`
    
3.  `WaitingForPVCResize` -> `ResizingPVCs` if more PVCs remain
    
4.  `WaitingForPVCResize` -> `SynchronizingStatefulSet` only after the final PVC reaches the required checkpoint
    
5.  Later phases are identical to `parallel`

### Progress Reporting Rules

The operator must report progress consistently for both strategies.

Rules:

1.  `totalPvcs` is set once at the start of the operation.
    
2.  `pvcsCheckpointed` increments when a PVC reaches the checkpoint required for workflow advancement.
    
3.  For `parallel`, `pvcsCheckpointed` may increase by more than one between reconcile cycles.
    
4.  For `sequential`, `pvcsCheckpointed` increases one PVC at a time.
    
5.  `resizeStrategy` must be visible in status.
    
6.  The operator must record the stable ordered PVC list for every active resize operation and the currently active PVC in `sequential` mode.

7.  `pvcStatuses` must contain one entry for each targeted PVC, including checkpoint type and restart requirement when known. If `orderedTargetPVCs` is persisted for recovery, the two lists must remain aligned.

### Happy Path Summary

The happy path for both strategies is identical after PVC checkpoint completion. The only difference is whether PVCs are processed all at once or one at a time before StatefulSet template synchronization begins.

For `parallel`, all PVCs are patched and then all PVCs must reach the checkpoint before the workflow advances.

For `sequential`, PVCs are processed one by one, looping between `ResizingPVCs` and `WaitingForPVCResize` until all PVCs are checkpointed, then the workflow advances.

## Failure Handling and Recovery

### Overview

This section defines how the operator behaves when the resize workflow cannot proceed normally. The goal is to ensure failures are surfaced clearly, recovery is predictable, and persistent data remains protected.

The operator must distinguish between:

1.  Recoverable conditions, where progress is temporarily blocked and may resume after retry or external remediation.
    
2.  Non-recoverable conditions, where the active resize attempt cannot continue automatically.

To support this distinction, the operator uses `Stalled` and `Failed` as separate, deterministic states in the resize workflow.

### Recovery Principles

The operator shall apply the following principles during failure handling and recovery:

1.  Storage safety first. The operator must never delete PVCs or PVs as part of resize failure handling.
    
2.  Fail before disruption. If validation or PVC expansion fails, the operator should stop before modifying the StatefulSet.
    
3.  Preserve recovery state. Before StatefulSet delete/recreate begins, the operator must persist enough metadata to recover from interruption.
    
4.  Surface machine-readable failure reasons. Users must be able to determine why the operation stopped.
    
5.  Do not silently restart the workflow from scratch. If an operation is interrupted, the operator must resume safely or surface an explicit recovery state.
    
6.  Partial progress must be visible. If some PVCs succeed and others fail, the operator must expose that condition in status.
    
7.  Retry only when justified. The operator should retry transient or rate-limited failures, but should not loop indefinitely on permanent errors.

8.  Derive recovery from observed cluster state first and persisted resize status second. A missing or stale status update must not be interpreted as proof that no mutation occurred.

9.  Phase values are intent markers, not completion confirmations. On recovery after a crash, the controller must always re-derive actual state from the live cluster rather than trusting the persisted phase as proof that the corresponding mutation succeeded or failed. For example, a persisted phase of `SynchronizingStatefulSet` does not prove which internal sub-step has completed; the controller must check whether backup metadata exists, whether delete was issued, whether the old StatefulSet still exists, and whether recreation has already occurred.

### State Semantics

The operator uses the following failure-related states:

|  State   |                                 Meaning                                  |                              Expected Operator Behavior                              |
|----------|--------------------------------------------------------------------------|--------------------------------------------------------------------------------------|
| `Stalled` | The operation is degraded but retryable, and safe continuation may still be possible | Preserve status, optionally retry with bounded backoff, and surface remediation hints |
| `Failed`  | The active resize attempt is terminal and cannot safely continue automatically | Stop autonomous progression for that attempt and require a new trigger or explicit retry |

Recommended interpretation:

1.  Use `Stalled` for transient infrastructure problems, quota exhaustion, rate limiting, pod scheduling delays, or interrupted recovery where a safe retry path exists.
    
2.  Use `Failed` for invalid requests, unsupported operations, or controller states where automatic continuation is not safe.

3.  `Stalled` is non-terminal. `completionTime` should not be set while the controller still intends to retry.

4.  `Failed` is terminal for the active operation. `completionTime` should be set when the controller enters `Failed`.

### Failure Categories

Resize failures generally fall into the following categories:

1.  Request validation failures
    
2.  PVC mutation failures
    
3.  Storage provider or Kubernetes control plane delays
    
4.  Partial PVC success with one or more PVC failures
    
5.  StatefulSet delete/recreate failures
    
6.  Pod recovery failures
    
7.  Operator interruption during in-flight reconciliation

These categories should be reflected in machine-readable `reason` values and user-facing `message` text.

### Failure Matrix

|                           Scenario                           |                     Expected Behavior                      | Recommended Phase |       Recommended Reason        |
|--------------------------------------------------------------|------------------------------------------------------------|-------------------|---------------------------------|
|           Requested size is equal to current size            |              Treat as no-op and do not resize             |    No active op   |       n/a                       |
| Invalid resize request, including shrink or persistence-disabled configuration | Reject request and do not start resize | `Failed` | `ShrinkNotSupported` or `InvalidResizeRequest` |
| Preconditions are not met before mutation, such as PVC not `Bound` or non-expandable `StorageClass` | Stop before PVC mutation or wait only when safe to do so | `Stalled` or `Failed` | `PVCNotBound` or `StorageClassNotExpandable` |
| PVC patch submission is rejected by the API server or operator permissions | Stop and surface the error; retry only if clearly transient | `Failed` or `Stalled` | `ResizeForbidden` or `ResizeFailed` |
| Provider-side PVC expansion is blocked by throttling, quota, or prolonged lack of progress | Retry after backoff when appropriate, otherwise require remediation | `Stalled` | `ResizeRateLimited`, `StorageQuotaExceeded`, or broader detail in `message` |
|        Some PVCs resize successfully and others fail         |       Surface partial progress and stop later phases       |     `Stalled`     |    `PartialResizeFailure`       |
| StatefulSet synchronization cannot be completed safely, including backup, delete, recreation, or sync wait failure | Preserve resources and retry only from a safe point | `Stalled` or `Failed` | `StatefulSetSyncFailed` or `ResizeFailed` |
|       Operator restarts during StatefulSet transition        | Resume from persisted state and observed resources         |     `Stalled`     |  `TemplateUpdateInterrupted`    |
|       Pods remain `Pending`, `NotReady`, or unschedulable   |              Surface workload recovery issue               |     `Stalled`     |      `PodRecoveryFailed`        |
| MarkLogic health check fails after infrastructure is healthy |         Surface application-level problem and stop         |     `Stalled`     | `MarkLogicHealthCheckFailed`    |

The controller should keep timeout semantics in `message`, events, and logs rather than creating separate public reason values for each timed-out sub-step.

### Retry and Resume Semantics

The operator should distinguish between transient errors and permanent errors.

##### Retry-Eligible Conditions

The operator may retry when the underlying condition is likely transient, including:

1.  API rate limiting
    
2.  Temporary cloud provider errors
    
3.  Delayed PVC status propagation
    
4.  Temporary pod scheduling failures
    
5.  Kubernetes API conflicts caused by concurrent reconciliation

Recommended behavior:

1.  Increment `retryCount` on each retry attempt.
    
2.  Preserve the current phase or transition to `Stalled` with a retryable reason.
    
3.  Update `message` to reflect that the operator is waiting to retry.
    
4.  Record `nextRetryTime` when the controller intends to retry automatically.

5.  Use bounded retry behavior rather than retrying indefinitely without visibility.

6.  If `retryCount` exceeds the maximum retry limit, the operator must transition to `Failed` rather than continuing to retry.

#### Retry Defaults

The following defaults apply to transient retry behavior in v1. These values should be configurable via operator flags or CR annotations in a future version.

|            Parameter            | Default Value |                                  Notes                                   |
|---------------------------------|---------------|--------------------------------------------------------------------------|
| Initial backoff interval        |   10 seconds  | First retry wait time after a transient failure                          |
| Maximum backoff interval        |    5 minutes  | Backoff ceiling to prevent excessively long waits                        |
| Maximum retry count per phase   |      15       | After this count, the operation transitions to `Failed`                  |
| Maximum total operation time    |     2 hours   | If the resize has not completed within this window, transition to `Failed` |

The backoff strategy is exponential with jitter. The controller doubles the interval on each retry up to the ceiling.

##### Non-Retryable Conditions

The operator should not keep retrying permanent errors, including:

1.  Shrink requests
    
2.  Non-expandable `StorageClass`
    
3.  Invalid target configuration
    
4.  Missing required permissions that are unlikely to self-resolve

Recommended behavior:

1.  Transition to `Failed`.
    
2.  Surface a clear `reason`.
    
3.  Require explicit user remediation before progress resumes.

4.  Do not continue automatically after a permanent error just because a future reconcile loop is triggered.

### Partial Failure Behavior

A resize operation may partially succeed, especially in `parallel` mode.

Examples:

1.  Two PVCs are accepted and expanded successfully, while one PVC is rejected due to quota.
    
2.  One PVC completes, but another remains stuck waiting on the provider.

In such cases, the operator shall:

1.  Record `pvcsCheckpointed` accurately.
    
2.  Populate `failedPVCs` with per-PVC details.
    
3.  Surface a user-visible `message` explaining the partial progress.
    
4.  Transition to `Stalled` and prevent later phases from proceeding until the targeted PVC set is consistent again.

Recommended behavior by strategy:

1.  In `parallel`, partial failure is more likely because multiple requests are in flight simultaneously.
    
2.  In `sequential`, partial failure is typically encountered one PVC at a time and may stop the workflow before later PVCs are attempted.

3.  The operator must not advance to StatefulSet synchronization or Pod restart while any targeted PVC remains outside the required checkpoint.

### Interrupted Operation Recovery

The operator must handle interruption during multi-step workflows, especially around StatefulSet delete/recreate.

Critical requirement:

Before deleting the StatefulSet, the operator must persist enough recovery metadata to determine what step was last completed and what resources must be checked during resume.

At minimum, the persisted recovery record must include:

1.  `operationID`

2.  `observedGeneration`

3.  `currentSize` and `targetSize`

4.  The stable ordered target PVC list

5.  Per-PVC checkpoint state in `pvcStatuses`

6.  The active PVC for sequential mode

7.  The original StatefulSet name and UID

8.  Whether delete has been issued

9.  The recreated StatefulSet UID, if present

10. Whether Pod restart is still pending, including `podsRestartPending`

If the operator restarts during or after the `SynchronizingStatefulSet` phase, it should:

1.  Detect whether the StatefulSet still exists.
    
2.  Detect whether Pods remain running as orphaned Pods.
    
3.  Determine whether StatefulSet recreation has already occurred.
    
4.  Resume from the last safe point or transition to `Stalled` with `TemplateUpdateInterrupted`.

5.  Distinguish the original StatefulSet from a recreated StatefulSet by UID rather than by name alone.

#### StatefulSet Recovery Decision Matrix

The following decision matrix defines the controller's recovery action based on persisted status and observed cluster state after a crash or restart during the StatefulSet delete/recreate window:

| `deleteIssued` | Old STS exists? | New STS exists? | Matching UID | Recovery Action |
|---|---|---|---|---|
| `false` | yes | no | n/a | Delete has not started. Resume `SynchronizingStatefulSet` from the backup-and-delete portion. |
| `true` | yes | no | n/a | Delete was issued but has not completed. Continue `SynchronizingStatefulSet` while waiting for deletion or re-issuing the orphan delete. |
| `true` | no | no | n/a | Delete completed but recreation has not started. Continue `SynchronizingStatefulSet` with recreation. |
| `true` | no | yes | Matches `recreatedUID` | Already recreated successfully. Advance to `RestartingPods` if restart is still required, otherwise continue to the next readiness or verification step. |
| `true` | no | yes | Unknown UID, `recreatedUID` not set | Ambiguous: another actor created a StatefulSet with the same name. Transition to `Stalled` with reason `TemplateUpdateInterrupted`. Requires manual investigation. |
| `false` | no | no | n/a | StatefulSet is missing but `deleteIssued` was not persisted (crash between status write and delete, or external deletion). Transition to `Stalled` with reason `TemplateUpdateInterrupted`. |

The controller must always verify that orphaned Pods and PVCs still exist before proceeding with any recovery action. If Pods or PVCs are missing, the operation must transition to `Failed` immediately.

The operator must not assume that interruption means the workflow should start over from `Validating`.

It must also not assume that a recreated StatefulSet implies that Pod restart has already occurred.

### Manual Intervention Expectations

The operator must provide enough information for a user to recover from blocked or failed resize operations.

Typical manual intervention scenarios include:

1.  Expanding backend storage quota
    
2.  Fixing a non-expandable or misconfigured `StorageClass`
    
3.  Resolving cluster scheduling issues
    
4.  Investigating MarkLogic health failures after infrastructure reconciliation
    
5.  Correcting operator RBAC or permission issues

The status contract should help the user answer these questions:

1.  What step failed?
    
2.  Why did it fail?
    
3.  Which PVCs succeeded?
    
4.  Which PVCs failed?
    
5.  Is the operation safe to retry?

6.  Is a newer desired target size deferred behind the active operation?

### User-Facing Recovery Guidance

When the operation is not successful, the status should make the next action obvious.

Recommended examples:

|         Condition          |                      Suggested User Action                      |
|----------------------------|-----------------------------------------------------------------|
|    `StorageQuotaExceeded`    |    Increase storage quota or retry after capacity is available  |
| `StorageClassNotExpandable`  | Migrate to an expandable storage class or do not attempt resize |
|        `PVCNotBound`         |   Wait for PVC binding or resolve storage provisioning issue    |
|     `ResizeRateLimited`      |           Wait for provider cooldown or retry window            |
|      `PodRecoveryFailed`     |   Resolve node capacity, readiness, affinity, taint, or topology issues |
| `MarkLogicHealthCheckFailed` |   Investigate MarkLogic application health before continuing    |

### Recovery Outcome Requirements

A recovery-capable implementation in v1 should satisfy the following outcomes:

1.  Invalid resize requests fail early and clearly.
    
2.  Transient infrastructure issues do not cause silent data loss or hidden controller loops.
    
3.  Partial success is visible and diagnosable.
    
4.  Interrupted StatefulSet reconciliation can be resumed safely or surfaced explicitly.
    
5.  The user can determine the difference between a blocked operation and a permanently failed one.

6.  The controller does not silently restart the workflow from the beginning after Pod crash or operator rollout.

### Required Infrastructure Permissions

The implementation requires permissions beyond the current StatefulSet-centric reconciliation path.

At minimum, the operator must have:

1.  `persistentvolumeclaims`: `get`, `list`, `watch`, `patch`, `update`

2.  `persistentvolumeclaims/status`: `get`

3.  `persistentvolumes`: `get`, `list`, `watch`

4.  `storageclasses`: `get`, `list`, `watch`

5.  `statefulsets`: `get`, `list`, `watch`, `create`, `delete`

6.  `pods`: `get`, `list`, `watch`, `delete`

7.  `events`: `create`, `patch`, `update`

8.  `marklogicgroups/status`: `get`, `patch`, `update`

#### Required RBAC Additions

The current operator ClusterRole does not include permissions for PVCs, PVC status, PVs, StorageClasses, or Events. The following rules must be added to the `manager-role` ClusterRole:

```yaml
# PVC permissions for resize
- apiGroups:
  - ""
  resources:
  - persistentvolumeclaims
  verbs:
  - get
  - list
  - watch
  - patch
  - update
- apiGroups:
  - ""
  resources:
  - persistentvolumeclaims/status
  verbs:
  - get
# PV permissions for reclaim policy warnings
- apiGroups:
  - ""
  resources:
  - persistentvolumes
  verbs:
  - get
  - list
  - watch
# StorageClass permissions for expansion validation (cluster-scoped)
- apiGroups:
  - storage.k8s.io
  resources:
  - storageclasses
  verbs:
  - get
  - list
  - watch
# Event permissions for lifecycle reporting
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

Note: `persistentvolumes` and `storageclasses` are cluster-scoped resources. The operator already uses a `ClusterRole`, so no additional binding type is required. The `events` entry covers both the core API group and the `events.k8s.io` group for broad compatibility.

Because v1 rejects resize when the StatefulSet `updateStrategy` is not `OnDelete`, no additional StatefulSet update permission is required for strategy mutation as part of this feature.

This section pairs well with Observability, since status, events, and logs are the main tools users rely on during recovery.

## Observability

### Overview

The operator must provide enough observability for a user to understand:

1.  Whether a resize operation has started
    
2.  What phase it is currently in
    
3.  Whether progress is being made
    
4.  Whether the operation is blocked or failed
    
5.  What action, if any, is required from the user
    

For v1, the primary observability surfaces are:

1.  Custom resource status
    
2.  Kubernetes events
    
3.  Operator logs
    

These surfaces must be consistent with one another and must reflect the same underlying workflow state.

### Status as the Primary Source of Truth

The custom resource status is the authoritative view of resize progress.

The operator shall expose resize progress under status.volumeResizeStatus and keep it updated throughout the workflow. This status object is intended to answer the most important operational questions without requiring log inspection.

At minimum, status must communicate:

1.  The current phase
    
2.  The current and target sizes
    
3.  The selected resize strategy
    
4.  The number of PVCs checkpointed versus total PVCs in scope
    
5.  Whether the operation is successful, stalled, or failed
    
6.  The most relevant machine-readable reason when the operation cannot proceed
    
7.  Timestamps for phase transitions, retry scheduling, and completion

8.  The active PVC in sequential mode

9.  The operation identifier and observed generation

10. Per-PVC checkpoint state, including whether each checkpoint completed online or still requires Pod restart

11. Deferred target size information when a newer desired size is waiting behind the active operation
    

Recommended interpretation:

1.  `phase` tells the user where the workflow is
    
2.  `reason` tells the user why progress is blocked or failed
    
3.  `message` tells the user what is happening in plain language
    
4.  `pvcsCheckpointed` and `totalPvcs` show concrete progress
    

### Conditions

In addition to status.volumeResizeStatus, the operator may expose resize state through standard Kubernetes conditions on the custom resource.

If conditions are used, they should complement the detailed resize status rather than duplicate it. The recommended approach is:

1.  Use conditions for high-level, stable signals such as success or failure
    
2.  Use status.volumeResizeStatus for detailed workflow state, progress counters, and failure breakdown
    

Recommended resize-related conditions for v1:

|     Condition Type     |                  Meaning                   |
|------------------------|--------------------------------------------|
| `VolumeResizeInProgress` |   A resize workflow is currently active    |
|  `VolumeResizeComplete`  | The resize workflow completed successfully |
|  `VolumeResizeDegraded`  |  The resize workflow is stalled or failed  |

Recommended condition semantics:

1.  Conditions should change only on meaningful lifecycle transitions
    
2.  Condition `Reason` values should align with the resize `reason` field where practical
    
3.  Condition messages should be short and stable, while status.volumeResizeStatus.message can provide more detail

4.  Conditions should not mirror every internal workflow phase; detailed phase tracking belongs only in `volumeResizeStatus`
    

### Kubernetes Events

The operator should emit Kubernetes events at major workflow transitions and on significant errors.

Events are useful for quick operational diagnosis and for users who prefer `kubectl describe` over raw status inspection.

Recommended events include:

| Event Type |        Example Reason        |                  When Emitted                   |
|------------|------------------------------|-------------------------------------------------|
|   `Normal`   |    `VolumeResizeRequested`     |       A valid resize request is detected        |
|   `Normal`   |    `VolumeResizeProgressing`   | PVC resize work is underway or waiting on storage progress |
|   `Normal`   |   `StatefulSetSyncStarted`     | StatefulSet synchronization has begun           |
|   `Normal`   |    `PodRestartStarted`         |     Controlled Pod restart has begun            |
|  `Warning`   |      `VolumeResizeStalled`     |     The workflow is blocked but may recover     |
|  `Warning`   |      `VolumeResizeFailed`      |    The operation has reached a failed state     |
|   `Normal`   |    `VolumeResizeCompleted`     |      The operation completed successfully       |

The event reason set should stay coarser than the full phase model. Event messages should carry the specific phase, PVC name, retry state, or timeout detail rather than expanding the public event reason list for each internal sub-step.

Event guidelines:

1.  Emit events only for meaningful state changes
    
2.  Avoid event spam inside polling loops
    
3.  Use `Warning` only when operator action is blocked, degraded, or failed
    
4.  Include the resource name and target size where useful
    

### Operator Logs

Operator logs are the detailed debugging surface for resize behavior. They are not the primary user-facing contract, but they must provide enough structured detail to diagnose failures that are not obvious from status alone.

Each major reconcile step should log:

1.  The target resource identity
    
2.  The current phase
    
3.  The `currentSize` and `targetSize`
    
4.  The selected `resizeStrategy`
    
5.  The PVC or PVCs being acted on
    
6.  Transition into and out of important phases
    
7.  Retry count and backoff-related behavior
    
8.  Terminal success or failure

9.  The active operation identifier
    

Recommended log fields include:

|     Field      |                      Purpose                      |
|----------------|---------------------------------------------------|
|   `namespace`    |          Identifies the target namespace          |
|      `name`      |       Identifies the target MarkLogic group       |
|     `phase`      |           Shows current workflow phase            |
|  `currentSize`   |     Shows original size at start of operation     |
|   `targetSize`   |             Shows desired final size              |
| `resizeStrategy` | Distinguishes parallel versus sequential behavior |
|      `pvc`       |   Identifies the PVC currently being processed    |
| `operationID`    |       Correlates logs across reconcile cycles     |
| `pvcsCheckpointed` |             Shows progress count                |
|   `totalPvcs`    |                 Shows total scope                 |
|     `reason`     |     Surfaces machine-readable failure reason      |
|   `retryCount`   |          Helps diagnose repeated retries          |

Logging guidelines:

1.  Use structured logs rather than freeform only
    
2.  Log phase transitions once per transition
    
3.  Avoid excessive repeated logs while waiting for PVC status or pod readiness
    
4.  Include enough context to correlate a single resize operation across multiple reconcile cycles
    

### User Inspection Commands

The spec should document how users observe the workflow in practice.

Recommended commands:

|                  Command                  |                           Purpose                            |
|-------------------------------------------|--------------------------------------------------------------|
| `kubectl get marklogicgroup <name> -o yaml` |     Inspect detailed status, phase, reason, and progress     |
|  `kubectl describe marklogicgroup <name>`   |              Inspect events and summary status               |
|              `kubectl get pvc`              |               Check PVC names and basic state                |
|      `kubectl describe pvc <pvc-name>`      | Inspect PVC capacity, conditions, and storage-related events |
|             `kubectl get pods`              |              Check pod lifecycle and readiness               |
|      `kubectl describe pod <pod-name>`      |          Inspect pod scheduling and restart issues           |

Recommended user guidance:

1.  Use custom resource status to understand the workflow state
    
2.  Use PVC inspection to validate storage-side progress
    
3.  Use pod inspection when the workflow reaches `RestartingPods`, `WaitingForPodsReady`, or `VerifyingResizeOutcome`
    
4.  Use operator logs when status and events do not fully explain the problem
    

### Observability Requirements

A v1 implementation should satisfy the following observability outcomes:

1.  A user can determine whether a resize is active
    
2.  A user can determine which workflow phase is currently executing
    
3.  A user can determine how many PVCs have completed versus how many remain
    
4.  A user can distinguish between a blocked operation and a permanently failed operation
    
5.  A user can identify the most likely next action when the operation does not complete successfully
    
6.  A user can correlate CR status, events, and logs without ambiguity
    

### Non-Goals for v1 Observability

The following are not required for v1:

1.  Dedicated Prometheus metrics for resize workflow internals
    
2.  Distributed tracing across operator, Kubernetes, and CSI components
    
3.  Per-step latency histograms
    
4.  A separate dashboard or UI
    

These may be added later, but they are not required for the core functional contract.

### Summary

For v1, observability is successful if the user can answer three questions quickly:

1.  What is the operator doing now?
    
2.  Why is it stuck or failed, if it is not progressing?
    
3.  Is it safe to wait, retry, or intervene?
    

## Testing Strategy

### Overview

The testing strategy for volume resize must verify both correctness and recoverability. Because this feature modifies persistent storage state and coordinates multiple Kubernetes resources, testing must cover more than the nominal happy path.

The v1 test plan should verify:

1.  Correct request validation
    
2.  Correct behavior for both `parallel` and `sequential` strategies
    
3.  Correct status, events, and recovery metadata updates
    
4.  Safe handling of partial failures and interrupted workflows
    
5.  Safe post-resize workload reconciliation

6.  Safe serialization of resize against other StatefulSet-mutating operations
    

Testing should be organized across three layers:

1.  Unit tests
    
2.  Controller or reconciliation tests
    
3.  End-to-end tests
    

#### Unit Tests

Unit tests should validate isolated logic without requiring a live Kubernetes cluster.

#### Validation Logic

Unit tests should cover:

1.  Requested size greater than current size
    
2.  Requested size equal to current size
    
3.  Requested size less than current size
    
4.  Persistence disabled
    
5.  Missing or invalid strategy value handling, if applicable
    
6.  Concurrent resize detection logic

7.  Deferred desired-size updates while an operation is already active
    

Expected outcomes:

1.  Valid increase is accepted
    
2.  Equal size is treated as no-op
    
3.  Shrink is rejected
    
4.  Invalid configuration produces a stable machine-readable reason
    

#### Strategy Selection Logic

Unit tests should cover:

1.  Default strategy selection when `resizeStrategy` is omitted
    
2.  Explicit `parallel` strategy
    
3.  Explicit `sequential` strategy

4.  Stable PVC ordering persistence for sequential mode
    

Expected outcomes:

1.  Default behavior resolves to `parallel`
    
2.  Parallel mode identifies all PVCs as targets for the current submission step
    
3.  Sequential mode identifies only one PVC at a time for the current submission step
    

#### Phase Transition Logic

Unit tests should cover transitions between:

1.  `Validating` and `ResizingPVCs`
    
2.  `ResizingPVCs` and `WaitingForPVCResize`
    
3.  `WaitingForPVCResize` and `ResizingPVCs` in sequential mode
    
4.  `WaitingForPVCResize` and `SynchronizingStatefulSet`
    
5.  Internal progression within `SynchronizingStatefulSet`

6.  `SynchronizingStatefulSet` and `RestartingPods`

7.  `RestartingPods`, `WaitingForPodsReady`, and `VerifyingResizeOutcome`
    
8.  Transitions into `Stalled` and `Failed`
    

Expected outcomes:

1.  Valid state transitions occur only when entry and exit criteria are met
    
2.  Sequential mode loops correctly until all PVCs are processed
    
3.  Invalid transitions are prevented
    

#### Status Update Logic

Unit tests should cover:

1.  Initialization of `volumeResizeStatus`
    
2.  Progress updates to `pvcsCheckpointed`
    
3.  Timestamp updates
    
4.  Recording `reason` and `message`
    
5.  Population of `failedPVCs`
    
6.  Retry count increments
    
7.  Terminal state recording

8.  Persistence of `operationID`, `observedGeneration`, and active PVC state

9.  Persistence of `pvcStatuses`, including checkpoint type and restart requirement

10. Recording `deferredTargetSize` and `deferredObservedGeneration` when desired size changes during an active operation
    

Expected outcomes:

1.  Status fields remain internally consistent
    
2.  Progress reflects actual workflow state
    
3.  Failure metadata is preserved across retries and transitions
    

### Controller and Reconciliation Tests

Controller tests should verify reconciliation logic across multiple reconcile cycles, usually using a fake Kubernetes client or controller-runtime test environment.

#### Happy Path: Parallel

Test scenario:

1.  A resize request increases `spec.persistence.size`
    
2.  The operator validates the request
    
3.  All PVC resize requests are submitted
    
4.  All PVCs reach the expected checkpoint
    
5.  StatefulSet backup, delete/recreate, and pod verification succeed
    

Assertions:

1.  All PVCs are patched
    
2.  Status phases progress in order
    
3.  `pvcsCheckpointed` reaches `totalPvcs`
    
4.  Terminal state is `Completed`
    

#### Happy Path: Sequential

Test scenario:

1.  A resize request increases `spec.persistence.size`
    
2.  The operator validates the request
    
3.  One PVC is resized and waited on at a time
    
4.  The workflow loops until all PVCs are complete
    
5.  StatefulSet reconciliation succeeds
    

Assertions:

1.  PVCs are patched in sequence, not all at once
    
2.  The workflow loops between `ResizingPVCs` and `WaitingForPVCResize`
    
3.  Progress increments one PVC at a time and `activePVC` is stable across reconcile cycles
    
4.  `pvcStatuses` records each PVC checkpoint before the next PVC is processed

5.  Terminal state is `Completed`
    

#### Validation Failure

Test scenarios:

1.  Shrink request
    
2.  Non-expandable `StorageClass`
    
3.  PVC not `Bound`
    
4.  Persistence disabled
    

Assertions:

1.  No PVC mutation occurs
    
2.  No StatefulSet mutation occurs
    
3.  The workflow enters `Failed` or `Stalled` with the expected reason
    

#### Partial Failure

Test scenario:

1.  One PVC patch succeeds
    
2.  Another PVC patch fails due to simulated provider or API rejection
    

Assertions:

1.  `pvcsCheckpointed` reflects partial progress
    
2.  `failedPVCs` is populated correctly
    
3.  The workflow transitions to `Stalled` or `Failed`
    
4.  Successful PVC mutations are not hidden or overwritten

5.  Later phases do not begin while any target PVC remains outside the required checkpoint

6.  `pvcStatuses` exposes which PVCs checkpointed, which failed, and which remain pending
    

#### Retry Behavior

Test scenario:

1.  A transient failure occurs, such as simulated rate limiting
    
2.  The next reconcile cycle succeeds
    

Assertions:

1.  `retryCount` increments
    
2.  The status reflects the temporary stall
    
3.  The operator resumes without restarting the entire workflow
    
4.  Terminal state becomes `Completed` after recovery
    

#### Interrupted StatefulSet Workflow

Test scenario:

1.  The workflow progresses into `SynchronizingStatefulSet`
    
2.  The StatefulSet is deleted
    
3.  The operator restarts or reconciliation state is reloaded before recreation
    

Assertions:

1.  Recovery metadata is preserved
    
2.  The operator detects the partially completed workflow
    
3.  The workflow resumes safely or transitions to `Stalled` with `TemplateUpdateInterrupted`
    
4.  The operator does not restart from `Validating` as though nothing happened

5.  The controller distinguishes the original and recreated StatefulSet by UID
    

#### Pod Recovery Failure

Test scenario:

1.  PVC resize and StatefulSet recreation succeed
    
2.  One or more pods fail to become healthy
    

Assertions:

1.  The workflow transitions to `Stalled` or `Failed`
    
2.  The reason reflects pod scheduling or health failure
    
3.  Completion is not reported prematurely

#### Overlapping Operation Requests

Test scenarios:

1.  A scale-up request is submitted while resize is active

2.  A scale-down request is submitted while resize is active

3.  An image or probe update is submitted while resize is active

4.  The desired size is increased again while resize is active

Assertions:

1.  The controller does not execute conflicting StatefulSet mutations concurrently with resize

2.  The active resize operation remains fenced to its snapshotted target size

3.  A later desired size increase is deferred rather than run concurrently

4.  `deferredTargetSize` and `deferredObservedGeneration` are visible while the newer target waits behind the active operation
    

### End-to-End Tests

End-to-end tests should validate the full workflow against a real Kubernetes cluster and supported storage drivers.

#### Required Happy Path Coverage

The v1 end-to-end suite should include:

1.  Parallel resize on a supported expandable storage class
    
2.  Sequential resize on a supported expandable storage class
    
3.  Online expansion path where no additional pod restart is required
    
4.  Offline expansion path where Pod restart is required

5.  Template synchronization path where StatefulSet recreation is required
    

Assertions:

1.  PVC capacity increases to the requested size
    
2.  The workflow reaches `Completed`
    
3.  Pods return healthy
    
4.  Data remains intact
    

#### Failure Coverage

The v1 end-to-end suite should include representative failure scenarios where practical:

1.  Unsupported or non-expandable storage class
    
2.  Provider or quota rejection
    
3.  Pod recovery failure after StatefulSet recreation
    
4.  Sequential mode interruption between PVCs
    
5.  Operator interruption during StatefulSet transition

6.  Operator interruption after StatefulSet recreation but before Pod restart
    

Assertions:

1.  Failure is surfaced in status and events
    
2.  Recovery behavior is predictable
    
3.  Data volumes are preserved
    
4.  Partial progress is not lost
    

### Suggested Test Matrix

|            Test Area            | Parallel | Sequential | Success Case | Failure Case |
|---------------------------------|----------|------------|--------------|--------------|
|           Validation            |   Yes    |    Yes     |     Yes      |     Yes      |
|      PVC resize submission      |   Yes    |    Yes     |     Yes      |     Yes      |
|     Wait for PVC completion     |   Yes    |    Yes     |     Yes      |     Yes      |
|    Sequential loop behavior     |    No    |    Yes     |     Yes      |     Yes      |
|       Partial PVC failure       |   Yes    |    Yes     |     Yes      |     Yes      |
| StatefulSet backup and recreate |   Yes    |    Yes     |     Yes      |     Yes      |
|        Pod verification         |   Yes    |    Yes     |     Yes      |     Yes      |
|      Interrupted recovery       |   Yes    |    Yes     |     Yes      |     Yes      |
| Overlapping mutation requests   |   Yes    |    Yes     |     Yes      |     Yes      |

### Observability Verification

Testing should verify observability, not just resource mutation.

Tests should assert:

1.  The expected phase is written to status
    
2.  `reason` and `message` are meaningful when failures occur
    
3.  `pvcsCheckpointed` and `totalPvcs` remain accurate
    
4.  Events are emitted at major lifecycle transitions
    
5.  Logs include enough information to correlate reconcile progress

6.  `operationID`, `activePVC`, `nextRetryTime`, `pvcStatuses`, and deferred target fields are present when relevant
    

This is important because a functionally correct resize workflow that is operationally opaque is not acceptable for v1.

### Non-Goals for v1 Testing

The v1 test plan does not need to prove every cloud-provider-specific edge case exhaustively. It should instead:

1.  Validate the contract on at least the primary supported storage environments
    
2.  Cover representative classes of transient and permanent failures
    
3.  Demonstrate that recovery behavior is safe and observable
    

Provider-specific stress or scale testing may be added later.

### Exit Criteria for Feature Readiness

The volume resize feature should not be considered ready for release until the following are true:

1.  Validation behavior is fully covered by unit tests
    
2.  Both `parallel` and `sequential` workflows are covered by controller tests
    
3.  At least one supported end-to-end environment demonstrates successful resize completion
    
4.  At least one representative failure scenario demonstrates safe and observable recovery behavior
    
5.  Status, events, and logs are validated as part of the test plan
    

## Rollout and Compatibility

### Overview

This section defines how the volume resize feature is introduced without disrupting existing MarkLogic deployments or existing operator behavior.

The rollout approach for v1 should prioritize:

1.  Backward compatibility for existing custom resources
    
2.  Safe introduction of new spec and status fields
    
3.  Predictable behavior when upgrading the operator
    
4.  Clear constraints around what combinations of Kubernetes storage behavior are supported
    

The feature should be additive. Existing users who do not change persistence size should observe no behavioral change.

### Backward Compatibility

The feature introduces new resize-related behavior, but it must not break existing resources or existing reconciliation flows.

The v1 implementation should satisfy the following compatibility requirements:

1.  Existing `MarklogicGroup` resources that do not use resize must continue to reconcile normally.
    
2.  Existing resources that do not define `spec.persistence.resizeStrategy` must continue to function without modification.
    
3.  The operator must default missing `resizeStrategy` to `parallel` rather than treating its absence as invalid.
    
4.  Existing resources without status.volumeResizeStatus must remain valid and readable.
    
5.  The operator must tolerate older objects created before the resize status fields existed.
    

Recommended implementation assumption:

1.  Resize-related status is created only when a resize operation begins.
    
2.  The absence of resize-related status means no active resize operation is in progress.
    

### CRD Evolution

The feature requires CRD evolution in two areas:

1.  New spec fields
    
2.  New status fields
    

##### Spec Additions

The new spec surface should be additive and optional.

Recommended additions:

1.  `spec.persistence.resizeStrategy`
    
2.  Annotation-based pause escape hatch `marklogic.progress.com/resize-paused`, if implemented in v1
    

Compatibility requirements:

1.  New fields must not change behavior for existing resources unless explicitly set
    
2.  Defaulting behavior must be stable and documented
    
3.  Validation rules must reject invalid changes without affecting unrelated updates
    

##### Status Additions

The new status surface should also be additive.

Recommended additions:

1.  status.volumeResizeStatus
    
2.  Any resize-related conditions, if used
    

Compatibility requirements:

1.  Existing clients that ignore unknown status fields must continue to work
    
2.  Absence of resize status must not be treated as an error
    
3.  Status additions must not interfere with existing condition handling
    

### Operator Upgrade Behavior

When upgrading the operator to a version that supports resize, the operator must not retroactively trigger resize behavior unless the user changes the desired storage size.

Required upgrade behavior:

1.  Existing MarkLogic groups continue reconciling without resize activity if no size change occurs
    
2.  The operator compares the effective current size with the new desired size only when reconciliation detects a meaningful spec change or an existing in-progress resize operation
    
3.  Operator upgrade alone must not be interpreted as a resize trigger
    
4.  The operator must preserve any in-progress resize state if upgrade occurs during a resize operation
    

This last point is important. If an operator upgrade happens while a resize is already active, the new operator version must either:

1.  Resume safely from stored status and recovery metadata
    
2.  Transition the operation to a clear recovery state such as `Stalled`
    

It must not silently discard the in-progress workflow state.

### Kubernetes and Storage Compatibility

The feature depends on Kubernetes and storage-provider behavior outside the operator itself.

The operator should explicitly document the compatibility envelope for v1.

Required compatibility assumptions:

1.  PVC expansion is supported by the target CSI driver
    
2.  The target `StorageClass` has `allowVolumeExpansion` enabled
    
3.  The requested resize is monotonic, meaning only increases are supported
    
4.  Workload-side remount or restart behavior is supported when filesystem expansion is not fully online
    

Documented support target for v1:

1.  EKS with AWS EBS CSI
    
2.  AKS with Azure Disk CSI
    

Documented limitations:

1.  Cloud provider throttling may delay progress
    
2.  Cloud provider quota or capacity constraints may prevent completion
    
3.  Some storage backends may require workload restart for filesystem expansion
    
4.  Provider-specific timing may vary significantly
    

The spec should describe these as environmental constraints, not operator defects.

### Strategy Compatibility

Supporting both `parallel` and `sequential` in v1 should not introduce compatibility ambiguity.

Recommended compatibility semantics:

1.  `parallel` is the default strategy for resources that do not explicitly define a strategy
    
2.  `sequential` is opt-in and must be safe for the same resource types and storage backends as `parallel`
    
3.  Both strategies share the same phase model, status contract, and terminal semantics
    
4.  Differences between strategies should affect execution order only, not the overall API contract
    

This is important because users and clients should not need a different observability model depending on strategy.

### Feature Rollout Guidance

For a safe rollout, the feature should be introduced in a way that allows validation in controlled environments before broad production use.

Recommended rollout guidance:

1.  Validate on non-production clusters first
    
2.  Exercise both `parallel` and `sequential` in supported storage environments
    
3.  Validate failure handling and recovery, not only successful resize
    
4.  Ensure monitoring and log access are available before production rollout
    
5.  Document operational runbooks for common blocked states
    

If the team wants additional caution, a feature gate or operator configuration flag may be introduced, but this is optional and not strictly required by the functional contract.

### Compatibility with Existing Workloads

The operator must preserve expected behavior for existing workloads that already use persistence.

Compatibility expectations:

1.  Existing persisted workloads continue operating normally until the requested size increases
    
2.  The feature does not require users to recreate workloads
    
3.  The feature does not require users to manually edit PVCs or StatefulSets
    
4.  Existing StatefulSet reconciliation logic must remain correct for workloads that never invoke resize
    

### Versioning Guidance

The resize feature should be treated as a backward-compatible additive capability within the existing API version, provided:

1.  New fields are optional
    
2.  Defaulting is stable
    
3.  Existing objects remain valid
    
4.  Existing clients are not required to understand resize-specific status fields
    

If future versions materially change the workflow contract, such as adding support for resizing `additionalVolumeClaimTemplates` or changing concurrent resize semantics, those changes should be documented explicitly as contract evolutions.

### Release Readiness Outcomes

The rollout and compatibility plan is sufficient for v1 if the following are true:

1.  Existing resources continue to work unchanged
    
2.  New fields are optional and correctly defaulted
    
3.  Operator upgrade does not trigger unintended resize behavior
    
4.  In-progress resize operations can survive operator restart or upgrade safely
    
5.  Supported storage environments and unsupported cases are clearly documented
    

## Open Questions

This section captures the final architectural recommendations for design points that materially affect API clarity, recovery behavior, and operator complexity.

### Is Stalled a Terminal State or a Recoverable Waiting State?

Recommendation:

Treat `Stalled` as recoverable and non-terminal. The controller may retry automatically with bounded backoff and should expose `retryCount` and `nextRetryTime` when automatic retry remains active.

Additionally, if `retryCount` exceeds the configured maximum retry limit (default: 15), the controller must auto-promote the state from `Stalled` to `Failed`. This prevents infinite retry loops that are invisible to users who only check periodically.

### What Happens If a New Resize Request Arrives During an Active Resize?

Recommendation:

Allow only one active resize execution per `MarklogicGroup`. Snapshot the active target size when the operation begins. If a larger desired size is submitted during the operation, defer it until the active operation completes or fails, then evaluate whether another resize is required.

Additionally, if the new desired size is smaller than the active `targetSize` but still larger than `currentSize`, this constitutes a conflicting intent that cannot be satisfied while the active operation is in flight. The operator must not start a second operation. The active operation completes against its snapshotted target. The new smaller-but-still-larger desired size is evaluated only after the active operation reaches a terminal state.

### Should Additional Volume Claim Templates Be Included in Scope?

Recommendation:

Keep v1 limited to the primary persistence-backed PVC. Additional volume claim templates should be a later feature because they expand the failure surface and may have different application-level restart implications.

### What Is the Authoritative Completion Signal?

Recommendation:

Completion requires all of the following:

1.  The requested PVC size equals the target size.

2.  Observed PVC capacity has reached the target size.

3.  No remaining filesystem-expansion-required signal is present for targeted PVCs.

4.  The recreated StatefulSet template reflects the target size.

5.  Pods are `Running` and `Ready`.

6.  MarkLogic health verification passes.

7.  The live CR generation has not advanced beyond `observedGeneration` in a way that invalidates the active operation. If the only relevant drift is a newer persistence size, it must be recorded in `deferredTargetSize` and reconciled after the active operation reaches a terminal state.

### Should Pause and Resume Be Part of v1?

Recommendation:

Do not introduce a formal resize-specific pause or resume API in v1. If the operator already supports a generic reconciliation pause mechanism, it may be reused.

As a lightweight v1 safety mechanism, the operator should support an annotation-based escape hatch: if `marklogic.progress.com/resize-paused: "true"` is set on the `MarklogicGroup`, the controller stops advancing resize phases, preserves the current `phase`, and sets `reason: Paused` with `message: Resize paused by user annotation` in status. Removing the annotation allows the controller to resume from the persisted phase and live cluster state.

### How Much Automatic Retry Is Appropriate?

Recommendation:

Use bounded exponential backoff for transient failures only. Retry rate limiting, temporary provider errors, delayed PVC status propagation, and short-lived Pod scheduling issues. Surface both `retryCount` and `nextRetryTime` in status.

Concrete defaults for v1:

1.  Initial backoff: 10 seconds
2.  Maximum backoff: 5 minutes
3.  Maximum retries per phase before `Failed`: 15
4.  Maximum total operation time before `Failed`: 2 hours

These values should be configurable via operator deployment flags in a future version.

### How Should Cluster-Level and Group-Level Configuration Interact?

Recommendation:

`MarklogicCluster` remains the source of desired inherited configuration, but `MarklogicGroup` owns the authoritative detailed resize status because PVCs, StatefulSet synchronization, and Pod restart are group-scoped operations. Group-level persistence settings override inherited cluster-level settings.

Merge semantics for `resizeStrategy`:

1.  If `resizeStrategy` is specified at the group level, the group-level value is authoritative.
2.  If `resizeStrategy` is omitted at the group level but specified at the cluster level, the cluster-level value is inherited.
3.  If `resizeStrategy` is omitted at both levels, the default is `parallel`.
4.  This follows the same inheritance pattern as other persistence fields.
    

## Appendix and Examples

### Terms and Definitions

|           Term            |                                                       Meaning                                                       |
|---------------------------|---------------------------------------------------------------------------------------------------------------------|
|   PersistentVolumeClaim   |                       Kubernetes resource representing requested persistent storage for a pod                       |
|       StorageClass        |                    Kubernetes resource that defines storage provisioning and expansion behavior                     |
|  FileSystemResizePending  | Common PVC signal indicating storage expansion is complete but filesystem expansion still requires workload-side action |
| StatefulSet orphan delete |                         Deleting a StatefulSet while preserving pods and persistent storage                         |
|     Parallel strategy     |                                         Resize all target PVCs concurrently                                         |
|    Sequential strategy    |                            Resize one PVC at a time and wait for each before continuing                             |
|        Checkpoint         | The state a PVC must reach before the resize workflow can advance past it. Concretely, either online expansion is complete (`status.capacity` >= target, no `FileSystemResizePending`) or offline expansion is acknowledged (`FileSystemResizePending` is present, confirming the block device was expanded and workload remount is required). |
|       currentSize         | The minimum of `pvc.status.capacity[storage]` across all target PVCs at the time the resize operation begins        |
|        targetSize         | The desired final volume size snapshotted from `spec.persistence.size` when the resize operation starts             |
| Safe partial-completion   | A state where PVCs have been expanded but the StatefulSet template has not yet been synchronized. This is a tolerable intermediate state: the workload continues to function with expanded storage, and template synchronization can be retried independently. |

### Example Resize Request

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: cluster-sample
spec:
    persistence:
        enabled: true
        size: 100Gi
  markLogicGroups:
    - name: dnode
      replicas: 3
      persistence:
        enabled: true
        size: 100Gi
        resizeStrategy: sequential
```

### Example In-Progress Status

```yaml
status:
    volumeResizeStatus:
        operationID: resize-20260331-dnode
        observedGeneration: 12
        phase: WaitingForPVCResize
        message: Waiting for PVC checkpoint completion for 2 of 3 PVCs
        currentSize: 20Gi
        targetSize: 100Gi
        deferredTargetSize: 150Gi
        deferredObservedGeneration: 13
        resizeStrategy: sequential
        activePVC: datadir-dnode-1
        pvcsCheckpointed: 1
        totalPvcs: 3
        pvcStatuses:
            - name: datadir-dnode-0
              podName: dnode-0
              requestedSize: 100Gi
              observedCapacity: 100Gi
              state: Checkpointed
              checkpointType: OnlineComplete
              restartRequired: false
            - name: datadir-dnode-1
              podName: dnode-1
              requestedSize: 100Gi
              observedCapacity: 20Gi
              state: WaitingForCheckpoint
              restartRequired: false
            - name: datadir-dnode-2
              podName: dnode-2
              requestedSize: 20Gi
              observedCapacity: 20Gi
              state: Pending
              restartRequired: false
        retryCount: 0
        lastTransitionTime: "2026-03-31T10:15:00Z"
        firstStartedTime: "2026-03-31T10:12:34Z"
```

### Example Completed Status

```yaml
status:
    volumeResizeStatus:
        operationID: resize-20260331-dnode
        observedGeneration: 12
        phase: Completed
        message: Volume resize completed successfully
        currentSize: 20Gi
        targetSize: 100Gi
        resizeStrategy: sequential
        pvcsCheckpointed: 3
        totalPvcs: 3
        pvcStatuses:
            - name: datadir-dnode-0
              podName: dnode-0
              requestedSize: 100Gi
              observedCapacity: 100Gi
              state: Checkpointed
              checkpointType: OnlineComplete
              restartRequired: false
            - name: datadir-dnode-1
              podName: dnode-1
              requestedSize: 100Gi
              observedCapacity: 100Gi
              state: Restarted
              checkpointType: OfflineComplete
              restartRequired: false
            - name: datadir-dnode-2
              podName: dnode-2
              requestedSize: 100Gi
              observedCapacity: 100Gi
              state: Checkpointed
              checkpointType: OnlineComplete
              restartRequired: false
        retryCount: 0
        lastTransitionTime: "2026-03-31T10:27:51Z"
        firstStartedTime: "2026-03-31T10:12:34Z"
        completionTime: "2026-03-31T10:27:51Z"
```

### Example Failed Status

```yaml
status:
    volumeResizeStatus:
        operationID: resize-20260331-dnode
        observedGeneration: 12
        phase: Failed
        reason: StorageClassNotExpandable
        message: StorageClass gp2 does not allow volume expansion
        currentSize: 20Gi
        targetSize: 100Gi
        resizeStrategy: parallel
        pvcsCheckpointed: 0
        totalPvcs: 3
        pvcStatuses:
            - name: datadir-dnode-0
              podName: dnode-0
              requestedSize: 20Gi
              observedCapacity: 20Gi
              state: Failed
              lastReason: StorageClassNotExpandable
              lastMessage: StorageClass gp2 does not allow volume expansion
            - name: datadir-dnode-1
              podName: dnode-1
              requestedSize: 20Gi
              observedCapacity: 20Gi
              state: Failed
              lastReason: StorageClassNotExpandable
              lastMessage: StorageClass gp2 does not allow volume expansion
            - name: datadir-dnode-2
              podName: dnode-2
              requestedSize: 20Gi
              observedCapacity: 20Gi
              state: Failed
              lastReason: StorageClassNotExpandable
              lastMessage: StorageClass gp2 does not allow volume expansion
        retryCount: 0
        lastTransitionTime: "2026-03-31T10:14:02Z"
        firstStartedTime: "2026-03-31T10:13:10Z"
        completionTime: "2026-03-31T10:14:02Z"
```

### Example Partial Failure Status

```yaml
status:
    volumeResizeStatus:
        operationID: resize-20260331-dnode
        observedGeneration: 12
        phase: Stalled
        reason: PartialResizeFailure
        message: 2 of 3 PVCs reached the required checkpoint; 1 PVC failed
        currentSize: 20Gi
        targetSize: 100Gi
        resizeStrategy: sequential
        activePVC: datadir-dnode-2
        pvcsCheckpointed: 2
        totalPvcs: 3
        pvcStatuses:
            - name: datadir-dnode-0
              podName: dnode-0
              requestedSize: 100Gi
              observedCapacity: 100Gi
              state: Checkpointed
              checkpointType: OnlineComplete
              restartRequired: false
            - name: datadir-dnode-1
              podName: dnode-1
              requestedSize: 100Gi
              observedCapacity: 100Gi
              state: Checkpointed
              checkpointType: OfflinePending
              restartRequired: true
            - name: datadir-dnode-2
              podName: dnode-2
              requestedSize: 100Gi
              observedCapacity: 20Gi
              state: Failed
              restartRequired: false
              lastReason: StorageQuotaExceeded
              lastMessage: Provider rejected resize due to insufficient capacity
        retryCount: 1
        nextRetryTime: "2026-03-31T10:23:00Z"
        warnings:
            - One PVC is blocked by provider quota
        failedPVCs:
            - name: datadir-dnode-2
              reason: StorageQuotaExceeded
              message: Provider rejected resize due to insufficient capacity
        lastTransitionTime: "2026-03-31T10:22:00Z"
        firstStartedTime: "2026-03-31T10:12:34Z"
```

### Example Happy Path Phase Progression

| Step |            Phase             |                                       Summary                                        |
|------|------------------------------|--------------------------------------------------------------------------------------|
|  1   |         Validating          | Operator verifies request, PVC state, StorageClass prerequisites, and PVC ordering   |
|  2   |        ResizingPVCs         | Operator submits PVC resize request or requests                                      |
|  3   |     WaitingForPVCResize     | Operator waits for PVCs to reach the required checkpoint                             |
|  4   | SynchronizingStatefulSet   | Operator captures recovery metadata and reconciles the StatefulSet template          |
|  5   |       RestartingPods        | Operator restarts Pods only if required                                              |
|  6   |      WaitingForPodsReady    | Operator waits for Pods to become healthy                                            |
|  7   |    VerifyingResizeOutcome   | Operator verifies PVC state, StatefulSet template state, and MarkLogic health        |
|  8   |         Completed           | Operator marks the resize as successful                                              |

### Example Event Sequence for an Offline-Expansion Path

This example shows the event trail when filesystem expansion requires Pod restart. For a fully online expansion path, `PodRestartStarted` is omitted.

| Order | Event Type |         Event Reason          |                           Meaning                           |
|-------|------------|-------------------------------|-------------------------------------------------------------|
|   1   |   Normal   |    VolumeResizeRequested      | Resize request detected                                      |
|   2   |   Normal   |    VolumeResizeProgressing    | PVC resize submission and checkpoint waiting are underway    |
|   3   |   Normal   |   StatefulSetSyncStarted      | StatefulSet synchronization has begun                        |
|   4   |   Normal   |      PodRestartStarted        | Controlled Pod restart started                               |
|   5   |   Normal   |    VolumeResizeCompleted      | Resize finished successfully                                 |