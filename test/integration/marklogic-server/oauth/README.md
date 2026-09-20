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

- A running Kubernetes cluster (minikube, kind, EKS, ...) selected explicitly by
  `INTEGRATION_CONTEXT`, with a selected/default `StorageClass` that can provision
  `PersistentVolumeClaim`s.
- The MarkLogic operator installed in the cluster (the tests create
  `MarklogicCluster` resources reconciled by the operator).
- Go toolchain and access to pull the MarkLogic, Keycloak, HAProxy, and curl
  images used by the fixtures.
- For the Authorization Code test, a **MarkLogic 12.1+** image (the Authorization
  Code flow is rejected on 12.0.x).

## Test suites

| Test | Gate env var | Namespace prefix | What it covers |
| --- | --- | --- | --- |
| `TestOAuthAuthorizationCodeInfrastructure` | `MARKLOGIC_OAUTH_AUTHORIZATION_CODE=true` | `ml-oauth-authorization-code` | Full OAuth 2.0 Authorization Code (PKCE) flow through HAProxy with SessionID affinity. |
| `TestOAuthResourceServerInfrastructure` | `MARKLOGIC_OAUTH_RESOURCE_SERVER=true` | `ml-oauth-resource-server` | Resource-server configuration and protected requests through HAProxy: valid token identity, missing token, and invalid token. |
| `TestHAProxySessionIDAffinityContract` | `MARKLOGIC_HAPROXY_SESSION_AFFINITY=true` | `ml-haproxy-session-affinity` | HAProxy native SessionID cookie affinity contract, using nginx backends (no MarkLogic). |

The unit tests in `oauth_setup_test.go` run without any gate and validate helper
logic (OpenID discovery parsing, redirect-URI validation, etc.).

Each run appends a generated suffix to its namespace prefix. For the common
runner, context/operator settings, storage selection, preflight limits, and
cleanup behavior, see [the integration runner guide](../README.md#running-the-tests).

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

With `INTEGRATION_CONTEXT`, `INTEGRATION_OPERATOR_NAMESPACE`, and
`INTEGRATION_OPERATOR_DEPLOYMENT` set as described in the runner guide:

```sh
MARKLOGIC_IMAGE=<your-marklogic-12.1+-image> \
MARKLOGIC_VERSION=12.1.0 \
make integration-test SCENARIO=oauth-authorization-code
```

Sub-cases (the original release requirement/spec reference still needs to be linked):

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
MARKLOGIC_IMAGE=<image-under-test> \
make integration-test SCENARIO=oauth-resource-server
```

The resource-server scenario installs an identity module on both MarkLogic nodes
and maps the disposable Keycloak identity to the test cluster's admin role. Through
HAProxy, it requires HTTP 200 with the expected identity for a valid bearer token
and rejects missing or malformed tokens. Rejections must be HTTP 401 or the
specific MarkLogic 12.0.3 HTTP 500 error for that input (empty token versus invalid
token); unrelated server errors and lost headers do not pass. Requests use no cookies or redirects
and verify TLS using the fixture CA. This does not yet cover expired tokens,
wrong issuer/audience, or fine-grained authorization. The test writes its temporary
module under `/tmp/oauth-resource-server/` in each MarkLogic container.

The installed operator must generate `h1-case-adjust authorization Authorization`
in HAProxy's global section and `option h1-case-adjust-bogus-server` in its HTTP
backends. HAProxy normalizes header names to lowercase; MarkLogic 12.0.3's OAuth
handler requires `Authorization`. This compatibility setting preserves the header
spelling on the backend connection. An older operator without this fix will fail
the positive bearer-token checks even when direct node requests succeed.
See the [HAProxy configuration manual](https://docs.haproxy.org/3.0/configuration.html#h1-case-adjust).

### HAProxy SessionID affinity contract

```sh
INTEGRATION_CONTEXT=<context> \
make integration-test SCENARIO=haproxy-session-affinity
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
| `oauth_load_balancer_affinity_test.go` | Protected bearer-token identity and rejection tests through HAProxy. |
| `session_affinity_test.go` | HAProxy SessionID affinity contract test using nginx backends. |

## Cleanup

Each test uses a fresh namespace and verifies ownership before deleting it.
Set `INTEGRATION_RETAIN_NAMESPACE=true` (or the legacy
`MARKLOGIC_OAUTH_RETAIN_NAMESPACE=true`) to retain it for inspection. The output
includes the generated namespace and context-specific cleanup command. Normal
cleanup waits for namespace and bound PV deletion and reports failures.

## Ownership

Requirements owner, test maintainer, named reviewers, and the original release
requirement link: **TBD — confirm with the Server and Kubernetes teams**. The
[contribution guide](../../CONTRIBUTING.md) describes the proposed responsibility
split; it does not assign people or change repository contribution policy.
