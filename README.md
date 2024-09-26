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

### Configure HAProxy Load Balancer
HAProxy is provided as a load balancer configured to support cookie-based session affinity and multi-statement transactions. These configurations are needed by some MarkLogic client applications, like mlcp. HAProxy is recommended for production workloads. 

#### Enable the HAProxy Load Balancer
The HAProxy Load Balancer is disabled by default. To enable it, provide the following configuration in the crd yaml file to be used for cluster creation:
```
haproxy:
    enabled: true
```
#### Configuration
HAProxy can be configured for cluster and group. By default, ports 8000, 8001, and 8002 are configured to handle HTTP traffic. 
Ports can be configured for additional app servers. For example, to add port 8010 for HTTP load balancing, add this configuration to the marklogicgroup.yaml file:
```
- name: my-app-1     
      type: HTTP
      port: 8010
      targetPort: 8010
```
#### Access HA Proxy
The HAProxy can be accessed from a service with the name of marklogic-haproxy. 

#### External access
By default, HAProxy is configured to provide access within the Kubernetes cluster. However, HAProxy can provide external access by setting the service type in the marklogicgroup.yaml file:
```
haproxy:  
  service:    
    type: LoadBalancer
```

> [!WARNING]
> Please note, by setting the haproxy service type to LoadBalancer, the MarkLogic endpoint is exposed to the public internet. Because of this, networkPolicy should be set to limit the sources that can visit MarkLogic.