#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

EXPECTED_CONTEXT="${EXPECTED_CONTEXT:-rancher-desktop}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-ml-lp-operator}"
TEST_NAMESPACE="${TEST_NAMESPACE:-ml-lp-test}"
RELEASE_NAME="${RELEASE_NAME:-ml-lp-operator}"
CLUSTER_NAME="${CLUSTER_NAME:-least-privilege}"
GROUP_NAME="${GROUP_NAME:-node}"
ADMIN_SECRET="${ADMIN_SECRET:-least-privilege-admin}"
OPERATOR_SECRET="${OPERATOR_SECRET:-least-privilege-operator}"
OPERATOR_ROLE="${OPERATOR_ROLE:-marklogic-operator}"
OPERATOR_USER="${OPERATOR_USER:-${CLUSTER_NAME}-operator}"
LOCAL_MANAGE_PORT="${LOCAL_MANAGE_PORT:-18002}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-20m}"

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    printf 'Required command not found: %s\n' "$1" >&2
    exit 1
  }
}

assert_context() {
  local current_context
  current_context="$(kubectl config current-context)"
  if [[ "${ALLOW_CONTEXT_OVERRIDE:-false}" != "true" && "${current_context}" != "${EXPECTED_CONTEXT}" ]]; then
    printf 'Refusing to continue: current context is %s, expected %s.\n' "${current_context}" "${EXPECTED_CONTEXT}" >&2
    printf 'Set ALLOW_CONTEXT_OVERRIDE=true only after verifying the target cluster.\n' >&2
    exit 1
  fi
}

secret_value() {
  kubectl get secret "$1" -n "$2" -o "jsonpath={.data.$3}" | base64 --decode
}

wait_for_cluster_ready() {
  kubectl wait marklogicgroup --all -n "${TEST_NAMESPACE}" --for=condition=Ready --timeout="${WAIT_TIMEOUT}"
}

marklogic_pod() {
  kubectl get pods -n "${TEST_NAMESPACE}" \
    -l "app.kubernetes.io/name=marklogic,app.kubernetes.io/instance=${GROUP_NAME}" \
    -o jsonpath='{.items[0].metadata.name}'
}

wait_for_replacement_pod() {
  local old_uid="$1"
  local pod_name new_uid
  for _ in {1..120}; do
    pod_name="$(marklogic_pod 2>/dev/null || true)"
    if [[ -n "${pod_name}" ]]; then
      new_uid="$(kubectl get pod "${pod_name}" -n "${TEST_NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      if [[ -n "${new_uid}" && "${new_uid}" != "${old_uid}" ]]; then
        kubectl wait "pod/${pod_name}" -n "${TEST_NAMESPACE}" --for=condition=Ready --timeout="${WAIT_TIMEOUT}" >/dev/null
        printf '%s\n' "${new_uid}"
        return
      fi
    fi
    sleep 2
  done
  printf 'Timed out waiting for a replacement MarkLogic pod.\n' >&2
  exit 1
}

start_manage_port_forward() {
  local pod_name
  pod_name="$(marklogic_pod)"
  kubectl port-forward -n "${TEST_NAMESPACE}" "pod/${pod_name}" "${LOCAL_MANAGE_PORT}:8002" >"${TMPDIR:-/tmp}/ml-lp-port-forward.log" 2>&1 &
  PORT_FORWARD_PID=$!
  export PORT_FORWARD_PID
  for _ in {1..30}; do
    if curl --silent --output /dev/null "http://127.0.0.1:${LOCAL_MANAGE_PORT}/manage/v2"; then
      return
    fi
    if ! kill -0 "${PORT_FORWARD_PID}" 2>/dev/null; then
      cat "${TMPDIR:-/tmp}/ml-lp-port-forward.log" >&2
      exit 1
    fi
    sleep 1
  done
  printf 'Timed out waiting for the Management API port-forward.\n' >&2
  exit 1
}

stop_manage_port_forward() {
  if [[ -n "${PORT_FORWARD_PID:-}" ]] && kill -0 "${PORT_FORWARD_PID}" 2>/dev/null; then
    kill "${PORT_FORWARD_PID}"
    wait "${PORT_FORWARD_PID}" 2>/dev/null || true
  fi
}

manage_request() {
  local username="$1"
  local password="$2"
  local method="$3"
  local path="$4"
  local body_file="${5:-}"
  local args=(--silent --show-error --fail-with-body --anyauth --user "${username}:${password}" --request "${method}")
  if [[ -n "${body_file}" ]]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@${body_file}")
  fi
  curl "${args[@]}" "http://127.0.0.1:${LOCAL_MANAGE_PORT}${path}"
}
