# Demo: Configuring Object Storage Credentials (AWS S3 / Azure Blob)

This is a demo-only walkthrough for applying AWS S3 and/or Azure Blob object storage
credentials via the operator. It covers credential configuration only — creating
object-storage-backed forests or backups is a separate, manual MarkLogic admin step
and is out of scope here.

## Prerequisites

- Operator installed and a `MarklogicCluster` already `Ready` (bootstrap host initialized).
- An AWS S3 bucket / Azure Storage account you already have keys for.
- `kubectl` access to the cluster namespace.

## Environment check before you start

The `marklogic-tiered-poc` EKS cluster's currently deployed operator (`...:1.3.1-RC2`)
predates the `objectStorage` feature — its CRD has no `spec.objectStorage` field. Confirm
before continuing:

```bash
kubectl get crd marklogicclusters.marklogic.progress.com -o jsonpath=\
  '{.spec.versions[?(@.name=="v1")].schema.openAPIV3Schema.properties.spec.properties.objectStorage}'
```

If this prints nothing, update the CRD and operator to this repo's code first:

```bash
# from the repo root
export IMG=308453789681.dkr.ecr.us-west-2.amazonaws.com/marklogic-tiered-poc/marklogic-operator:objectstorage-demo
make docker-build docker-push IMG=$IMG
make install                 # applies the updated CRDs (adds spec.objectStorage)
make deploy IMG=$IMG          # redeploys the controller with objectStorage support
kubectl -n marklogic-operator-system rollout status deploy/marklogic-operator-controller-manager
```

## Already provisioned for this demo (do not redo)

- S3 bucket: `pzhou-k8s-test` (us-west-2, public access blocked)
- IAM user `marklogic-objstore-demo` with an inline policy scoped to only that bucket
- Kubernetes Secret `ml-s3-credentials` already created in the `marklogic` namespace with `accessKey`/`secretKey`
- Existing cluster to edit: `MarklogicCluster/ml-tiered-poc` in namespace `marklogic`

## Part 1: AWS S3 credentials

1. Patch the existing cluster to add the `objectStorage.aws` block (region is `us-west-2` to match the bucket):

   ```bash
   kubectl -n marklogic patch marklogiccluster ml-tiered-poc --type merge -p '
   spec:
     objectStorage:
       aws:
         authType: secret
         secretName: ml-s3-credentials
         region: us-west-2
   '
   ```

2. Watch the operator apply it:

   ```bash
   kubectl -n marklogic get marklogiccluster ml-tiered-poc -o jsonpath='{.status.objectStorage.aws}' | jq
   ```

   Expect `phase: Applied` and an `appliedFingerprint` (a hash, never the actual key material).

3. (Optional) Confirm at the MarkLogic Management API level, from inside the cluster or via port-forward:

   ```bash
   kubectl -n marklogic port-forward svc/ml-tiered-poc-node 8002:8002 &
   curl -s -u admin:<bootstrap-password> \
     "http://localhost:8002/manage/v2/credentials/properties?type=aws&format=json" | jq
   ```

   Expect `access-key` in plaintext and `secret-key` as an opaque ciphertext blob — never the raw secret value.

## Part 2: Azure Blob credentials (only if you want both providers in the demo)

1. Create a Secret with the two required keys — `storageAccount` and `storageKey`:

   ```bash
   kubectl create secret generic ml-azure-credentials \
     --from-literal=storageAccount=mystorageacct \
     --from-literal=storageKey=base64key==
   ```

2. Add the `objectStorage.azure` block:

   ```yaml
   spec:
     objectStorage:
       azure:
         authType: secret       # only 'secret' is supported in this release
         secretName: ml-azure-credentials
   ```

   ```bash
   kubectl apply -f marklogiccluster.yaml
   ```

3. Check status the same way:

   ```bash
   kubectl get marklogiccluster <name> -o jsonpath='{.status.objectStorage.azure}' | jq
   ```

## Demo: rotation is automatic

Update either Secret's value and re-apply — no edit to the `MarklogicCluster` is needed.
`generate-updated-secret.sh` below rebuilds `ml-s3-credentials` from whatever
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (and optional `AWS_SESSION_TOKEN`) are
currently in your shell env, so you can point it at a rotated IAM key or a real STS
temporary credential:

```bash
#!/usr/bin/env bash
# generate-updated-secret.sh — rebuild and re-apply ml-s3-credentials from env vars.
set -euo pipefail

: "${AWS_ACCESS_KEY_ID:?export the new access key first}"
: "${AWS_SECRET_ACCESS_KEY:?export the new secret key first}"

args=(
  --from-literal=accessKey="$AWS_ACCESS_KEY_ID"
  --from-literal=secretKey="$AWS_SECRET_ACCESS_KEY"
)
# sessionToken is optional — omit AWS_SESSION_TOKEN to rotate a plain IAM key.
if [[ -n "${AWS_SESSION_TOKEN:-}" ]]; then
  args+=(--from-literal=sessionToken="$AWS_SESSION_TOKEN")
fi

kubectl -n marklogic create secret generic ml-s3-credentials "${args[@]}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Run it directly with a rotated long-lived key:

```bash
AWS_ACCESS_KEY_ID=AKIA... AWS_SECRET_ACCESS_KEY=NEWKEY... ./generate-updated-secret.sh
```

Or demo the opt-in `sessionToken` path with a real STS temporary credential:

```bash
eval "$(aws sts get-session-token --duration-seconds 900 --query \
  'Credentials.[join(`=`,[`export AWS_ACCESS_KEY_ID`,AccessKeyId]),join(`=`,[`export AWS_SECRET_ACCESS_KEY`,SecretAccessKey]),join(`=`,[`export AWS_SESSION_TOKEN`,SessionToken])]' \
  --output text)"
./generate-updated-secret.sh
```

The operator detects the change (fingerprint differs) and re-applies automatically;
`status.objectStorage.aws.lastAppliedTime` updates. With a session token present, the
Management API payload now includes `session-token` — visible in plaintext if you repeat
the Part 1 step 3 `curl`, which is the accepted opt-in tradeoff (see [SPEC]Object Storage.md,
Security NFR #5).

## If something goes wrong

Check `status.objectStorage.<provider>.reason` for a machine-readable cause:

| Reason | Likely cause |
|---|---|
| `SecretNotFound` | `secretName` doesn't exist in the cluster's namespace |
| `SecretKeyMissing` | Secret exists but is missing `accessKey`/`secretKey` (AWS) or `storageAccount`/`storageKey` (Azure) |
| `AuthenticationFailed` | Bootstrap admin credential is invalid |
| `InsufficientPrivilege` | Bootstrap user lacks `manage-admin`+`security` (or `credentials-set-{aws,azure}`) |
| `ManagementAPIUnreachable` | Bootstrap host not reachable yet |

`status.objectStorage.<provider>.message` never contains secret material — safe to paste into a ticket.

## More demo scenarios

### Correctness / value
- Configure both AWS and Azure, then run an actual MarkLogic backup to S3/Azure using the applied credentials — proves the real payoff, not just a green status.
- Break AWS only (bad key) while Azure stays healthy: show `status.objectStorage.aws.phase=Failed` next to `status.objectStorage.azure.phase=Applied` at the same time — proves provider independence.

### Operational / GitOps
- Idempotency: change something unrelated (e.g. replica count) and show `status.objectStorage.*.lastAppliedTime` does **not** move — proves it isn't re-applying every reconcile.
- Remove a provider block (e.g. delete `objectStorage.azure`): show `phase: Disabled`, and call out that MarkLogic still has those credentials active — the operator does not revoke on removal.

### Failure/troubleshooting
- Delete the Secret after it was successfully applied → next reconcile flips to `SecretNotFound`.
- Bootstrap admin without `manage-admin`+`security` → `InsufficientPrivilege`.
- Pair a failure with `kubectl describe`/controller logs to show no secret material leaks even on the error path.

### Security/validation
- Try `authType: instanceRole` (AWS) or `managedIdentity` (Azure) in the spec — CRD CEL validation rejects it immediately with an explanatory message, before it ever reaches the controller.

### Cluster-wide behavior
- On a multi-host cluster, apply credentials once, then `curl` `/manage/v2/credentials/properties` from a **non-bootstrap** host to show it's visible cluster-wide, not per-pod.

## Not covered by this demo

- AWS IRSA / instance-role keyless access — not supported by MarkLogic, do not attempt.
- Azure managed identity — deferred, not available in this release.
- Creating object-storage-backed forests or configuring backups — administrator step after credentials are applied; the operator does not automate this.
