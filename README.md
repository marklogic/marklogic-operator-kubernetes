# MarkLogic Operator for Kuberentes

## Introduction

The MarkLogic Operator for Kubernetes is an operator that allows you to deploy and manage MarkLogic clusters on Kubernetes. It provides a declarative way to define and manage MarkLogic resources. For detailed documentation, please refer to [MarkLogic Operator for Kubernetes](https://docs.progress.com/bundle/marklogic-server-on-kubernetes).

## Getting Started

### Prerequisites

[Helm](https://helm.sh/docs/intro/install/) v3.0.0 or later and [Kubectl](https://kubernetes.io/docs/tasks/tools/) v1.30 or same as your Kubernetes version must be installed locally in order to use MarkLogic operator helm chart. 

### Kubernetes Version

This operator supports Kubernetes 1.30 or later.

### MarkLogic Version

This operator supports MarkLogic 11.1 or later.

### Run MarkLogic Operator for Kubernetes using Helm Chart

1. Add MarkLogic Operator for Kubernetes Helm Repo:
```sh
helm repo add marklogic-operator https://marklogic.github.io/marklogic-operator-kubernetes/

helm repo update
```

2. Install or upgrade the Helm Chart for MarkLogic Operator: 
```sh
helm upgrade marklogic-operator marklogic-operator/marklogic-operator-kubernetes --version=1.1.0 --install --namespace marklogic-operator-system --create-namespace
```

3. Make sure the Marklogic Operator pod is running:
```sh
kubectl get pods -n marklogic-operator-system 
```

4. Use this command to verify CRDs are correctly installed:
```sh
 kubectl get crd -n marklogic-operator-system | grep 'marklogic'
```

### Install MarkLogic Cluster
Once MarkLogic Operator Pod is running, use your custom manifests or choose from sample manifests from this repository located in the ./config/samples directory.
Optionally, create a dedicated namespace for new MarkLogic resources.
  ```sh
  kubectl create namespace <namespace-name>
  ```
To deploy marklogic single group, use the `quick_start.yaml` from the config/samples: 
  ```sh
  kubectl apply -f quick_start.yaml --namespace=<namespace-name>
  ```
Once the installation is complete and the pod is in a running state, the MarkLogic Admin UI can be accessed using the port-forwarding command:

  ```sh
  kubectl port-forward <pod-name> 8000:8000 8001:8001 --namespace=<namespace-name>
  ```

If you used the automatically generated admin credentials, use these steps to extract the admin username, password, and wallet-password from a secret:

1. Run this command to fetch all of the secret names:
  ```sh
  kubectl get secrets --namespace=<namespace-name>
  ```
The MarkLogic admin secret name is in the format  `<marklogicCluster-name>-admin`. For example if markLogicCluster name is `single-node`, the secret name is `single-node-admin`.

2. Using the secret name from step 1, retrieve the MarkLogic admin credentials using these commands:
  ```sh
  kubectl get secret single-node-admin --namespace=<namespace-name> -o jsonpath='{.data.username}' | base64 --decode; echo

  kubectl get secret single-node-admin --namespace=<namespace-name> -o jsonpath='{.data.password}' | base64 --decode; echo

  kubectl get secret single-node-admin --namespace=<namespace-name> -o jsonpath='{.data.wallet-password}' | base64 --decode; echo
  ```

For additional manifests to deploy a MarkLogic cluster inside a Kubernetes cluster, see [Operator manifest](https://docs.progress.com/bundle/marklogic-server-on-kubernetes/operator/Operator-manifest.html) in the documentation.

## Clean Up

#### Cleaning up MarkLogic Cluster
Use this step to delete MarkLogic cluster and other resources created from the manifests used in the above [step](#install-marklogic-cluster):
  ```sh
  kubectl delete -f quick_start.yaml --namespace=<namespace-name>
  ```

#### Deleting Helm chart
Use these steps to delete MarkLogic Operator Helm chart and the namespace created:
```sh
helm delete marklogic-operator --namespace marklogic-operator-system
kubectl delete namespace marklogic-operator-system
```

## Known Issues and Limitations

1. The latest released version of fluent/fluent-bit:4.1.1 has high security vulnerabilities. If you decide to enable the log collection feature, choose and deploy the fluent-bit or an alternate image with no vulnerabilities as per your requirements.
2. Known Issues and Limitations for the MarkLogic Server Docker image can be viewed using the link: https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations.
3. If you're updating the group name configuration, ensure that you delete the pod to apply the changes, as we are using the OnDelete upgrade strategy.
