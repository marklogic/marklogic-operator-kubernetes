# MarkLogic Operator Namespace/Cluster Scope Implementation

## Summary of Changes

This implementation adds the capability for the MarkLogic Operator to run in either **cluster-scoped** or **namespace-scoped** mode, with support for watching multiple specific namespaces, giving you maximum flexibility in how the operator watches and manages MarkLogic resources.

## Deployment Modes

### 1. Cluster-Scoped (Default)
Watches ALL namespaces in the cluster

### 2. Namespace-Scoped - Single Namespace
Watches a single specific namespace

### 3. Namespace-Scoped - Multiple Namespaces (NEW!)
Watches multiple specific namespaces with a single operator instance

## What Was Changed

### 1. Operator Code (`cmd/main.go`)
- Added `WATCH_NAMESPACE` environment variable support with comma-separated list parsing
- Added `--watch-namespace` command-line flag supporting multiple namespaces
- Configured the manager to watch specific namespaces when `WATCH_NAMESPACE` is set
- Added proper logging to indicate which mode and which namespace(s) the operator is watching
- Support for parsing comma-separated namespace strings (e.g., "ns1,ns2,ns3")

### 2. Kubernetes RBAC Resources (`config/rbac/`)
- Created `role_namespaced.yaml` - Role for namespace-scoped permissions
- Created `role_binding_namespaced.yaml` - RoleBinding for namespace-scoped deployment
- Existing `role.yaml` (ClusterRole) remains for cluster-scoped deployment

### 3. Helm Chart Configuration

#### `charts/marklogic-operator-kubernetes/values.yaml`
- Added `scope` section with:
  - `scope.type`: "cluster" (default) or "namespace"
  - `scope.watchNamespace`: specific namespace to watch

#### `charts/marklogic-operator-kubernetes/templates/manager-rbac.yaml`
- Made RBAC resources conditional based on `scope.type`
- Creates ClusterRole/ClusterRoleBinding when `scope.type=cluster`
- Creates Role/RoleBinding when `scope.type=namespace`

#### `charts/marklogic-operator-kubernetes/templates/deployment.yaml`
- Added `WATCH_NAMESPACE` environment variable (only when namespace-scoped)
- Value comes from `scope.watchNamespace` or defaults to release namespace

### 4. Documentation

#### `docs/operator-scope-configuration.md`
Comprehensive guide covering:
- When to use cluster vs namespace scope
- Deployment examples for both modes
- Configuration parameters
- Verification steps
- Migration between scopes
- Troubleshooting

#### Updated `README.md`
- Added operator scope options section
- Updated installation instructions with both modes
- Added links to detailed documentation

### 5. Examples

Created example files in `examples/`:
- `cluster-scoped-deployment.sh` - Complete cluster-scoped deployment example
- `namespace-scoped-deployment.sh` - Namespace-scoped deployment examples
- `multi-namespace-deployment.sh` - Multiple operators for different namespaces
- `values-cluster-scoped.yaml` - Values file for cluster-scoped mode
- `values-namespace-scoped.yaml` - Values file for namespace-scoped mode

## How It Works

### Cluster-Scoped Mode (Default)
```
┌─────────────────────────────────────┐
│   Kubernetes Cluster                │
│                                     │
│  ┌────────────────┐                │
│  │ Operator NS    │                │
│  │  - Operator    │────┬───────┐   │
│  └────────────────┘    │       │   │
│                        │       │   │
│  ┌────────────────┐    │       │   │
│  │ Namespace A    │◄───┘       │   │
│  │  - ML Cluster  │            │   │
│  └────────────────┘            │   │
│                                │   │
│  ┌────────────────┐            │   │
│  │ Namespace B    │◄───────────┘   │
│  │  - ML Cluster  │                │
│  └────────────────┘                │
│                                     │
│  Single operator watches ALL        │
│  namespaces (requires ClusterRole)  │
└─────────────────────────────────────┘
```

### Namespace-Scoped Mode
```
┌─────────────────────────────────────┐
│   Kubernetes Cluster                │
│                                     │
│  ┌────────────────┐                │
│  │ Operator NS    │                │
│  │  - Operator    │────┐           │
│  └────────────────┘    │           │
│                        │           │
│  ┌────────────────┐    │           │
│  │ Namespace A    │◄───┘           │
│  │  - ML Cluster  │                │
│  └────────────────┘                │
│                                     │
│  ┌────────────────┐                │
│  │ Namespace B    │                │
│  │  - ML Cluster  │ (ignored)      │
│  └────────────────┘                │
│                                     │
│  Operator watches ONLY Namespace A  │
│  (requires Role in watched NS)      │
└─────────────────────────────────────┘
```

## Quick Start

### Deploy in Cluster-Scoped Mode (Default)
```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --create-namespace
```

### Deploy in Namespace-Scoped Mode
```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-prod \
  --create-namespace \
  --set scope.type=namespace
```

### Deploy to Watch Different Namespace
```bash
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespace=marklogic-prod
```

## Benefits

### Cluster-Scoped Benefits
- ✅ Single operator manages all namespaces
- ✅ Centralized management
- ✅ Simplified operations

### Namespace-Scoped Benefits
- ✅ Works with limited permissions (no cluster-admin needed)
- ✅ Better isolation in multi-tenant environments
- ✅ Can deploy multiple operators per cluster
- ✅ Team-specific management

## Testing Recommendations

1. **Test cluster-scoped mode:**
   ```bash
   # Deploy operator
   helm install test-cluster-op ./charts/marklogic-operator-kubernetes \
     --namespace ml-operators --create-namespace
   
   # Create resources in different namespaces
   kubectl create namespace test-a
   kubectl create namespace test-b
   
   # Deploy ML clusters in both
   # Verify operator manages both
   ```

2. **Test namespace-scoped mode:**
   ```bash
   # Deploy operator
   helm install test-ns-op ./charts/marklogic-operator-kubernetes \
     --namespace ml-operators --create-namespace \
     --set scope.type=namespace \
     --set scope.watchNamespace=test-a
   
   # Deploy ML cluster in test-a (should work)
   # Deploy ML cluster in test-b (should be ignored)
   ```

3. **Test migration:**
   ```bash
   # Start with cluster-scoped
   # Uninstall and reinstall as namespace-scoped
   # Verify existing ML clusters are managed correctly
   ```

## Files Modified/Created

### Modified Files:
- `cmd/main.go`
- `charts/marklogic-operator-kubernetes/values.yaml`
- `charts/marklogic-operator-kubernetes/templates/manager-rbac.yaml`
- `charts/marklogic-operator-kubernetes/templates/deployment.yaml`
- `README.md`

### New Files:
- `config/rbac/role_namespaced.yaml`
- `config/rbac/role_binding_namespaced.yaml`
- `docs/operator-scope-configuration.md`
- `examples/cluster-scoped-deployment.sh`
- `examples/namespace-scoped-deployment.sh`
- `examples/multi-namespace-deployment.sh`
- `examples/values-cluster-scoped.yaml`
- `examples/values-namespace-scoped.yaml`

## Next Steps

1. **Build and test the operator:**
   ```bash
   # Build the operator image
   make docker-build
   
   # Test locally
   make install  # Install CRDs
   make run      # Run operator locally
   ```

2. **Test the Helm chart:**
   ```bash
   # Lint the chart
   helm lint ./charts/marklogic-operator-kubernetes
   
   # Test template rendering
   helm template marklogic-operator ./charts/marklogic-operator-kubernetes \
     --set scope.type=namespace \
     --debug
   ```

3. **Update documentation:**
   - Consider adding this to official docs
   - Add migration guides for existing deployments
   - Include troubleshooting scenarios

4. **Consider future enhancements:**
   - Support for watching multiple specific namespaces (comma-separated list)
   - Dynamic namespace watching via ConfigMap
   - Metrics for per-namespace resource management
