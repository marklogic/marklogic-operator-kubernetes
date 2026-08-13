#!/usr/bin/env bash
set -euo pipefail

# True-parallel Minikube e2e runner using isolated Minikube profiles.
#
# Topologies:
# - SHARDS=2: parallel shard A (cluster non-istio) + shard B (helm namespace)
#             optional istio run can execute after parallel phase.
# - SHARDS=3: parallel shard A + shard B + shard C (istio)
#
# This script is designed to run from any directory.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SHARDS="${SHARDS:-3}"
REPEATS="${REPEATS:-1}"
RUN_ISTIO="${RUN_ISTIO:-1}"
RUN_VOLUME_RESIZE="${RUN_VOLUME_RESIZE:-1}"
IMG="${IMG:-marklogic-kubernetes-operator:timing-local}"
E2E_MARKLOGIC_IMAGE_VERSION="${E2E_MARKLOGIC_IMAGE_VERSION:-progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6}"
OUTPUT_ROOT="${OUTPUT_ROOT:-${REPO_ROOT}/test/timing/out}"
FAIL_FAST="${FAIL_FAST:-0}"

# Optional: set to 1 if you need fully separate Minikube homes per shard
# to avoid shared-state contention on some CI hosts.
ISOLATE_MINIKUBE_HOME="${ISOLATE_MINIKUBE_HOME:-0}"

TIMESTAMP="$(date +"%Y%m%d-%H%M%S")"
OUT_DIR="${OUTPUT_ROOT}/minikube-parallel-${TIMESTAMP}"
LOG_DIR="${OUT_DIR}/logs"
CSV_FILE="${OUT_DIR}/timings.csv"

mkdir -p "${LOG_DIR}"

cat > "${CSV_FILE}" <<'EOF'
iteration,shard,phase,seconds,exit_code,log_file
EOF

# Cluster-scoped non-Istio suite set.
CLUSTER_NON_ISTIO_REGEX='^Test(OperatorReady|MarklogicCluster|MlClusterWithEdnode|TlsWithSelfSigned|TlsWithNamedCert|TlsWithMultiNode|HAPorxyPathBaseEnabled|HAPorxWithNoPathBasedDisabled|LogCollectionDisabled|LogCollectionPartialLogs|LogCollectionCustomResources|LogCollectionCustomFilters|MetricsEndpoint|DynamicHostLifecycleClusterScoped|VolumeResizeClusterScoped)$'
CLUSTER_NON_ISTIO_NO_RESIZE_REGEX='^Test(OperatorReady|MarklogicCluster|MlClusterWithEdnode|TlsWithSelfSigned|TlsWithNamedCert|TlsWithMultiNode|HAPorxyPathBaseEnabled|HAPorxWithNoPathBasedDisabled|LogCollectionDisabled|LogCollectionPartialLogs|LogCollectionCustomResources|LogCollectionCustomFilters|MetricsEndpoint|DynamicHostLifecycleClusterScoped)$'
HELM_NO_RESIZE_REGEX='^Test(OperatorReady|MarklogicClusterNamespaceScoped|MlClusterWithEdnode|TlsWithSelfSigned|TlsWithNamedCert|TlsWithMultiNode|HAProxyPathBasedEnabled|HAProxyPathBasedDisabled|LogCollectionDisabled|LogCollectionPartialLogs|LogCollectionCustomResources|LogCollectionCustomFilters|MetricsEndpointInsecure|NamespaceScopedRBAC)$'

log() {
  printf "%s\n" "$*"
}

run_timed_for_shard() {
  local iteration="$1"
  local shard="$2"
  local phase="$3"
  local profile="$4"
  local kubeconfig_path="$5"
  local cmd="$6"

  local log_file="${LOG_DIR}/${iteration}-${shard}-${phase}.log"
  local start_ts end_ts elapsed exit_code minikube_home

  start_ts="$(date +%s)"
  set +e
  (
    cd "${REPO_ROOT}"

    export MINIKUBE_PROFILE="${profile}"
    export KUBECONFIG="${kubeconfig_path}"
    if [[ "${ISOLATE_MINIKUBE_HOME}" == "1" ]]; then
      minikube_home="${OUT_DIR}/minikube-home-${profile}"
      mkdir -p "${minikube_home}"
      export MINIKUBE_HOME="${minikube_home}"
    fi

    bash -lc "${cmd}"
  ) >"${log_file}" 2>&1
  exit_code="$?"
  set -e
  end_ts="$(date +%s)"

  elapsed="$((end_ts - start_ts))"
  printf "%s,%s,%s,%s,%s,%s\n" \
    "${iteration}" "${shard}" "${phase}" "${elapsed}" "${exit_code}" "${log_file}" >> "${CSV_FILE}"

  log "[${iteration}] ${shard}/${phase}: ${elapsed}s (exit=${exit_code})"
  return "${exit_code}"
}

run_cluster_shard() {
  local iteration="$1"
  local shard="$2"
  local profile="$3"
  local kubeconfig_path="$4"
  local cluster_regex

  if [[ "${RUN_VOLUME_RESIZE}" == "1" ]]; then
    cluster_regex="${CLUSTER_NON_ISTIO_REGEX}"
  else
    cluster_regex="${CLUSTER_NON_ISTIO_NO_RESIZE_REGEX}"
  fi

  local failed=0
  if ! run_timed_for_shard "${iteration}" "${shard}" "setup" "${profile}" "${kubeconfig_path}" \
    "make e2e-setup-minikube IMG=${IMG}"; then
    failed=1
  fi

  if [[ "${failed}" -eq 0 ]]; then
    if ! run_timed_for_shard "${iteration}" "${shard}" "test" "${profile}" "${kubeconfig_path}" \
      "IMG=${IMG} E2E_DOCKER_IMAGE=${IMG} E2E_MARKLOGIC_IMAGE_VERSION=${E2E_MARKLOGIC_IMAGE_VERSION} go test -v -count=1 -timeout 60m ./test/e2e -run '${cluster_regex}'"; then
      failed=1
    fi
  fi

  if ! run_timed_for_shard "${iteration}" "${shard}" "cleanup" "${profile}" "${kubeconfig_path}" \
    "make e2e-cleanup-minikube"; then
    failed=1
  fi

  return "${failed}"
}

run_helm_ns_shard() {
  local iteration="$1"
  local shard="$2"
  local profile="$3"
  local kubeconfig_path="$4"

  local failed=0
  if ! run_timed_for_shard "${iteration}" "${shard}" "setup" "${profile}" "${kubeconfig_path}" \
    "make e2e-setup-minikube IMG=${IMG}"; then
    failed=1
  fi

  if [[ "${failed}" -eq 0 ]]; then
    if [[ "${RUN_VOLUME_RESIZE}" == "1" ]]; then
      if ! run_timed_for_shard "${iteration}" "${shard}" "test" "${profile}" "${kubeconfig_path}" \
        "make e2e-test-helm-namespace IMG=${IMG}"; then
        failed=1
      fi
    else
      if ! run_timed_for_shard "${iteration}" "${shard}" "test" "${profile}" "${kubeconfig_path}" \
        "E2E_DOCKER_IMAGE=${IMG} E2E_MARKLOGIC_IMAGE_VERSION=${E2E_MARKLOGIC_IMAGE_VERSION} go test -v -count=1 -timeout 45m ./test/e2e-helm -run '${HELM_NO_RESIZE_REGEX}'"; then
        failed=1
      fi
    fi
  fi

  if ! run_timed_for_shard "${iteration}" "${shard}" "cleanup" "${profile}" "${kubeconfig_path}" \
    "make e2e-cleanup-minikube"; then
    failed=1
  fi

  return "${failed}"
}

run_istio_shard() {
  local iteration="$1"
  local shard="$2"
  local profile="$3"
  local kubeconfig_path="$4"

  local failed=0
  if ! run_timed_for_shard "${iteration}" "${shard}" "setup" "${profile}" "${kubeconfig_path}" \
    "make e2e-setup-minikube-istio IMG=${IMG}"; then
    failed=1
  fi

  if [[ "${failed}" -eq 0 ]]; then
    if ! run_timed_for_shard "${iteration}" "${shard}" "test" "${profile}" "${kubeconfig_path}" \
      "make e2e-test-istio IMG=${IMG} E2E_ISTIO_AMBIENT=true"; then
      failed=1
    fi
  fi

  if ! run_timed_for_shard "${iteration}" "${shard}" "cleanup" "${profile}" "${kubeconfig_path}" \
    "make e2e-cleanup-minikube"; then
    failed=1
  fi

  return "${failed}"
}

print_summary() {
  log ""
  log "Summary (avg seconds by shard/phase):"
  awk -F',' '
    NR > 1 {
      key = $2 "/" $3
      sum[key] += $4
      count[key] += 1
      failed[key] += ($5 != 0)
    }
    END {
      printf "%-34s %-10s %-10s\n", "shard/phase", "avg_s", "failures"
      for (k in sum) {
        printf "%-34s %-10.2f %-10d\n", k, sum[k] / count[k], failed[k]
      }
    }
  ' "${CSV_FILE}" | sort

  log ""
  log "Artifacts:"
  log "- CSV: ${CSV_FILE}"
  log "- Logs: ${LOG_DIR}"
}

run_parallel_round() {
  local iteration="$1"
  local p1="ml-shard-a-${TIMESTAMP}-r${iteration}"
  local p2="ml-shard-b-${TIMESTAMP}-r${iteration}"
  local p3="ml-shard-c-${TIMESTAMP}-r${iteration}"
  local k1="${OUT_DIR}/kubeconfig-${p1}"
  local k2="${OUT_DIR}/kubeconfig-${p2}"
  local k3="${OUT_DIR}/kubeconfig-${p3}"

  local fail_count=0
  local pid_a pid_b pid_c

  run_cluster_shard "${iteration}" "shard-a-cluster" "${p1}" "${k1}" &
  pid_a="$!"
  run_helm_ns_shard "${iteration}" "shard-b-helm" "${p2}" "${k2}" &
  pid_b="$!"

  pid_c=""
  if [[ "${SHARDS}" == "3" && "${RUN_ISTIO}" == "1" ]]; then
    run_istio_shard "${iteration}" "shard-c-istio" "${p3}" "${k3}" &
    pid_c="$!"
  fi

  wait "${pid_a}" || fail_count="$((fail_count + 1))"
  wait "${pid_b}" || fail_count="$((fail_count + 1))"
  if [[ -n "${pid_c}" ]]; then
    wait "${pid_c}" || fail_count="$((fail_count + 1))"
  fi

  # In 2-shard mode, run Istio after parallel phase if requested.
  if [[ "${SHARDS}" == "2" && "${RUN_ISTIO}" == "1" ]]; then
    if ! run_istio_shard "${iteration}" "shard-c-istio-serial" "${p3}" "${k3}"; then
      fail_count="$((fail_count + 1))"
    fi
  fi

  return "${fail_count}"
}

main() {
  if [[ "${SHARDS}" != "2" && "${SHARDS}" != "3" ]]; then
    log "SHARDS must be 2 or 3"
    exit 2
  fi

  local total_failures=0
  local i

  log "Minikube true-parallel pack"
  log "- shards: ${SHARDS}"
  log "- repeats: ${REPEATS}"
  log "- run_istio: ${RUN_ISTIO}"
  log "- run_volume_resize: ${RUN_VOLUME_RESIZE}"
  log "- img: ${IMG}"
  log "- isolate_minikube_home: ${ISOLATE_MINIKUBE_HOME}"
  log "- output: ${OUT_DIR}"

  for ((i = 1; i <= REPEATS; i++)); do
    log ""
    log "=== Iteration ${i}/${REPEATS} ==="
    if ! run_parallel_round "${i}"; then
      total_failures="$((total_failures + 1))"
      [[ "${FAIL_FAST}" == "1" ]] && break
    fi
  done

  print_summary

  if [[ "${total_failures}" -gt 0 ]]; then
    log "Completed with failures across ${total_failures} iterations"
    exit 1
  fi

  log "Completed successfully"
}

main "$@"
