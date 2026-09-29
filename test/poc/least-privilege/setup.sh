#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

for command_name in kubectl helm curl jq openssl docker rdctl; do
  require_command "${command_name}"
done
assert_context

printf 'Context: %s\n' "$(kubectl config current-context)"
kubectl version
helm version --short
kubectl get nodes -o wide
kubectl get storageclass

node_architecture="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')"
if [[ "${node_architecture}" != "arm64" ]]; then
  printf 'This harness currently expects the Rancher Desktop arm64 node; found %s.\n' "${node_architecture}" >&2
  exit 1
fi
if [[ "$(rdctl list-settings | jq -r '.virtualMachine.useRosetta')" != "true" ]]; then
  printf 'Rancher Desktop Rosetta support must be enabled for the amd64 MarkLogic image.\n' >&2
  exit 1
fi

if ! kubectl get storageclass -o json | jq -e '.items[] | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true" or .metadata.annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true")' >/dev/null; then
  printf 'No default StorageClass is configured.\n' >&2
  exit 1
fi

kubectl create namespace "${OPERATOR_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

docker build --platform linux/arm64 --tag marklogic-operator-kubernetes:ml-lp-arm64 "${REPO_ROOT}"

helm upgrade --install "${RELEASE_NAME}" "${REPO_ROOT}/charts/marklogic-operator-kubernetes" \
  --namespace "${OPERATOR_NAMESPACE}" \
  --values "${SCRIPT_DIR}/values.yaml" \
  --wait \
  --timeout 5m

if ! kubectl get secret "${ADMIN_SECRET}" -n "${TEST_NAMESPACE}" >/dev/null 2>&1; then
  if kubectl get "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" >/dev/null 2>&1; then
    printf 'The cluster exists but bootstrap Secret %s is missing; setup cannot recover its password.\n' "${ADMIN_SECRET}" >&2
    exit 1
  fi
  admin_password="${ADMIN_PASSWORD:-$(openssl rand -base64 30 | tr -d '\n')}"
  kubectl create secret generic "${ADMIN_SECRET}" \
    --namespace "${TEST_NAMESPACE}" \
    --from-literal=username="${ADMIN_USERNAME:-admin}" \
    --from-literal=password="${admin_password}"
  unset admin_password
fi

kubectl apply -f "${SCRIPT_DIR}/cluster.yaml"
wait_for_cluster_ready
kubectl wait pod -n "${TEST_NAMESPACE}" \
  -l "app.kubernetes.io/name=marklogic,app.kubernetes.io/instance=${GROUP_NAME}" \
  --for=condition=Ready --timeout="${WAIT_TIMEOUT}"

trap stop_manage_port_forward EXIT
start_manage_port_forward

admin_username="$(secret_value "${ADMIN_SECRET}" "${TEST_NAMESPACE}" username)"
admin_password="$(secret_value "${ADMIN_SECRET}" "${TEST_NAMESPACE}" password)"

role_status="$(curl --silent --output /dev/null --write-out '%{http_code}' "${manage_curl_args[@]}" --anyauth \
  --user "${admin_username}:${admin_password}" \
  "${MANAGE_SCHEME}://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2/roles/${OPERATOR_ROLE}?format=json")"
if [[ "${role_status}" == "404" ]]; then
  manage_request "${admin_username}" "${admin_password}" POST '/manage/v2/roles' "${SCRIPT_DIR}/role.json" >/dev/null
elif [[ "${role_status}" == "200" ]]; then
  manage_request "${admin_username}" "${admin_password}" PUT "/manage/v2/roles/${OPERATOR_ROLE}/properties" "${SCRIPT_DIR}/role.json" >/dev/null
else
  printf 'Unexpected role lookup status: %s\n' "${role_status}" >&2
  exit 1
fi

user_payload="$(mktemp)"
trap 'rm -f "${user_payload}" "${user_payload}.update"; stop_manage_port_forward' EXIT

user_status="$(curl --silent --output /dev/null --write-out '%{http_code}' "${manage_curl_args[@]}" --anyauth \
  --user "${admin_username}:${admin_password}" \
  "${MANAGE_SCHEME}://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2/users/${OPERATOR_USER}?format=json")"
if [[ "${user_status}" == "404" ]]; then
  operator_password="$(openssl rand -base64 30 | tr -d '\n')"
  jq --null-input \
    --arg username "${OPERATOR_USER}" \
    --arg password "${operator_password}" \
    --arg role "${OPERATOR_ROLE}" \
    '{"user-name": $username, "description": "Dedicated service account for the MarkLogic Kubernetes Operator", "password": $password, "role": [$role]}' >"${user_payload}"
  manage_request "${admin_username}" "${admin_password}" POST '/manage/v2/users' "${user_payload}" >/dev/null
elif [[ "${user_status}" == "200" ]]; then
  if ! kubectl get secret "${OPERATOR_SECRET}" -n "${TEST_NAMESPACE}" >/dev/null 2>&1; then
    printf 'Operator user %s exists but Secret %s is missing; setup cannot recover its password.\n' "${OPERATOR_USER}" "${OPERATOR_SECRET}" >&2
    exit 1
  fi
  operator_password="$(secret_value "${OPERATOR_SECRET}" "${TEST_NAMESPACE}" password)"
  jq --null-input \
    --arg username "${OPERATOR_USER}" \
    --arg role "${OPERATOR_ROLE}" \
    '{"user-name": $username, "description": "Dedicated service account for the MarkLogic Kubernetes Operator", "role": [$role]}' >"${user_payload}.update"
  manage_request "${admin_username}" "${admin_password}" PUT "/manage/v2/users/${OPERATOR_USER}/properties" "${user_payload}.update" >/dev/null
else
  printf 'Unexpected user lookup status: %s\n' "${user_status}" >&2
  exit 1
fi

kubectl create secret generic "${OPERATOR_SECRET}" \
  --namespace "${TEST_NAMESPACE}" \
  --from-literal=username="${OPERATOR_USER}" \
  --from-literal=password="${operator_password}" \
  --dry-run=client -o yaml | kubectl apply -f -

unset admin_username admin_password operator_password

kubectl patch "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" --type=merge \
  -p "{\"spec\":{\"auth\":{\"secretName\":\"${OPERATOR_SECRET}\"}}}"
wait_for_cluster_ready

printf 'Credential handoff applied. The bootstrap Secret is retained for recovery.\n'