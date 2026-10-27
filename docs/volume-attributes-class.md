# Change Disk Performance Without Restarting MarkLogic

Operator 1.4 supports Kubernetes VolumeAttributesClass (VAC) selections for
initial provisioning and live changes to operator-managed CSI volumes. For
example, change AWS EBS gp3 IOPS and throughput without changing volume capacity
or restarting MarkLogic Pods. Kubernetes and the CSI driver apply the change;
the operator selects the class and reports progress.

A VAC-only change does not recreate StatefulSets, Pods, PVCs, or PVs. This is not
a guarantee against provider performance effects or unrelated workload failures.
Back up important data and test the intended profiles before production use.

## Prerequisites

| Requirement | What to verify |
| --- | --- |
| Operator | Install Operator 1.4 with its matching CRDs and RBAC. Older CRDs can prune configuration or progress fields. |
| Kubernetes | Use **1.34 or later** for the GA `storage.k8s.io/v1` examples below. The VAC API and feature must be enabled. |
| Earlier Kubernetes | The controller can discover `storage.k8s.io/v1beta1` on 1.31-1.33; use that API only with distribution-specific feature configuration and separate qualification. The 1.32 beta lane has not been qualified by the EKS test below. Alpha VAC APIs are not supported. The operator's general Kubernetes 1.30 minimum is unchanged. |
| CSI components | A VAC-aware external-provisioner for Day 1, and an external-resizer plus CSI driver implementing `ControllerModifyVolume` for Day 2. Expansion support alone is insufficient. Check the distribution's supported driver/sidecar versions and feature gates. |
| Storage | A matching CSI StorageClass and complete, valid performance profiles. For these examples, use EBS gp3 and `ebs.csi.aws.com`, not EKS Auto Mode's different provisioner. |
| Access | The operator needs the storage permissions described under [RBAC](#rbac-and-upgrades). A storage administrator creates the cluster-scoped VACs; the operator does not create them. The CSI controller separately needs provider permissions, including EBS volume modification. |
| Workload | A watched namespace, an existing admin Secret with `username` and `password` keys, and the approved MarkLogic image/service account for your environment. |

Check the selected cluster before running any command:

```bash
kubectl config current-context
kubectl version
kubectl api-resources --api-group=storage.k8s.io
kubectl get storageclass
kubectl get volumeattributesclass
```

Finding the API and matching driver names does not prove the CSI components can
provision or modify a volume. Qualify both operations. Other CSI providers need
their own parameters and qualification; do not copy gp3 parameters to them.

## Create Storage Profiles

Have a storage administrator save this as `vac-profiles.yaml` and apply it.
Choose unused names, or reuse existing objects with the same parameters; do not
overwrite another workload's shared profiles.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ml-gp3
provisioner: ebs.csi.aws.com
allowVolumeExpansion: true
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
parameters:
  type: gp3
  encrypted: "true"
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: ml-gp3-standard
driverName: ebs.csi.aws.com
parameters:
  type: gp3
  iops: "3000"
  throughput: "125"
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: ml-gp3-performance
driverName: ebs.csi.aws.com
parameters:
  type: gp3
  iops: "6000"
  throughput: "250"
```

```bash
kubectl apply -f vac-profiles.yaml
```

The EBS throughput values are MiB/s. These complete profiles control type, IOPS,
and throughput; do not assume an omitted parameter preserves its previous value.
VAC driver names and parameters are immutable. Create a new class to offer a
different profile, then change the CR reference.

Cloud modification limits apply even when Kubernetes accepts a request. Before
another change, check the provider's previous modification state, frequency
limits, and any cool-down period. EBS currently permits up to **four modifications
per volume in a rolling 24-hour period**, with every previous modification
complete before another modification; `optimizing` is not `completed`. Do not
rapidly alternate profiles or assume a universal six-hour wait. Consult the
current [EBS modification requirements](https://docs.aws.amazon.com/ebs/latest/userguide/modify-volume-requirements.html)
and [volume modification guidance](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-modify-volume.html).
The operator does not bypass provider limits or automatically roll back changes.

## Day 1: Select a Profile

Set the namespace explicitly. It must already exist and be watched by the
operator. Substitute your namespace, existing Secret, image, and service account.
Each example creates a separate billable 20Gi volume; deploy only the examples
you intend to use. Use new CR/claim names for a fresh test, not retained claims.

```bash
NAMESPACE=marklogic
kubectl get namespace "$NAMESPACE"
kubectl get secret ml-admin -n "$NAMESPACE"
kubectl get serviceaccount marklogic-workload -n "$NAMESPACE"
```

### Cluster-Level Configuration

Save as `vac-cluster.yaml`. The embedded group inherits the cluster's complete
persistence configuration. The operator creates the child `MarklogicGroup`;
make later changes through the parent, not that generated child.

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: vac-cluster
spec:
  auth:
    secretName: ml-admin
  image: progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6
  serviceAccountName: marklogic-workload
  updateStrategy: OnDelete
  hugePages:
    enabled: false
  persistence:
    enabled: true
    size: 20Gi
    storageClassName: ml-gp3
    volumeAttributesClassName: ml-gp3-standard
  markLogicGroups:
    - name: vac-cluster-node
      replicas: 1
      isBootstrap: true
      groupConfig:
        name: Default
        enableXdqpSsl: true
      resources:
        requests:
          cpu: "1"
          memory: 2Gi
        limits:
          cpu: "2"
          memory: 4Gi
```

```bash
kubectl apply -n "$NAMESPACE" -f vac-cluster.yaml
kubectl wait -n "$NAMESPACE" --for=create pod/vac-cluster-node-0 --timeout=5m
kubectl wait -n "$NAMESPACE" --for=condition=Ready pod/vac-cluster-node-0 --timeout=10m
kubectl wait -n "$NAMESPACE" \
  --for=jsonpath='{.status.currentVolumeAttributesClassName}'=ml-gp3-standard \
  pvc/datadir-vac-cluster-node-0 --timeout=15m
```

`WaitForFirstConsumer` deliberately leaves a claim Pending until its consumer
can be scheduled. A Bound claim or Ready Pod alone does not prove VAC completion.

### Standalone Group Configuration

Alternatively, save as `vac-group.yaml`. A standalone group uses `secretName`
directly, not the cluster's `auth.secretName` field.

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicGroup
metadata:
  name: vac-group
spec:
  name: vac-group
  replicas: 1
  secretName: ml-admin
  image: progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6
  serviceAccountName: marklogic-workload
  updateStrategy: OnDelete
  hugePages:
    enabled: false
  resources:
    requests:
      cpu: "1"
      memory: 2Gi
    limits:
      cpu: "2"
      memory: 4Gi
  persistence:
    enabled: true
    size: 20Gi
    storageClassName: ml-gp3
    volumeAttributesClassName: ml-gp3-standard
```

```bash
kubectl apply -n "$NAMESPACE" -f vac-group.yaml
kubectl wait -n "$NAMESPACE" --for=create pod/vac-group-0 --timeout=5m
kubectl wait -n "$NAMESPACE" --for=condition=Ready pod/vac-group-0 --timeout=10m
kubectl wait -n "$NAMESPACE" \
  --for=jsonpath='{.status.currentVolumeAttributesClassName}'=ml-gp3-standard \
  pvc/datadir-vac-group-0 --timeout=15m
```

### Per-Group Overrides and Additional Volumes

For a cluster-owned group, configure
`spec.markLogicGroups[].persistence.volumeAttributesClassName` on the parent.
Supplying `persistence` replaces the **whole object**: repeat `enabled`, `size`,
`storageClassName`, and other required settings. An omitted group persistence
object inherits the cluster's object. An omitted/empty class inside a supplied
override does not inherit the cluster's class.

As an **alternative Day-2 flow**, first record the baseline and perform the
provider checks under [Day 2](#day-2-change-performance-live). Then set an
explicit override for the first embedded group without replacing the other
entries in `markLogicGroups`. Skip this command when following the cluster-wide
Day-2 example:

```bash
kubectl patch marklogiccluster vac-cluster -n "$NAMESPACE" --type=json \
  -p '[{"op":"add","path":"/spec/markLogicGroups/0/persistence","value":{"enabled":true,"size":"20Gi","storageClassName":"ml-gp3","volumeAttributesClassName":"ml-gp3-performance"}}]'
```

For an existing persistence override, retain that group's own capacity,
StorageClass, and other settings rather than replacing them with these example
values. A group with this override will not follow later cluster-level profile
changes until the override is removed or updated.

For each additional claim, select its class in
`spec.additionalVolumeClaimTemplates[].spec.volumeAttributesClassName`.
Additional templates do not inherit the primary volume's class. A per-group
additional-template list replaces the whole cluster list, not individual entries.
Declare these templates before initial provisioning; adding, removing, or renaming
StatefulSet claim templates is not a supported live performance change. See the
[API examples and inheritance rules](spec/volume-attributes-class.md#api-and-user-experience-contract).

## Day 2: Change Performance Live

Record the Pod/StatefulSet/PVC/PV identities, restart count, capacity, and an
application-level data check before the change. Ensure the target class uses the
same CSI driver and the provider permits another modification. Change only the
class; keep capacity and StorageClass unchanged.

For the cluster example:

```bash
kubectl patch marklogiccluster vac-cluster -n "$NAMESPACE" --type=merge \
  -p '{"spec":{"persistence":{"volumeAttributesClassName":"ml-gp3-performance"}}}'
kubectl wait -n "$NAMESPACE" \
  --for=jsonpath='{.status.currentVolumeAttributesClassName}'=ml-gp3-performance \
  pvc/datadir-vac-cluster-node-0 --timeout=30m
```

For the standalone group example instead:

```bash
kubectl patch marklogicgroup vac-group -n "$NAMESPACE" --type=merge \
  -p '{"spec":{"persistence":{"volumeAttributesClassName":"ml-gp3-performance"}}}'
kubectl wait -n "$NAMESPACE" \
  --for=jsonpath='{.status.currentVolumeAttributesClassName}'=ml-gp3-performance \
  pvc/datadir-vac-group-0 --timeout=30m
```

Also check the current-generation status described below; a timeout is a signal
to inspect progress, not proof of permanent failure. A successful CR patch or
`VolumeAttributesClassRequested` Event proves submission, not application.
Kubernetes/CSI performs the asynchronous change and its retries.

Historical StatefulSet claim templates remain unchanged. New replicas receive
the latest effective selection; retained claims are reused, not replaced. Only
active, verified operator-managed claims are modified. Externally supplied PVCs
under `additionalVolumes` are not managed by this feature.

Removing or emptying the selection stops future VAC management; it does **not**
clear an existing PVC reference, revert cloud performance, or cancel native work.
To return to standard performance, explicitly select the standard class after
the provider permits another change. Capacity expansion is a separate operation;
resize and native VAC work are serialized by the storage-operation guard.

## Monitor Completion

```bash
kubectl get marklogiccluster vac-cluster -n "$NAMESPACE" -o jsonpath='generation={.metadata.generation}{"\n"}{.status.storage.volumeAttributesClass}{"\n"}{.status.conditions}{"\n"}'
kubectl get marklogicgroup vac-cluster-node -n "$NAMESPACE" -o jsonpath='generation={.metadata.generation}{"\n"}{.status.storage.volumeAttributesClass}{"\n"}{.status.conditions}{"\n"}'
kubectl get pvc datadir-vac-cluster-node-0 -n "$NAMESPACE" -o jsonpath='requested={.spec.volumeAttributesClassName}{"\n"}applied={.status.currentVolumeAttributesClassName}{"\n"}{.status.modifyVolumeStatus}{"\n"}{.status.conditions}{"\n"}'
kubectl describe marklogicgroup vac-cluster-node -n "$NAMESPACE"
kubectl describe pvc datadir-vac-cluster-node-0 -n "$NAMESPACE"
kubectl get events -n "$NAMESPACE" --sort-by=.metadata.creationTimestamp
```

For the standalone example, inspect `vac-group` and `datadir-vac-group-0` instead.
The group reports each target in `status.storage.volumeAttributesClass.pvcs`;
the parent aggregates its configured groups.

| Observation | Meaning |
| --- | --- |
| `StorageAttributesValidated=True` | API/class/driver checks passed, not CSI application or parameter acceptance. |
| `Pending` / `InProgress` | Waiting for a claim, dispatch, binding, resize, or native modification. Read the per-PVC reason and message. |
| `Completed` and `StorageAttributesSynced=True` | All active targets have matching desired/requested/applied classes with no active native modification/error, for the reported generation. |
| `Failed` | A current-target validation, admission, or native failure needs attention. |
| `Stalled` | No meaningful progress for 30 minutes; diagnostic only, not automatic cancellation or rollback. |
| `Unknown` | The operator cannot establish completion, including unsupported or unreadable capabilities and missing evidence. |

Require `status.storage.volumeAttributesClass.observedGeneration` and the
`StorageAttributesSynced` condition's `observedGeneration` to equal that CR's
`metadata.generation`. Check the parent and child independently. A stale True
condition, zero active PVCs, or the requested class alone is not success.

For EBS, also verify the actual PV's volume and provider lifecycle. Set the region
to the cluster's region; these commands are read-only:

```bash
AWS_REGION=us-west-2
PV=$(kubectl get pvc datadir-vac-cluster-node-0 -n "$NAMESPACE" -o jsonpath='{.spec.volumeName}')
VOLUME=$(kubectl get pv "$PV" -o jsonpath='{.spec.csi.volumeHandle}')
aws ec2 describe-volumes --region "$AWS_REGION" --volume-ids "$VOLUME" \
  --query 'Volumes[].{ID:VolumeId,GiB:Size,Type:VolumeType,IOPS:Iops,Throughput:Throughput}' --output table
aws ec2 describe-volumes-modifications --region "$AWS_REGION" --volume-ids "$VOLUME" \
  --query 'VolumesModifications[].{State:ModificationState,Progress:Progress,Started:StartTime}' --output table
```

Expect the same volume and 20Gi capacity, now at 6000 IOPS / 250 MiB/s. Native
VAC completion may precede the end of EBS optimization. Compare the recorded
workload/storage identities and application data, not only these settings.

## Troubleshoot Safely

| Symptom | Action |
| --- | --- |
| `VACUnsupported` | Verify served GA/beta API and feature configuration. On confirmed API absence, new claims omit VAC and provision using StorageClass/driver defaults, with warnings; this is not performance-profile success. Existing claims are preserved. Repair prerequisites or remove the desired selection. |
| `VACNotFound` | Correct the CR name or create the missing class. New claims can provision without VAC; bound active claims can converge after repair. Inspect the actual saved reference on an unbound claim, not only the latest desired class. |
| `VACDriverMismatch` | Match the VAC driver to the StorageClass and bound CSI PV. The operator does not substitute defaults for this error. Do not replace the live claim to bypass validation. |
| Native `Infeasible` | Read `modifyVolumeStatus.targetVolumeAttributesClassName`, PVC conditions/Events, CSI resizer logs, and provider diagnostics. A result applies only to its recorded target. Correct invalid parameters by creating/selecting another class when the provider permits it; the operator does not automatically roll back. |
| `ModifyVolumeError=True` / `VolumeAttributesClassModifyError` | A retryable provider error can remain `InProgress`. Check quotas, IAM, parameter constraints, and prior modification/cool-down state; CSI owns retries. Do not toggle profiles repeatedly. |
| Pending PVC / `PendingClassMismatch` | Resolve scheduling, WFFC binding, missing original provisioning class, and CSI provisioning errors. An unbound claim's saved VAC reference cannot be changed in place; the operator waits for binding before applying the latest desired class. |
| `Forbidden` or discovery/lookup timeout | Repair API discovery/storage RBAC or connectivity. These are not proof that VAC is unsupported and do not trigger default-profile fallback. |
| `VACFieldNotPersisted` / `VACAdmissionStateNotPersisted` | Install matching CRDs and repair feature/admission configuration. Dropped-reference writes are suppressed durably, rather than retried on every reconcile. After repair, use the intentional group retry below. |
| Pod stuck `ContainerCreating` with `FailedMount` | Inspect referenced Secrets and volumes. In particular, an admin Secret owned by a deleted cluster may disappear. Restore the correct credentials; a new password does not unlock a retained old database. |

After repairing admission, change the following token on the affected group
(including a cluster-owned child), using a new value for each intentional retry:

```bash
kubectl annotate marklogicgroup vac-cluster-node -n "$NAMESPACE" \
  marklogic.progress.com/vac-admission-retry=after-admission-repair-1 --overwrite
```

This is not a bypass of binding, ownership, driver, or operation checks. Operator
restarts and unrelated image changes do not clear a rejected request's latch.
Do not edit PVC status, remove protection finalizers, force-delete claims, or
restart workloads as a VAC recovery procedure. Retained/precreated claims may
contain data and continue incurring charges; inspect group
`status.storage.unusedPVCs`, current Pod references, and retention/reclaim policy
before any separately approved storage cleanup.

## RBAC and Upgrades

Update the operator's CRDs **and** RBAC when upgrading to 1.4. Namespace scope
still needs cluster-scoped storage reads; namespace Roles alone are insufficient.
Keep the existing Helm release ownership and watch configuration. Use the
[scope guide](operator-scope-configuration.md#storage-and-vac-permissions)
for the complete generated permissions and scope-specific roles.

| Resource | Required storage-related access |
| --- | --- |
| PVCs | `create`, `get`, `list`, `watch`, `patch`, `update`; create supports safe scale-up precreation. |
| PVC status | `get`; never write Kubernetes-owned progress. |
| PVs | `get` to verify the binding and actual CSI driver. |
| VACs (`storage.k8s.io`) | `get` on externally managed GA/beta class references. No create/update/delete grant is added for the operator. |
| StorageClasses | `get`, `list`, `watch`. |
| MarkLogic cluster/group status | `get`, `patch`, `update`. |
| Core and `events.k8s.io` Events | `create`, `patch`, `update`. |

Cluster-scoped installations include these reads in the manager ClusterRole.
Namespace-scoped Helm installations use the separate
`<chart-fullname>-storageclass-reader` ClusterRole and binding for storage reads,
with PVC access in each watched namespace. Neither mode grants PV or VAC writes
or deletion for this feature. Kubernetes API discovery uses standard authenticated
discovery permissions. These permissions are separate from the CSI driver's AWS
IAM policy and the administrator's permissions to create VACs.

## Example Validation

All five YAML resources above (StorageClass, two VACs, cluster, and standalone
group) passed strict server-side dry-run admission against the EKS environment
on 21 October 2026. The complete embedded-group override also passed server-side
dry-run admission. The shell blocks passed Bash syntax validation. Dry-run
admission does not allocate storage or prove runtime CSI support.

The focused EKS test on 21 October 2026 used Kubernetes 1.34.11, EBS CSI 1.66.0,
external-provisioner 6.3.0, external-resizer 2.2.1, and MarkLogic
`12.0.3-ubi9-rootless-2.2.6`. It provisioned a fresh parent/child with a standard
20Gi gp3 profile, then changed only the parent selection to performance.
Kubernetes and AWS agreed on 3000/125 to 6000/250; identities, capacity, Pod
restart count, an authenticated document, and a file checksum were preserved.
AWS was still optimizing at the last check. Standalone-group provisioning and
live additional-volume changes were also exercised in the earlier EKS checks.

This evidence is for the deployed candidate, not a qualification of every 1.4
build, CSI provider, or beta API. The full automated VAC suite was not run in
this focused check. The [E2E instructions](../test/README.md#vac-core-e2e)
describe the separate destructive harness: use an exclusive disposable cluster,
not an existing demo or shared cluster. Opt-out transitions, restart during an
active modification, and fault/platform lanes need separate qualification.

See the [Operator 1.4 release-note entry](release-notes-1.4.md) for upgrade
requirements and remaining publication gates.

## References

- [Kubernetes VolumeAttributesClass](https://kubernetes.io/docs/concepts/storage/volume-attributes-classes/)
- [Controller/API specification](spec/volume-attributes-class.md)
- [Storage and scope permissions](operator-scope-configuration.md#storage-and-vac-permissions)