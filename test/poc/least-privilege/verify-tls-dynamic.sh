#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

for command_name in kubectl curl jq base64; do
  require_command "${command_name}"
done
assert_context

DYNAMIC_GROUP="${DYNAMIC_GROUP:-dynamic}"
DYNAMIC_SECRET="${DYNAMIC_SECRET:-${CLUSTER_NAME}-manage-admin}"
evidence_dir="${EVIDENCE_DIR:-${SCRIPT_DIR}/evidence}"
mkdir -p "${evidence_dir}"
results_file="${evidence_dir}/tls-dynamic-results.tsv"
printf 'test\texpected\tactual\tresult\n' >"${results_file}"

record_result() {
  local test_name="$1"
  local expected="$2"
  local actual="$3"
  local result="$4"
  printf '%s\t%s\t%s\t%s\n' "${test_name}" "${expected}" "${actual}" "${result}" | tee -a "${results_file}"
}

dynamic_group_index() {
  kubectl get "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" -o json |
    jq -er --arg group "${DYNAMIC_GROUP}" '.spec.markLogicGroups | to_entries[] | select(.value.name == $group) | .key'
}

set_dynamic_replicas() {
  local replicas="$1"
  local index
  index="$(dynamic_group_index)"
  kubectl patch "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/spec/markLogicGroups/${index}/replicas\",\"value\":${replicas}}]" >/dev/null
}

wait_for_dynamic_idle() {
  local expected_replicas="$1"
  local group_json phase reason message
  for _ in {1..180}; do
    group_json="$(kubectl get "marklogicgroup/${DYNAMIC_GROUP}" -n "${TEST_NAMESPACE}" -o json 2>/dev/null || true)"
    if [[ -n "${group_json}" ]] && jq -e --argjson expected "${expected_replicas}" '
      .status.dynamic.phase == "Idle" and
      .status.dynamic.dynamicHostsEnabled == true and
      .status.dynamic.desiredReplicas == $expected and
      .status.dynamic.localReadyReplicas == $expected and
      .status.dynamic.readyReplicas == $expected and
      ((.status.dynamic.hosts // []) | length == $expected) and
      all((.status.dynamic.hosts // [])[]; (.hostId // "") != "" and (.state == "joined" or .state == "rejoined"))
    ' <<<"${group_json}" >/dev/null; then
      printf '%s\n' "${group_json}"
      return 0
    fi
    phase="$(jq -r '.status.dynamic.phase // "Pending"' <<<"${group_json:-{}}" 2>/dev/null || true)"
    if [[ "${phase}" == "Failed" ]]; then
      reason="$(jq -r '.status.dynamic.reason // "unknown"' <<<"${group_json}")"
      message="$(jq -r '.status.dynamic.message // ""' <<<"${group_json}")"
      printf 'Dynamic reconciliation failed: %s: %s\n' "${reason}" "${message}" >&2
      return 1
    fi
    sleep 5
  done
  kubectl get "marklogicgroup/${DYNAMIC_GROUP}" -n "${TEST_NAMESPACE}" -o yaml >&2 || true
  return 1
}

original_replicas="$(kubectl get "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" -o json |
  jq -er --arg group "${DYNAMIC_GROUP}" '.spec.markLogicGroups[] | select(.name == $group) | .replicas')"
restore_needed=false
cleanup() {
  stop_manage_port_forward
  if [[ "${restore_needed}" == "true" ]]; then
    set_dynamic_replicas "${original_replicas}" || true
  fi
}
trap cleanup EXIT

if [[ "$(kubectl get "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.tls.enableOnDefaultAppServers}')" != "true" ]]; then
  record_result tls-enabled true false FAIL
  exit 1
fi
record_result tls-enabled true true PASS

start_manage_port_forward
operator_username="$(secret_value "${OPERATOR_SECRET}" "${TEST_NAMESPACE}" username)"
operator_password="$(secret_value "${OPERATOR_SECRET}" "${TEST_NAMESPACE}" password)"
https_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --insecure --anyauth \
  --user "${operator_username}:${operator_password}" \
  "https://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2/hosts?view=status&format=json")"
if [[ "${https_status}" != "200" ]]; then
  record_result tls-manage-api 200 "${https_status}" FAIL
  exit 1
fi
record_result tls-manage-api 200 "${https_status}" PASS

http_status="$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 \
  "http://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2" || true)"
if [[ "${http_status}" == "200" ]]; then
  record_result plaintext-manage-api non-200 "${http_status}" FAIL
  exit 1
fi
record_result plaintext-manage-api non-200 "${http_status:-000}" PASS
stop_manage_port_forward
PORT_FORWARD_PID=''
unset operator_username operator_password

kubectl wait --for=create "secret/${DYNAMIC_SECRET}" -n "${TEST_NAMESPACE}" --timeout="${WAIT_TIMEOUT}" >/dev/null
dynamic_username="$(secret_value "${DYNAMIC_SECRET}" "${TEST_NAMESPACE}" username)"
if [[ "${dynamic_username}" != "${CLUSTER_NAME}-manage-admin" ]]; then
  record_result dynamic-credential-user "${CLUSTER_NAME}-manage-admin" "${dynamic_username}" FAIL
  exit 1
fi
record_result dynamic-credential-user "${CLUSTER_NAME}-manage-admin" "${dynamic_username}" PASS
unset dynamic_username

initial_status="$(wait_for_dynamic_idle "${original_replicas}")"
record_result dynamic-initial-state "Idle/${original_replicas}" \
  "$(jq -r '.status.dynamic.phase + "/" + (.status.dynamic.readyReplicas | tostring)' <<<"${initial_status}")" PASS

if kubectl get pvc -n "${TEST_NAMESPACE}" -l "app.kubernetes.io/instance=${DYNAMIC_GROUP}" -o json |
  jq -e '.items | length > 0' >/dev/null; then
  record_result dynamic-storage ephemeral pvc-present FAIL
  exit 1
fi
record_result dynamic-storage ephemeral no-pvc PASS

restore_needed=true
set_dynamic_replicas 2
kubectl wait --for=create "pod/${DYNAMIC_GROUP}-1" -n "${TEST_NAMESPACE}" --timeout="${WAIT_TIMEOUT}" >/dev/null
kubectl wait "pod/${DYNAMIC_GROUP}-1" -n "${TEST_NAMESPACE}" --for=condition=Ready --timeout="${WAIT_TIMEOUT}" >/dev/null
scaled_status="$(wait_for_dynamic_idle 2)"
record_result dynamic-scale-up "Idle/2" \
  "$(jq -r '.status.dynamic.phase + "/" + (.status.dynamic.readyReplicas | tostring)' <<<"${scaled_status}")" PASS

set_dynamic_replicas 1
kubectl wait --for=delete "pod/${DYNAMIC_GROUP}-1" -n "${TEST_NAMESPACE}" --timeout="${WAIT_TIMEOUT}" >/dev/null
final_status="$(wait_for_dynamic_idle 1)"
record_result dynamic-scale-down "Idle/1" \
  "$(jq -r '.status.dynamic.phase + "/" + (.status.dynamic.readyReplicas | tostring)' <<<"${final_status}")" PASS
restore_needed=false

jq 'del(.metadata.managedFields)' <<<"${final_status}" >"${evidence_dir}/dynamic-group-redacted.json"
kubectl get events -n "${TEST_NAMESPACE}" --field-selector "involvedObject.name=${DYNAMIC_GROUP}" \
  --sort-by=.metadata.creationTimestamp >"${evidence_dir}/dynamic-events.txt"

printf 'TLS and dynamic-host verification complete. Evidence: %s\n' "${evidence_dir}"
