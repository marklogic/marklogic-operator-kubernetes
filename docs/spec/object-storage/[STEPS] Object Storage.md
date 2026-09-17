# Object Storage Credentials Research — Log and Findings

Research log for the "Object Storage Credentials Research and Prototype" story
(`docs/spec/object-storage/[JIRA]Ojbect Storage.md`), validating the assumptions recorded in
`docs/spec/object-storage/[SPEC]Object Storage.md`. This document is a running log: each
session records what was run, the raw evidence observed, and the conclusion.
The findings matrix summarizes the outcome per acceptance criterion. Sessions and change
batches below are historical evidence, not current setup instructions. In particular, the
initial exclusion of STS tokens was superseded by optional `sessionToken` pass-through.
The functional spec is the current contract; tokens must be refreshed externally.

Review correction (2026-09-16): structured Management API error fields can echo submitted
secrets just like raw response bodies. The client now discards both `message` and
`messageCode` and reports HTTP status only. Older descriptions below record the original
implementation, which did not satisfy that guarantee. The public UID salt does not prevent
offline credential guessing; it only scopes digest equality to a cluster.

Environment for this research:

- Docker (Rancher Desktop) running locally on macOS.
- Image: `progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6` (the same
  image pinned in the operator's `Makefile` `E2E_MARKLOGIC_IMAGE_VERSION`).
- No Kubernetes/operator involved in the live API experiments. Sessions 1–6 use
  one container; Session 7 uses two hosts and Session 8 adds MinIO.

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

1.  **It solves a different problem.** An object-storage filesystem adapter, optionally exposed through CSI, presents a
    bucket as a filesystem mount with driver-specific semantics; full POSIX behavior is not implied. MarkLogic's native S3/Azure support instead speaks the object APIs
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

## Session 7 — E2: multi-host propagation, confirmed on a real two-host cluster

Closes Todo E2. Until now every test ran against a single self-initialized host, so the SPEC's
central design choice — "credentials are cluster-wide, reconcile once against the bootstrap host"
— rested on documentation rather than observation.

### Environment

Two containers of the same pinned image on a user-defined Docker network:

```sh
docker network create ml-objstor-net

docker run -d --name ml-node1 --hostname ml-node1 --network ml-objstor-net --dns-search=. \
  -p 18000-18002:8000-8002 \
  -e MARKLOGIC_INIT=true -e MARKLOGIC_ADMIN_USERNAME=admin -e MARKLOGIC_ADMIN_PASSWORD=admin123 \
  progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6

docker run -d --name ml-node2 --hostname ml-node2 --network ml-objstor-net --dns-search=. \
  -p 18010-18012:8000-8002 \
  -e MARKLOGIC_INIT=true -e MARKLOGIC_JOIN_CLUSTER=true -e MARKLOGIC_BOOTSTRAP_HOST=ml-node1 \
  -e MARKLOGIC_ADMIN_USERNAME=admin -e MARKLOGIC_ADMIN_PASSWORD=admin123 \
  progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6
```

Both nodes then report the same two-host cluster (`ml-node1` marked `bootstrap`, `ml-node2`).

**`--dns-search=.` is required and the reason is worth recording.** On the first attempt the host
machine's DHCP search domain leaked into the containers, so MarkLogic registered itself as
`ml-node2.libpubwifi.aclibrary.org`. That FQDN does not resolve on the Docker network, and the
node wedged rather than failing fast:

```
SVC-SOCHN: Socket hostname error: getaddrinfo ml-node2.libpubwifi.aclibrary.org: Name or service not known
...
Warning: Hung 247 sec
Warning: Canary thread sleep was 190180 ms
```

The join had already been recorded on the bootstrap, so `/manage/v2/hosts` listed two hosts while
the second was unusable — a *partially joined* state that looks healthy from the bootstrap's point
of view. Worth remembering when diagnosing operator-managed clusters: host count alone is not a
liveness signal.

### Results

| # | Action | Observed |
|---|---|---|
| 1 | Baseline read on both nodes | `{"aws":null}` / `{"azure":null}` on both |
| 2 | `PUT` aws on **bootstrap** | `204`; node2 immediately returns `access-key: AKIAPROPAGATION01` |
| 3 | `PUT` azure on **bootstrap** | `204`; node2 immediately returns `storage-account: propagationacct` |
| 4 | `PUT` aws on **non-bootstrap** | `204`; **node1 returns `access-key: AKIAREVERSE00002`** |
| 5 | `DELETE` aws on bootstrap | `204`; node2 returns `{"aws":null}`, while azure remains set |

### Conclusions

1.  **Credentials are genuinely cluster-wide.** Writing on one host is visible on the other for
    both providers, with no propagation delay observable at a 2-second granularity. The SPEC's
    "one credential set per provider per cluster" model is confirmed against real multi-host
    behavior, not just documentation.
2.  **`DELETE` propagates too**, and clears only the targeted provider — Azure survived the AWS
    delete. Provider independence now holds on a multi-host cluster, not only single-host.
3.  **Any host accepts the write, not just the bootstrap.** Step 4 is the surprise: a `PUT` to the
    non-bootstrap host returned `204` and propagated back to the bootstrap. The SPEC currently
    *mandates* reconciling against the bootstrap host. That is a safe default and worth keeping —
    it matches the existing `Ensure*` security operations and keeps behavior predictable — but it
    is now known to be a **convention rather than a MarkLogic requirement**. See the follow-up
    note below.
4.  **Ciphertext is stable across reads.** The same stored `storage-key` returned an identical
    blob in steps 3 and 5. This refines the Session 2 finding: the non-determinism comes from
    *re-encryption on each write*, not from the read path. Fingerprinting is still required, but
    the reason is precisely "every write re-encrypts", not "reads are unstable".

### Follow-up worth considering (not a v1 change)

Because any host accepts the write, a future hardening could fall back to another host when the
bootstrap is unreachable, instead of parking the provider in `phase=Pending` with
`reason=BootstrapNotReady`. That would make object storage configuration resilient to bootstrap
downtime. Deliberately **not** proposed for v1: it widens the failure surface and diverges from
how every other `Ensure*` operation in the operator behaves. Recorded here so the option is not
lost.

---

## Session 8 — Access layer: credentials actually work (MinIO)

Every earlier session proved only that MarkLogic *accepted* a credential write. Sessions 2–3 used
fabricated keys like `AKIATESTKEY1234567`, which the endpoint stored happily because it does not
validate them. Nothing had confirmed the credentials enable real object I/O — which is the whole
premise of the epic. This session closes that gap using MinIO, at zero cloud cost.

### Setup

```sh
docker run -d --name minio --network ml-objstor-net --network-alias ml-backup.minio --dns-search=. \
  -p 19000:9000 -p 19001:9001 \
  -e MINIO_ROOT_USER=mlaccesskey -e MINIO_ROOT_PASSWORD=mlsecretkey123 \
  minio/minio server /data --console-address ":9001"

docker run --rm --network ml-objstor-net --entrypoint sh minio/mc -c \
  "mc alias set local http://minio:9000 mlaccesskey mlsecretkey123 && mc mb local/ml-backup"
```

Group settings redirect MarkLogic from real S3 to MinIO:

```sh
curl --anyauth -u admin:admin123 -X PUT -H 'Content-type: application/json' \
  -d '{"s3-domain":"minio:9000","s3-protocol":"http","s3-server-side-encryption":"none"}' \
  http://localhost:18002/manage/v2/groups/Default/properties
```

### The proof

| # | Action | Result |
|---|---|---|
| 1 | Apply MinIO keys via the credentials API | `204` |
| 2 | `xdmp:save("s3://ml-backup/hello.xml", <hello>world</hello>)` | Succeeded |
| 3 | `mc ls local/ml-backup` | `20B STANDARD hello.xml` |
| 4 | `xdmp:document-get("s3://ml-backup/hello.xml")` | `<hello>world</hello>` |
| 5 | **Control:** `DELETE` credentials, retry the write | `SVC-AWSCRED: ... No AWS security credentials` |
| 6 | Restore credentials, write again | Succeeded — `after-restore.xml` in the bucket |

Steps 5 and 6 are what make this a causal proof rather than a coincidence: with the credential set
removed the identical operation fails, and restoring it makes the operation work again. The
credentials configured through `PUT /manage/v2/credentials/properties` are demonstrably what
enables object I/O.

### Findings

1.  **The access layer is confirmed.** The epic's core promise — configure credentials, get usable
    object storage — holds. Combined with Session 7's propagation result, the operator's model is
    validated end to end at both the configuration and access layers.

2.  **The no-credentials control fails with `SVC-AWSCRED`.** This local Docker experiment
    had no IRSA token or role configuration, so it does not independently test web identity.
    The v1 decision remains based on the documentation/image evidence in Session 6.

3.  **MarkLogic uses virtual-host-style S3 addressing.** The first backup attempt failed because
    MarkLogic resolves `<bucket>.<s3-domain>` — `ml-backup.minio` — not `<s3-domain>/<bucket>`.
    Reaching `minio:9000` worked while `ml-backup.minio:9000` did not, and adding a Docker network
    alias for the bucket hostname fixed it. Anyone testing against an S3-compatible store (MinIO,
    Ceph, LocalStack) must provide bucket-style DNS. Worth documenting for users who set
    `s3-domain` to a private endpoint.

4.  **Endpoint configuration lives in group settings, not the credentials API.** `s3-domain`,
    `s3-protocol`, `s3-proxy`, `s3-server-side-encryption`, and `s3-server-side-encryption-kms-key`
    are all group-level. This confirms the SPEC's Requirement Review #5 position that region and
    endpoint wiring is an environment concern, and names the exact knobs.

5.  **The default `s3-server-side-encryption: aes256` breaks non-AWS stores.** It must be set to
    `none` for MinIO. Not a v1 concern since the operator does not touch group settings, but it is
    a real trap for anyone pointing MarkLogic at an S3-compatible endpoint.

6.  **Backups need more than credentials.** `POST {"operation":"backup-database"}` to
    `s3://ml-backup/backup1` failed with `No such directory:
    s3://ml-backup/backup1/Forests/Documents` even with working credentials and connectivity —
    MarkLogic expects the backup directory structure to already exist. This supports the SPEC's
    Requirement Review #2 scoping decision: credential configuration is the prerequisite the
    operator owns, while backup setup remains an administrator task. Object I/O itself is
    unambiguously working, as steps 2–4 show.

---

## Findings Matrix

| # | JIRA Acceptance Criterion | Status | Evidence / Conclusion |
|---|---|---|---|
| Must 1 | Confirm `PUT`/`GET` behavior, response codes, payload shape, masking | **Confirmed — SPEC corrected** | Response code is `204`, not `201`. `type` must be in the JSON body, not the query string (Azure fails with `400` otherwise — a bug in the original SPEC request shape). `secret-key` is encrypted (non-deterministic ciphertext), not masked with a placeholder. `session-token` is returned in **plaintext**, unmasked. |
| Must 2 | Confirm minimum privileges; dedicated least-privilege user practicality | **Confirmed** | `manage-admin` alone → `403` (not `401` as documented). `manage-admin`+`security` → success. Granular `manage`+`manage-admin`+`credentials-set-{aws,azure}` → success, confirmed per-provider scoped. A dedicated least-privilege user is practical for v1 using the granular privilege combination. |
| Must 3 | Validate complete Secret-backed static credential flow | **Confirmed** | Both providers apply successfully with the corrected (body-`type`) request shape; re-applying identical material is idempotent at the MarkLogic level (`204` both times, no error). |
| Must 4 | Validate fingerprinting approach | **Confirmed and implemented** | Live evidence (Session 2, Test 4) proves `GET`-based comparison cannot work: identical `secret-key` input produces different ciphertext output on each apply, making fingerprinting the *only* viable option. Both halves are now built and tested: the salted digest ([pkg/objectstorage](../../../pkg/objectstorage/fingerprint.go), salt from `metadata.uid`, HMAC-SHA256 with length-prefixed canonical encoding) and the Secret watch that triggers reconciliation ([cluster controller](../../../internal/controller/marklogiccluster_controller.go)). See Todos C1 and C2. |
| Must 5 | Resolve supported-usage boundary (credentials-only vs. backups/forests) | **Confirmed** | Resolved by design reasoning in SPEC's Requirement Review #2; nothing observed here contradicts it. The SPEC's *Out of Scope* list now also states that the forest, backup, region, and endpoint behaviors are excluded as **unvalidated**, not merely unwanted, as Must 5 requires. |
| Must 6 | Findings matrix produced | **This table** | — |
| Must 7 | Validate status/condition model | **Unit-tested; full controller e2e remains** | Observed error shapes (`{"errorResponse":{"statusCode","status","messageCode","message"}}`) drove an eight-reason failure vocabulary with a retriability column in the SPEC's *Status Contract*, splitting `401` (bad admin credential) from `403` (missing MarkLogic privilege) so the most likely misconfiguration is diagnosable from status alone. A *Phase Semantics* table was added, including that `Disabled` means "unmanaged", not "unconfigured". Unit tests exercise the reason vocabulary and requeue policy (Todo C3); this does not claim a live controller e2e run. |
| Nice 8 | AWS IRSA/instance-role prototype | **`No-Go` — verified impossible** | Session 6. No EKS needed. MarkLogic's credential order has exactly three steps (security DB → env vars → IAM Role) and is **not** the AWS SDK provider chain, so empty credentials do not fall through to IRSA. The IAM Role step is gated on `MARKLOGIC_AWS_ROLE`, which the docs say is ignored when `MARKLOGIC_EC2_HOST=0` — and the operator's rootless image hard-sets exactly that. No web-identity support exists in the image or docs. Session 8 tests absence of credentials only, not an IRSA-configured environment. |
| Nice 9 | Azure managed identity investigation | **`Defer`** | Not attempted — needs Azure infrastructure. Already out of v1 scope regardless. Scoped to image `12.0.3-ubi9-rootless-2.2.6`; other supported versions untested. |
| Nice 10 | CSI abstraction research | **`Defer` — position recorded** | Session 5. CSI is complementary, not an alternative: it cannot configure MarkLogic's native object APIs, the epic's backup scenario is native-path, forest-on-mount is the unsupported pattern Requirement Review #2 already excludes, and CSI brings a second credential surface of its own. Did not block the native path. |
| — | Credential revocation via `DELETE` | **Confirmed working — deliberately deferred** | Session 4. Deferred on API-design grounds (removal is ambiguous between "revoke" and "stop managing"), not effort. |
| — | Least-privilege MarkLogic user | **Decision: use bootstrap admin in v1** | Session 3 proved a dedicated user is practical; deferred as a hardening path because the operator already holds the bootstrap admin credential, so a second user adds lifecycle surface without reducing capability. |
| — | AWS `sessionToken` (STS) support | **Implemented as optional pass-through** | Supersedes the initial exclusion below. Included in payload and fingerprint when supplied; the operator does not renew it or track expiry. MarkLogic returns it in plaintext, so verification must suppress returned values. |
| — | Access layer: do the credentials actually work? | **Confirmed against MinIO** | Session 8. Credentials applied through the API enable real object I/O: write, list, and read all succeed. Removing the credential set makes the identical write fail with `SVC-AWSCRED`, and restoring it makes it work again — a causal proof, not a coincidence. |
| — | Cluster-wide propagation (multi-host) | **Confirmed on a real two-host cluster** | Session 7. Writes on the bootstrap are visible on the non-bootstrap host for both providers, with no observable delay; `DELETE` propagates too and clears only the targeted provider. Also found: **any host accepts the write**, not just the bootstrap — so targeting the bootstrap is a convention, not a MarkLogic requirement. |
| — | Fingerprint salt source | **Decision: derive from `MarklogicCluster.metadata.uid`** | Stable across restarts and replicas, unique per cluster (prevents direct cross-cluster digest equality), no extra resource. Build-time constant rejected (shared across installs; upgrade invalidates all fingerprints → mass re-apply); generated Secret rejected (extra resource to manage and back up). |

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

The checked items below are complete; each records its evidence and remaining e2e limits.

- [x] **C1 — Fingerprinting prototype (JIRA Must 4).** Implemented as real, tested code in
      [pkg/objectstorage/fingerprint.go](../../../pkg/objectstorage/fingerprint.go) with
      [tests](../../../pkg/objectstorage/fingerprint_test.go). All pass under `go test ./pkg/objectstorage/...`.

      **Salt source: `MarklogicCluster.metadata.uid`.** Stable across controller restarts and
      replicas, unique per cluster (so it also prevents direct cross-cluster digest equality of identical
      credentials), and needs no extra resource. A build-time constant was rejected because it is
      shared across installations and would invalidate every stored fingerprint on operator
      upgrade, triggering a simultaneous re-apply across all managed clusters; a generated Secret
      was rejected as an extra resource to create, grant RBAC for, and back up, whose loss causes
      the same mass re-apply.

      Design decisions made while implementing:
      - **HMAC-SHA256, not `SHA256(salt || data)`.** The salt is the HMAC key, which sidesteps
        length-extension concerns and is the standard construction for keyed digests.
      - **Length-prefixed encoding.** Naive concatenation is not injective: `{"ab": "c"}` and
        `{"a": "bc"}` would produce the same digest. Each key and value is prefixed with its
        length. There is a regression test for exactly this collision.
      - **Provider is mixed into the digest**, so AWS and Azure material that happens to share
        values cannot collide.
      - **Canonical ordering by sorted key**, since Go randomises map iteration order per range —
        the determinism test loops 100 times to actually exercise that.
      - **`String()` returns `[REDACTED]`** on both material types. This is a stronger guarantee
        than "remember not to log it": `%v`, `%+v`, `%s`, and pointer forms are all covered by
        `fmt.Stringer`, and there is a test asserting every one of them.

      Mitigating context from B1: because MarkLogic accepts redundant re-applies cleanly, a
      fingerprint defect degrades to extra writes and noisy rotation events rather than data loss.
- [x] **C2 — Secret-watch reconciliation trigger (JIRA Must 4, second half).** Implemented in
      [internal/controller/marklogiccluster_controller.go](../../../internal/controller/marklogiccluster_controller.go)
      with [tests](../../../internal/controller/marklogiccluster_secret_watch_test.go). A fingerprint is not
      a watch; this is the mechanism that makes a Secret edit wake the controller.

      **Finding 1 — the watch is nearly free.** The concern was that watching Secrets would force
      controller-runtime to cache every Secret in scope, which in the operator's default
      cluster-scoped mode ([cmd/main.go](../../../cmd/main.go)) means every Secret in the cluster. It turns
      out that cost is **already being paid**: [pkg/k8sutil/secret.go](../../../pkg/k8sutil/secret.go)
      already calls `client.Get` on `corev1.Secret` through the manager's cached client, which
      starts a Secret informer regardless. The watch adds event handling, not cache footprint.

      **Finding 2 — no RBAC change needed.** [config/rbac/role.yaml](../../../config/rbac/role.yaml) already
      grants `watch` on `secrets`.

      **Finding 3 — a trap that would have shipped silently.** The controller applies its predicate
      via `WithEventFilter`, which is **global to every watch**, and its `UpdateFunc` ends in
      `default: return false`. Adding `Watches(&corev1.Secret{}, ...)` on its own would therefore
      have compiled, looked correct, and **never fired on rotation** — the one event it exists to
      catch — because Secret updates fall through to the default branch. An explicit
      `case *corev1.Secret:` was required. The group controller hit the same thing and carries a
      `case *corev1.Pod:` for the same reason. There is a dedicated regression test naming this.

      **Design notes.** The predicate compares only `Data`, so metadata-only Secret churn
      (resourceVersion bumps, label edits) does not trigger reconciles. The mapper filters to
      clusters that actually reference the Secret and is backed by the cache, so it costs no API
      calls. It currently matches `spec.auth.secretName`; object storage Secret names join the same
      helper (`clusterReferencesSecret`) when `spec.objectStorage` lands.

      **Behavior change to be aware of:** rotating the admin auth Secret now triggers a cluster
      reconcile where previously it did not. That is desirable and reconcile is idempotent, but it
      is a change to existing shipping behavior, not purely additive groundwork.
- [x] **C3 — Status/condition model validation (JIRA Must 7).** Closed. The full reason vocabulary
      is exercised by unit tests over the reconcile (`objectstorage_reconcile_test.go`) covering
      `SecretNotFound`, `SecretKeyMissing`, `BootstrapNotReady`, `InsufficientPrivilege`,
      `InvalidPayload`, `AuthenticationFailed`, `ManagementAPIUnreachable`, and
      `ManagementAPIError`, plus the requeue policy for each. The live tests in
      `credentials_live_test.go` confirm the `401` mapping against a real server. Remaining
      verification of the full controller loop belongs to e2e, not this research story.
- [x] **C4 — Record the out-of-scope boundary explicitly (JIRA Must 5).** Done. The SPEC's
      *Out of Scope* list now states that forest, backup, region, and endpoint behaviors are
      excluded **because they were not validated**, not merely because they are unwanted, and
      confirms the epic's "supports configuring storage access for backups" is satisfied at the
      *access* layer only. Revocation and AWS keyless were added to the same list.

### D. Deferred "Nice to Have" items — decisions recorded

Each needed a written `Go` / `No-Go` / `Defer` verdict so the story can close. **D1 is `No-Go`
(ruled out on evidence); D2 and D3 are `Defer`.** The SPEC has been made internally consistent
with those outcomes.

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
- [x] **D3 — CSI abstractions (Nice 10): `Defer` — see Session 5 above** for the written position.
      Confirmed it did not block the MarkLogic-native credential path, which is fully validated
      independently of it.

### E. Wrap up the research story

- [x] **E1 — Broaden the evidence base.** **Decision: findings are scoped to one image version, and
      that is sufficient.** The repo pins `12.0.3-ubi9-rootless-2.2.6` in the `Makefile`, both CRD
      defaults, and every sample; the only other version in play is the `latest-12` nightly used by
      Jenkins. So "the image versions supported by the operator" is effectively one version plus a
      moving nightly, and a version matrix would add little. Re-running Sessions 2, 3, 7, and 8
      against a new default image is worthwhile whenever that default is bumped.
- [x] **E2 — Multi-host sanity check.** Done — see Session 7. A real two-host cluster confirms
      credentials set on the bootstrap are readable from the non-bootstrap host for both
      providers, that `DELETE` propagates, and that provider independence holds across hosts.
      Surfaced one design-relevant surprise: **any host accepts the write**, so "reconcile against
      the bootstrap" is a convention rather than a MarkLogic constraint.
- [x] **E3 — Capture the POC code.** **Decision: the commands in Sessions 1–4 of this document are
      the deliverable.** They are complete, copy-pasteable, and carry their observed output inline,
      which an extracted script would not. Extracting them into a separate harness would add a file
      to maintain for throwaway research the JIRA explicitly says need not ship. The only
      credentials appearing here are the throwaway local ones (`admin:admin123`, `Passw0rd123!`)
      and obviously fake key material (`AKIATESTKEY...`); no real secrets are present.
- [x] **E4 — Tear down the research environment.** Done. `ml-node1`, `ml-node2`, the `minio`
      container, and the `ml-objstor-net` network are removed. The test roles and users from
      Session 3 went with the original `ml-objstor-poc` container, which no longer existed by the
      time Session 7 rebuilt the environment. Unrelated pre-existing containers on the host were
      left untouched.
- [x] **E5 — Refresh the matrix and close.** Done. Every Findings Matrix row now carries a verdict
      or a decision; no row reads "Not attempted" or "Partially informed" without one. All seven
      **Must** criteria are met and all three **Nice to Have** items have written verdicts
      (`No-Go`, `Defer`, `Defer`). **The research story is complete.** Implementation proceeded in
      parallel and the SPEC's Task Breakdown is now finished apart from the optional aggregate
      `ObjectStorageReady` condition.

---

## Change Inventory (draft commit)

Recorded so the draft commit can be re-cut cleanly later. Everything below landed in a **single
draft commit**; the intended final split is given at the end.

### Documentation

| File | Change |
|---|---|
| `docs/spec/object-storage/[SPEC]Object Storage.md` | Corrections A1–A4, findings B1–B8, scope decisions D1–D3, the `sessionToken` removal, and a new *Fingerprinting* section. |
| `docs/spec/object-storage/[STEPS] Object Storage.md` | Sessions 5 and 6, the Remaining Todos list (A–E), decision rows in the Findings Matrix, and this inventory. |

Substantive SPEC edits, by section:

- **Compatibility** — rewritten. AWS keyless is `No-Go` (mechanism does not exist), not deferred.
- **Requirement Review** — #3 privileges + least-privilege decision; #4 ciphertext not masking;
  #6 `sessionToken` excluded; new #9 (redundant re-apply is safe) and #10 (`null` = never
  configured).
- **Validation Status** — every row re-verdicted; new rows for Secret-watch, multi-host
  propagation, the status model, and revocation.
- **Background** — `204` not `201`; body `type` not query string; `403` not `401`;
  *Keyless Access* replaced by *Credential Resolution Order (and why keyless is unavailable)*.
- **Requirements** — `sessionToken` removed from the Secret key contract and spec fields;
  `instanceRole` reserved-but-rejected; Out of Scope expanded with the unvalidated framing.
- **Status Contract** — new *Phase Semantics* and eight-row *Failure Reasons* tables.
- **Fingerprinting** — new section: why salted, salt source, rejected alternatives, computation.
- **Controller Workflow** — step 4 `204`; step 5 no longer a keyless path; step 6 error-body rule.
- **Task Breakdown** — items 2, 3, 4, 7, 8 updated; implemented items ticked.

### New code

| File | Purpose |
|---|---|
| `pkg/objectstorage/fingerprint.go` | `Fingerprint()`, `AWSMaterial`, `AzureMaterial`. HMAC-SHA256 keyed on the cluster UID, length-prefixed canonical encoding, provider mixed into the digest, `String()` → `[REDACTED]`. |
| `pkg/objectstorage/fingerprint_test.go` | Determinism (100 iterations, to defeat Go's randomised map ordering), per-field change detection, salt scoping, provider scoping, boundary-collision regression, no-leak assertions, redaction across `%v`/`%+v`/`%s`/pointer forms. |
| `internal/controller/marklogiccluster_secret_watch_test.go` | Mapper tests (referencing / unreferenced / cross-namespace / non-Secret input) and predicate tests, including the regression guard for the `WithEventFilter` trap. |

### Modified code

`internal/controller/marklogiccluster_controller.go`:

1.  `Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToMarklogicClusters))`.
2.  `secretToMarklogicClusters` — cache-backed `List` scoped to the Secret's namespace, filtered
    to clusters that actually reference it.
3.  `clusterReferencesSecret` — currently matches `spec.auth.secretName` only; the extension point
    for `spec.objectStorage.<provider>.secretName`.
4.  `case *corev1.Secret:` in the predicate's `UpdateFunc`, comparing `Data` only.
5.  Imports: `corev1`, `types`, `handler`, `reconcile`.

### Behavior change (call out in the final commit)

Rotating the Secret named by `spec.auth.secretName` now triggers a `MarklogicCluster` reconcile
where it previously did not. Reconcile is idempotent so this is safe, but it alters existing
shipping behavior rather than only adding new paths — it is the one part of this change with
runtime impact on current users.

### What was *not* changed

- No API types added — `spec.objectStorage` does not exist yet, so no CRD regeneration, no
  `zz_generated.deepcopy.go` churn, and no `make manifests` run was needed.
- No RBAC change — `config/rbac/role.yaml` already grants `watch` on `secrets`.
- No `pkg/mlmanage` client work — `EnsureAWSCredentials` / `EnsureAzureCredentials` remain unbuilt.
- No Helm chart, sample, or `config/` changes.

### Verification run

```sh
go build ./...
go vet ./internal/controller/... ./pkg/objectstorage/...
go test ./pkg/objectstorage/... -count=1
go test ./internal/controller/... -run 'TestSecretToMarklogicClusters|TestClusterPredicate' -count=1
```

All passed. The `-run` filter is deliberate: it keeps the envtest-backed Ginkgo suite (`TestAPIs`)
out of the loop, since these are pure unit tests using the fake client.

### Second batch — API types (after the draft commit)

Task Breakdown item 1, landed after `c50adb1`:

| File | Change |
|---|---|
| `api/v1/objectstorage_types.go` | New. `ObjectStorageConfig`, `AWSObjectStorage`, `AzureObjectStorage`, `ObjectStorageStatus`, `ObjectStorageProviderStatus`, the `ObjectStoragePhase` and `ObjectStorageFailureReason` enums, and `ReferencedSecretNames()`. |
| `api/v1/objectstorage_types_test.go` | New. Table tests for `ReferencedSecretNames`. |
| `api/v1/marklogiccluster_types.go` | `spec.objectStorage` and `status.objectStorage` wired in. |
| `api/v1/zz_generated.deepcopy.go` | Regenerated. |
| `config/crd/bases/...marklogicclusters.yaml` | Regenerated. |
| `charts/.../templates/marklogiccluster-crd.yaml` | Regenerated, +148 lines. |
| `internal/controller/marklogiccluster_controller.go` | `clusterReferencesSecret` now also matches provider Secrets. |
| `internal/controller/marklogiccluster_secret_watch_test.go` | Added coverage for object storage Secret rotation. |

Two notes worth keeping:

- **`make manifests` is not enough.** The Helm chart ships its own copy of the CRDs under
  `charts/marklogic-operator-kubernetes/templates/`, regenerated only by `make helm` (which runs
  kustomize + helmify + `hack/helmify-post-process.sh`). Skipping it leaves Helm installs on a
  stale schema that silently drops `spec.objectStorage` — the CRD would accept the field being
  absent and the operator would simply never see it. Verified `make helm` touched only the cluster
  CRD template and caused no other chart churn.
- **The reserved auth values are rejected with real messages, not a bare enum error.** `authType`
  permits `secret` plus the reserved value per provider, and a CEL rule rejects the reserved one
  with an explanation (for AWS, that MarkLogic does not use the AWS credential provider chain).
  A bare `Enum=secret` would have produced "Unsupported value" with no indication of why.

### Third batch — management client (Task Breakdown item 2)

| File | Change |
|---|---|
| `pkg/mlmanage/credentials.go` | New. `AWSCredentials`, `AzureCredentials`, `EnsureAWSCredentials`, `EnsureAzureCredentials`, the payload builders, and the `CredentialsError` type. |
| `pkg/mlmanage/credentials_test.go` | New. Stub-server tests for request shape, validation, status handling, leak safety, and transport failure. |
| `pkg/mlmanage/client.go` | Two methods added to the `Client` interface. |
| `pkg/k8sutil/dynamic_reconcile_test.go`, `internal/controller/marklogicgroup_controller_test.go` | Existing stubs extended to satisfy the widened interface. |

Three decisions worth keeping:

- **`doJSON`'s error could not be reused.** It embeds the raw response body in the message
  (`returned status %d: %s`). For every other endpoint that is helpful; for this one the body is a
  reflection risk, so `putCredentials` discards that error and builds a `CredentialsError` from the
  status code plus MarkLogic's `errorResponse.messageCode` / `message` only. A test proves the point
  by pointing the client at a server that echoes the request body back and asserting no credential
  material reaches `err.Error()`.
- **`CredentialsError.StatusCode == 0` means "no response".** That is what lets the controller tell
  `ManagementAPIUnreachable` apart from an HTTP-level rejection, without string-matching. The
  status-code → `ObjectStorageFailureReason` mapping deliberately stays in the controller so
  `pkg/mlmanage` does not import `api/v1`.
- **Widening `Client` costs two stub updates.** `fakeDynamicManagementClient` and
  `stubDynamicManagementClient` both implement the full interface, so each needed the two new
  methods. A narrower `CredentialsClient` interface would have avoided that and would let future
  object storage tests stub 2 methods instead of ~17 — worth reconsidering if the stub burden grows.

### Fourth batch — Secret resolution and reconcile (Task Breakdown items 3, 4, 5)

| File | Change |
|---|---|
| `pkg/k8sutil/objectstorage_reconcile.go` | New. `ReconcileObjectStorage`, per-provider resolve/fingerprint/apply, status construction, failure-reason mapping, requeue policy, and the bootstrap-host client. |
| `pkg/k8sutil/objectstorage_reconcile_test.go` | New. Fake-client tests covering both providers, skip-if-unchanged, rotation, provider independence, Secret problems, readiness gating, leak safety, reason mapping, and requeue policy. |
| `pkg/k8sutil/handler.go` | `ReconcileObjectStorage` wired into `ReconsileMarklogicClusterHandler`, after HAProxy. |

Decisions and observations:

- **Requeue is driven by the reason, not by "did it fail".** `InvalidPayload` and
  `InsufficientPrivilege` are terminal until a human intervenes, so they are not requeued;
  everything else is. Without this the operator would hot-loop against a `403` forever, which is
  the single most likely misconfiguration given Requirement Review #3.
- **Empty Secret values count as missing.** `secretValue` trims and treats blank as absent, so a
  Secret key present with an empty value yields `SecretKeyMissing` rather than being sent to
  MarkLogic and rejected as `InvalidPayload`. There is a test for the whitespace-only case.
- **Skip-if-unchanged needs the *previous* status, so status must survive the reconcile.** The
  fingerprint is compared against `status.objectStorage.<provider>.appliedFingerprint`, which means
  a status write that silently fails would turn every reconcile into a re-apply. Per B1 that is
  noisy rather than dangerous, but it is why the status update error is propagated as
  `result.Error` rather than logged and ignored.
- **The bootstrap-host FQDN is rebuilt, not read.** There is no cluster-level field holding it;
  `marklogicServer.go` computes `<group>-0.<group>.<ns>.svc.<domain>` when creating child groups,
  so the reconcile derives it the same way. If that formula ever changes, both places must change
  together — a latent coupling worth knowing about.
- **`NewObjectStorageManagementClient` is a package variable** so tests can substitute a stub,
  matching the existing `NewDynamicManagementClient` pattern.

### Fifth batch — samples, chart correction, CEL tests (Task Breakdown items 6, 7, 8)

| File | Change |
|---|---|
| `config/samples/object-storage.yaml` | New. Secret-backed AWS + Azure sample with inline guidance on the Secret keys, rotation behaviour, and why keyless is unavailable. |
| `config/samples/kustomization.yaml` | Sample registered. |
| `internal/controller/objectstorage_crd_validation_test.go` | New. Seven CEL cases against an envtest API server. |

- **The Helm task was based on a false premise and was corrected rather than done.**
  `charts/marklogic-operator-kubernetes` renders only the operator Deployment, RBAC,
  ServiceAccount, Service, and the two CRDs — it never renders a `MarklogicCluster`. Adding
  `objectStorage` values would have advertised a capability the chart does not have. Its only
  object storage responsibility is shipping the regenerated CRD schema.
- **CEL rules can only be tested against a real API server**, so these live in envtest rather than
  as unit tests. All seven cases pass: the three valid shapes, both missing-`secretName` rules, and
  both reserved auth values rejecting with their explanatory messages. The test skips cleanly if
  envtest assets are missing, so it does not break contributors without them.
  Run with `KUBEBUILDER_ASSETS="$PWD/$(bin/setup-envtest use 1.31.0 --bin-dir bin -p path)"` — the
  path must be absolute, since `go test` runs from the package directory.

**Pre-existing bug found, not introduced and not fixed here:** `kustomize build config/samples`
fails because `complete.yaml` and `minimal-production.yaml` both declare
`MarklogicCluster/ml-cluster` in namespace `prod`. Confirmed against `HEAD` with the new sample
removed, so it predates this work. Worth a separate one-line fix (rename one of them).

### Sixth batch — live integration tests and access-layer validation

| File | Change |
|---|---|
| `pkg/mlmanage/credentials_live_test.go` | New. Four tests against a real Management API, gated on `ML_MANAGE_ENDPOINT`. |

- **Placed in `pkg/mlmanage`, not `test/integration`.** The Manage app server rejects basic auth
  (`401`) and answers only digest, so the verification read needs the client's existing digest
  implementation; putting the test elsewhere would mean duplicating that crypto. `test/integration`
  is also Kubernetes-based and not referenced by any make target, so a test placed there would not
  run in the normal workflow.
- Verified against the live two-node cluster: apply-and-read for both providers, three repeated
  applies (idempotency), rotation, and a `401` control using a deliberately wrong password. The
  tests assert `access-key` round-trips, `secret-key` does **not** come back in plaintext, and no
  `session-token` is ever stored.
- Sessions 7 and 8 were recorded from these runs plus the MinIO work; see those sections for the
  propagation and access-layer evidence.

### Intended final commit split
The draft is one commit for convenience. When re-cut, prefer three, so the only change with
runtime impact is visible in the log instead of buried under a large docs diff:

1.  `record object storage credentials research findings` — both spec documents.
2.  `add salted fingerprinting for object storage credentials` — `pkg/objectstorage/`.
3.  `watch Secrets on MarklogicCluster for credential rotation` — controller + test, with the
    behavior-change note in the body.

Repo convention is `MLE-XXXXX: summary (#PR)`; the ticket number for this story still needs to be
filled in.
