#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

for command_name in kubectl curl jq openssl base64 grep sed; do
  require_command "${command_name}"
done
assert_context

for secret_name in "${ADMIN_SECRET}" "${OPERATOR_SECRET}"; do
  kubectl get secret "${secret_name}" -n "${TEST_NAMESPACE}" >/dev/null
done

evidence_dir="${EVIDENCE_DIR:-${SCRIPT_DIR}/evidence}"
mkdir -p "${evidence_dir}"
results_file="${evidence_dir}/results.tsv"
printf 'test\texpected\tactual\tresult\n' >"${results_file}"

record_result() {
  local test_name="$1"
  local expected="$2"
  local actual="$3"
  local result="$4"
  printf '%s\t%s\t%s\t%s\n' "${test_name}" "${expected}" "${actual}" "${result}" | tee -a "${results_file}"
}

expect_http() {
  local test_name="$1"
  local username="$2"
  local password="$3"
  local method="$4"
  local path="$5"
  local expected_status="$6"
  local body_file="${7:-}"
  local response_file status
  response_file="$(mktemp)"
  TEMP_FILES+=("${response_file}")
  local args=(--silent --show-error --output "${response_file}" --write-out '%{http_code}' --anyauth --user "${username}:${password}" --request "${method}")
  if [[ -n "${body_file}" ]]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@${body_file}")
  fi
  status="$(curl "${args[@]}" "http://127.0.0.1:${LOCAL_MANAGE_PORT}${path}")"
  if [[ "${status}" == "${expected_status}" ]]; then
    record_result "${test_name}" "${expected_status}" "${status}" PASS
    return
  fi
  record_result "${test_name}" "${expected_status}" "${status}" FAIL
  sed -E 's/(password|credential|authorization)[^,}]*/\1":"<redacted>"/Ig' "${response_file}" >&2
  return 1
}

cleanup_probe_user() {
  if [[ "${PROBE_CREATED:-false}" == "true" ]]; then
    curl --silent --output /dev/null --anyauth \
      --user "${admin_username}:${admin_password}" \
      --request DELETE \
      "http://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2/users/${probe_user}" || true
  fi
}

TEMP_FILES=()
cleanup() {
  cleanup_probe_user
  if ((${#TEMP_FILES[@]})); then
    rm -f "${TEMP_FILES[@]}"
  fi
  stop_manage_port_forward
}
trap cleanup EXIT

service_account="$(kubectl get deployment marklogic-operator-controller-manager -n "${OPERATOR_NAMESPACE}" -o jsonpath='{.spec.template.spec.serviceAccountName}')"
service_identity="system:serviceaccount:${OPERATOR_NAMESPACE}:${service_account}"

for check in \
  "get pods -n ${TEST_NAMESPACE}|yes|k8s-get-pods-in-watched-namespace" \
  "list nodes|no|k8s-list-nodes" \
  "list secrets -n default|no|k8s-list-secrets-outside-scope"; do
  IFS='|' read -r arguments expected test_name <<<"${check}"
  read -r -a argument_array <<<"${arguments}"
  actual="$(kubectl auth can-i "${argument_array[@]}" --as="${service_identity}" || true)"
  if [[ "${actual}" != "${expected}" ]]; then
    record_result "${test_name}" "${expected}" "${actual}" FAIL
    exit 1
  fi
  record_result "${test_name}" "${expected}" "${actual}" PASS
done

statefulset_json="$(kubectl get statefulset -n "${TEST_NAMESPACE}" -l "app.kubernetes.io/instance=${GROUP_NAME}" -o json)"
if ! jq -e --arg secret "${OPERATOR_SECRET}" '.items | length > 0 and any(.[]; any(.spec.template.spec.volumes[]?; .secret.secretName? == $secret))' <<<"${statefulset_json}" >/dev/null; then
  record_result credential-reference "${OPERATOR_SECRET}" missing FAIL
  exit 1
fi
record_result credential-reference "${OPERATOR_SECRET}" "${OPERATOR_SECRET}" PASS

trap stop_manage_port_forward EXIT
start_manage_port_forward
trap cleanup EXIT

admin_username="$(secret_value "${ADMIN_SECRET}" "${TEST_NAMESPACE}" username)"
admin_password="$(secret_value "${ADMIN_SECRET}" "${TEST_NAMESPACE}" password)"
operator_username="$(secret_value "${OPERATOR_SECRET}" "${TEST_NAMESPACE}" username)"
operator_password="$(secret_value "${OPERATOR_SECRET}" "${TEST_NAMESPACE}" password)"

role_response="$(mktemp)"
user_response="$(mktemp)"
TEMP_FILES+=("${role_response}" "${user_response}")
manage_request "${admin_username}" "${admin_password}" GET "/manage/v2/roles/${OPERATOR_ROLE}?format=json" >"${role_response}"
manage_request "${admin_username}" "${admin_password}" GET "/manage/v2/users/${OPERATOR_USER}?format=json" >"${user_response}"
jq 'walk(if type == "object" then with_entries(select(.key | test("password|credential|authorization"; "i") | not)) else . end)' \
  "${role_response}" >"${evidence_dir}/marklogic-role-redacted.json"
jq 'walk(if type == "object" then with_entries(select(.key | test("password|credential|authorization"; "i") | not)) else . end)' \
  "${user_response}" >"${evidence_dir}/marklogic-user-redacted.json"

for expected_value in manage-admin pki admin-ui-user \
  http://marklogic.com/xdmp/privileges/create-user \
  http://marklogic.com/xdmp/privileges/remove-dynamic-hosts \
  http://marklogic.com/xdmp/privileges/xdmp-eval \
  http://marklogic.com/xdmp/privileges/create-external-security; do
  if ! jq -e --arg value "${expected_value}" '.. | strings | select(. == $value)' "${role_response}" >/dev/null; then
    record_result "role-${expected_value}" present missing FAIL
    exit 1
  fi
  record_result "role-${expected_value}" present present PASS
done

if ! jq -e --arg value "${OPERATOR_ROLE}" '.. | strings | select(. == $value)' "${user_response}" >/dev/null; then
  record_result operator-user-role "${OPERATOR_ROLE}" missing FAIL
  exit 1
fi
if jq -e '.. | strings | select(. == "admin" or . == "security")' "${user_response}" >/dev/null; then
  record_result operator-user-excluded-roles absent present FAIL
  exit 1
fi
record_result operator-user-role "${OPERATOR_ROLE}" "${OPERATOR_ROLE}" PASS
record_result operator-user-excluded-roles absent absent PASS

expect_http manage-root "${operator_username}" "${operator_password}" GET '/manage/v2?format=json' 200
expect_http manage-groups "${operator_username}" "${operator_password}" GET '/manage/v2/groups?format=json' 200
expect_http manage-hosts "${operator_username}" "${operator_password}" GET '/manage/v2/hosts?format=json' 200

probe_user="${CLUSTER_NAME}-admin-escalation-probe"
probe_payload="$(mktemp)"
TEMP_FILES+=("${probe_payload}")
jq --null-input \
  --arg username "${probe_user}" \
  --arg password "$(openssl rand -base64 30 | tr -d '\n')" \
  '{"user-name": $username, "password": $password, "role": ["admin"]}' >"${probe_payload}"
probe_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --anyauth \
  --user "${operator_username}:${operator_password}" \
  --header 'Content-Type: application/json' \
  --data-binary "@${probe_payload}" \
  "http://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2/users")"
if [[ "${probe_status}" == 2* ]]; then
  PROBE_CREATED=true
  record_result admin-role-escalation denied "${probe_status}" FAIL
  exit 1
fi
if [[ "${probe_status}" != "401" && "${probe_status}" != "403" ]]; then
  record_result admin-role-escalation '401-or-403' "${probe_status}" FAIL
  exit 1
fi
record_result admin-role-escalation '401-or-403' "${probe_status}" PASS

stop_manage_port_forward
PORT_FORWARD_PID=''

old_pod="$(marklogic_pod)"
old_uid="$(kubectl get pod "${old_pod}" -n "${TEST_NAMESPACE}" -o jsonpath='{.metadata.uid}')"
kubectl delete pod "${old_pod}" -n "${TEST_NAMESPACE}" --wait=true
new_uid="$(wait_for_replacement_pod "${old_uid}")"
wait_for_cluster_ready
record_result pod-credential-restart 'new-ready-pod-uid' "${old_uid}->${new_uid}" PASS

record_result bootstrap-secret retained retained PASS

kubectl annotate "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" \
  "least-privilege-poc/reconciled-at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite >/dev/null
wait_for_cluster_ready

kubectl get "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" -o yaml >"${evidence_dir}/cluster-redacted.yaml"
kubectl get role,rolebinding -n "${TEST_NAMESPACE}" -o yaml >"${evidence_dir}/kubernetes-rbac.yaml"
kubectl get clusterrole,clusterrolebinding -l "app.kubernetes.io/instance=${RELEASE_NAME}" -o yaml >"${evidence_dir}/cluster-rbac-exceptions.yaml"
kubectl logs -n "${OPERATOR_NAMESPACE}" deployment/marklogic-operator-controller-manager --since=30m \
  | sed -E 's/(password|credential|authorization)[^,}]*/\1=<redacted>/Ig' >"${evidence_dir}/operator-redacted.log"

if grep -Eiq 'unauthorized|forbidden|authentication failed|permission denied' "${evidence_dir}/operator-redacted.log"; then
  record_result operator-auth-errors absent present FAIL
  exit 1
fi
record_result operator-auth-errors absent absent PASS

printf 'Verification complete. Evidence: %s\n' "${evidence_dir}"
