# Webhook Validation and Certificate Management Design

## Objective

Implement validating admission webhooks that reject `MarklogicCluster` and `MarklogicGroup` CRs when applied to namespaces outside the operator watch scope, with two certificate provider modes:

1. `selfSigned` (default): no cert-manager dependency
2. `certManager` (optional): cert-manager managed issuance and rotation

## Current State Summary

- Namespace watch scope is already configured through `WATCH_NAMESPACE` and manager cache scoping.
- Webhook server object exists in manager startup (`cmd/main.go`), but webhook registration/resources are not fully wired in this chart path.
- Chart currently focuses on controller deployment/RBAC and CRDs.

## Proposed Helm Values Schema

Add the following section to `charts/marklogic-operator-kubernetes/values.yaml`:

```yaml
webhook:
  enabled: true
  failurePolicy: Fail
  timeoutSeconds: 10

  namespaceValidation:
    enabled: true

  certs:
    # selfSigned | certManager
    provider: selfSigned

    # shared settings
    secretName: marklogic-operator-webhook-server-cert
    serviceName: marklogic-operator-webhook-service
    port: 9443

    # self-signed mode
    selfSigned:
      regenerateOnUpgrade: false
      validityDays: 365

    # cert-manager mode
    certManager:
      enabled: false
      issuerRef:
        kind: ClusterIssuer
        name: marklogic-operator-selfsigned
        group: cert-manager.io
      duration: 8760h
      renewBefore: 720h
```

### Validation Rules

- If `webhook.enabled=false`, no webhook resources are rendered.
- If `webhook.certs.provider=certManager`, then `webhook.certs.certManager.enabled=true` must be set.
- If `scope.type=namespace` and `webhook.namespaceValidation.enabled=true`, webhook denies CRs in namespaces not in watch scope.

## Template Set

Create these templates under `charts/marklogic-operator-kubernetes/templates/`:

1. `webhook-service.yaml`
- Defines service that routes to manager webhook port (`9443`).

2. `validating-webhook-configuration.yaml`
- Registers webhook for `marklogicclusters` and `marklogicgroups` on `CREATE`/`UPDATE`.
- Uses configurable `failurePolicy` and `timeoutSeconds`.
- References service + webhook path.
- `caBundle` behavior depends on cert provider:
  - `selfSigned`: set from generated CA bundle
  - `certManager`: injected by cert-manager cainjector

3. `webhook-certgen-job.yaml` (self-signed mode)
- Helm hook Job (`pre-install,pre-upgrade`) that:
  - Generates CA + server keypair with SANs:
    - `<service>`
    - `<service>.<namespace>`
    - `<service>.<namespace>.svc`
  - Creates/updates secret with `tls.crt`, `tls.key`, `ca.crt`
  - Patches webhook `caBundle`
- Recommended hook metadata:
  - `helm.sh/hook: pre-install,pre-upgrade`
  - `helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded`

4. `certmanager-issuer.yaml` (optional)
- Optional Issuer/ClusterIssuer helper (if chart should provision one).

5. `certmanager-certificate.yaml` (cert-manager mode)
- Creates `Certificate` resource writing to shared webhook secret.
- Includes dnsNames for webhook service identity.

## Deployment Template Changes

Update `templates/deployment.yaml` manager container:

1. Add webhook args:
- `--webhook-port=9443`

2. Add cert volume mount:
- Mount path: `/tmp/k8s-webhook-server/serving-certs`

3. Add secret volume:
- Secret name from `webhook.certs.secretName`

## Manager Runtime Changes

In `cmd/main.go`, set explicit webhook options:

```go
webhookServer := webhook.NewServer(webhook.Options{
    Port:    9443,
    CertDir: "/tmp/k8s-webhook-server/serving-certs",
    TLSOpts: tlsOpts,
})
```

Register validation handlers for:

- `MarklogicCluster` create/update
- `MarklogicGroup` create/update

## Namespace Validation Logic

For each admission request:

1. Read incoming object namespace from AdmissionReview request.
2. Determine allowed namespaces from `WATCH_NAMESPACE`:
- empty => cluster-scoped, allow all
- non-empty => parse comma-separated namespace list
3. If namespace is not allowed, reject request with clear message:

`namespace <ns> is outside operator watch scope (<watchNamespaces>)`

This ensures apply-time failure instead of silent non-reconciliation.

## Certificate Generation and Storage

### Mode A: selfSigned

Generation:
- Helm hook Job generates CA and serving certificate.

Storage:
- Kubernetes Secret in operator namespace (name from `webhook.certs.secretName`):
  - `tls.crt`
  - `tls.key`
  - `ca.crt`

Distribution:
- Manager pod mounts secret for TLS serving cert.
- Webhook `caBundle` patched from `ca.crt`.

Rotation:
- Triggered on upgrade or explicit regeneration policy.

### Mode B: certManager

Generation:
- cert-manager issues serving cert from configured issuer.

Storage:
- Same Secret name (`webhook.certs.secretName`) maintained by cert-manager.

Distribution:
- Manager pod mounts secret for serving TLS.
- cainjector injects CA into `ValidatingWebhookConfiguration`.

Rotation:
- Automatic via cert-manager renewal settings.

## Suggested Rollout Plan

1. Implement `selfSigned` first as default path.
2. Add cert-manager provider support using same secret contract.
3. Enable namespace validation webhook with `failurePolicy=Fail` by default.
4. Add chart tests for both providers and namespace denial behavior.

## Security and Operational Notes

- Keep webhook timeout low (`10s` default).
- Use `sideEffects: None` and `admissionReviewVersions: ["v1"]`.
- Ensure cert SANs exactly match service DNS names used by API server.
- Keep one secret contract for both providers to avoid deployment branching complexity.
