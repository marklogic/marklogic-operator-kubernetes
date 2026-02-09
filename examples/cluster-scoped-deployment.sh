# Example: Cluster-Scoped Deployment
# This deploys the operator to watch ALL namespaces in the cluster

# Install with Helm
helm install marklogic-operator ./charts/marklogic-operator-kubernetes \
  --namespace marklogic-operator-system \
  --create-namespace \
  --set scope.type=cluster

# Verify deployment
kubectl get deployment -n marklogic-operator-system
kubectl logs -n marklogic-operator-system deployment/marklogic-operator-controller-manager -c manager

# Now you can create MarkLogic clusters in any namespace
kubectl create namespace marklogic-dev
kubectl create namespace marklogic-prod

# Deploy MarkLogic cluster in dev
kubectl apply -f - <<EOF
apiVersion: marklogic.progress.com/v1
kind: MarklogicCluster
metadata:
  name: ml-cluster-dev
  namespace: marklogic-dev
spec:
  replicas: 3
  # ... other specs
EOF

# Deploy MarkLogic cluster in prod  
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

# The single operator in marklogic-operator-system will manage both clusters
