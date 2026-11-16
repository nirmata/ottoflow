# Proposal: Native GitHub Webhook Trigger (Pluggable Signature Verification)

**Status**: Draft for design review. Deferred: no implementation until this design is reviewed and accepted.  
**Issue**: [#5](https://github.com/nirmata/ottoflow/issues/5)  
**Related**: [DESIGN_WEBHOOK_TRIGGER.md](DESIGN_WEBHOOK_TRIGGER.md), [Webhook Triggers](../user/tasks/triggers.md#webhook-triggers)

> Nothing in this proposal is implemented. GitHub's signature scheme handles replays differently from
> OttoFlow's (see [Replay handling](#replay-handling-needs-review)), so the design needs review before any
> code is written.

---

## Problem

The `webhook` trigger accepts one signature scheme, OttoFlow's own. `X-OttoFlow-Signature: sha256=<hex>`
is an HMAC-SHA256 over `v1:{timestamp}:{path}:{body}`, and `X-OttoFlow-Timestamp` must be within
5 minutes of the server's clock. GitHub signs webhook deliveries differently: `X-Hub-Signature-256:
sha256=<hex>` is an HMAC-SHA256 hex digest of the request body only, keyed with the webhook's secret,
and no timestamp header is sent. So a GitHub organization or repository webhook cannot call OttoFlow directly.

The documented workaround is a GitHub Actions job that builds a request body, signs it with the OttoFlow
scheme and posts it (`samples/workflows/features/github-actions-webhook.yaml`). It has costs:

1. **Per-repository duplication.** An organization-wide service, for example one that reacts to pull
   requests across many repositories, needs the Actions workflow in every repository instead of one
   organization webhook per Workflow.
2. **Latency and cost.** Each event runs an Actions job only to sign and post a request.
3. **Secret spread.** Every repository that runs the shim needs the OttoFlow signing key, as a repository
   or organization Actions secret. With native verification, each Workflow's secret lives in its GitHub
   webhook's settings and one Kubernetes Secret.
4. **No extension point.** Other providers' webhooks (for example GitLab or Bitbucket) hit the same
   wall, and each needs its own shim.

## Current webhook server

This proposal builds on the webhook server as it works today, checked against the code on `main` at
`2a6ea892`. DESIGN_WEBHOOK_TRIGGER.md gives a different request order and content-type rule; this section
follows the code.

- **Endpoint.** `POST /webhooks/{namespace}/{workflowName}` on a plain-HTTP listener in the controller
  (`--webhook-trigger-addr`; Helm `controller.webhookTrigger`, default `:8083`). TLS terminates at an
  ingress or service mesh. Only the leader replica opens the listener.
- **Request order.** Method, path, timestamp, body (capped at 1 MiB; a larger body gets 413), Workflow
  lookup, webhook trigger lookup, content type, Secret, signature, rate limit, then run creation. An
  unknown Workflow, a Workflow without a webhook trigger, a missing or unusable Secret, a bad signature
  and a missing or stale timestamp all get 401.
- **Content type.** `application/json` is required (415 otherwise) only when `celFilter`,
  `inputMapping` or `dedupKey` is set, and the body is parsed as JSON only in that case. Invalid JSON
  gets 400.
- **Secret.** Read only from the Workflow's namespace; a `secretRef.namespace` naming another
  namespace is rejected at admission and refused at request time. The key is `hmac-key` unless
  `secretRef.key` names another, and the value must be at least 32 bytes.
- **Signature.** The computed and supplied digests are compared with `hmac.Equal`.
- **Rate limit.** Per Workflow, after the signature check: by default 60 requests per minute with a
  burst of 10 (`rateLimit.requestsPerMinute` up to 3600, `rateLimit.burst` up to 100). Over the limit
  gets 429.
- **Run creation.** In order: `celFilter` (false gets 200 `filtered`; an evaluation error gets 400),
  `inputMapping` (an expression that fails is skipped and the run is still created), dedup (200
  `deduped`), the Workflow's concurrent-run limit (429), then the WorkflowRun is created (202). A
  failed create gets 500.
- **CEL.** Trigger expressions see one variable, `object`, the parsed body. Request headers are not
  visible to `celFilter`, `inputMapping` or `dedupKey`.
- **Dedup.** Applies when `dedupWindow` is set, or when `dedupKey` is set (window defaults to 10
  minutes). The key is the value of `dedupKey`, or the Workflow itself when `dedupKey` is unset or
  fails to evaluate. `dedupWindow` is at most 1 hour (admission). A key is recorded only after the
  WorkflowRun is created. Dedup state is kept in the leader's memory, so it is lost when the controller
  restarts or leadership moves.
- **Run metadata.** `status.trigger.webhookRequest` records the caller's address and a request ID that
  the server generates. No request header values are recorded.
- **Admission.** At most one webhook trigger per Workflow.
- **Metrics.** Among others, `ottoflow_webhook_requests_total{namespace,workflow,result}` and
  `ottoflow_webhook_hmac_failures_total{namespace,workflow}`.

## Proposed design

Make signature verification a per-trigger choice through a new optional `provider` field on
`WebhookTrigger`. The OttoFlow scheme stays the default; `github` is the first alternative.

### API (`api/v1alpha1/workflow_types.go`)

```go
// WebhookTrigger gains two optional fields. Existing fields are unchanged.
type WebhookTrigger struct {
    SecretRef WebhookSecretRef `json:"secretRef"`

    // Provider selects how inbound requests are verified.
    // "ottoflow" (default): X-OttoFlow-Signature over v1:{timestamp}:{path}:{body},
    //                       with the X-OttoFlow-Timestamp window.
    // "github":             X-Hub-Signature-256, HMAC-SHA256 over the raw body.
    // +kubebuilder:validation:Enum=ottoflow;github
    // +kubebuilder:default=ottoflow
    // +optional
    Provider string `json:"provider,omitempty"`

    // GitHub holds GitHub-specific filtering. Valid only when provider is github.
    // +optional
    GitHub *GitHubWebhookOptions `json:"github,omitempty"`

    // CELFilter, InputMapping, DedupKey, DedupWindow, RateLimit: unchanged.
}

// GitHubWebhookOptions filters GitHub deliveries before CEL evaluation.
type GitHubWebhookOptions struct {
    // Events allowlists X-GitHub-Event values (for example "pull_request", "push").
    // Empty accepts all events. "ping" is always answered 200 and creates no run.
    // A filter, not a security check: X-GitHub-Event is not signed.
    // +optional
    Events []string `json:"events,omitempty"`

    // Actions allowlists the payload's "action" field (for example "opened",
    // "synchronize") for events that carry one. Empty accepts all actions.
    // +optional
    Actions []string `json:"actions,omitempty"`
}
```

Example:

```yaml
spec:
  triggers:
    - webhook:
        provider: github
        secretRef:
          name: gh-webhook-secret        # key defaults to hmac-key
        github:
          events: [pull_request]
          actions: [opened, synchronize]
        celFilter: 'object.pull_request.draft == false'
        inputMapping:
          repo: object.repository.full_name
          headRef: object.pull_request.head.ref
          headSha: object.pull_request.head.sha
        dedupKey: "object.repository.full_name + '#' + string(object.pull_request.number) + '@' + object.pull_request.head.sha"
```

The GitHub webhook points at `POST https://<ingress>/webhooks/{namespace}/{workflowName}` with the same
secret as the Kubernetes Secret. No shim is needed.

One webhook URL names one Workflow, and a Workflow has at most one webhook trigger, so each Workflow that
reacts to an organization needs its own organization webhook. Each also needs its own secret (see Replay
handling). GitHub allows up to 20 webhooks per event type per organization.

### Verification (`internal/workflow/controller/webhook_server.go`)

`verifyTimestamp` stays in the handler for `provider: ottoflow` (see Order below). Move the signature
check in `handleWebhook` (`verifyHMAC`) behind a small interface:

```go
// signatureVerifier authenticates one inbound webhook request.
type signatureVerifier interface {
    // Verify returns the computed digest, with a nil error only if the request is
    // authentically signed. Implementations compare digests with hmac.Equal.
    Verify(r *http.Request, path string, body, secret []byte) (digest []byte, err error)
}

// ottoflowVerifier: today's verifyHMAC, unchanged behaviour.
// githubVerifier:   HMAC-SHA256(secret, body) against X-Hub-Signature-256 ("sha256=" + hex).
```

The handler picks the verifier from the trigger's `provider`. Today the timestamp is checked
before the body is read and the Workflow is looked up. With a per-trigger scheme, the handler must find
the Workflow and its trigger before it knows whether a timestamp is required, so that check moves after
the lookup. Accepted: a `provider: ottoflow` request with a missing or stale timestamp now reaches the
Workflow and trigger lookup before its 401. The lookup reads the controller's cache and makes no API
server call. The request is still rejected before the body read and before the Secret read, a live API
call because Secrets are not cached. A request with a current timestamp already reaches the lookup,
the body read and the Secret read today, since the timestamp is not authenticated until the signature
check. Everything after verification (rate limit, `celFilter`, `inputMapping`, dedup, the
concurrent-run limit, run creation) stays shared by both schemes, except that `provider: github` adds
ping and allowlist handling (GitHub-specific rules below) and the replay check, and names the run from
the signature digest (see Replay handling).

Order: method, path, Workflow lookup, trigger lookup, timestamp (`provider: ottoflow` only), body read,
content type, Secret, signature, replay check (`provider: github` only; Replay handling, question 5),
then as today.

Unchanged requirements for both schemes: verify the signature before anything in the body is parsed or
evaluated (true today), and keep the 32-byte minimum secret length.

GitHub-specific rules:

- Accept only `X-Hub-Signature-256`, never the legacy SHA-1 `X-Hub-Signature`. GitHub sends the
  signature headers only when the webhook has a secret, so a webhook without one fails verification.
- Require JSON deliveries (`Content-Type: application/json`). GitHub can also deliver form-encoded
  payloads.
- Answer `ping`, which GitHub sends when the webhook is created, with 200 after the signature check,
  and create no run.
- An event or action outside the allowlist gets 200 `filtered`, like a `celFilter` that returns false.
- `github.events` is a filter, not a security check. `X-GitHub-Event` is outside the signed material, so
  anyone holding one signed delivery can resend its body with any event name and it still verifies. To
  filter on something such a sender cannot change, use the signed body: `github.actions`, or a
  `celFilter` such as `has(object.pull_request)`.
- `github.actions` reads the payload, so setting it means parsing the body even when `celFilter`,
  `inputMapping` and `dedupKey` are unset; today's rule parses only when one of them is set. The design
  should say whether reading `action` and building CEL's `object` share one parse.
- Record `X-GitHub-Delivery` and `X-GitHub-Event` in the run's trigger metadata, so runs can be traced
  to GitHub's delivery log. Neither is signed, so the recorded values are unverified.
  `WebhookRequestInfo` has no fields for them today.
- Respond within GitHub's 10-second limit. GitHub treats a slower response as a failed delivery, and it
  does not redeliver failed deliveries automatically. Runs are created synchronously today, and the
  server's write timeout is also 10 seconds, so the timeout does not keep a slow response inside the
  limit. Whether run creation needs a time budget is open question 7.
- GitHub verifies the endpoint's TLS certificate by default, so the ingress needs a certificate GitHub
  trusts.

### Replay handling (needs review)

The two schemes differ in what the signature covers:

| | OttoFlow scheme | GitHub scheme |
|---|---|---|
| Signed material | `v1:{timestamp}:{path}:{body}` | body only |
| Freshness | timestamp must be within 5 minutes | nothing signed |
| Endpoint binding | request path is signed | not signed |
| Delivery identity | none | `X-GitHub-Delivery` header: one GUID per event, reused on redelivery, not signed |

What follows for the design:

- The 5-minute timestamp window cannot apply to GitHub deliveries. Replay protection has to come from a
  record of deliveries already accepted.
- The path is not signed, so a delivery signed with one secret verifies at every endpoint that uses
  that secret. Each Workflow with `provider: github` needs its own secret: the replay record is per
  Workflow, so it does not stop a delivery sent to one Workflow from being replayed to another that
  shares the secret.

  The rule is documented, not enforced. Secret edits do not pass through Workflow admission, so a
  value check made there goes stale as soon as either Secret changes, and the Secret may not exist yet
  at that point. Rejecting a shared `secretRef` name would not catch the same value in two Secrets.
- GitHub recommends `X-GitHub-Delivery` for replay protection. The header is outside the signed
  material, so a record keyed only on it does not bind to the signed payload.

To settle in review:

1. Where the record lives. It should survive controller restarts and leader changes, or the proposal
   should state the window it does cover. A record that survives restarts is stored outside the
   controller's memory, so the design should say what lookup and write it adds to a request that must
   finish within GitHub's 10 seconds.

   Answer: not the dedup state, which is in the leader's memory and lost on restart or leader change.
   Name the WorkflowRun from the Workflow and the signature digest, so the API server's name uniqueness
   is the record. It adds no write (the Create already happens) and survives restarts and leader
   changes.

   Name: the Workflow name, a hyphen and the first 13 hex characters of the digest. That suffix is 14
   characters, the same as today's random suffix. The run name is also a label value, capped at 63
   characters, so Workflow names keep the room they have today. The digest is an HMAC under the
   Workflow's secret, so a sender without the secret cannot force a collision; an accidental one among
   a Workflow's runs is negligible at 52 bits.

   Response: a cached Get on that name catches most replays early (question 5); Create's AlreadyExists
   catches the rest. Both answer 200 `deduped`, the status the dedup gate already returns, never 500:
   GitHub shows a 500 as a failed delivery and invites another redelivery.

   Lifetime: the record lasts as long as the run, and run cleanup deletes completed runs by two bounds:
   `run.retentionMinutes` (default 7 days) and `run.maxAllowed` (default 50 completed runs per Workflow,
   oldest first, at any age). The record covers GitHub's 3-day redelivery window only when the Workflow
   completes fewer than `maxAllowed` runs in 3 days, and a busy Workflow can pass 50 runs within
   minutes. Once the run is deleted, by either bound or by hand, only rotating the secret stops a replay
   of that delivery.

   Alternatives: a separate API object per delivery (one extra write inside GitHub's 10 seconds, plus
   its own garbage collection and RBAC, but a lifetime independent of run cleanup), or in-memory with
   the covered window stated (replays after a restart get through).
2. What the record keys on: the delivery ID, a hash of the signed body, or both, and whether the
   already-verified signature digest can serve as that hash instead of hashing the body again.

   Answer: key on the Workflow plus the verified signature digest, as in question 1. Not the delivery ID:
   it is unsigned (above), so a replay with a fresh delivery ID would miss any record keyed on it. The
   digest is already computed by verification, so the body is not hashed again. The delivery ID is
   recorded for tracing only.
3. How long entries are kept, and how the record's size is bounded.

   Answer: as long as the run; run cleanup bounds both (Lifetime, question 1).
4. When a delivery is recorded. Dedup today records a key only after the WorkflowRun is created, so a
   delivery that failed can be redelivered. A replay record needs the same property.

   Answer: the record is the WorkflowRun, so a delivery is recorded only when Create succeeds; one that
   fails before that leaves no record and can be redelivered.
5. Whether the existing `dedupKey`/`dedupWindow` gate (window at most 1 hour) can carry this, or a
   separate mechanism is needed, and where the check sits in the request order. A `dedupKey` that fails
   to evaluate falls back to one run per Workflow per window. The dedup gate runs after the rate limit,
   `celFilter` and `inputMapping`, so a replay caught there has already counted against the rate limit
   and, when CEL fields are set, cost a parse and CEL evaluation. A delivery-ID or digest key is
   available right after verification.

   Answer: a separate mechanism, given those limits and the restart loss (question 1). It sits right
   after signature verification and before the rate limit, so a replay the cached Get catches costs
   neither rate-limit budget nor a parse.
6. Whether `provider: github` should require a replay record instead of leaving it optional.

   Answer: required for `provider: github`. Nothing GitHub signs expires, so without a record a captured
   delivery can be replayed for as long as the secret is valid.

### CEL environment

Trigger CEL sees only `object` today. GitHub sends the event name in the `X-GitHub-Event` header, which
CEL cannot see. Options:

- Bind a `headers` map for every provider. Broad: GitHub signs no header value, and the OttoFlow scheme
  signs only `X-OttoFlow-Timestamp`.
- Bind named GitHub values only (for example the event name and delivery ID).
- Add no binding. Filter with `github.events` and `github.actions`, and record the delivery ID only in
  run metadata.

The design should say whether inputs and dedup may depend on unsigned header values. Filtering may, but
not as a security check (`github.events` above); replay may not (Replay handling, question 2).

## Architecture impact

| Area | Change |
|---|---|
| `api/v1alpha1/workflow_types.go`, `zz_generated.deepcopy.go`, CRDs (`config/crd/bases/`, `charts/ottoflow/crds/`) | `provider` and `github` on `WebhookTrigger` |
| `api/v1alpha1/workflowrun_types.go` | Delivery ID and event name on `WebhookRequestInfo` |
| `internal/workflow/controller/webhook_server.go` | Verifier interface, GitHub verifier, ping and allowlist handling, handler order, the replay check (cached Get after the signature check, before the rate limit) |
| `internal/workflow/controller/trigger_manager.go` | Trigger metadata; CEL bindings, if any; run name from the Workflow and signature digest, and Create's AlreadyExists answered as `deduped` |
| `internal/webhook/workflow_validator.go` | Reject a `github` block unless `provider: github`; existing rules unchanged |
| Helm chart | None required (same listener) |
| `docs/user/tasks/triggers.md`, `docs/dev/DESIGN_WEBHOOK_TRIGGER.md`, `docs/user/reference/api/` | GitHub provider section; regenerated API reference |
| `samples/workflows/features/` | A native GitHub sample; keep the Actions shim sample |

No new CRDs, ports or deployments. `crypto/hmac` and `crypto/sha256` are already used. The replay record
is the WorkflowRun's name, so it needs no new storage.

## Implementation phases

After this design is reviewed and accepted:

1. **Verifier extraction, no behaviour change.** Move `verifyHMAC` into `ottoflowVerifier`; the timestamp
   check stays in the handler, and the existing webhook tests pass unchanged.
2. **API and admission.** New fields, CRD and deepcopy regeneration, validation.
3. **GitHub verifier and request handling.** Signature, ping, allowlists, delivery metadata, handler
   order.
4. **Replay record,** as answered under Replay handling; `provider: github` requires it (question 6), so
   phase 3 does not ship without it.
5. **Docs and samples.** Provider section in the triggers guide, design doc update, a native sample, and
   secret setup (the recipe in [Create the HMAC secret](../user/tasks/triggers.md#create-the-hmac-secret),
   with the same value in GitHub's webhook secret field).
6. **Follow-ups, in separate PRs.** Other providers through the same interface.

## Test strategy

- **Unit, `githubVerifier`.** GitHub's documented vector (secret `It's a Secret to Everybody`, payload
  `Hello, World!`, signature
  `sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17`), tampered body, wrong
  secret, missing header, `sha1=` or unprefixed header. The vector's secret is 26 bytes, below the
  32-byte minimum, so it tests the verifier directly, not the handler.
- **Unit, dispatch.** `provider` unset uses the OttoFlow verifier (regression); `provider: github` uses
  the GitHub verifier; a `github` block with `provider: ottoflow` is rejected at admission.
- **Handler tests** (httptest, as in `webhook_test.go`). Ping gets 200 and creates no run; an event or
  action outside the allowlist gets 200 `filtered`; an allowed pull request delivery gets 202 and a
  WorkflowRun with mapped inputs; a bad signature gets 401; a missing or stale timestamp still gets 401
  for `provider: ottoflow`; a replayed delivery gets 200 `deduped` and creates no run, whether the
  cached Get or Create's AlreadyExists catches it.
- **Integration (envtest).** A GitHub-style POST creates a run whose `status.trigger` has type `Webhook`
  and records the delivery ID and event name.
- **Manual.** A real repository webhook against a test cluster, including GitHub's redeliver action.

## Open questions

1. **Replay record.** Proposed answers under [Replay handling](#replay-handling-needs-review).
2. **Header bindings in CEL.** A `headers` map for all providers, named GitHub values, or none, and
   whether inputs and dedup may depend on unsigned header values (filtering and replay are answered
   above).
3. **Allowlists or CEL only.** `github.events` and `github.actions` duplicate what CEL could express
   once the event name is visible. `github.events` reads a header, so it can filter before the body is
   parsed; `github.actions` reads the payload, so it cannot. Where the allowlists run relative to the
   rate limit is also open. After it, deliveries that are then filtered still count against the
   per-Workflow limit, so on a busy organization webhook a wanted delivery can get 429 and be lost
   (question 6). Before it, `github.events` adds no parse, but `github.actions` would parse signed
   deliveries before any limit applies.
4. **Secrets.** Reuse `hmac-key`, or add a conventional `github-secret` key? (Answered under
   Replay handling: each `provider: github` Workflow needs its own secret; the rule is documented, not
   enforced.)
5. **Body size.** GitHub payloads can be up to 25 MB; the server rejects bodies over 1 MiB with 413.
   Keep the cap, or raise it for `provider: github`? Today the body is read before the Workflow lookup
   and the signature check, and the rate limit applies only after the signature check, so the cap bounds
   how much a request, signed or not, can make the server read. A cap per provider needs the Workflow and
   trigger lookup before the body is read. The server's ReadTimeout (whole request, from the start of
   the read) and WriteTimeout (from the end of the headers, covering the body read, handler and
   response) are both 10 seconds, the same as GitHub's response limit. Reading 25 MB in 10 seconds needs
   about 2.5 MB/s (20 Mbit/s) with no time left for processing; 1 MiB needs about 105 KB/s. Raising
   ReadTimeout alone does not help, since WriteTimeout and GitHub's limit still apply. The body is held
   in memory before the signature check, and the chart's default controller memory limit is 128 MiB
   (134 MB): five concurrent unsigned 25 MB requests hold 125 MB of it, not counting the process's own
   memory, and six exceed it.

   Answer: keep 1 MiB for every provider. A larger GitHub delivery gets 413 and shows as failed in
   GitHub's delivery log. Raising it later needs a per-provider cap, a bound on concurrent body reads, a
   higher memory limit, and both timeouts changed together.
6. **Rate limits.** GitHub does not redeliver failed deliveries automatically, so a 429 loses the event
   unless someone redelivers it. Should the defaults (60 per minute, burst 10) differ for
   `provider: github`?
7. **Response time.** Runs are created synchronously, and GitHub counts a response slower than 10
   seconds as a failed delivery. Does run creation need a time budget, or is a slow create an accepted
   risk?
8. **Metrics.** Add a `provider` label to `ottoflow_webhook_requests_total` now, or when a second
   provider lands?
