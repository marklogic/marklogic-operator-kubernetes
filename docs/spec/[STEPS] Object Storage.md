# Object Storage Credentials Research — Log and Findings

Research log for the "Object Storage Credentials Research and Prototype" story
(`docs/spec/[JIRA]Ojbect Storage.md`), validating the assumptions recorded in
`docs/spec/[SPEC]Object Storage.md`. This document is a running log: each
session records what was run, the raw evidence observed, and the conclusion.
The findings matrix at the end summarizes the outcome per acceptance criterion.

Environment for this research:

- Docker (Rancher Desktop) running locally on macOS.
- Image: `progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6` (the same
  image pinned in the operator's `Makefile` `E2E_MARKLOGIC_IMAGE_VERSION`).
- No Kubernetes/operator involved — a single standalone container is used to
  talk directly to the Management API, isolating the MarkLogic behavior from
  the operator's reconciliation logic.

---

## Session 1 — Environment setup

Started a standalone MarkLogic container, self-initialized (no operator involved):

```sh
docker run -d --name ml-objstor-poc \
  -p 18000-18002:8000-8002 \
  -e MARKLOGIC_INIT=true \
  -e MARKLOGIC_ADMIN_USERNAME=admin \
  -e MARKLOGIC_ADMIN_PASSWORD=admin123 \
  progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6
```

Confirmed the Manage app server was ready before testing:

```
$ curl -s -o /dev/null -w '%{http_code}' --anyauth --user admin:admin123 http://localhost:18002/manage/v2/hosts?format=json
200
```

Note: `admin/v1/timestamp` returns `200` immediately (it's unauthenticated/always-available),
so it is **not** a reliable readiness signal by itself — `/manage/v2/hosts` returning `200`
with real credentials is the signal that the security database finished initializing.
The container logged "Installing admin username and password, and initialize the security
database and objects" for roughly 30–45 seconds after `MARKLOGIC_INIT` before the Manage
server accepted authenticated requests.

---

## Session 2 — AC1: Management API behavior (`PUT`/`GET` `/manage/v2/credentials/properties`)

### Test 1 — PUT with `?type=` query string only (as the SPEC currently documents)

```
$ curl --anyauth --user admin:admin123 -X PUT -H "Content-type: application/json" \
    -d '{"access-key":"AKIATESTKEY1234567","secret-key":"testSecretKeyValue1234567890","session-token":"testSessionToken123"}' \
    "http://localhost:18002/manage/v2/credentials/properties?type=aws"
HTTP_STATUS:204
```

AWS succeeded with **`204 No Content`**, not the documented `201 Created`.

```
$ curl --anyauth --user admin:admin123 -X PUT -H "Content-type: application/json" \
    -d '{"storage-account":"mystorageacct","storage-key":"dGVzdHN0b3JhZ2VrZXk="}' \
    "http://localhost:18002/manage/v2/credentials/properties?type=azure"
{"errorResponse":{"statusCode":"400","status":"Bad Request","messageCode":"MANAGE-INVALIDPAYLOAD",
 "message":"MANAGE-INVALIDPAYLOAD: (err:FOER0000) Payload has errors in structure, content-type or values. Invalid node"}}
HTTP_STATUS:400
```

Azure **failed with `400`** using the exact request shape the SPEC's Background section documents
(`?type=azure` query string, body = `{storage-account, storage-key}`).

### Test 2 — Official docs example (`type` embedded in the JSON body, no query string)

Fetched `https://docs.marklogic.com/REST/PUT/manage/v2/credentials/properties`. The documented
example is:

```
curl -X PUT --digest -u admin:admin -H "Content-type: application/json" \
  -d '{"type": "aws", "access-key": "AWS-ACCESS-KEY", "secret-key": "SECRET-KEY"}' \
  http://localhost:8002/manage/v2/credentials/properties
```

Note there is **no `?type=` query string** in the documented example — `type` is a field inside
the JSON body. Retested against the live container with this shape for both providers:

```
$ curl --anyauth --user admin:admin123 -X PUT -H "Content-type: application/json" \
    -d '{"type":"aws","access-key":"AKIABODYTEST","secret-key":"bodyTestSecret123"}' \
    "http://localhost:18002/manage/v2/credentials/properties"
HTTP_STATUS:204

$ curl --anyauth --user admin:admin123 -X PUT -H "Content-type: application/json" \
    -d '{"type":"azure","storage-account":"mystorageacct","storage-key":"dGVzdHN0b3JhZ2VrZXk="}' \
    "http://localhost:18002/manage/v2/credentials/properties"
HTTP_STATUS:204

$ curl --anyauth --user admin:admin123 "http://localhost:18002/manage/v2/credentials/properties?type=azure&format=json"
{"azure":{"storage-account":"mystorageacct","storage-key":"9VYIAARnAA0sHLcADgEAAAAFJABGRkZGRi1NYXJrTG9naWMtQ1JFREVOVElBTFMtUk9PVC1LRVkGEAABTVvyqgwPmmJkUmJYCRB5CCAAshQqAkUBxi5myHA1Zoj2hkmLXhxx8QhwbH0wwKC0OXYAAAAAAAAAAAAAAAAAAK+G3vE="}}
HTTP_STATUS:200
```

Azure succeeded once `"type":"azure"` was included **in the body**.

### Test 3 — Isolating the exact contract (query string vs. body field)

| Query string | Body has `"type"` field | Result |
|---|---|---|
| `?type=azure` | No (only `storage-account`/`storage-key`) | `400 MANAGE-INVALIDPAYLOAD` (re-confirmed twice) |
| `?type=azure` | Yes (`"type":"azure"` + fields) | `204` (works) |
| none | Yes (`"type":"azure"` + fields) | `204` (works) |
| `?type=aws` | No (only `access-key`/`secret-key`/`session-token`) | `204` (works — AWS is the documented default when no type is specified, so the server accepts AWS-shaped fields without an explicit type) |

**Conclusion: the `?type=` query-string parameter has no effect on how the server interprets the
payload. The JSON body must contain a `"type": "aws"` or `"type": "azure"` field.** AWS-shaped
requests happen to succeed without an explicit body `type` only because MarkLogic defaults to
`aws` when no type is determinable — this is coincidental, not because the query string was
honored. **This is a functional bug relative to the current SPEC and Task Breakdown**, which
document `PUT /manage/v2/credentials/properties?type=aws|azure` with a body that has no `type`
field. As currently specified, the Azure path (`BuildAzureCredentialsPayload`) would fail every
request with `400`.

### Test 4 — `GET` masking behavior

```
$ curl --anyauth --user admin:admin123 "http://localhost:18002/manage/v2/credentials/properties?type=aws&format=json"
{"aws":{"access-key":"AKIATESTKEY1234567",
        "secret-key":"9VYJAAR3AA0sHLcADgEAAAAFJABGRkZGRi1NYXJrTG9naWMtQ1JFREVOVElBTFMtUk9PVC1LRVkGEAA138MAUQv43P1KJiTnKWhuCDAA9GDfLw9gxJJd7ScaC4gszCT/ne73nQuLdkx/izxaY1WX8WF/wSUPgZ2oL5iQFE+ZAAAAAAAAAAAAAAAAAADDo2nb",
        "session-token":"testSessionToken123"}}
```

Re-applied the **same** `secret-key` value with a different `session-token` and re-read:

```
$ curl ... -X PUT ... -d '{"access-key":"AKIATESTKEY1234567","secret-key":"testSecretKeyValue1234567890","session-token":"DIFFERENT-TOKEN-999"}' ...
HTTP_STATUS:204
$ curl ... GET ...
{"aws":{"access-key":"AKIATESTKEY1234567",
        "secret-key":"9VYJAAR3AA0sHLcADgEAAAAFJABGRkZGRi1NYXJrTG9naWMtQ1JFREVOVElBTFMtUk9PVC1LRVkGEADLLorbhKBv3lx2qsYDZEJRCDAA9ByMqDwIE5ZKJCjoFpen27i+DCubh8NZivgDz8qWuyvkpYYKlrNj72LSvzfE7znQAAAAAAAAAAAAAAAAAABzw3cu",
        "session-token":"DIFFERENT-TOKEN-999"}}
```

Findings:

1.  **`access-key` is returned in plaintext.** Not sensitive by MarkLogic's classification, but
    worth confirming: `access-key` must still be treated as non-public in operator logs/status
    since it identifies the AWS principal.
2.  **`secret-key` is not "masked" (e.g. `****`) — it is returned as an opaque encrypted blob**
    (visibly wrapped by MarkLogic's internal credentials vault, the ciphertext contains the
    string `MarkLogic-CREDENTIALS-ROOT-KEY` when the base64 is decoded, confirming it's an
    MarkLogic-internal-key-encrypted value, not a hash or redaction placeholder).
3.  **The ciphertext is non-deterministic.** Re-applying the identical `secret-key` value produced
    a *different* ciphertext on `GET` (confirmed above — same plaintext secret, different blob
    each time, presumably due to a random IV/nonce per encryption). **This directly confirms the
    SPEC's core assumption in Requirement Review #4 and JIRA AC4: `GET`-based comparison of the
    stored value is not viable for drift detection, because identical input produces different
    output on each apply. Fingerprinting the resolved Secret material (not the API's returned
    value) is the only reliable approach.**
4.  **`session-token` is returned in plaintext, unmasked and unencrypted**, unlike `secret-key`.
    This is a real security-relevant finding not currently called out anywhere in the SPEC: if
    users provide an STS session token, it will be visible in plaintext to anyone who can `GET`
    `/manage/v2/credentials/properties` with sufficient privilege. The SPEC's Security NFR
    section should note this is a MarkLogic-side behavior the operator cannot mask, and should
    reinforce (per Requirement Review #6) that IRSA/instance-role is preferred over session
    tokens specifically because of this exposure, not just token expiry.
5.  Before any value has ever been set, `GET ?type=azure` returns `{"azure": null}` (JSON) /
    `<azure/>` (XML) — a clean "unconfigured" default, useful for controller logic that wants to
    detect "never configured" vs. "configured with older material."

---

## Session 3 — AC2: Minimum required privileges

Created three test roles/users directly via the Management API (`POST /manage/v2/roles`,
`POST /manage/v2/users`) to test each privilege combination empirically rather than trusting
documentation alone.

### Test 1 — `manage-admin` role only (matches the dynamic-host feature's existing user)

```
$ curl --anyauth --user objstor-manageadmin-user:Passw0rd123! -X PUT -H "Content-type: application/json" \
    -d '{"type":"aws","access-key":"AKIAPRIVTEST","secret-key":"privTestSecret123"}' \
    "http://localhost:18002/manage/v2/credentials/properties"
{"errorResponse":{"statusCode":"403","status":"Forbidden","messageCode":"",
  "message":"You do not have credentials to access /manage/v2/credentials/properties ."}}
HTTP_STATUS:403
```

**Confirmed: `manage-admin` alone is insufficient** — matches the SPEC's Requirement Review #3
assumption that the dynamic-host feature's `manage-admin`-only user cannot be reused as-is.

**Correction to the documented response code:** the official docs state a `401` is returned for
insufficient privileges. The live server returned **`403 Forbidden`** with a clear message. `401`
was only ever observed for missing/invalid credentials (unauthenticated), not for an
authenticated-but-under-privileged user. The SPEC and any future client code should treat `403`
as the "insufficient privilege" signal, not `401`.

### Test 2 — `manage-admin` + `security` roles

```
$ curl --anyauth --user objstor-manageadmin-security-user:Passw0rd123! -X PUT -H "Content-type: application/json" \
    -d '{"type":"aws","access-key":"AKIAPRIVTEST2","secret-key":"privTestSecret456"}' \
    "http://localhost:18002/manage/v2/credentials/properties"
HTTP_STATUS:204
```

**Confirmed: `manage-admin` + `security` roles are sufficient.** Matches the SPEC's Requirement
Review #3 and Background: Required Privileges sections exactly.

### Test 3 — Granular execute privileges (`manage`, `manage-admin`, `credentials-set-aws`), no `security` role

Created a role with only execute privileges (no built-in roles):

```json
{"role-name":"objstor-privonly-aws","privilege":[
  {"privilege-name":"manage","action":"http://marklogic.com/xdmp/privileges/manage","kind":"execute"},
  {"privilege-name":"manage-admin","action":"http://marklogic.com/xdmp/privileges/manage-admin","kind":"execute"},
  {"privilege-name":"credentials-set-aws","action":"http://marklogic.com/xdmp/privileges/credentials-set-aws","kind":"execute"}
]}
```

```
$ curl --anyauth --user objstor-privonly-aws-user:Passw0rd123! -X PUT ... -d '{"type":"aws",...}' ...
HTTP_STATUS:204   # succeeds for AWS

$ curl --anyauth --user objstor-privonly-aws-user:Passw0rd123! -X PUT ... -d '{"type":"azure",...}' ...
{"errorResponse":{"statusCode":"403","status":"Forbidden", ...}}
HTTP_STATUS:403   # fails for Azure — this user only has credentials-set-aws
```

**Confirmed:**

1.  A **dedicated least-privilege role/user is practical for v1** — the granular privilege
    combination (`manage` + `manage-admin` + `credentials-set-aws` and/or `credentials-set-azure`)
    works exactly as documented and does not require the broader `security` role.
2.  **Privilege scoping is genuinely per-provider.** A user with only `credentials-set-aws` cannot
    configure Azure credentials (`403`), and vice versa (not separately retested but implied by
    the identical privilege model documented for `credentials-set-azure`). This means the
    optional hardening path recorded in the SPEC's Security NFR #4 could be tightened further: a
    deployment could grant `credentials-set-aws` only, `credentials-set-azure` only, or both,
    depending on which providers are actually configured — worth considering as a refinement to
    the "optional hardening path" language.

---

## Session 4 — Bonus: `DELETE /manage/v2/credentials/properties`

Not part of the JIRA's Must Achieve list, but directly relevant to the SPEC's Follow-Up item
"Optional credential revocation when a provider block is removed from the spec."

```
$ curl --anyauth --user admin:admin123 -X DELETE "http://localhost:18002/manage/v2/credentials/properties?type=aws"
HTTP_STATUS:415   # fails without a Content-Type header, even with no body

$ curl --anyauth --user admin:admin123 -X DELETE -H "Content-type: application/json" \
    "http://localhost:18002/manage/v2/credentials/properties?type=aws"
HTTP_STATUS:204

$ curl --anyauth --user admin:admin123 "http://localhost:18002/manage/v2/credentials/properties?type=aws&format=json"
{"aws":null}
```

**Finding:** `DELETE` works and cleanly clears a provider's credential set (back to the
"unconfigured" `null` state), provided a `Content-Type` header is sent even though there is no
request body. This is a simple, low-risk operation — worth reconsidering whether "credential
revocation on provider-block removal" should move from the SPEC's Follow-Ups (not in v1) into
v1 scope, since the API makes it trivial rather than risky.

---

## Session 5 — CSI abstractions: written position (JIRA Nice 10)

No live testing; this is the design question the epic flagged ("further research is required to
assess whether Kubernetes CSI abstractions are a viable integration option"). Answering it does
not require a cluster, only a decision about what CSI would and would not replace.

**Verdict: `Defer`. CSI is neither an alternative to nor a prerequisite for the MarkLogic-native
credential path. It is a complementary, separately-scoped capability.**

Reasoning:

1.  **It solves a different problem.** A CSI driver (Mountpoint for S3, BlobFuse, s3fs) presents a
    bucket as a POSIX mount. MarkLogic's native S3/Azure support instead speaks the object APIs
    directly, and the credentials endpoint validated in Sessions 2–3 is what enables that. Making
    a bucket appear as a directory does nothing to configure MarkLogic's native path, so CSI
    cannot substitute for this feature — a cluster with a CSI mount and no credentials still
    cannot run an S3 backup addressed by an `s3://` URI.
2.  **The scenarios the epic names are native-path scenarios.** Scheduled backups to S3/Azure —
    the epic's primary motivator — are configured in MarkLogic with object-storage URIs, not
    filesystem paths. Routing those through a mount would be working against the product.
3.  **Object-storage-as-filesystem is a poor fit for MarkLogic's I/O.** Forest data assumes
    filesystem semantics (in-place updates, fsync durability, byte-range rewrites, locking) that
    object stores emulate imperfectly or not at all. Pointing forest data directories at a CSI
    mount is exactly the kind of unsupported configuration *Requirement Review* #2 already rules
    out, and it would be riskier than the native path rather than safer.
4.  **Where it could add value, it is orthogonal.** CSI is plausible for adjacent, append-or-
    read-mostly uses — staging ingest corpora, exporting logs, sharing read-only reference data —
    none of which involve MarkLogic's object storage credentials and none of which are blocked by
    this feature. Those would be ordinary `volumes`/`volumeMounts` on the pod spec, requiring no
    operator-specific API.
5.  **It carries its own credential problem.** CSI drivers authenticate through their own
    mechanisms (driver-specific secrets, IRSA on the node/pod). Adopting CSI would add a second,
    parallel credential surface rather than reusing the one being built here, so it is additive
    complexity, not a simplification.

**Conclusion:** CSI stays a follow-up research item, unchanged from the SPEC's existing position,
and it did not block or alter the MarkLogic-native credential path. If it is ever picked up, it
should be scoped as a general volume-mounting capability rather than as part of object storage
credential configuration.

---

## Session 6 — AWS keyless (IRSA): verified **not possible** (JIRA Nice 8)

Revisiting the `Defer` verdict from Todo D1. No EKS cluster was needed: the question is settled by
MarkLogic's own documentation plus the contents of the image the operator ships. The outcome is
stronger than "unvalidated" — **the SPEC's stated mechanism does not exist.**

### Evidence 1 — MarkLogic does not use the AWS credential provider chain

The SPEC claimed: *"When the AWS credential set is empty, MarkLogic uses the standard AWS
credential provider chain."* The MarkLogic 12 documentation
([Configure AWS credentials](https://docs.progress.com/bundle/marklogic-server-on-aws-12/page/topics/managing-marklogic-server-on-ec2/configuring-marklogic-for-amazon-simple-storage-service--s3-/configure-aws-credentials.html))
states a MarkLogic-specific order of precedence with exactly three entries:

> 1. Credentials configured in the MarkLogic Security database
> 2. Environment variables
> 3. IAM Role

This is not the AWS SDK provider chain. Notably absent is any web-identity-token step — which is
the *only* mechanism IRSA uses.

### Evidence 2 — the IAM Role step is gated, and the gate is closed in containers

[Configure an IAM role with an AWS access policy](https://docs.progress.com/bundle/marklogic-server-on-aws-12/page/topics/managing-marklogic-server-on-ec2/configuring-marklogic-for-amazon-simple-storage-service--s3-/configure-aws-credentials/configuring-an-iam-role-with-an-aws-access-policy.html)
(identical text in the v10, v11, and v12 bundles):

> IAM roles are only used on the server if the `MARKLOGIC_AWS_ROLE` environment variable is set.
> This happens automatically for you **unless you disable the EC2 configuration (such as setting
> `MARKLOGIC_EC2_HOST=0`), in which case the server will not use the `MARKLOGIC_AWS_ROLE`
> variable.**

And `MARKLOGIC_AWS_ROLE` "is fetched from the IAM Role associated with the instance" — i.e. from
EC2 instance metadata, which is the **instance-profile** mechanism, not IRSA.

The rootless image the operator pins sets that gate closed at build time. From
`marklogic/marklogic-docker`, `dockerFiles/marklogic-server-ubi-rootless:base`:

```
MARKLOGIC_JOIN_TLS_ENABLED=false \
MARKLOGIC_EC2_HOST=0 \
TZ=UTC
```

`src/scripts/start-marklogic-rootless.sh` writes that value into `/etc/marklogic.conf`, and the
image's own test suite asserts the result:

```robot
Verify That marklogic.conf contains    MARKLOGIC_PID_FILE    MARKLOGIC_UMASK    MARKLOGIC_USER    MARKLOGIC_EC2_HOST=0
```

So in the operator's image, EC2 configuration is disabled by default, and per the documentation
above the server therefore ignores IAM roles entirely.

### Evidence 3 — no web identity support anywhere

A keyword search of `marklogic/marklogic-docker` for `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`,
and `web-identity` returns **nothing**. A documentation search for `MARKLOGIC_AWS_ROLE` returns
6 results across v10/v11/v12, all EC2-centric; none mention EKS, IRSA, service accounts, or
projected tokens. The operator repo sets none of these variables either (`MARKLOGIC_EC2_HOST`,
`MARKLOGIC_AWS_ROLE`, `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE` — zero matches).

### Conclusion

**Verdict: `No-Go` for IRSA, not merely `Defer`.** IRSA requires the consumer to exchange a
projected service-account token via STS `AssumeRoleWithWebIdentity`. MarkLogic has no such step in
its credential resolution order and no support for the associated environment variables. Empty
credentials do not fall through to the AWS SDK chain, because MarkLogic does not use that chain.

A *related but different* mode may be reachable: overriding `MARKLOGIC_EC2_HOST=1` in the pod so
MarkLogic re-enables EC2 configuration and picks up `MARKLOGIC_AWS_ROLE` from IMDS. This is
untested and comes with real caveats:

- it yields the **node's** instance-profile role, not a pod-scoped identity, so every pod on the
  node shares the same S3 access — a security downgrade relative to IRSA, and arguably worse than
  a scoped Kubernetes Secret;
- it depends on pods being able to reach IMDS (`169.254.169.254`), which EKS clusters frequently
  block via a hop limit of 1 specifically to prevent this;
- it re-enables MarkLogic's broader EC2 bootstrap behavior inside a container, which the image
  authors deliberately turned off.

That variant is the only thing an EKS test could still tell us, and it is not what the SPEC
proposed. Recommend not pursuing it without a product-side statement that it is supported.

---

## Findings Matrix

| # | JIRA Acceptance Criterion | Status | Evidence / Conclusion |
|---|---|---|---|
| Must 1 | Confirm `PUT`/`GET` behavior, response codes, payload shape, masking | **Confirmed — SPEC needs correction** | Response code is `204`, not `201`. `type` must be in the JSON body, not the query string (Azure fails with `400` otherwise — a functional bug in the current SPEC's documented request shape). `secret-key` is encrypted (non-deterministic ciphertext), not masked with a placeholder. `session-token` is returned in **plaintext**, unmasked. |
| Must 2 | Confirm minimum privileges; dedicated least-privilege user practicality | **Confirmed** | `manage-admin` alone → `403` (not `401` as documented). `manage-admin`+`security` → success. Granular `manage`+`manage-admin`+`credentials-set-{aws,azure}` → success, confirmed per-provider scoped. A dedicated least-privilege user is practical for v1 using the granular privilege combination. |
| Must 3 | Validate complete Secret-backed static credential flow | **Confirmed** | Both providers apply successfully with the corrected (body-`type`) request shape; re-applying identical material is idempotent at the MarkLogic level (`204` both times, no error). |
| Must 4 | Validate fingerprinting approach | **Confirmed as necessary, not yet prototyped in Go** | Live evidence (Session 2, Test 4) proves `GET`-based comparison cannot work: identical `secret-key` input produces different ciphertext output on each apply. This validates the SPEC's fingerprint-based approach as the *only* viable option, but the Go-level details (salt stability across restarts, canonical field ordering, Secret-watch-triggered reconciliation) still require code-level prototyping — not exercised in this session. |
| Must 5 | Resolve supported-usage boundary (credentials-only vs. backups/forests) | **Confirmed** | Resolved by design reasoning in SPEC's Requirement Review #2; nothing observed here contradicts it. The SPEC's *Out of Scope* list now also states that the forest, backup, region, and endpoint behaviors are excluded as **unvalidated**, not merely unwanted, as Must 5 requires. |
| Must 6 | Findings matrix produced | **This table** | — |
| Must 7 | Validate status/condition model | **Model designed; not yet exercised** | Observed error shapes (`{"errorResponse":{"statusCode","status","messageCode","message"}}`) drove an eight-reason failure vocabulary with a retriability column in the SPEC's *Status Contract*, splitting `401` (bad admin credential) from `403` (missing MarkLogic privilege) so the most likely misconfiguration is diagnosable from status alone. A *Phase Semantics* table was added, including that `Disabled` means "unmanaged", not "unconfigured". Remaining: exercise every reason against a live controller (see Todo C3). |
| Nice 8 | AWS IRSA/instance-role prototype | **`No-Go` — verified impossible** | Session 6. No EKS needed. MarkLogic's credential order has exactly three steps (security DB → env vars → IAM Role) and is **not** the AWS SDK provider chain, so empty credentials do not fall through to IRSA. The IAM Role step is gated on `MARKLOGIC_AWS_ROLE`, which the docs say is ignored when `MARKLOGIC_EC2_HOST=0` — and the operator's rootless image hard-sets exactly that. No web-identity support exists in the image or docs. The SPEC's stated mechanism does not exist. |
| Nice 9 | Azure managed identity investigation | **`Defer`** | Not attempted — needs Azure infrastructure. Already out of v1 scope regardless. Scoped to image `12.0.3-ubi9-rootless-2.2.6`; other supported versions untested. |
| Nice 10 | CSI abstraction research | **`Defer` — position recorded** | Session 5. CSI is complementary, not an alternative: it cannot configure MarkLogic's native object APIs, the epic's backup scenario is native-path, forest-on-mount is the unsupported pattern Requirement Review #2 already excludes, and CSI brings a second credential surface of its own. Did not block the native path. |
| — | Credential revocation via `DELETE` | **Confirmed working — deliberately deferred** | Session 4. Deferred on API-design grounds (removal is ambiguous between "revoke" and "stop managing"), not effort. |
| — | Least-privilege MarkLogic user | **Decision: use bootstrap admin in v1** | Session 3 proved a dedicated user is practical; deferred as a hardening path because the operator already holds the bootstrap admin credential, so a second user adds lifecycle surface without reducing capability. |

## Corrections Needed in `[SPEC]Object Storage.md` — **applied**

Based on this research, the following statements in the SPEC were contradicted by live-cluster
evidence. All four have since been corrected in `[SPEC]Object Storage.md`; see Remaining Todos
group A for what changed where. They are retained here as the evidence trail:

1.  **Response code:** "Returns `201 Created` on success" (Background: Credential Model) should
    be `204 No Content`.
2.  **Request shape:** The documented `PUT /manage/v2/credentials/properties?type=aws|azure`
    pattern (Background, Task Breakdown item 2) does not work for Azure. The `type` must be
    included in the JSON request body (e.g. `{"type": "azure", "storage-account": ..., "storage-key": ...}`),
    not solely as a query-string parameter. `BuildAWSCredentialsPayload`/`BuildAzureCredentialsPayload`
    must emit a `type` field.
3.  **Privilege-failure status code:** "a status code of 401 (Unauthorized) is returned if the
    caller lacks privileges" (Background: Required Privileges) should be `403 Forbidden` for an
    authenticated-but-under-privileged user; `401` is reserved for missing/invalid credentials.
4.  **Masking language:** "The Management API masks secret material on read" (Requirement Review
    #4) is imprecise — `secret-key` is returned as encrypted ciphertext (not a masked placeholder),
    and `session-token` is **not protected at all** (returned in plaintext). The Security NFR
    section should note the `session-token` plaintext exposure explicitly.

---

## Remaining Todos

Derived from a review of `[EPIC]Object Storage.md`, `[JIRA]Ojbect Storage.md`,
`[SPEC]Object Storage.md`, and the sessions above. Each item records why it is still open and
which acceptance criterion it closes. Items are ordered so that the SPEC is made truthful first,
then the still-unvalidated research questions are closed, then the story is formally wrapped.

### A. Correct the SPEC to match observed behavior (blocks implementation stories)

These are the four corrections listed in the previous section, restated as actionable edits.
**All four have been applied to `[SPEC]Object Storage.md`.**

- [x] **A1 — Response code.** Changed `201 Created` to `204 No Content` in *Background: Credential
      Model*, in *Controller Workflow* step 4, and in *Task Breakdown* item 2.
- [x] **A2 — Request shape.** Replaced the `?type=aws|azure` query-string contract with a body
      `"type"` field in *Background: Credential Model* (both HTTP examples), *Task Breakdown*
      item 2 (both the implementation and payload-builder bullets), and the client-test bullet in
      *Task Breakdown* item 8. Recorded that the query string is accepted but ignored, and that
      AWS-shaped payloads succeed without a body `type` only because `aws` is the server-side
      default.
- [x] **A3 — Privilege-failure code.** *Background: Required Privileges* now states that an
      authenticated-but-under-privileged caller receives `403`, with `401` reserved for
      missing/invalid credentials; the *Credential Model* response-code line lists both. Also
      folded in the per-provider scoping of the `credentials-set-*` privileges (overlaps B5).
- [x] **A4 — Masking language.** *Requirement Review* #4 now describes `secret-key` as
      non-deterministic ciphertext rather than a mask, and notes that `access-key` and
      `session-token` are returned in plaintext. Added *Security NFR* #5 covering the
      `session-token` plaintext exposure and its link to Requirement Review #6.

### B. Fold the new findings into the SPEC (behavior the SPEC did not describe)

**All applied to `[SPEC]Object Storage.md`.**

- [x] **B1 — Idempotency evidence.** Added *Requirement Review* #9: MarkLogic accepts a redundant
      re-apply with `204`, so skip-if-unchanged is a cost/noise optimization rather than a
      correctness property. *Credential Application* AC2 now says so explicitly, which lowers the
      risk profile of any fingerprint bug.
- [x] **B2 — "Never configured" detection.** Added *Requirement Review* #10 and a new *Phase
      Semantics* table in the *Status Contract*. The `{"<provider>": null}` response gives a way to
      tell "never configured" from "configured but fingerprint unknown" (relevant after status loss
      or operator upgrade). Also pinned down that `Disabled` means "not managed by the operator",
      **not** "not configured in MarkLogic" — a distinction that only exists because v1 does not
      revoke on removal.
- [x] **B3 — Distinct failure reasons.** Replaced the single `ManagementAPIError` with an
      eight-row *Failure Reasons* table carrying a retriability column: `BootstrapNotReady`,
      `SecretNotFound`, `SecretKeyMissing`, `InvalidPayload` (`400`), `AuthenticationFailed`
      (`401`), `InsufficientPrivilege` (`403`), `ManagementAPIUnreachable`, and
      `ManagementAPIError` as catch-all. Closes the actionability half of **JIRA Must 7**.
- [x] **B4 — Error-body handling rule.** *Controller Workflow* step 6 now specifies that messages
      are built from the status code plus `errorResponse.messageCode`/`message` only, and that the
      response body is treated as untrusted regardless — so the no-secret-leakage guarantee does
      not depend on MarkLogic's error text staying the way it is today.
- [x] **B5 — Per-provider privilege scoping.** *Security NFR* #4, *Requirement Review* #3, and
      *Follow-Ups* now record that `credentials-set-aws`/`credentials-set-azure` are independently
      scoped, making the granular privilege set the tighter hardening option.
- [x] **B6 — Least-privilege user decision.** Recorded in *Requirement Review* #3: **v1 uses the
      bootstrap admin credential.** Rationale — a dedicated user is confirmed practical, but the
      operator already holds the bootstrap admin credential for other reconcile steps, so an extra
      MarkLogic user adds lifecycle surface without reducing what the operator can do. Kept as a
      hardening path.
- [x] **B7 — `DELETE` / revocation scope decision.** Recorded in *Validation Rules* #6: **stays
      out of v1.** Rationale — the blocker is not effort but ambiguity: spec removal could mean
      "revoke" or "stop managing", and guessing wrong silently breaks running backups. That makes
      it an API-design question (it needs an explicit opt-in signal), which is a better reason to
      defer than the "risky API" assumption it replaces.
- [x] **B8 — Link findings from the SPEC.** *Validation Status* now points at this document as the
      authoritative evidence record, rows are flipped from `Pending POC` to their real verdicts,
      and new rows were added for claims the table never tracked: Secret-watch triggering,
      multi-host propagation, the status/reason model, and revocation.

### C. Close the still-unvalidated research questions

These are the only items that still need hands-on work. The SPEC's *Task Breakdown* has been
updated so each has an explicit home in implementation, but none are validated yet.

- [ ] **C1 — Fingerprinting Go prototype (JIRA Must 4, the only Must still open).** Live evidence
      proved `GET`-based comparison is impossible, which validates *that* fingerprinting is needed
      but not *how* it behaves in the operator. Still to prototype in Go:
      - salt stability across controller restarts and across replicas (where the salt lives — a
        generated Secret, a fixed build-time constant, or derived from cluster identity — and what
        happens to `appliedFingerprint` when the salt changes);
      - canonical field ordering so equivalent material always fingerprints identically;
      - optional-field behavior, in particular that adding/removing `sessionToken` changes the
        fingerprint;
      - a change to *every* credential field individually produces a different fingerprint;
      - assertions that raw values never reach status, events, logs, or error strings.

      Mitigating context from B1: because MarkLogic accepts redundant re-applies cleanly, a
      fingerprint defect degrades to extra writes and noisy rotation events rather than data loss —
      so this is a correctness-of-behavior question, not a safety blocker. *Task Breakdown* item 3
      now carries the salt-source decision and the per-field test requirements.
- [ ] **C2 — Secret-watch reconciliation trigger (JIRA Must 4, second half).** A fingerprint is not
      a watch. Confirm the mechanism that makes a Secret edit wake the `MarklogicCluster`
      controller — `Watches` on `v1.Secret` with a handler mapping back to referencing clusters,
      versus a periodic resync — and record the RBAC and cache implications (the operator's
      namespace scoping is described in `docs/operator-scope-configuration.md`). Without this,
      the *Credential Rotation* acceptance criteria cannot be met. A `Watches` task has been added
      to *Task Breakdown* item 4, but the approach is not yet validated against the operator's
      actual cache configuration.
- [ ] **C3 — Status/condition model validation (JIRA Must 7).** Currently "partially informed."
      Confirm the full field set against the failure modes actually reachable in-cluster
      (missing Secret, missing key, bootstrap not ready, `403`, `400`, transient network error),
      and confirm each maps to a distinct, actionable `reason`. Depends on B3.
- [x] **C4 — Record the out-of-scope boundary explicitly (JIRA Must 5).** Done. The SPEC's
      *Out of Scope* list now states that forest, backup, region, and endpoint behaviors are
      excluded **because they were not validated**, not merely because they are unwanted, and
      confirms the epic's "supports configuring storage access for backups" is satisfied at the
      *access* layer only. Revocation and AWS keyless were added to the same list.

### D. Deferred "Nice to Have" items — decisions recorded

Each needed a written `Go` / `No-Go` / `Defer` verdict so the story can close. **All three are
`Defer`**, and the SPEC has been made internally consistent with that outcome.

- [x] **D1 — AWS IRSA prototype (Nice 8): `No-Go` (upgraded from `Defer`).** Originally deferred as
      "not attempted, needs EKS". **Session 6 closed it without an EKS cluster: the mechanism the
      SPEC described does not exist.** MarkLogic does not use the AWS SDK credential provider
      chain, and its IAM-role step is disabled by the `MARKLOGIC_EC2_HOST=0` that the operator's
      own image hard-sets. The SPEC's conditional language has been removed from *Compatibility*,
      *Validation Status*, *Accepted Scope Summary*, *In Scope*, and *Platform Compatibility* #2,
      and AWS keyless moved to *Out of Scope* — now on evidence rather than on absence of evidence.

      **`instanceRole` is kept in the API as a reserved-but-rejected value**, mirroring how Azure
      already treats `managedIdentity`. Removing it outright would make a future addition a
      breaking enum change; reserving it means enabling it later only relaxes a validation rule.
      *Controller Workflow* step 5 no longer implements a keyless path (CRD validation rejects the
      value before the controller sees it), the status contract no longer carries an empty-
      fingerprint special case, and the IRSA sample was dropped from the task list.

      Knock-on effect worth flagging: *Requirement Review* #6 recommended IRSA as the answer for
      expiring STS tokens. With keyless now ruled out rather than merely postponed, **v1 has no
      answer for temporary credentials** — an expired `sessionToken` stays expired until something
      external updates the Secret. That is now stated plainly rather than being papered over by a
      recommendation pointing at functionality that cannot be built as described.
- [x] **D2 — Azure managed identity (Nice 9): `Defer`.** Not attempted; requires Azure
      infrastructure. Already out of scope in the SPEC regardless of outcome. Tested version scope
      is recorded in E1.
- [x] **D3 — CSI abstractions (Nice 10): `Defer` — see Session 5 below** for the written position.
      Confirmed it did not block the MarkLogic-native credential path, which is fully validated
      independently of it.

### E. Wrap up the research story

- [ ] **E1 — Broaden the evidence base.** All sessions ran against a single image
      (`12.0.3-ubi9-rootless-2.2.6`) in a single standalone Docker container. **JIRA Must 1**
      asks for behavior "on the MarkLogic image versions supported by the operator" (plural).
      Re-run the Session 2 and Session 3 checks against the other supported image versions, or
      record explicitly that findings are scoped to this one version.
- [ ] **E2 — Multi-host sanity check.** Every test used a single self-initialized host. Confirm the
      cluster-wide claim holds on a real multi-host cluster: apply against the bootstrap host and
      `GET` from a non-bootstrap host to verify the credential set propagates. This underpins the
      SPEC's central "reconcile once against the bootstrap host" design choice, which is currently
      taken from documentation rather than observation.
- [x] **E3 — Capture the POC code.** **Decision: the commands in Sessions 1–4 of this document are
      the deliverable.** They are complete, copy-pasteable, and carry their observed output inline,
      which an extracted script would not. Extracting them into a separate harness would add a file
      to maintain for throwaway research the JIRA explicitly says need not ship. The only
      credentials appearing here are the throwaway local ones (`admin:admin123`, `Passw0rd123!`)
      and obviously fake key material (`AKIATESTKEY...`); no real secrets are present.
- [ ] **E4 — Tear down the research environment.** Remove the `ml-objstor-poc` container and the
      test roles/users (`objstor-manageadmin-user`, `objstor-manageadmin-security-user`,
      `objstor-privonly-aws-user`, and their roles). **Do this last** — E1 and E2 still need a live
      instance, so the container should survive until those are either done or explicitly dropped.
- [ ] **E5 — Refresh the matrix and close.** After A–D, update the Findings Matrix so no row reads
      "Not attempted" or "Partially informed" without an accompanying decision, then mark the
      research story complete and unblock the implementation stories in the SPEC's *Task Breakdown*.
