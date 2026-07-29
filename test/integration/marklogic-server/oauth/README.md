# OAuth Integration Tests

Integration tests that validate MarkLogic OAuth 2.0 authentication behind the
operator-managed HAProxy load balancer, running against a disposable, in-cluster
[Keycloak](https://www.keycloak.org/) identity provider.

These tests provision a real two-node MarkLogic cluster, HAProxy, and Keycloak in
a Kubernetes namespace, wire up TLS end to end, configure MarkLogic external
security plus an OAuth App Server through the Management API, and then exercise the
OAuth flows through the load balancer.

> These are **opt-in** tests. Each is gated behind an environment variable and is
> skipped by default so it does not run in the normal unit-test pass.

## Prerequisites

- A running Kubernetes cluster (minikube, kind, EKS, ...) selected by the current
  `kubectl` context, with a default `StorageClass` that can provision
  `PersistentVolumeClaim`s.
- The MarkLogic operator installed in the cluster (the tests create
  `MarklogicCluster` resources reconciled by the operator).
- Go toolchain and access to pull the MarkLogic, Keycloak, HAProxy, and curl
  images used by the fixtures.
- For the Authorization Code test, a **MarkLogic 12.1+** image (the Authorization
  Code flow is rejected on 12.0.x).

## Test suites

| Test | Gate env var | Namespace | What it covers |
| --- | --- | --- | --- |
| `TestOAuthAuthorizationCodeInfrastructure` | `MARKLOGIC_OAUTH_AUTHORIZATION_CODE=true` | `ml-oauth-authorization-code` | Full OAuth 2.0 Authorization Code (PKCE) flow through HAProxy with SessionID affinity. |
| `TestOAuthResourceServerInfrastructure` | `MARKLOGIC_OAUTH_RESOURCE_SERVER=true` | `ml-oauth-session-affinity` | Resource-server (JWT bearer) external security + OAuth App Server setup. |
| `TestHAProxySessionIDAffinityContract` | `MARKLOGIC_HAPROXY_SESSION_AFFINITY=true` | `ml-haproxy-session-affinity` | HAProxy native SessionID cookie affinity contract, using nginx backends (no MarkLogic). |

The unit tests in `oauth_setup_test.go` run without any gate and validate helper
logic (OpenID discovery parsing, redirect-URI validation, etc.).

## Environment variables

| Variable | Required for | Purpose |
| --- | --- | --- |
| `MARKLOGIC_OAUTH_AUTHORIZATION_CODE` | Authorization Code test | Set to `true` to enable the test. |
| `MARKLOGIC_OAUTH_RESOURCE_SERVER` | Resource-server test | Set to `true` to enable the test. |
| `MARKLOGIC_HAPROXY_SESSION_AFFINITY` | Affinity contract test | Set to `true` to enable the test. |
| `MARKLOGIC_IMAGE` | Authorization Code test (12.1+) | MarkLogic server image to deploy. Required for the Authorization Code flow. |
| `MARKLOGIC_OAUTH_REDIRECT_URI` | optional | Overrides the OAuth redirect/callback URI advertised to Keycloak. |
| `MARKLOGIC_OAUTH_RETAIN_NAMESPACE` | optional | Set to `true` to keep the namespace after the test for inspection instead of deleting it. |

## Running the tests

### Authorization Code flow (MarkLogic 12.1+)

```sh
MARKLOGIC_OAUTH_AUTHORIZATION_CODE=true \
MARKLOGIC_IMAGE=<your-marklogic-12.1+-image> \
MARKLOGIC_OAUTH_RETAIN_NAMESPACE=true \
go test -v -count=1 -timeout 45m \
  -run TestOAuthAuthorizationCodeInfrastructure \
  ./test/integration/marklogic-server/oauth
```

Sub-cases (from `docs/test/OAuth Test Spec.md`):

- **TC1 – SessionID before authentication:** the OAuth App Server sets a
  `SessionID` cookie and redirects to Keycloak with an Authorization Code + PKCE
  request before the user authenticates.
- **TC2 – affinity completes flow:** with HAProxy `SessionID` affinity, the IdP
  callback returns to the same node that started the flow, so the code exchange
  (using the confidential client secret) succeeds.
- **TC3 – cross-node callback fails:** a callback delivered to a different node
  fails, because the in-flight PKCE verifier and OAuth `state` are node-local
  (MarkLogic reports `XDMP-OAUTH: Novel OAuth state ... potential CSRF attack`).
  This proves affinity is required.

### Resource-server (JWT bearer)

```sh
MARKLOGIC_OAUTH_RESOURCE_SERVER=true \
go test -v -count=1 -timeout 45m \
  -run TestOAuthResourceServerInfrastructure \
  ./test/integration/marklogic-server/oauth
```

### HAProxy SessionID affinity contract

```sh
MARKLOGIC_HAPROXY_SESSION_AFFINITY=true \
go test -v -count=1 -timeout 20m \
  -run TestHAProxySessionIDAffinityContract \
  ./test/integration/marklogic-server/oauth
```

## Keycloak fixture

The Keycloak realm `marklogic-oauth` is imported at startup with two clients:

- `marklogic-oauth-client` — a **public** client with direct access grants
  enabled, used by the resource-server (password grant) flow.
- `marklogic-oauth-authcode-client` — a **confidential** client (with a secret)
  used by the Authorization Code flow, because MarkLogic 12.1 authenticates at the
  token endpoint with a client secret when exchanging the authorization code.

A disposable test user `oauth-test-user` is seeded for authentication.

## Files

| File | Purpose |
| --- | --- |
| `oauth_setup.go` | Shared infrastructure builder (TLS, Keycloak, MarkLogic cluster, HAProxy, client pod) and external-security / App Server config. |
| `oauth_setup_test.go` | Unit tests for the setup helpers. |
| `oauth_authorization_code_test.go` | Authorization Code flow test (TC1/TC2/TC3). |
| `oauth_load_balancer_affinity_test.go` | Resource-server external security + OAuth App Server setup test. |
| `session_affinity_test.go` | HAProxy SessionID affinity contract test using nginx backends. |

## Cleanup

By default each test deletes its namespace on completion. Set
`MARKLOGIC_OAUTH_RETAIN_NAMESPACE=true` to retain it for inspection, then remove it
manually when finished:

```sh
kubectl delete ns ml-oauth-authorization-code
kubectl delete ns ml-oauth-session-affinity
kubectl delete ns ml-haproxy-session-affinity
```
