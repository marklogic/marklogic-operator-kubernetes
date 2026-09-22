# Demo: Configuring Object Storage Credentials (AWS S3 / Azure Blob)

This walkthrough configures cluster-wide credentials. `Applied` means MarkLogic accepted
the credential write; it does **not** prove that a key is valid, a token is unexpired,
or a backup can access a bucket. Backup and forest setup remain administrator tasks.

## Prerequisites

- An operator and CRD built from code that supports `spec.objectStorage`.
- An initialized `MarklogicCluster`, a cloud storage account/bucket, and suitable credentials.
- `bash`, `python3`, `kubectl`, and `curl`; provisioning also uses `make`, Docker and the cloud CLI.
- The scripts contain example context, namespace, registry and cluster names. Review those
  settings before running. They use existing AKS/EKS infrastructure; they do not create it.
- Run scripts as whole files without shell tracing (`bash -x` exposes secrets).

Check the installed schema in the intended context:

```bash
kubectl --context "$CONTEXT" get crd marklogicclusters.marklogic.progress.com -o jsonpath=\
  '{.spec.versions[?(@.name=="v1")].schema.openAPIV3Schema.properties.spec.properties.objectStorage}'
```

If absent, update both CRDs and the operator. Do not assume that the historical demo
resources still exist; inspect the selected cluster before the presentation.

## Scripts

Scripts live in `docs/spec/object-storage/demo/`. Every API request is pinned to its
configured context, so separate EKS/AKS terminal panes cannot redirect each other's work.
Scripts pass credential values through environment variables and stdin; verification
prints only presence flags. Provisioning builds an amd64 image using the Makefile target;
these demo clusters must have compatible nodes.

| Script | Purpose |
|---|---|
| `eks-01-provision.sh` | Build/push the operator to ECR, apply manifests, and bootstrap the demo cluster |
| `aks-01-provision.sh` | Build/push to ACR, apply manifests, configure `acr-pull`, and bootstrap the demo cluster |
| `eks-02-apply-aws-credentials.sh` | Apply AWS credentials supplied through environment variables and wait for their fingerprint to reach `Applied` |
| `aks-02-apply-azure-credentials.sh` | Fetch the Azure account key, apply its Secret, and wait for its fingerprint to reach `Applied` |
| `aks-03-apply-fake-multi-provider-rotation.sh` | Apply both providers with dummy values, rotate both Secrets, and check each resulting fingerprint |
| `verify-credentials.sh <context> <namespace> <cluster> <service> <aws\|azure>` | Read credential presence directly from MarkLogic using digest authentication and a temporary port-forward |
| `common.sh` | Shared helpers; source only |

Provisioning hashes the image inputs, including `pkg/`, and deployment configuration to
skip unchanged builds. It renders from a temporary config copy and records the hash only
after rollout succeeds. A manifest apply failure stops the script; a pre-existing Helm
Deployment with a different immutable selector requires resolving the installation mismatch
before retrying. The scripts do not silently accept a partial manifest update.

Run provisioning before presenting:

```bash
bash docs/spec/object-storage/demo/eks-01-provision.sh
bash docs/spec/object-storage/demo/aks-01-provision.sh
```

`--switch` only changes the interactive context. `--recreate` deletes the selected demo
MarklogicCluster and datadir PVCs identified from its owned groups, **destroying their data**.
Use it only for disposable demos. The default skips CR creation when it already exists,
but still waits for the bootstrap pod. Pod readiness does not imply credential readiness.

## AWS credentials and rotation

Supply `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from your credential source as exported
environment variables. For temporary credentials also export `AWS_SESSION_TOKEN`; unset it
when using long-lived keys. The script neither creates nor deactivates IAM access keys.
An external process must refresh temporary credentials before expiry.

```bash
bash docs/spec/object-storage/demo/eks-02-apply-aws-credentials.sh
bash docs/spec/object-storage/demo/verify-credentials.sh \
  pzhou@marklogic-tiered-poc.us-west-2.eksctl.io marklogic ml-tiered-poc node aws
```

The applied CR configuration is:

```yaml
spec:
  objectStorage:
    aws:
      authType: secret
      secretName: ml-s3-credentials
      region: us-west-2 # informational only; does not configure MarkLogic's region
```

To rotate, export the replacement credentials and rerun the credential script. It updates
the Secret and reapplies the same CR patch; the changed Secret alone is sufficient to
trigger reconciliation. For a demonstration with **no CR patch during rotation**, use the
fake multi-provider script below. The wait helper checks the expected fingerprint, so an
old `Applied` status cannot be mistaken for successful rotation.

Secrets are applied server-side using the `object-storage-demo` field manager. If another
manager owns these keys, resolve that ownership conflict explicitly before retrying.
These Secrets should be dedicated to this demo. Do not print them or commit real values.

## Azure credentials

```bash
bash docs/spec/object-storage/demo/aks-02-apply-azure-credentials.sh
bash docs/spec/object-storage/demo/verify-credentials.sh \
  pzhou-test-k8s marklogic ml-tiered-blob-poc node azure
```

The Secret must be in the same namespace as the cluster and contain `storageAccount`
and `storageKey`. The CR references it through:

```yaml
spec:
  objectStorage:
    azure:
      authType: secret
      secretName: ml-azure-credentials
```

Verification respects `spec.auth.secretName`, defaulting to `<cluster>-admin`. For TLS-enabled
Management APIs, set `ML_CA_CERT` to the issuing CA file and `ML_MANAGE_HOST` to a hostname
covered by the server certificate. The port-forward still routes traffic locally.
The verifier never prints the raw GET response: MarkLogic returns AWS session tokens in
plaintext, even though the secret key is encrypted.

## Both providers and rotation

```bash
bash docs/spec/object-storage/demo/aks-03-apply-fake-multi-provider-rotation.sh
```

This script replaces both provider references on the demo cluster with **fake credentials**.
It waits for each initial fingerprint, changes only the Secrets, then waits for both new
fingerprints. It proves credential application and rotation, not cloud authentication.
Rerun the real credential scripts to restore real provider references afterward.

## Failure and lifecycle scenarios

- Remove the AWS Secret or its required `secretKey` while Azure remains configured: AWS
  becomes `Failed` with `SecretNotFound` or `SecretKeyMissing`; Azure remains `Applied`.
  A nonempty but invalid cloud key will still be accepted by the credentials API.
- Change an unrelated CR field: unchanged credentials retain `lastAppliedTime`.
- Remove one provider while retaining the other: the removed provider becomes `Disabled`.
  Removing all providers clears `status.objectStorage`. Neither action revokes stored keys.
- Try AWS `instanceRole` or Azure `managedIdentity`: CRD validation rejects these reserved modes.
- Use a real object read/write or backup as a separate cloud-access check. Management API
  read-back alone cannot establish that access works.

| Reason | Meaning |
|---|---|
| `BootstrapNotReady` | Bootstrap discovery/readiness or admin-Secret prerequisites failed |
| `SecretNotFound` | Provider Secret is missing or could not be read |
| `SecretKeyMissing` | A required provider key is absent or blank |
| `AuthenticationFailed` | Credential PUT returned HTTP 401 |
| `InsufficientPrivilege` | Credential PUT returned HTTP 403; fix privileges and trigger another reconcile |
| `InvalidPayload` | Credential PUT returned HTTP 400 |
| `ManagementAPIUnreachable` | Credential PUT had a transport error or HTTP 5xx |
| `ManagementAPIError` | Unexpected credential PUT result |

Failures during the earlier host-readiness probe are reported as `BootstrapNotReady`, so
an invalid admin credential can appear there before a credential PUT is attempted.
See the [functional spec](%5BSPEC%5DObject%20Storage.md) and
[research log](%5BSTEPS%5D%20Object%20Storage.md) for the contract and evidence limits.

## Offline checks

```bash
python3 -m unittest discover -s docs/spec/object-storage/demo -p 'test_*.py' -v
for script in docs/spec/object-storage/demo/*.sh; do bash -n "$script" || exit; done
```

These tests use local command stubs; they do not provision infrastructure or validate live cloud access.
