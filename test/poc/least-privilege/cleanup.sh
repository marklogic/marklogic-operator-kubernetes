#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

for command_name in kubectl helm; do
  require_command "${command_name}"
done
assert_context

kubectl delete "marklogiccluster/${CLUSTER_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found --wait=true
helm uninstall "${RELEASE_NAME}" -n "${OPERATOR_NAMESPACE}" --ignore-not-found

if [[ "${DELETE_PVCS:-false}" == "true" ]]; then
  kubectl delete pvc -n "${TEST_NAMESPACE}" -l "app.kubernetes.io/instance=${GROUP_NAME}" --ignore-not-found
else
  printf 'PVCs retained. Set DELETE_PVCS=true to remove them.\n'
  kubectl get pvc -n "${TEST_NAMESPACE}" 2>/dev/null || true
fi

if [[ "${DELETE_NAMESPACES:-false}" == "true" ]]; then
  kubectl delete namespace "${TEST_NAMESPACE}" "${OPERATOR_NAMESPACE}" --ignore-not-found
else
  printf 'Namespaces retained. Set DELETE_NAMESPACES=true to remove them.\n'
fi

printf '\nRetained MarkLogic CRDs:\n'
kubectl get crd -o name | grep 'marklogic.progress.com' || true
printf '\nRetained cluster-scoped RBAC for the release:\n'
kubectl get clusterrole,clusterrolebinding -l "app.kubernetes.io/instance=${RELEASE_NAME}" -o name 2>/dev/null || true
