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

1. Add MarkLogic Operator for Kubernees Helm Repo:
```sh
helm repo add marklogic-operator-kubernetes https://raw.githubusercontent.com/marklogic/marklogic-operator-kubernetes/gh-pages/

helm repo update
```

2. Install the Helm Chart for MarkLogic Operator: 
```sh
helm upgrade marklogic-operator-kubernetes marklogic-operator-kubernetes/marklogic-operator-kubernetes --version=1.0.0 --install --namespace marklogic-operator-system --create-namespace
```

3. Make sure the Marklogic Operator pod is running:
```sh
kubectl get pods -n marklogic-operator-system 
```

### Install MarkLogic Cluster
Once MarkLogic Operator Pod is running, use your custom manifests or choose from sample manifests from this repository located in the ./config/samples directory. For example, to deploy marklogic single group, use the `quick_start.yaml` from the samples: 
```sh
kubectl apply -f quick_start.yaml
```
Once the installation is complete and the pod is in a running state, the MarkLogic Admin UI can be accessed using the port-forwarding command:

  ```shell
  kubectl port-forward <pod-name> 8000:8000 8001:8001 --namespace=<namespace-name>
  ```

If you used the automatically generated admin credentials, use these steps to extract the admin username, password, and wallet-password from a secret:

1. Run this command to fetch all of the secret names:
  ```shell
  kubectl get secrets 
  ```
The MarkLogic admin secret name is in the format  `<marklogicCluster-name>-admin`. For example if markLogicCluster name is `single-node`, the secret name is `single-node-admin`.

2. Using the secret name from step 1, retrieve the MarkLogic admin credentials using these commands:
  ```shell
  kubectl get secret single-node-admin -o jsonpath='{.data.username}' | base64 --decode 

  kubectl get secret single-node-admin -o jsonpath='{.data.password}' | base64 --decode 

  kubectl get secret single-node-admin -o jsonpath='{.data.wallet-password}' | base64 --decode 
  ```

Please note that if configurations are provided for both the cluster and the group, the values specified for the cluster will take precedence and be applied.
For additional manifests to deploy a MarkLogic cluster inside a Kubernetes cluster, see [Operator manifest](https://docs.progress.com/bundle/marklogic-server-on-kubernetes/operator/Operator-manifest.html) in the documentation.

## Clean Up

#### Cleaning up MarkLogic Cluster
Use these steps to delete MarkLogic cluster and other resources created from the manifests used in the above [step](#install-marklogic-cluster).
```sh
kubectl delete -f quick_start.yaml
```

#### Deleting Helm chart
Use these steps to delete MarkLogic Operator Helm chart.
```sh
helm delete marklogic-operator-kubernetes
```

## Known Issues and Limitations

1. The latest released version of fluent/fluent-bit:3.2.5 has high and critical security vulnerabilities. If you decide to enable the log collection feature, choose and deploy the fluent-bit or an alternate image with no vulnerabilities as per your requirements.
2. Known Issues and Limitations for the MarkLogic Server Docker image can be viewed using the link: https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations.
3. If you're updating the group name configuration, ensure that you delete the pod to apply the changes, as we are using the OnDelete upgrade strategy.