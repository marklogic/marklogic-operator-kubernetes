# HAProxy hot reload

Hot reload is the default behavior whenever HAProxy is enabled. No separate
hot-reload setting is required:

```yaml
spec:
  haproxy:
    enabled: true
```

Existing HAProxy deployments receive a one-time rolling update to adopt the new
startup and volume layout. Configuration-only changes thereafter do not replace
the pod or container.
HAProxy runs in foreground master-worker mode. A private Unix master socket
controls synchronous reloads; no management port is exposed.

## Requirements

- An HAProxy image supporting master-worker mode, the master CLI `reload` command
  with synchronous `Success=1` responses, and `stats socket ... expose-fd listeners`.
  The operator's default HAProxy image supports this workflow.
- Updated operator images and RBAC from this release. The operator requires
  `create` on `pods/exec` in managed namespaces.
- The operator image must also be pullable in the cluster namespace, using
  `spec.haproxy.imagePullSecrets` if needed. An init container copies the static
  reloader from that image into a writable `emptyDir`. It does not need a shell,
  shared process namespace, or a Kubernetes API token.
- `POD_NAME` and `POD_NAMESPACE` identify the operator's own image. Both Helm and
  Kustomize deployments provide these variables. For a locally run operator, set
  `HAPROXY_RELOADER_IMAGE` to the corresponding operator image.

The runtime volume respects the HAProxy pod's filesystem group and container
identity. The init container runs as the non-root user from the operator image.
Custom images must provide `haproxy` in `PATH`.

## Transaction and failure handling

1. Render configuration deterministically and inspect the active hash on every
   ready replica. Unchanged hashes do not trigger reloads.
2. Validate the candidate on **all** replicas using `haproxy -c -f`.
3. On each changed replica, atomically replace a writable active file, request a
   synchronous master CLI reload, and confirm a current worker exists. Existing
   workers drain without a forced drain timeout, preserving active connections.
4. Update the operator-managed ConfigMap only after every replica succeeds.
   The ConfigMap is the startup configuration for replacement containers.
5. If any validation, activation, or ConfigMap commit fails, restore every changed
   replica to the previous ConfigMap configuration. Invalid syntax never replaces
   an active file. An interrupted per-pod apply is recovered from the last
   confirmed local configuration on the next status/apply request.

Activation across replicas is sequential, not a distributed atomic operation.
There can be a short interval with different valid configurations on different
replicas. Failure triggers compensating rollback. Unreachable or terminated
pods can prevent immediate rollback; these errors are explicitly reported and
reconciliation retries rather than claiming success.

All desired replicas must be ready before a transaction starts. A scaled-to-zero
deployment reports Pending. Changes to image, replicas, scheduling, security
context, or certificate volume layout follow the normal Kubernetes deployment
workflow rather than being configuration-only reloads.

Edit the cluster specification, not the generated ConfigMap. ConfigMap watches
reconcile drift back to the operator-generated configuration. Updating TLS Secret
contents alone is not a configuration-hash change and is outside this workflow.
Backend session/stick-table migration across workers is not introduced here.

## Observability

`status.haproxyReload` contains:

- `phase`: `Pending`, `Success`, or `Failed`
- `observedGeneration` and `configurationHash` for the attempted configuration
- `lastAttemptTime` and a diagnostic `message`

`HAProxyReloadReady` is true only after all replicas confirm the configuration.
The operator logs transitions and emits `HAProxyReloadPending`,
`HAProxyReloadSuccess`, or Warning `HAProxyReloadFailed` events on the cluster.
Rollback errors are included in failed results. Unchanged results do not refresh
timestamps or emit duplicate events. The reported hash on failure identifies
the rejected target, not a claim that it is active.

## Tests

```sh
go test ./pkg/k8sutil -run 'TestHAProxy'
go test -v ./pkg/haproxyreload -count=1
```

The latter runs a real local HAProxy with continuous HTTP traffic and an
established TCP stream. It tests changed/unchanged configurations, invalid syntax,
valid syntax with a listener-bind failure, explicit rollback, and interrupted
apply recovery. It skips only when `haproxy` is absent from `PATH`.
Use an up-to-date HAProxy binary: older macOS builds have a known
[master-worker reload hang](https://github.com/haproxy/haproxy/issues/3045).
Kubernetes deployment/exec integration additionally needs a disposable cluster
and an operator image built with the reloader.
