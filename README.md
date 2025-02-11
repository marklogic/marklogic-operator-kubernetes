# MarkLogic Kubernetes Operator

## Introduction

The MarkLogic Operator for Kubernetes is an operator that allows you to deploy and manage MarkLogic clusters on Kubernetes. It provides a declarative way to define and manage MarkLogic resources. For detailed documentation, please refer [MarkLogic Kubernetes Operator](https://).

## Getting Started

### Kubernetes Version

This Operator supports Kubernetes 1.30 or later.

### MarkLogic Version

This Operator supports MarkLogic 11.1 or later.

### Running MarkLogic Operator

#### Run MarkLogic Operator locally
To run MarkLogic Kuberentes Operator locally, use following steps:
1. Check out the source code from the marklogic/marklogic-kubernetes-operator GitHub repository.
```
git clone https://github.com/marklogic/marklogic-kubernetes-operator.git
```

2. Use below commands to run the MarkLogic Operator locally:
  * Build the project
```sh
make build
```
   * Install the CRD to Kubernetes cluster
```sh
make install
```
   * Run the operator controller
```sh
make run
```
#### Running Operator using Helm Chart

1. Add MarkLogic Kubernetes Operator Helm Repo
```sh
helm repo add marklogic-operator https://raw.githubusercontent.com/marklogic/marklogic-kubernetes-operator/gh-pages/

helm repo update
```
3. Install the Helm Chart for MarkLogic Operator: 
```sh
helm upgrade marklogic-operator marklogic-operator/marklogic-operator --version=1.0.0 --install --namespace marklogic-operator-system --create-namespace
```
4. Make sure the Operator Pod is running:
```sh
kubectl get pods -n marklogic-operator-system 
```

### Install MarkLogic Cluster with MarkLogic Operator
Once MarkLogic Operator is running, use your custom manifests or choose from sample manifests from this repository located in the samples folder under config. For example, to deploy marklogic single group, use the following script: 
```sh
kubectl apply -f marklogicgroup.yaml
```
Please refer [Documentation]() for more sample manifests with different configurations to deploy MarkLogic cluster inside a Kubernetes cluster.

## Known Issues and Limitations

1. The latest released version of fluent/fluent-bit:3.2.5 has known high and critical security vulnerabilities. If you decide to enable the log collection feature, choose and deploy the fluent-bit or an alternate image with no vulnerabilities as per your requirements. 
2. Known Issues and Limitations for the MarkLogic Server Docker image can be viewed using the link: https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations.