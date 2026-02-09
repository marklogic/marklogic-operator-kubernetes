# Example: Multi-Namespace Setup with Multiple Operators
# Deploy separate operators for different namespaces
# Useful in multi-tenant environments where each team manages their own operator

# Create namespaces
kubectl create namespace team-a-marklogic
kubectl create namespace team-b-marklogic
kubectl create namespace operators

# Deploy operator for Team A
helm install ml-operator-team-a ./charts/marklogic-operator-kubernetes \
  --namespace operators \
  --set scope.type=namespace \
  --set scope.watchNamespace=team-a-marklogic

# Deploy operator for Team B
helm install ml-operator-team-b ./charts/marklogic-operator-kubernetes \
  --namespace operators \
  --set scope.type=namespace \
  --set scope.watchNamespace=team-b-marklogic

# Verify both operators
kubectl get deployments -n operators

# Team A deploys their cluster
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: team-a-cluster
  namespace: team-a-marklogic
spec:
  replicas: 3
  # ... Team A specific configuration
EOF

# Team B deploys their cluster  
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: team-b-cluster
  namespace: team-b-marklogic
spec:
  replicas: 5
  # ... Team B specific configuration
EOF

# Check which operator is managing each cluster
kubectl logs -n operators deployment/marklogic-operator-controller-manager-team-a -c manager | grep "team-a-cluster"
kubectl logs -n operators deployment/marklogic-operator-controller-manager-team-b -c manager | grep "team-b-cluster"

# Cleanup specific operator
# helm uninstall ml-operator-team-a -n operators
# helm uninstall ml-operator-team-b -n operators
