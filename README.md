# MarkLogic Kubernetes Operator (Private Repo)

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

### Deploy Operator with Helm Chart in Private Repo
The EA release is currently in a private repository. To access and work with this private repository, please follow these steps:
1. First you need to create a Fine-grained tokens in Github with Read-only permission for marklogic/marklogic-kubernetes-operator repository
   Once you have the token, put store it in a safe place and put it in the environmental variable:
```sh
GITHUB_TOKEN=<YOUR_GITHUB_TOKEN>
```
2. Then add the private repo to Helm repository with the GITHUB_TOKEN: 
```sh
helm repo add marklogic-private https://raw.githubusercontent.com/marklogic/marklogic-kubernetes-operator/gh-pages/ --username <YOUR_USERNAME> --password $GITHUB_TOKEN
helm repo update
```
3. Install the Helm Chart for MarkLogic Operator: 
```sh
helm upgrade marklogic-operator marklogic-private/marklogic-operator --version=1.0.0-ea1 --install --namespace marklogic-operator-system --create-namespace
```
4. Check the Operator Pod and make sure it is in Running state:
```sh
kubectl get pods -n marklogic-operator-system 
```

### Run Operator locally
After checking out the source code, you can run the MarkLogic Operator locally by following these steps:
```sh
make build # build the project
make install # instal CRD to Kubernetes cluster
make run # run the operator controller locally
```

### Deploy Operator locally
After checking out the source code, you can deploy the MarkLogic Operator locally by following these steps:
```sh
make build # build the project
make docker-build # build the operator to docker image
make docker-push # push the operator to remote docker repo
make deploy # deploy the CRD and Operator into marklogic-operator-system namespace
```

### Build Helm Chart locally
If you don't have the GITHUB_TOKEN that is required to visit the Github Repo, you can also build the Helm Chart locally.
First build the Helm Chart
```sh
make build
make docker-build
make helm
```
Then deploy the Operator with Helm Chart
```sh
helm upgrade marklogic-operator ./charts/marklogic-operator --install --namespace marklogic-operator-system --create-namespace
```

## Install MarkLogic Cluster with MarkLogic Operator
Once MarkLogic Operator is installed, go to config/samples folder and pick one sample file to deploy. For example, to deploy marklogic single group, use the following script: 
```sh
kubectl apply -f marklogicgroup.yaml
```