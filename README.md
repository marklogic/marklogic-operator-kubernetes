# MarkLogic Kubernetes Operator

## Introduction

The MarkLogic Kubernetes Operator is a tool that allows you to deploy and manage MarkLogic clusters on Kubernetes. It provides a declarative way to define and manage MarkLogic resources such as clusters, databases, and forests.

## Code Structure
api/ Contains the API definition
internal/controller Contains the code for controllers
config/ Contains configuration files to launch the project on a cluster
config/samples/ Contains the samples to create marklogic cluster using the Operator.
test/ Contains E2E tests for the operator
pkg/ Contains golang packges to support reconciliation, utilities.

## Run or deploy Operator

### Run Operator locally
```sh
make build # build the project
make install # instal CRD to Kubernetes cluster
make run # run the operator controller locally
```

### Deploy Operator locally
```sh
make build # build the project
make docker-build # build the operator to docker image
make docker-push # push the operator to remote docker repo
make deploy # deploy the CRD and Operator into marklogic-operator-system namespace
```

### Deploy Operator with Helm Chart
1. Add the Helm repository: 
```sh
helm repo add marklogic-operator https://marklogic.github.io/marklogic-kubernetes-operator/
```

2. Install the Helm Chart for MarkLogic Operator: 
```sh
helm upgrade marklogic-operator marklogic-operator/marklogic-operator --install --namespace marklogic-operator-system --create-namespace
```

## Install MarkLogic Cluster with MarkLogic Operator
Once MarkLogic Operator is installed, go to config/samples folder and pick one sample file to deploy. For example, to deploy marklogic single group, use the following script: 
```sh
kubectl apply -f marklogicgroup.yaml
```