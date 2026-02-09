# Example: Watching Multiple Specific Namespaces
# Deploy operator to watch multiple namespaces simultaneously

# Create the namespaces to watch
kubectl create namespace team-a-ns
kubectl create namespace team-b-ns
kubectl create namespace team-c-ns

# Option 1: Using comma-separated string
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operators \
  --create-namespace \
  --set scope.type=namespace \
  --set scope.watchNamespaces="team-a-ns,team-b-ns,team-c-ns"

# Option 2: Using values file with array
cat > multi-namespace-values.yaml <<EOF
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
  -f multi-namespace-values.yaml

# Verify the operator is watching multiple namespaces
kubectl logs -n marklogic-operators \
  deployment/marklogic-operator-controller-manager -c manager | grep "watch"

# Expected output:
# "operator will watch resources in multiple namespaces" namespaces=["team-a-ns","team-b-ns","team-c-ns"]

# Verify RBAC resources were created in each namespace
kubectl get role,rolebinding -n team-a-ns | grep marklogic-operator
kubectl get role,rolebinding -n team-b-ns | grep marklogic-operator
kubectl get role,rolebinding -n team-c-ns | grep marklogic-operator

# Deploy MarkLogic clusters in different watched namespaces
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: team-a-cluster
  namespace: team-a-ns
spec:
  replicas: 3
  # ... configuration
EOF

kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: team-b-cluster
  namespace: team-b-ns
spec:
  replicas: 5
  # ... configuration
EOF

# Try to create a cluster in a non-watched namespace (should be ignored)
kubectl create namespace team-d-ns
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: team-d-cluster
  namespace: team-d-ns  # NOT watched by operator
spec:
  replicas: 3
  # ... configuration
EOF

# Check that only watched namespaces are managed
kubectl get marklogicclusters -A

# Cleanup
# helm uninstall marklogic-operator -n marklogic-operators
# kubectl delete namespace team-a-ns team-b-ns team-c-ns team-d-ns
