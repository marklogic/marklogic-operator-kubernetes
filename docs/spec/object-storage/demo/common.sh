#!/usr/bin/env bash
# Shared helpers; source this file after setting CONTEXT and NS.
# Pin every request so simultaneous EKS/AKS terminals cannot switch each other's target.
kube() { command kubectl --context "$CONTEXT" "$@"; }

compute_source_hash() {
  python3 - <<'PY'
import hashlib
from pathlib import Path
h = hashlib.sha256()
for root in ('api', 'cmd', 'internal', 'pkg', 'config', 'Dockerfile', '.dockerignore', 'Makefile', 'go.mod', 'go.sum'):
    p = Path(root)
    for f in sorted(p.rglob('*') if p.is_dir() else [p]):
        if f.is_file():
            for value in (str(f).encode(), f.read_bytes()):
                h.update(len(value).to_bytes(8, 'big'))
                h.update(value)
print(h.hexdigest())
PY
}

# Values arrive through environment variables, never process arguments or stdout.
# Server-side apply avoids a second copy in the last-applied annotation.
apply_credential_secret() {
  python3 - "$NS" "$SECRET_NAME" "$@" <<'PY' | kube apply --server-side --field-manager=object-storage-demo -f -
import base64, json, os, sys
values = {}
for mapping in sys.argv[3:]:
    key, variable = mapping.split('=', 1)
    value = os.environ.get(variable, '')
    if value:
        values[key] = base64.b64encode(value.encode()).decode()
print(json.dumps({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {
    'name': sys.argv[2], 'namespace': sys.argv[1]}, 'type': 'Opaque', 'data': values}))
PY
}

# Require the fingerprint of the current Secret, so an old Applied status cannot pass.
wait_applied() {
  local provider="$1" secret_name="$2" expected status
  expected=$(kube -n "$NS" get secret "$secret_name" -o json | python3 -c '
import base64, hashlib, hmac, json, sys
uid, provider = sys.argv[1:]
secret = json.load(sys.stdin)["data"]
keys = ("accessKey", "secretKey", "sessionToken") if provider == "aws" else ("storageAccount", "storageKey")
h = hmac.new(uid.encode(), digestmod=hashlib.sha256)
for value in [provider] + [v for k in sorted(keys) for v in (k, base64.b64decode(secret.get(k, "")).decode().strip())]:
    b = value.encode(); h.update(len(b).to_bytes(8, "big")); h.update(b)
print(h.hexdigest())
' "$(kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o jsonpath='{.metadata.uid}')" "$provider")
  for ((attempt=0; attempt<60; attempt++)); do
    status=$(kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o "jsonpath={.status.objectStorage.${provider}.phase} {.status.objectStorage.${provider}.appliedFingerprint}")
    if [[ "$status" == "Applied $expected" ]]; then
      echo "$provider: $status"
      return 0
    fi
    sleep 2
  done
  echo "Timed out waiting for $provider to apply the current Secret; check status.objectStorage.$provider." >&2
  return 1
}

recreate_demo_cluster() {
  local uid groups pvcs pvc
  uid=$(kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" --ignore-not-found -o jsonpath='{.metadata.uid}')
  [[ -n "$uid" ]] || return 0
  groups=$(kube -n "$NS" get marklogicgroups -o json | python3 -c '
import json, sys
print("\n".join(g["spec"]["name"] for g in json.load(sys.stdin)["items"] if any(o["uid"] == sys.argv[1] for o in g["metadata"].get("ownerReferences", []))))
' "$uid")
  pvcs=$(kube -n "$NS" get pvc -o json | python3 -c '
import json, re, sys
patterns = [re.compile(r"datadir-" + re.escape(g) + r"-\d+$") for g in sys.argv[1].splitlines()]
print("\n".join(p["metadata"]["name"] for p in json.load(sys.stdin)["items"] if any(r.fullmatch(p["metadata"]["name"]) for r in patterns)))
' "$groups")
  echo "Deleting MarklogicCluster/$CLUSTER_NAME and its datadir PVCs (data will be lost)."
  kube -n "$NS" delete marklogiccluster "$CLUSTER_NAME" --cascade=foreground --wait=true --timeout=180s
  while IFS= read -r pvc; do
    [[ -z "$pvc" ]] || kube -n "$NS" delete pvc "$pvc" --ignore-not-found --wait=true --timeout=180s
  done <<< "$pvcs"
}
