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
| `TestOAuthAuthorizationCodeInfrastructure` | `MARKLOGIC_OAUTH_AUTHORIZATION_CODE=true` | `ml-oauth-authorization-code` | Curl-driven Authorization Code (PKCE) login, authenticated identity through HAProxy, and deterministic cross-node rejection. |
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

- **TC1 – SessionID before authentication:** require exactly HTTP 302/303,
  a `SessionID` cookie, the expected Keycloak authorization endpoint, and nonempty
  PKCE/state parameters with `S256` and `response_type=code`. Stop before login.
- **TC2 – authenticated session through HAProxy:** complete the Keycloak login
  and callback with the cookie jar. The callback must return HTTP 200/302/303
  without restarting authentication. A separate request to `/identity.xqy`
  using that cookie jar must return HTTP 200 and the expected authenticated user.
  A 403 or unrelated server error cannot pass, even if the identity request succeeds.
- **TC3 – cross-node callback fails for the expected reason:** start on node 0
  and send the callback directly to node 1 without the initiating cookie. Require
  HTTP 400/401/403/500 **and** an HTML error field containing
  `XDMP-OAUTH: Novel OAuth state ... potential CSRF attack`. Generic 401/403/500,
  TLS failures, and a redirect back to login all fail this assertion.

The negative assertion deliberately accepts only the documented state-error
signature. Its exact response format/status must still be validated against the
chosen MarkLogic 12.1+ build. If a version changes that behavior, add captured,
redacted evidence and a narrow regression case instead of accepting any 500.

All five driver requests verify the fixture CA and allow HTTPS only. Each has a
5-second connection timeout and a 20-second total timeout. The driver validates
form destinations, retains cookies only in temporary files that are removed on
exit, and emits stage/status markers instead of session cookies, authorization
codes, OAuth state, or raw authentication responses.

Coverage limits: the Authorization Code suite proves authenticated behavior
through HAProxy and explicit cross-node rejection. It does not yet record the
backend identity for both the initial request and callback. The separate
HAProxy/nginx contract checks backend identity when replaying a SessionID cookie.
TC3 does not disable affinity on a load balancer, and these tests do not validate
a full browser or AWS ALB/NLB configuration.

Local regression tests execute the real shell/curl driver against a synthetic
HTTPS service, including an untrusted certificate at each of the five request
stages. They do not substitute for live MarkLogic validation:

```sh
go test ./test/integration/marklogic-server/oauth -run TestAuthCode -count=1
```

These driver tests require `sh` and `curl` and permission to bind loopback ports;
the driver tests skip if curl is unavailable. Assertion tests need no cluster.

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
module under `/tmp/oauth-identity/` in each MarkLogic container.

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
| `oauth_authorization_code_test.go` | Authorization Code live flow (TC1/TC2/TC3) and management helpers. |
| `authcode_flow_script_test.go` | TLS-verifying curl driver with sanitized output. |
| `authcode_assertions_test.go` / `authcode_response_test.go` | Exact result validators and negative regression cases. |
| `authcode_driver_test.go` | Local HTTPS tests of the shell/curl driver and TLS failures. |
| `identity_probe_test.go` | Shared protected identity module for both OAuth flows. |
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
