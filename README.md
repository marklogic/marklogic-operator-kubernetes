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

For additional manifests to deploy a MarkLogic cluster inside a Kubernetes cluster, see [Operator manifest](https://docs.progress.com/bundle/marklogic-server-on-kubernetes/operator/Operator-manifest.html) in the documentation.

For Fluent Bit log collection configuration, including secret-backed environment variables for authenticated OpenTelemetry exports, see [Fluent Bit Log Collection](./docs/log-collection.md).

### Configure Object Storage Credentials
`spec.objectStorage` on a `MarklogicCluster` applies AWS S3 and/or Azure Blob credentials as a cluster-wide MarkLogic setting through the bootstrap host's Management API. Credentials always come from Secrets in the cluster's namespace; see [object-storage.yaml](./config/samples/object-storage.yaml).

| Provider | Required Secret keys | Optional keys |
|---|---|---|
| `aws` | `accessKey`, `secretKey` | `sessionToken` |
| `azure` | `storageAccount`, `storageKey` | none |

* Leading and trailing whitespace is trimmed; extra keys are ignored. Removing or emptying `sessionToken` applies the credentials without a token. Refreshing temporary credentials before they expire is up to you.
* Only `authType: secret` is supported. `region` is informational.
* The bootstrap admin identity needs the `manage-admin` and `security` roles (or `manage`/`manage-admin` plus the `credentials-set-aws`/`credentials-set-azure` privileges).
* Updating a referenced Secret rotates the credentials; no change to the cluster spec is needed.

Each provider reports an independent result in `status.objectStorage.<provider>`:

| Phase | Meaning |
|---|---|
| `Pending` | A required application is waiting for the bootstrap host (`BootstrapNotReady`). |
| `Applied` | MarkLogic accepted the credentials. |
| `Detached` | The Secret was deleted after a recorded successful application. |
| `Failed` | The Secret or the application failed; see `reason` and `message`. |

`Applied` alone does not mean the current Secret was applied. Before relying on the credentials (or deleting the Secret), check that `observedGeneration` equals the cluster's `metadata.generation`, `observedSecret` equals the current Secret's name, UID and resourceVersion, and `detachEligible` is `true`. Cloud access is not verified by the operator.

You can delete a provider's Secret after a successful application; the provider becomes `Detached` and the credentials already stored in MarkLogic keep working (temporary credentials can still expire). Recreating the Secret re-applies the credentials. Removing a provider from the spec stops management but does not revoke credentials in MarkLogic.

After fixing an external problem, such as MarkLogic privileges, retry a failed provider by changing the value of the `marklogic.progress.com/reconcile-request` annotation on the `MarklogicCluster`. This annotation is not propagated to the MarkLogic pods, so it does not restart them. Do not put credentials in annotations or manifests.

**Install and upgrade:** apply the updated `MarklogicCluster` CRD before starting an operator version that reconciles `spec.objectStorage`, because an older schema prunes the new status fields. For Helm, apply the CRD template first, then upgrade the release:
```sh
helm template <release> <chart> --show-only templates/marklogiccluster-crd.yaml | kubectl apply --server-side --force-conflicts -f -
```
Keep the provider Secrets until each provider reports the checks above.

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

1. The `fluent/fluent-bit:5.1.0` image has high security vulnerabilities. The default image is `fluent/fluent-bit:5.1.3`; review its security status and select an approved Fluent Bit or alternate image that meets your requirements before enabling log collection.
2. Known Issues and Limitations for the MarkLogic Server Docker image can be viewed using the link: [https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations](https://github.com/marklogic/marklogic-docker?tab=readme-ov-file#Known-Issues-and-Limitations).
3. If you're updating the group name configuration, ensure that you delete the pod to apply the changes, as we are using the OnDelete upgrade strategy.