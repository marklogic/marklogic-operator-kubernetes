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
helm upgrade marklogic-operator marklogic-private/marklogic-operator --version=1.0.0-ea2 --install --namespace marklogic-operator-system --create-namespace
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
HAProxy can be configured for cluster. To setup the access for default App Servers for 8000, 8001 and 8002, uncomment the appServer seciton in marklogicgroup.yaml.
```
    appServers:
      - name: "app-service"
        port: 8000
        path: "/console"
      - name: "admin"
        port: 8001
        path: "/adminUI"
      - name: "manage"
        port: 8002
        path: "/manage"
```
Ports can be configured for additional app servers. For example, to add port 8010 for HTTP load balancing, add this configuration to the marklogicgroup.yaml file appServer section:
```
- name: my-app-1     
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

## Deployment with TLS Enabled
The MarkLogic Kubernetes Operator supports installing MarkLogic with HTTPS enabled on the default app servers. The default app servers are App-Services (8000), Admin (8001), and Manage (8002)
### Choose the type of certificate 
Two types of certificates are supported: standard certificates and temporary certificates.
* Temporary Certificates - A temporary certificate is ideal for development purposes. When using a temporary certificate for MarkLogic App Servers, a signed certificate does not need to be supplied. The certificate will be generated automatically.
* Standard Certificates - A standard certificate is issued by a trusted Certificate Authority (CA) for a specific domain (host name for MarkLogic server).  A standard certificate is strongly recommended for production environments. Support is provided for both named certificates and wildcard certificates.
  + Named Certificate - Each host must possess a designated certificate with a matching common name (CN).
  +  Wildcard Certificate - A single wildcard certificate can be used for all hosts within a cluster.

### Configure a MarkLogic cluster with a temporary certificate
To configure a temporary certificate, simply add the following option to CR yaml file:
```
tls:
  enableOnDefaultAppServers: true
```

### Configure a MarkLogic cluster with a standard certificate
To configure a MarkLogic cluster with a standard certificate, follow these steps:
1. Obtain a certificate with a common name matching the hostname of the MarkLogic host.  The certificate must be signed by a trusted Certificate Authority (CA). Either a publicly rooted CA or a private CA can be used. This example uses a private CA and a 2-node cluster.
2. Use this script to generate a self-signed CA certificate with openSSL. The script will create ca-private-key.pem as the CA key and cacert.pem as the private CA certificate:
```
# Generate private key for CA
openssl genrsa -out ca-private-key.pem 2048
 
# Generate the self-signed CA certificate
openssl req -new -x509 -days 3650 -key ca-private-key.pem -out cacert.pem
```
3. Use the script below to generate a private key and CSR for the marklogic-0 pod.  After running the script, tls.key is generated as a private key and a host certificate for the marklogic-0 pod.
>Note: The filename for the private key must be tls.key and the filename for host certificate must be tls.crt.
* If the release name is "marklogic", then the host name for the marklogic-0 pod will be "marklogic-0.marklogic.default.svc.cluster.local".
* The host name for the marklogic-1 pod will be "marklogic-1.marklogic.default.svc.cluster.local".
```
# Create private key
openssl genpkey -algorithm RSA -out tls.key
 
# Create CSR for marklogic-0
# Use marklogic-0.marklogic.default.svc.cluster.local as Common Name(CN) for CSR
openssl req -new -key tls.key -out tls.csr
 
# Sign CSR with private CA
openssl x509 -req -CA cacert.pem -CAkey ca-private-key.pem -in tls.csr -out tls.crt -days 365
```
4. Use this script below to generate secrets for the host certificate and the CA certificate. Repeat these steps to generate the certificate for the marklogic-1 host and create the secret marklogic-1-cert.  After running the script,  secretes are created for marklogic-0 and marklogic-1. One secret is also created for the private CA certificate.
```
# Generate Secret for marklogic-0 host certificate
kubectl create secret generic marklogic-0-cert --from-file=tls.crt --from-file=tls.key
 
# Generate Secret for private CA certificate
kubectl create secret generic ca-cert --from-file=cacert.pem
```
1. Once the certificate is created within Kubernetes secrets, add the following section to the CR yaml file and follow the instructions outlined in Install the Operator.
```
tls:
  enableOnDefaultAppServers: true
  certSecretNames:
    - "marklogic-0-cert"
    - "marklogic-1-cert" 
  caSecretName: "ca-cert"
```



## Known Issues and Limitations

1. The latest released version of fluent/fluent-bit:3.2.5 has known high and critical security vulnerabilities. If you decide to enable the log collection feature, choose and deploy the fluent-bit or an alternate image with no vulnerabilities as per your requirements. 
2. Known Issues and Limitations for the MarkLogic Server Docker image can be viewed using the link: https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations.