#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
scenario="${SCENARIO:-}"
case "$scenario" in
  oauth-resource-server) test_name=TestOAuthResourceServerInfrastructure; gate=MARKLOGIC_OAUTH_RESOURCE_SERVER ;;
  oauth-authorization-code) test_name=TestOAuthAuthorizationCodeInfrastructure; gate=MARKLOGIC_OAUTH_AUTHORIZATION_CODE ;;
  haproxy-session-affinity) test_name=TestHAProxySessionIDAffinityContract; gate=MARKLOGIC_HAPROXY_SESSION_AFFINITY ;;
  *) echo 'Set SCENARIO to oauth-resource-server, oauth-authorization-code, or haproxy-session-affinity.' >&2; exit 2 ;;
esac
: "${INTEGRATION_CONTEXT:?Set INTEGRATION_CONTEXT to an explicit Kubernetes context}"
command -v kubectl >/dev/null
command -v go >/dev/null
if [[ "$scenario" != haproxy-session-affinity ]]; then
  : "${INTEGRATION_OPERATOR_NAMESPACE:?Set INTEGRATION_OPERATOR_NAMESPACE}"
  : "${INTEGRATION_OPERATOR_DEPLOYMENT:?Set INTEGRATION_OPERATOR_DEPLOYMENT}"
  : "${MARKLOGIC_IMAGE:?Set MARKLOGIC_IMAGE to the image under test}"
fi
if [[ "$scenario" == oauth-authorization-code ]]; then
  # Custom image tags cannot reliably identify the server version. Require an
  # explicit declaration, and retain the image reference in the run output.
  : "${MARKLOGIC_VERSION:?Set MARKLOGIC_VERSION to the server version, for example 12.1.0}"
  if [[ ! "$MARKLOGIC_VERSION" =~ ^([0-9]+)\.([0-9]+)(\.[0-9]+)?([-+].*)?$ ]]; then
    echo 'MARKLOGIC_VERSION must be a numeric major.minor[.patch] version.' >&2; exit 2
  fi
  major=$((10#${BASH_REMATCH[1]})); minor=$((10#${BASH_REMATCH[2]}))
  if (( major < 12 || (major == 12 && minor < 1) )); then
    echo 'Authorization Code Flow requires MarkLogic 12.1+.' >&2; exit 2
  fi
fi
printf 'Scenario: %s\nContext: %s\nCommit: %s\nMarkLogic image: %s\n' "$scenario" "$INTEGRATION_CONTEXT" "$(git rev-parse HEAD)" "${MARKLOGIC_IMAGE:-not-used}"
if [[ -n "$(git status --porcelain --untracked-files=normal)" ]]; then
  echo 'Working tree contains changes; the commit alone does not identify this run.'
fi
# Anchor selection and clear all gates before enabling exactly one live suite.
exec env MARKLOGIC_OAUTH_RESOURCE_SERVER=false MARKLOGIC_OAUTH_AUTHORIZATION_CODE=false \
  MARKLOGIC_HAPROXY_SESSION_AFFINITY=false "$gate=true" \
  go test -v -count=1 -timeout "${INTEGRATION_TIMEOUT:-45m}" -run "^${test_name}$" ./test/integration/marklogic-server/oauth
