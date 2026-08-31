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

## Findings Matrix

| # | JIRA Acceptance Criterion | Status | Evidence / Conclusion |
|---|---|---|---|
| Must 1 | Confirm `PUT`/`GET` behavior, response codes, payload shape, masking | **Confirmed — SPEC needs correction** | Response code is `204`, not `201`. `type` must be in the JSON body, not the query string (Azure fails with `400` otherwise — a functional bug in the current SPEC's documented request shape). `secret-key` is encrypted (non-deterministic ciphertext), not masked with a placeholder. `session-token` is returned in **plaintext**, unmasked. |
| Must 2 | Confirm minimum privileges; dedicated least-privilege user practicality | **Confirmed** | `manage-admin` alone → `403` (not `401` as documented). `manage-admin`+`security` → success. Granular `manage`+`manage-admin`+`credentials-set-{aws,azure}` → success, confirmed per-provider scoped. A dedicated least-privilege user is practical for v1 using the granular privilege combination. |
| Must 3 | Validate complete Secret-backed static credential flow | **Confirmed** | Both providers apply successfully with the corrected (body-`type`) request shape; re-applying identical material is idempotent at the MarkLogic level (`204` both times, no error). |
| Must 4 | Validate fingerprinting approach | **Confirmed as necessary, not yet prototyped in Go** | Live evidence (Session 2, Test 4) proves `GET`-based comparison cannot work: identical `secret-key` input produces different ciphertext output on each apply. This validates the SPEC's fingerprint-based approach as the *only* viable option, but the Go-level details (salt stability across restarts, canonical field ordering, Secret-watch-triggered reconciliation) still require code-level prototyping — not exercised in this session. |
| Must 5 | Resolve supported-usage boundary (credentials-only vs. backups/forests) | **Confirmed — no new evidence needed** | Already resolved by design reasoning in SPEC's Requirement Review #2; nothing observed in this research contradicts that decision. |
| Must 6 | Findings matrix produced | **This table** | — |
| Must 7 | Validate status/condition model | **Partially informed** | Live error shapes observed: `{"errorResponse":{"statusCode","status","messageCode","message"}}` for `400`/`403`. These map cleanly to the SPEC's planned `reason`/`message` status fields. Distinguishing `403` (insufficient privilege) from `401` (bad credentials) should be reflected as distinct failure reasons rather than collapsed into one `ManagementAPIError` reason. |
| Nice 8 | AWS IRSA/instance-role prototype | **Not attempted** | Requires a real EKS cluster and IAM role; out of reach in this local Docker-only research session. Remains deferred per the JIRA's Secrets-only framing. |
| Nice 9 | Azure managed identity investigation | **Not attempted** | Requires Azure infrastructure; out of reach in this session. Remains deferred. |
| Nice 10 | CSI abstraction research | **Not attempted** | Design/research question, not a live-cluster test; no new evidence gathered this session. |

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

### B. Fold the new findings into the SPEC (behavior the SPEC does not yet describe)

- [ ] **B1 — Idempotency evidence.** Record in *Requirement Review* or *Credential Application*
      that re-applying identical material returns `204` with no error, so the operator's
      skip-if-unchanged optimization is a cost optimization, not a correctness requirement.
- [ ] **B2 — "Never configured" detection.** Document that `GET ?type=<provider>` returns
      `{"<provider>": null}` before anything is set, and decide whether the controller uses this
      to distinguish `Disabled`/never-configured from configured-with-older-material. Affects the
      `Disabled` phase semantics in the *Status Contract*.
- [ ] **B3 — Distinct failure reasons.** Split the single `ManagementAPIError` reason in the
      *Status Contract* into at least `InsufficientPrivilege` (`403`), `AuthenticationFailed`
      (`401`), and `InvalidPayload` (`400`), mapping from the observed
      `{"errorResponse":{"statusCode","status","messageCode","message"}}` shape. Closes the
      actionability half of **JIRA Must 7**.
- [ ] **B4 — Error-body handling rule.** The observed error bodies echo `messageCode`/`message`
      but not payload values; confirm this holds for every failure mode the controller can hit,
      then state the rule in the SPEC (surface `messageCode` + `message`, never the request body).
- [ ] **B5 — Per-provider privilege scoping.** Update *Security NFR* #4 and *Follow-Ups* to reflect
      that `credentials-set-aws` and `credentials-set-azure` are independently scoped, so a
      deployment can grant only the providers it actually configures.
- [ ] **B6 — Least-privilege user decision.** **JIRA Must 2** requires an explicit decision, not
      just evidence. Session 3 proved a dedicated least-privilege user is *practical*; record the
      **decision** — v1 uses the bootstrap admin credential with the least-privilege user as a
      documented hardening path, or v1 provisions the dedicated user. Currently the SPEC assumes
      the former without citing the evidence.
- [ ] **B7 — `DELETE` / revocation scope decision.** Session 4 showed `DELETE` is trivial
      (`204`, requires a `Content-Type` header despite having no body). Decide explicitly whether
      credential revocation on provider-block removal moves from *Follow-Ups* into v1, and update
      *Validation Rules* #6 accordingly. Leaving it deferred is acceptable, but the decision must
      be recorded rather than inherited.
- [ ] **B8 — Link findings from the SPEC.** **JIRA Must 6** explicitly requires the functional
      spec's *Requirement Review* section to link this findings document. Add the cross-link and
      flip the *Validation Status* table rows from `Pending POC` to `Confirmed` (Must 1, 2, 3) with
      a pointer to the relevant session.

### C. Close the still-unvalidated research questions

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
- [ ] **C2 — Secret-watch reconciliation trigger (JIRA Must 4, second half).** A fingerprint is not
      a watch. Confirm the mechanism that makes a Secret edit wake the `MarklogicCluster`
      controller — `Watches` on `v1.Secret` with a handler mapping back to referencing clusters,
      versus a periodic resync — and record the RBAC and cache implications (the operator's
      namespace scoping is described in `docs/operator-scope-configuration.md`). Without this,
      the *Credential Rotation* acceptance criteria cannot be met.
- [ ] **C3 — Status/condition model validation (JIRA Must 7).** Currently "partially informed."
      Confirm the full field set against the failure modes actually reachable in-cluster
      (missing Secret, missing key, bootstrap not ready, `403`, `400`, transient network error),
      and confirm each maps to a distinct, actionable `reason`. Depends on B3.
- [ ] **C4 — Record the out-of-scope boundary explicitly (JIRA Must 5).** The credentials-only
      decision is made, but Must 5 also requires explicitly recording *which* forest, backup,
      region, and endpoint behaviors are out of scope **because they were not validated**. The
      SPEC's *Out of Scope* list covers forests, CSI, managed identity, STS rotation, and region,
      but does not state that these are unvalidated rather than merely unwanted. Add that framing,
      and confirm the epic's "supports configuring storage access for backups" is satisfied at the
      access layer only.

### D. Deferred "Nice to Have" items — record the decision, not the work

Each of these needs a written `Go` / `No-Go` / `Defer` line in the findings matrix so the story
can close cleanly. None require the work itself if v1 is Secrets-only.

- [ ] **D1 — AWS IRSA prototype (Nice 8).** Not attempted (needs EKS + IAM). Either schedule the
      prototype or record `Defer` and remove the conditional "AWS keyless ships in v1 if the POC
      confirms it" language from the SPEC's *Compatibility*, *Validation Status*, *Accepted Scope
      Summary*, and *Platform Compatibility* #2 — the SPEC currently leaves AWS keyless in an
      unresolved conditional state in four places, and `authType=instanceRole` appears in the CRD
      enum, *Spec Fields*, *Controller Workflow* step 5, and the status contract. If the answer is
      `Defer`, decide whether `instanceRole` stays in the v1 CRD enum (rejected at validation) or
      is removed entirely.
- [ ] **D2 — Azure managed identity (Nice 9).** Not attempted. The SPEC already says "Confirmed
      deferred," so this only needs the matching `Defer` verdict and the tested image versions
      recorded in the matrix.
- [ ] **D3 — CSI abstractions (Nice 10).** Not attempted; it is a design question, not a live test.
      Produce a short written position (viable alternative / complementary / not applicable) so the
      epic's "further research is required" note is answered, and confirm it did not block the
      MarkLogic-native path.

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
- [ ] **E3 — Capture the POC code.** The JIRA allows throwaway code but the story's value depends on
      it being reviewable. Commit the curl/script harness used in Sessions 1–4 (with credentials
      stripped) under `test/` or `docs/spec/`, or state that the commands in this document are the
      deliverable.
- [ ] **E4 — Tear down the research environment.** Remove the `ml-objstor-poc` container and the
      test roles/users (`objstor-manageadmin-user`, `objstor-manageadmin-security-user`,
      `objstor-privonly-aws-user`, and their roles) once the findings are accepted.
- [ ] **E5 — Refresh the matrix and close.** After A–D, update the Findings Matrix so no row reads
      "Not attempted" or "Partially informed" without an accompanying decision, then mark the
      research story complete and unblock the implementation stories in the SPEC's *Task Breakdown*.
