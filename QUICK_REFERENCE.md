# Quick Reference: MarkLogic Operator Scope Configuration

## TL;DR

Choose your deployment mode:

| Mode | Command | Use Case |
|------|---------|----------|
| **Cluster-Scoped** (Default) | `helm install marklogic-operator ./charts/marklogic-operator-kubernetes -n marklogic-operator-system --create-namespace` | You have cluster-admin access and want to manage MarkLogic across all namespaces |
| **Namespace-Scoped** | `helm install marklogic-operator ./charts/marklogic-operator-kubernetes -n marklogic-prod --create-namespace --set scope.type=namespace` | Limited permissions, multi-tenant, or single-namespace management |

## Configuration Parameters

```yaml
scope:
  type: cluster          # or "namespace"
  watchNamespaces: ""    # Only for namespace-scoped
                         # Supports: "single", "ns1,ns2,ns3", or ["ns1", "ns2"]
```

## Common Scenarios

### 1. Default Deployment (Cluster-Scoped)
```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --create-namespace
```

### 2. Namespace-Scoped (Same Namespace)
```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-prod \
  --create-namespace \
  --set scope.type=namespace
```

### 3. Namespace-Scoped (Different Namespace)
```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespace=marklogic-prod
```

### 4. Multiple Operators (One Per Team/Namespace)
```bash
helm install team-a-operator ./charts/marklogic-operator-kubernetes \
  --namespace operators \
  --set scope.type=namespace \
  --set scope.watchNamespaces=namespace-a

helm install team-b-operator ./charts/marklogic-operator-kubernetes \
  --namespace operators \
  --set scope.type=namespace \
  --set scope.watchNamespaces=namespace-b
```

### 5. Single Operator Watching Multiple Namespaces
```bash
# Using comma-separated string
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespaces="team-a-ns,team-b-ns,team-c-ns"

# Or using values file with array
cat > values.yaml <<EOF
scope:
  type: namespace
  watchNamespaces:
    - team-a-ns
    - team-b-ns
    - team-c-ns
EOF

helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  -f values.yaml
```

## Verification

```bash
# Check operator logs
kubectl logs -n <operator-namespace> \
  deployment/marklogic-operator-controller-manager -c manager | grep "watch"

# Expected output for cluster-scoped:
# "operator will watch resources in all namespaces (cluster-scoped)"

# Expected output for namespace-scoped (single):
# "operator will watch resources in namespace" namespace="<watched-namespace>"

# Expected output for namespace-scoped (multiple):
# "operator will watch resources in multiple namespaces" namespaces=["ns1","ns2","ns3"]
```

## RBAC Resources

| Mode | Resources Created |
|------|-------------------|
| Cluster-Scoped | ClusterRole + ClusterRoleBinding |
| Namespace-Scoped | Role + RoleBinding (in watched namespace) |

## When to Use Which Mode?

### Use Cluster-Scoped When:
- ✅ You have cluster-admin privileges
- ✅ You want centralized management across all namespaces
- ✅ You prefer a single operator instance

### Use Namespace-Scoped When:
- ✅ You have limited permissions (no cluster-admin)
- ✅ You're in a multi-tenant environment
- ✅ You want isolation between teams/namespaces
- ✅ You need multiple operators per cluster

## Troubleshooting

### Operator not watching resources?
```bash
# Check WATCH_NAMESPACE env var
kubectl get deployment <operator-deployment> -n <namespace> \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="WATCH_NAMESPACE")]}'
```

### Permission errors?
```bash
# For namespace-scoped, check Role exists in watched namespace
kubectl get role,rolebinding -n <watched-namespace> | grep marklogic-operator

# For cluster-scoped, check ClusterRole exists
kubectl get clusterrole,clusterrolebinding | grep marklogic-operator
```

## Documentation

- Full Documentation: [docs/operator-scope-configuration.md](./docs/operator-scope-configuration.md)
- Implementation Details: [IMPLEMENTATION_SUMMARY.md](./IMPLEMENTATION_SUMMARY.md)
- Examples: [examples/](./examples/)

## Migration

### Cluster → Namespace
```bash
helm uninstall marklogic-operator -n marklogic-operator-system
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  -n marklogic-prod --set scope.type=namespace
```

### Namespace → Cluster
```bash
helm uninstall marklogic-operator -n marklogic-prod
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  -n marklogic-operator-system
```

---

**Need more details?** See [docs/operator-scope-configuration.md](./docs/operator-scope-configuration.md)
