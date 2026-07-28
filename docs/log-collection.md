# Fluent Bit Log Collection

## Secret-Backed Environment Variables

The Fluent Bit sidecar can consume Kubernetes environment variables configured in `spec.logCollection.env`. This uses the standard Kubernetes `EnvVar` schema, including `valueFrom.secretKeyRef`, so secret values are not written to the Fluent Bit ConfigMap.

Create the secret in the same namespace as the MarkLogic resource:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: otel-auth
stringData:
  token: replace-with-your-token
```

Reference one key from that secret in the log collection configuration:

```yaml
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: example
spec:
  logCollection:
    enabled: true
    env:
      - name: OTEL_AUTH_TOKEN
        valueFrom:
          secretKeyRef:
            name: otel-auth
            key: token
    outputs: |
      - name: opentelemetry
        match: kube.marklogic.logs.*
        host: otel-collector.observability
        port: 4317
        grpc: 'on'
        header:
          - Authorization Bearer ${OTEL_AUTH_TOKEN}
```

The same `logCollection.env` field is available on an individual `MarklogicGroup`. A group-level `logCollection` overrides the cluster-level log collection configuration for that group.

`POD_NAME` and `NAMESPACE` remain available automatically in the Fluent Bit container. The operator copies configured environment-variable references to the Fluent Bit container without resolving secret values. Kubernetes resolves the reference when it starts the pod.
