# OAuth Integration Tests

Integration tests that validate MarkLogic OAuth 2.0 authentication behind the
HAProxy load balancer, running against a disposable, in-cluster
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
| `MARKLOGIC_OAUTH_REDIRECT_URI` | optional | Optional fixture configuration; Authorization Code fixes its callback to the test LB to preserve identical origins. |
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

Sub-cases mapped to **MLE-17734 — OAuth 2.0 AppServer Client Load Balancer Testing**:

- **TC1 – SessionID before authentication:** require **HTTP 302 or 303**, a
  nonempty `SessionID` cookie with `Path=/` and `HttpOnly`, the expected Keycloak
  authorization endpoint, and PKCE/state parameters with `S256` and
  `response_type=code`. Record the actual selected backend before stopping.
  This accepts the redirect mechanisms permitted by [RFC 6749 section 1.7](https://www.rfc-editor.org/rfc/rfc6749.html#section-1.7), including MarkLogic's initial 303. Other status codes do not pass.
  The original requirement's 302 example was clarified to include 303 on September 22, 2026; cookie, routing, PKCE and identity checks are unchanged.
- **TC2 – same backend and authenticated identity:** complete real Keycloak
  login and the callback with the original cookie jar. Require observed initial
  and callback backend names to match, and the original SessionID to reach the
  proxy on callback. Require callback HTTP 200/302/303 without restarting login,
  followed by HTTP 200 and the expected authenticated identity at `/identity.xqy`.
- **TC3 – controlled cross-node failure through the LB:** use the same listener,
  logical URL, Host, certificate, redirect URI, and complete cookie/state/PKCE
  flow. Select a separate HAProxy backend with **no persistence rules**. It uses
  `balance roundrobin` with deterministic `use-server` fault injection: the initial
  request goes to node 0 and `/oauth/callback` to node 1. Require observed backend
  identities to differ and the original SessionID to be present at callback.
  Then require HTTP 400/401/403/500 **and** the specific HTML error field
  `XDMP-OAUTH: Novel OAuth state ... potential CSRF attack`. Missing routing
  evidence, same-node delivery, lost cookies, generic errors, and login restarts
  cannot pass.

The Authorization Code suite uses a **test-owned standalone HAProxy** in the
run's namespace, with the operator's HAProxy disabled only on this disposable CR.
This allows observations and fault injection without changing the shared operator.
It does not validate the operator-generated HAProxy configuration. The bearer
suite continues to use the operator-managed proxy.

HAProxy overwrites `X-Test-Backend` using its actual `srv_name` for each response;
`node-0` and `node-1` map to individually addressed MarkLogic Pod DNS names printed
with their live Management API versions. Backend connections verify the fixture
CA and the destination hostname. `X-Test-Affinity` selects the test mode and is
removed before forwarding to MarkLogic. The positive backend learns the SHA-256
of MarkLogic's original response cookie with `stick store-response` and matches
it on requests; it never inserts, strips, or renames SessionID. Hashing avoids
truncation of long session cookies. See the
[HAProxy stick-table documentation](https://docs.haproxy.org/3.0/configuration.html#stick%20store-response).
The spec's illustrative `cookie SessionID insert` is intentionally not copied:
this test needs the application-issued cookie, rather than a new proxy cookie.

Cookie hashes travel only inside the TLS test flow to compare the initial
response cookie with the callback's received cookie. Only a boolean comparison
leaves the driver; cookie values and hashes, OAuth codes, state, and tokens are
never included in test evidence. Temporary cookie and response files are deleted
on exit. Proxy request logging is disabled. All driver requests verify TLS, allow
HTTPS only, and use connection and total deadlines. Form destinations are checked.

The runner records stages, cases, image IDs, source revision/dirty status, and
cleanup in JSON/JUnit. Verbose output includes sanitized per-case evidence and
live Server versions. `MARKLOGIC_VERSION` remains a caller declaration; both
nodes must independently report 12.1+ before authentication setup proceeds.
The suite also queries each node to verify distinct host IDs, the same two cluster
members, and the same Security database ID. OAuth client scope and authentication
method are explicitly `openid profile` and `Client secret`.
The exact state-error response is a narrow expected signature, not permission to
accept any 500. Preserve failures and capture redacted evidence when product
behavior differs.

Coverage is limited to this HAProxy configuration and curl/Keycloak `form_post`.
It does not establish NGINX, AWS ALB, browser SameSite behavior, or the spec's
illustrative GET callback path. The Authorization Code tests require live Server
execution; disabled-gate skips and synthetic HTTPS regressions are not evidence
of Server acceptance.

Local regression tests execute the real shell/curl driver against a synthetic
HTTPS service, including an untrusted certificate at each of the five request
stages. They do not substitute for live MarkLogic validation:

```sh
go test ./test/integration/marklogic-server/oauth -run TestAuthCode -count=1
```

These driver tests require `sh` and `curl` and permission to bind loopback ports;
the driver tests skip if curl is unavailable. Assertion tests need no cluster.

### Live validation status

On September 22, 2026, a fresh EKS run using actual MarkLogic **12.1.0** and
HAProxy **3.4.3** passed all three MLE-17734 cases under the clarified **302 or
303** initial-redirect contract. The runner exited successfully and verified
namespace and bound-PV cleanup.

| Case | Observed behavior | Result |
| --- | --- | --- |
| TC1 SessionID before authentication | Initial 303; SessionID, explicit Path=/, HttpOnly, expected IdP and PKCE present; actual backend recorded | Passed |
| TC2 same-node callback and successful identity | node-0 → node-0 → node-0 with SessionID preserved; callback 302; final identity request 200 and `oauth-user:oauth-test-user` | Passed |
| TC3 cross-node state failure | No-affinity route node-0 → node-1 with SessionID preserved; callback 500 and NOVEL_OAUTH_STATE | Passed |

Run ID: `d5b3fc5a-4361-4699-b340-811b98682dd2`; disposable namespace:
`ml-oauth-authorization-code-9jdwn`. These results cover the test-owned HAProxy
Authorization Code scenario on EKS; other scenarios/platforms need their own runs.

Earlier login loops came from missing access-token audience in the Keycloak
fixture. Adding the confidential client's explicit audience mapper fixed the
flow. Earlier formal runs also rejected initial 303 because the original text
specified 302. Acceptance now permits both as allowed by RFC 6749 section 1.7;
all cookie, routing, PKCE and authenticated-identity checks remain enforced.
No Server or proxy status-code rewriting was used. Local regression tests reject
other initial statuses, missing cookies, wrong identity and unrelated errors.

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
  Its audience mapper adds `aud=marklogic-oauth-authcode-client` to access tokens.
  Keycloak's default `azp` claim alone does not satisfy MarkLogic 12.1's audience
  validation and causes repeated login redirects after code exchange.

A disposable test user `oauth-test-user` is seeded for authentication.

## Files

| File | Purpose |
| --- | --- |
| `oauth_setup.go` | Shared infrastructure builder (TLS, Keycloak, MarkLogic cluster, HAProxy, client pod) and external-security / App Server config. |
| `oauth_setup_test.go` | Unit tests for the setup helpers. |
| `oauth_authorization_code_test.go` | Authorization Code live flow (TC1/TC2/TC3) and management helpers. |
| `authcode_flow_script_test.go` | TLS-verifying curl driver with sanitized output. |
| `authcode_assertions_test.go` / `authcode_response_test.go` | Exact result validators and negative regression cases. |
| `authcode_proxy_test.go` | Test-owned TLS HAProxy, SessionID observation and controlled no-affinity routing. |
| `authcode_version_test.go` | Actual runtime version verification on both nodes. |
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

Original requirement: **MLE-17734** (Server team supplied spec). Requirements owner,
test maintainer and named reviewers: **TBD — confirm with the Server and Kubernetes teams**. The
[contribution guide](../../CONTRIBUTING.md) describes the proposed responsibility
split; it does not assign people or change repository contribution policy.
