# MarkLogic Operator for Kubernetes

## Introduction

The MarkLogic Operator for Kubernetes is an operator that allows you to deploy and manage MarkLogic clusters on Kubernetes. It provides a declarative way to define and manage MarkLogic resources. For detailed documentation, please refer to [MarkLogic Operator for Kubernetes](https://docs.progress.com/bundle/marklogic-server-on-kubernetes).

## Getting Started

### Prerequisites

[Helm](https://helm.sh/docs/intro/install/) v3.0.0 or later and [Kubectl](https://kubernetes.io/docs/tasks/tools/) v1.30 or same as your Kubernetes version must be installed locally in order to use MarkLogic operator helm chart. 

### Kubernetes Version

This operator supports Kubernetes 1.30 or later.

### MarkLogic Version

This operator supports MarkLogic 11.1 or later.

### Operator Scope Options

The MarkLogic Operator can be deployed in two modes:

- **Cluster-Scoped (Default)**: Watches and manages MarkLogic resources across all namespaces in the cluster
- **Namespace-Scoped**: Watches and manages MarkLogic resources only in a specific namespace

For detailed information on scope configuration, see [Operator Scope Configuration](./docs/operator-scope-configuration.md).

### Run MarkLogic Operator for Kubernetes using Helm Chart

1. Add MarkLogic Operator for Kubernetes Helm Repo:
```sh
helm repo add marklogic-operator https://marklogic.github.io/marklogic-operator-kubernetes/

helm repo update
```

2. Install or upgrade the Helm Chart for MarkLogic Operator: 

**For cluster-scoped deployment (default - watches all namespaces):**
```sh
helm upgrade marklogic-operator marklogic-operator/marklogic-operator-kubernetes --version=1.3.1 --install --namespace marklogic-operator-system --create-namespace
```

**For namespace-scoped deployment (watches only a specific namespace):**
```sh
# Watch the same namespace where operator is deployed
helm upgrade marklogic-operator marklogic-operator/marklogic-operator-kubernetes --version=1.3.1 --install --namespace marklogic-prod --create-namespace --set scope.type=namespace

# Or watch a different namespace
helm upgrade marklogic-operator marklogic-operator/marklogic-operator-kubernetes --version=1.3.1 --install --namespace marklogic-operator-system --create-namespace --set scope.type=namespace --set scope.watchNamespaces=marklogic-prod

# Or watch multiple namespaces
helm upgrade marklogic-operator marklogic-operator/marklogic-operator-kubernetes --version=1.3.1 --install --namespace marklogic-operator-system --create-namespace --set scope.type=namespace --set scope.watchNamespaces="prod,staging,dev"
```

See [Operator Scope Configuration](./docs/operator-scope-configuration.md) for more deployment options and examples.

3. Make sure the Marklogic Operator pod is running:
```sh
kubectl get pods -n marklogic-operator-system 
```

4. Use this command to verify CRDs are correctly installed:
```sh
kubectl get crd -n marklogic-operator-system | grep 'marklogic'
```

### Install MarkLogic Cluster
Once MarkLogic Operator Pod is running, use your custom manifests or choose from sample manifests from this repository located in the `./config/samples` directory.
Optionally, create a dedicated namespace for new MarkLogic resources:
```sh
kubectl create namespace <namespace-name>
```

To deploy a MarkLogic single group, use the `quick_start.yaml` from the `config/samples`: 
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
The MarkLogic admin secret name is in the format `<marklogicCluster-name>-admin`. For example, if the markLogicCluster name is `single-node`, the secret name is `single-node-admin`.

2. Using the secret name from step 1, retrieve the MarkLogic admin credentials using these commands:
```sh
kubectl get secret single-node-admin --namespace=<namespace-name> -o jsonpath='{.data.username}' | base64 --decode; echo

kubectl get secret single-node-admin --namespace=<namespace-name> -o jsonpath='{.data.password}' | base64 --decode; echo

kubectl get secret single-node-admin --namespace=<namespace-name> -o jsonpath='{.data.wallet-password}' | base64 --decode; echo
```

### Operator User Credentials

The Operator creates the MarkLogic user `marklogic-kubernetes-operator` and assigns it the `marklogic-operator` role. That role inherits `manage-admin`, `pki`, and `admin-ui-user`, and receives the execute privileges `create-user`, `xdmp:remove-dynamic-hosts`, `admin-issue-dynamic-host-token`, `xdmp:eval`, and `create-external-security` for the Operator's Management API tasks.

The generated credential is stored in the cluster-owned Secret `<marklogicCluster-name>-operator`, with `username` and `password` keys. The password is 32 alphanumeric characters generated from a cryptographically secure random source. Kubernetes garbage collection removes this Secret when its MarklogicCluster is deleted. After the Operator verifies that the user can authenticate and the bootstrap host is online, it records the active Secret in MarklogicGroup status at `.status.credentialSecretName`. Reapplying the MarklogicCluster manifest does not reset this handoff.

The bootstrap admin Secret remains in Kubernetes and mounted into MarkLogic pods for Admin API startup operations, initial setup, role/user reconciliation, and recovery. The operator Secret is mounted separately and is used for intended Management API operations after handoff. If operator authentication fails, the controller falls back to the retained admin Secret and repairs the operator role/user using the credentials in the operator Secret. Credential changes update the pod template revision. With `OnDelete`, the controller replaces stale pods one at a time after the new identity is accepted; with `RollingUpdate`, Kubernetes performs the rollout according to the configured strategy. Persistent volume claims are retained. Keep the admin Secret available; deleting it removes the recovery path.

To rotate the operator password, update the Secret's `password` value; the Operator reconciles the MarkLogic user and serially replaces pods using the old credential revision. The Operator does not rotate passwords automatically. To use a user-managed Secret instead, set `spec.auth.operatorSecretName` on the MarklogicCluster and provide a `password` key. The fixed MarkLogic username remains `marklogic-kubernetes-operator`; the supplied Secret is not owned or deleted by the Operator.

To check which credential is active for a group, run:
```sh
kubectl get marklogicgroup <group-name> --namespace=<namespace> -o jsonpath='{.status.credentialSecretName}'
```

For additional manifests to deploy a MarkLogic cluster inside a Kubernetes cluster, see [Operator manifest](https://docs.progress.com/bundle/marklogic-server-on-kubernetes/operator/Operator-manifest.html) in the documentation.

For Fluent Bit log collection configuration, including secret-backed environment variables for authenticated OpenTelemetry exports, see [Fluent Bit Log Collection](./docs/log-collection.md).

## Clean Up

#### Cleaning up MarkLogic Cluster
Use this step to delete the MarkLogic cluster and other resources created from the manifests used in the above [step](#install-marklogic-cluster):
```sh
kubectl delete -f quick_start.yaml --namespace=<namespace-name>
```

Manually delete the persistent volume claims:
```sh
kubectl delete pvc -n <namespace-name> -l app.kubernetes.io/name=marklogic
```

#### Deleting Helm chart
Use these steps to delete the MarkLogic Operator Helm chart and the namespace created:
```sh
helm delete marklogic-operator --namespace marklogic-operator-system
kubectl delete namespace marklogic-operator-system
```

> **Note for version 1.2.0 and later:** The MarkLogic Operator includes the `helm.sh/resource-policy: keep` annotation. When you delete the operator using the Helm command above, the Custom Resource Definitions (CRDs) and your existing MarkLogic deployments are preserved and will **not** be deleted automatically. 

#### Deleting Custom Resource Definitions (CRDs)
To perform a complete system wipe and manually delete the Custom Resource Definitions (CRDs):

**Warning:** Deleting a CRD will automatically delete all Custom Resources of that type across the entire Kubernetes cluster. Only proceed if you are certain you want to permanently remove all MarkLogic resources.

```sh
kubectl delete crd marklogicclusters.marklogic.progress.com
kubectl delete crd marklogicgroups.marklogic.progress.com
```

## Known Issues and Limitations

1. The latest released version of `fluent/fluent-bit:5.1.0` has high security vulnerabilities. If you decide to enable the log collection feature, choose and deploy the fluent-bit or an alternate image with no vulnerabilities as per your requirements.
2. Known Issues and Limitations for the MarkLogic Server Docker image can be viewed using the link: [https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations](https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations).
3. If you're updating the group name configuration, ensure that you delete the pod to apply the changes, as we are using the OnDelete upgrade strategy.