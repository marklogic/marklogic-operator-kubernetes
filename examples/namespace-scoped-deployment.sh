# Example: Namespace-Scoped Deployment
# This deploys the operator to watch ONLY a specific namespace

# Option 1: Watch the same namespace where operator is deployed
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-prod \
  --create-namespace \
  --set scope.type=namespace

# Verify deployment
kubectl get deployment -n marklogic-prod
kubectl logs -n marklogic-prod deployment/marklogic-operator-controller-manager -c manager

# Deploy MarkLogic cluster in the same namespace
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: ml-cluster-prod
  namespace: marklogic-prod
spec:
  replicas: 5
  # ... other specs
EOF

# ---

# Option 2: Watch a different namespace than where operator is deployed

# First, create the namespace to watch
kubectl create namespace marklogic-prod

# Install operator in operators namespace, watching marklogic-prod
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespace=marklogic-prod

# Verify the watched namespace
kubectl get deployment -n marklogic-operators \
  -o jsonpath='{.items[0].spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="WATCH_NAMESPACE")].value}'

# Deploy MarkLogic cluster in watched namespace
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: ml-cluster-prod
  namespace: marklogic-prod
spec:
  replicas: 5
  # ... other specs
EOF

# Note: If you try to create a cluster in a different namespace, it will be ignored
kubectl create namespace marklogic-dev
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: ml-cluster-dev
  namespace: marklogic-dev  # This will NOT be managed by the operator
spec:
  replicas: 3
  # ... other specs
EOF
