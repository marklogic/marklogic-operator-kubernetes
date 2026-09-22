#!/usr/bin/env bash
# Read credential presence without printing returned values (STS tokens are plaintext).
# Usage: bash verify-credentials.sh <context> <namespace> <cluster> <service> <aws|azure>
set -euo pipefail
CONTEXT="${1:?kube context required}"
NS="${2:?namespace required}"
CLUSTER_NAME="${3:?MarklogicCluster name required}"
SVC="${4:?service name required}"
PROVIDER="${5:?provider required: aws or azure}"
case "$PROVIDER" in aws|azure) ;; *) echo 'provider must be aws or azure' >&2; exit 1;; esac
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

umask 077
VERIFY_DIR=$(mktemp -d)
PF_PID=""
cleanup() {
  if [[ -n "$PF_PID" ]]; then kill "$PF_PID" 2>/dev/null || true; wait "$PF_PID" 2>/dev/null || true; fi
  rm -rf "$VERIFY_DIR"
}
trap cleanup EXIT
CLUSTER_JSON=$(kube -n "$NS" get marklogiccluster "$CLUSTER_NAME" -o json)
ADMIN_SECRET=$(printf '%s' "$CLUSTER_JSON" | python3 -c 'import json,sys; c=json.load(sys.stdin); print((c["spec"].get("auth") or {}).get("secretName") or c["metadata"]["name"]+"-admin")')
SCHEME=$(printf '%s' "$CLUSTER_JSON" | python3 -c 'import json,sys; print("https" if (json.load(sys.stdin)["spec"].get("tls") or {}).get("enableOnDefaultAppServers") else "http")')
# curl config is private and deleted on exit; credentials never appear in curl argv.
kube -n "$NS" get secret "$ADMIN_SECRET" -o json | python3 -c '
import base64,json,sys
s=json.load(sys.stdin)["data"]
u,p=(base64.b64decode(s[k]).decode() for k in ("username","password"))
if not u or not p: raise SystemExit("admin Secret must contain username and password")
print("user = " + json.dumps(u+":"+p))
' > "$VERIFY_DIR/curl.conf"

# Let kubectl allocate a free port; never connect to an unrelated listener on 8002.
command kubectl --context "$CONTEXT" -n "$NS" port-forward --address 127.0.0.1 "svc/$SVC" :8002 >"$VERIFY_DIR/forward.log" 2>&1 &
PF_PID=$!
PORT=""
for ((attempt=0; attempt<30; attempt++)); do
  kill -0 "$PF_PID" 2>/dev/null || { echo 'Port-forward failed' >&2; exit 1; }
  PORT=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) -> 8002$/\1/p' "$VERIFY_DIR/forward.log")
  [[ -z "$PORT" ]] || break
  sleep 1
 done
[[ -n "$PORT" ]] || { echo 'Port-forward timed out' >&2; exit 1; }
TLS_ARGS=(--noproxy "*")
REQUEST_HOST=localhost
REQUEST_PORT="$PORT"
# For operator-issued TLS certificates, supply the issuing CA explicitly.
if [[ "$SCHEME" == https ]]; then
  : "${ML_CA_CERT:?set ML_CA_CERT to the CA certificate for this cluster}"
  : "${ML_MANAGE_HOST:?set ML_MANAGE_HOST to a hostname covered by the server certificate}"
  REQUEST_HOST="$ML_MANAGE_HOST"
  REQUEST_PORT=8002
  TLS_ARGS+=(--cacert "$ML_CA_CERT" --connect-to "$ML_MANAGE_HOST:8002:127.0.0.1:$PORT")
fi
curl --silent --show-error --fail --digest --max-time 30 --config "$VERIFY_DIR/curl.conf" "${TLS_ARGS[@]}" \
  "$SCHEME://$REQUEST_HOST:$REQUEST_PORT/manage/v2/credentials/properties?type=$PROVIDER&format=json" | \
  python3 -c '
import json,sys
provider=sys.argv[1]
try:
    body=json.load(sys.stdin)
except (ValueError, TypeError):
    raise SystemExit("Management API did not return valid JSON")
if provider not in body: raise SystemExit("Management API response is missing the provider")
value=body[provider]
if not isinstance(value, dict): raise SystemExit("Provider has no stored credentials")
keys=("access-key", "secret-key") if provider=="aws" else ("storage-account", "storage-key")
if not all(value.get(k) for k in keys): raise SystemExit("Provider is missing required credentials")
print(json.dumps({"provider":provider, "configured":True, "sessionTokenPresent":bool(value.get("session-token"))}))
' "$PROVIDER"
