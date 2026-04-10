# MarkLogic Operator Scope Configuration

The MarkLogic Operator can be deployed in two different scopes:

## 1. Cluster-Scoped (Default)

In cluster-scoped mode, the operator watches and manages MarkLogic resources across **all namespaces** in the Kubernetes cluster.

### When to use:
- You want to manage MarkLogic clusters across multiple namespaces from a single operator
- You have cluster-admin privileges
- You prefer centralized management

### Requirements:
- ClusterRole and ClusterRoleBinding permissions
- Cluster-admin or equivalent privileges to create cluster-wide resources

### Deployment:

```bash
# Install with default cluster-scoped configuration
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --create-namespace
```

Or with explicit values:

```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --create-namespace \
  --set scope.type=cluster
```

## 2. Namespace-Scoped

In namespace-scoped mode, the operator watches and manages MarkLogic resources only in a **specific namespace**.

### When to use:
- You have limited permissions (no cluster-admin access)
- You want to isolate operator permissions to a single namespace
- You prefer decentralized management with multiple operators per cluster
- You're working in a multi-tenant environment

### Requirements:
- Role and RoleBinding permissions (namespace-level only)
- Permissions to create resources in the target namespace

### Deployment Examples:

#### Watch the same namespace where the operator is deployed:

```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-prod \
  --create-namespace \
  --set scope.type=namespace
```

#### Watch a different namespace than where the operator is deployed:

```bash
# Operator deployed in marklogic-operator-system, watching marklogic-prod
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespaces=marklogic-prod
```

#### Watch multiple specific namespaces:

```bash
# Using comma-separated string
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespaces="team-a-ns,team-b-ns,team-c-ns"

# Or using a values file with array
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

**Note:** When watching different namespace(s), you must ensure:
1. The watched namespace(s) exist
2. The operator's ServiceAccount has the necessary Role/RoleBinding in each watched namespace (automatically created by Helm)

## Configuration Parameters

### values.yaml parameters:

```yaml
scope:
  # Deployment scope: "cluster" or "namespace"
  type: cluster  # or "namespace"
  
  # Only applicable when scope.type is "namespace"
  # If empty, defaults to the release namespace
  # Supports single namespace, comma-separated list, or array
  watchNamespaces: ""  # e.g., "marklogic-prod" or "ns1,ns2,ns3" or ["ns1", "ns2", "ns3"]
```

## Verification

After deploying the operator, verify the scope configuration:

```bash
# Check operator logs
kubectl logs -n <operator-namespace> deployment/marklogic-operator-controller-manager -c manager

# For cluster-scoped, you should see:
# "operator will watch resources in all namespaces (cluster-scoped)"

# For namespace-scoped, you should see:
# "operator will watch resources in namespace" namespace="<watched-namespace>"
```

## RBAC Resources Created

### Cluster-Scoped Mode:
- `ClusterRole`: marklogic-operator-manager-role
- `ClusterRoleBinding`: marklogic-operator-manager-rolebinding

### Namespace-Scoped Mode:
- `Role`: marklogic-operator-manager-role (in watched namespace)
- `RoleBinding`: marklogic-operator-manager-rolebinding (in watched namespace)

## Migration Between Scopes

### From Cluster to Namespace:

```bash
# Uninstall cluster-scoped operator
helm uninstall marklogic-operator -n marklogic-operator-system

# Install namespace-scoped operator
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-prod \
  --set scope.type=namespace
```

### From Namespace to Cluster:

```bash
# Uninstall namespace-scoped operator
helm uninstall marklogic-operator -n marklogic-prod

# Install cluster-scoped operator
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --set scope.type=cluster
```

## Multi-Namespace Watching with Namespace-Scoped Mode

### Option 1: Single Operator Watching Multiple Namespaces (Recommended)

You can configure a single operator to watch multiple specific namespaces:

```bash
# Single operator watching multiple namespaces
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespaces="namespace-a,namespace-b,namespace-c"

# Or using array in values file
cat > values.yaml <<EOF
scope:
  type: namespace
  watchNamespaces:
    - namespace-a
    - namespace-b
    - namespace-c
EOF

helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  -f values.yaml
```

The operator will automatically create Role and RoleBinding in each watched namespace.

### Option 2: Multiple Operators (One Per Namespace)

Alternatively, you can deploy separate operators for complete isolation:

```bash
# Operator 1 watching namespace-a
helm install marklogic-operator-a ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --set scope.type=namespace \
  --set scope.watchNamespaces=namespace-a

# Operator 2 watching namespace-b
helm install marklogic-operator-b ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --set scope.type=namespace \
  --set scope.watchNamespaces=namespace-b
```

## Troubleshooting

### Issue: Operator not watching resources

**Check:**
1. Verify WATCH_NAMESPACE environment variable:
   ```bash
   kubectl get deployment marklogic-operator-controller-manager -n <namespace> -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="WATCH_NAMESPACE")].value}'
   ```
   Note: For multiple namespaces, this should show a comma-separated list (e.g., "ns1,ns2,ns3")

2. Verify RBAC permissions:
   ```bash
   # For namespace-scoped (check each watched namespace)
   kubectl get role,rolebinding -n <watched-namespace> | grep marklogic-operator
   
   # For cluster-scoped
   kubectl get clusterrole,clusterrolebinding | grep marklogic-operator
   ```

3. Check operator logs for watched namespaces:
   ```bash
   kubectl logs -n <operator-namespace> deployment/marklogic-operator-controller-manager -c manager | grep "watch"
   ```

### Issue: Permission denied errors

**Solution:**
- For namespace-scoped: Ensure the operator has Role permissions in the watched namespace
- For cluster-scoped: Ensure you have cluster-admin privileges or the appropriate ClusterRole is created

## Best Practices

1. **Use cluster-scoped** when you have cluster-admin access and want centralized management
2. **Use namespace-scoped** in multi-tenant environments or when you have limited permissions
3. **Keep operator namespace separate** from application namespaces for better isolation
4. **Use meaningful namespace names** to avoid confusion in multi-namespace setups
5. **Monitor operator logs** during initial deployment to confirm scope configuration
