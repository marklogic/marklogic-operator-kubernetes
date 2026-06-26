# HAProxy and fluent-bit SecurityContext Configuration

## Overview

This document explains how to configure security contexts for HAProxy and fluent-bit sidecars in your MarkLogic Kubernetes deployment.

If security context fields are not set in the CR, the operator now applies secure defaults for HAProxy, MarkLogic, and fluent-bit containers.

## Architecture

### HAProxy
- **Type**: Standalone Deployment
- **SecurityContext Support**: Both pod-level (`podSecurityContext`) and container-level (`securityContext`)
- **Reason**: HAProxy runs in its own Deployment with full autonomy over security policies

### fluent-bit
- **Type**: Sidecar container in MarkLogic StatefulSet pods
- **SecurityContext Support**: Container-level (`securityContext`) only
- **Pod-level Context**: Inherited from parent MarkLogic pod's `podSecurityContext`
- **Reason**: fluent-bit shares the pod namespace and cannot override the pod's security context

## Default Behavior (When CR Does Not Set securityContext)

When security context fields are not explicitly set in the CR, the operator applies hardening defaults while allowing container images to use their built-in user identities:

- **HAProxy Pod (`spec.haproxy.podSecurityContext`)**
  - `fsGroup: 101` (for filesystem access)

- **HAProxy Container (`spec.haproxy.securityContext`)**
  - `allowPrivilegeEscalation: false` (prevent escalation)
  - `readOnlyRootFilesystem: false` (HAProxy may need to write runtime state)
  - `capabilities.drop: ["ALL"]` (drop all capabilities)
  - `capabilities.add: ["NET_BIND_SERVICE"]` (required to bind to ports)
  - **runAsUser**: Not set (image default applies)

- **MarkLogic Container (`spec.securityContext`)**
  - `runAsUser: 1000` (MarkLogic UID - based on image default)
  - `runAsNonRoot: true`
  - `allowPrivilegeEscalation: false`
  - `readOnlyRootFilesystem: false`
  - `capabilities.drop: ["ALL"]`

- **fluent-bit Container (`spec.logCollection.securityContext` and group override)**
  - `allowPrivilegeEscalation: false`
  - `readOnlyRootFilesystem: true`
  - `capabilities.drop: ["ALL"]`
  - **runAsUser**: Not set (image default applies)

**Custom runAsUser**: To specify a custom UID, explicitly set `runAsUser` in the CR for HAProxy or fluent-bit. When provided, CR values override the defaults.

## Configuration

### HAProxy Security Context

Configure HAProxy security at the cluster level in `spec.haproxy`:

```yaml
haproxy:
  enabled: true
  image: "haproxytech/haproxy-alpine:3.4.0"

  # Pod-level security context
  # Applies to the entire HAProxy Deployment pod
  podSecurityContext:
    fsGroup: 101
    fsGroupChangePolicy: "OnRootMismatch"

  # Container-level security context
  # Overrides pod-level settings for the HAProxy container
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: false
    capabilities:
      drop:
        - ALL
      add:
        - NET_BIND_SERVICE  # Required to bind to ports < 1024
```

### fluent-bit Security Context

Configure fluent-bit security at cluster level in `spec.logCollection` and/or per-group in `spec.markLogicGroups[*].logCollection`:

#### Cluster-Level (applies to all groups unless overridden)

```yaml
logCollection:
  enabled: true
  image: "fluent/fluent-bit:2.1.10"

  # Container-level security context for fluent-bit
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop:
        - ALL

  resources:
    requests:
      cpu: 100m
      memory: 200Mi
    limits:
      cpu: 200m
      memory: 500Mi

  files:
    errorLogs: true
    accessLogs: true
    requestLogs: true
```

#### Group-Level (overrides cluster-level for specific groups)

```yaml
markLogicGroups:
  - name: bootstrap
    replicas: 1
    isBootstrap: true

    logCollection:
      enabled: true
      image: "fluent/fluent-bit:2.1.10"
      securityContext:
        allowPrivilegeEscalation: false
      resources:
        requests:
          cpu: 100m
          memory: 200Mi
        limits:
          cpu: 200m
          memory: 500Mi
```

## Security Best Practices

### HAProxy

1. **Minimal Capabilities**: Drop all capabilities except `NET_BIND_SERVICE` if binding to privileged ports
2. **Custom UID**: Set `runAsUser` and `runAsNonRoot: true` explicitly if you want to enforce non-root execution
3. **Read-only Filesystem**: Set to `false` only if HAProxy needs to write runtime state
4. **FSGroup**: Set to match HAProxy's group ID (typically `101`)

### fluent-bit

1. **Custom UID**: Set `runAsUser` and `runAsNonRoot: true` explicitly if you want to enforce non-root execution
2. **Read-only Root Filesystem**: Set to `true` to prevent unauthorized modifications
3. **Drop All Capabilities**: fluent-bit does not require any special capabilities
4. **No Privilege Escalation**: Always set `allowPrivilegeEscalation: false`
5. **Per-Group UID Control**: If needed, set different UIDs per group for audit separation

## Important Notes

### fluent-bit Inherits Pod Security Context

fluent-bit sidecars **cannot have their own pod-level security context**. They always inherit the parent MarkLogic pod's `spec.podSecurityContext`. The `spec.logCollection.securityContext` field applies **only to the fluent-bit container itself**, allowing you to:

- Run fluent-bit as a different user than MarkLogic
- Apply stricter capabilities restrictions
- Set read-only filesystem for the container

Example:

```yaml
# Pod-level (applies to ALL containers in the pod, including fluent-bit if not overridden)
podSecurityContext:
  fsGroup: 2
  fsGroupChangePolicy: "OnRootMismatch"

# Container-level (applies ONLY to fluent-bit sidecar)
logCollection:
  securityContext:
    readOnlyRootFilesystem: true
    allowPrivilegeEscalation: false
```

### Image Configurability

Both images are fully editable through the CR:

- **HAProxy**: `spec.haproxy.image` (cluster-level only)
- **fluent-bit**: `spec.logCollection.image` (cluster-level) and `spec.markLogicGroups[*].logCollection.image` (group-level override)

This allows you to use hardened images, apply security patches, or switch to alternative implementations.

## Example CR

See [security-context-example.yaml](../config/samples/security-context-example.yaml) for a complete working example with both HAProxy and fluent-bit security contexts configured.

## Validation

To validate your CR:

```bash
kubectl apply --dry-run=client -f your-cr.yaml
```

To deploy:

```bash
kubectl apply -f your-cr.yaml
```

To check security contexts are applied:

```bash
# HAProxy Deployment
kubectl describe deployment marklogic-haproxy -n your-namespace

# MarkLogic StatefulSet (includes fluent-bit sidecar)
kubectl describe statefulset your-cluster-name -n your-namespace

# Verify running containers
kubectl get pods -o jsonpath='{.items[*].spec.containers[*].securityContext}' -n your-namespace
```
