# Design: Webhook Trigger — HTTP-Based WorkflowRun Initiation

**Date**: June 2026  
**Status**: Implemented

---

## Problem

OttoFlow's existing `cron` and `event` triggers both require the triggering system to have Kubernetes API access. There is no path for external systems (CI/CD pipelines, Slack bots, GitHub Actions, REST clients, SaaS control planes) to fire WorkflowRuns without holding K8s RBAC.

---

## Design

### Architecture Overview

```
External caller (GitHub Actions / CI / Slack bot)
        │
        │  POST /webhooks/{ns}/{name}
        │  X-OttoFlow-Signature: sha256=...
        │  X-OttoFlow-Timestamp: 1719270000
        │
        ▼
┌──────────────────────────────────────┐
│  WebhookServer  (port :8083)         │  implements manager.Runnable
│  ── plain net/http, no TLS ──        │  started by mgr.Add()
│                                      │
│  handleWebhook()                     │
│    1. parse {ns}/{name} from path    │
│    2. verify timestamp window        │──→ 401 on stale/missing (before body read)
│    3. content-type check             │──→ 415 on non-JSON
│    4. read raw body (limit 1 MiB)    │
│    5. lookup Workflow via k8sClient  │──→ 401 (not 404) — no existence leak
│    6. find webhook trigger spec      │──→ 401 (not 404) — no existence leak
│    7. fetch HMAC secret + verify sig │──→ 401 on failure
│    8. delegate to TriggerManager     │
└──────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────┐
│  TriggerManager.CreateWorkflowRunFromWebhook()  │
│                                      │
│  Gate 1: celFilter (optional)        │──→ drop (200 OK, no run)
│  Gate 2: inputMapping via CEL        │
│  Gate 3: dedup (dedupKey/dedupWindow)│──→ drop (200 OK, no run)
│  Gate 4: MaxConcurrentRuns           │──→ 429 Too Many Requests
│  Gate 5: create WorkflowRun         │
└──────────────────────────────────────┘
        │
        ▼
   202 Accepted
   {"runName": "...", "namespace": "...", "status": "Pending"}
```

**Why no TLS on the webhook port?** HMAC-SHA256 provides authentication and message integrity independent of transport encryption. The signature proves the caller knows the shared secret and that the body was not tampered with. TLS on top is desirable in production but should be handled by the cluster's ingress/service mesh layer (standard Kubernetes practice), not by the controller binary. Admission webhook TLS (existing `certmanager` package) is unrelated — those are inbound from the API server on a different path. This matches how FluxCD's `notification-controller` `ReceiverServer` works: plain HTTP on a dedicated port, TLS terminated by ingress.

---

## API Changes

### `api/v1alpha1/workflow_types.go`

**Extend `Trigger` struct:**

```go
type Trigger struct {
    Cron    *CronTrigger    `json:"cron,omitempty"`
    Event   *EventTrigger   `json:"event,omitempty"`
    Webhook *WebhookTrigger `json:"webhook,omitempty"`  // NEW
}
```

**New `WebhookTrigger` struct:**

```go
// WebhookTrigger defines an HTTP-based trigger that fires a WorkflowRun
// when a signed POST request is received at /webhooks/{namespace}/{workflowName}.
type WebhookTrigger struct {
    // SecretRef references a Kubernetes Secret containing the HMAC signing key.
    // The Secret must have a key named "secret" whose value is the shared secret.
    // +required
    SecretRef WebhookSecretRef `json:"secretRef"`

    // CELFilter is an optional CEL boolean expression evaluated against the parsed
    // request body (available as `object`). If false or the expression errors,
    // the request is acknowledged (200) but no WorkflowRun is created.
    // +optional
    CELFilter string `json:"celFilter,omitempty"`

    // InputMapping maps workflow input names to CEL expressions evaluated against
    // the parsed JSON body (available as `object`). Results are coerced to strings.
    // If omitted, no inputs are passed to the WorkflowRun.
    // +optional
    InputMapping map[string]string `json:"inputMapping,omitempty"`

    // DedupKey is a CEL expression evaluated against the request body to extract
    // a deduplication key. Requests with the same key within DedupWindow are dropped.
    // +optional
    DedupKey string `json:"dedupKey,omitempty"`

    // DedupWindow is the time window for deduplication when DedupKey is set.
    // Defaults to 10 minutes if DedupKey is set and DedupWindow is omitted.
    // Maximum of 1 hour — a window longer than 1 hour would silently suppress all
    // webhook requests for an entire day after the first one, which is very difficult
    // to debug. If you need longer dedup windows, use an external idempotency key.
    //
    // NOTE: +kubebuilder:validation:Maximum does NOT apply to *metav1.Duration because
    // Duration marshals to a JSON string (e.g. "10m"), not a number. The 1-hour cap is
    // enforced in the admission webhook validator (internal/webhook/workflow_webhook.go):
    //   if wt.DedupWindow != nil && wt.DedupWindow.Duration > time.Hour {
    //       return field.Invalid(...)
    //   }
    // +optional
    DedupWindow *metav1.Duration `json:"dedupWindow,omitempty"`

    // RateLimit configures per-workflow rate limiting on inbound webhook requests.
    // +optional
    RateLimit *WebhookRateLimit `json:"rateLimit,omitempty"`
}

type WebhookSecretRef struct {
    // Name of the Kubernetes Secret.
    // +required
    Name string `json:"name"`

    // Namespace of the Secret. In v1, must equal the Workflow's namespace.
    // Cross-namespace references are rejected by the admission webhook.
    // +optional
    Namespace string `json:"namespace,omitempty"`

    // Key is the data key within the Secret that holds the HMAC signing key.
    // Defaults to "hmac-key". Set this to accommodate existing Secrets that
    // store credentials under a different key name (e.g., "token", "secret").
    // +kubebuilder:default=hmac-key
    // +optional
    Key string `json:"key,omitempty"`
}

type WebhookRateLimit struct {
    // RequestsPerMinute is the maximum number of accepted requests per minute
    // for this workflow's webhook endpoint. Defaults to 60.
    // +kubebuilder:default=60
    // +optional
    RequestsPerMinute int `json:"requestsPerMinute,omitempty"`

    // Burst is the maximum number of requests allowed in a short burst above the
    // per-minute average. Defaults to 10. A value of 10 accommodates retry storms
    // (e.g., GitHub Actions 3-retry policy) without permitting a sustained flood.
    // +kubebuilder:default=10
    // +optional
    Burst int `json:"burst,omitempty"`
}
```

### `api/v1alpha1/workflowrun_types.go`

**Extend `TriggerInfo` — update existing enum marker at `workflowrun_types.go:368`:**

```go
type TriggerInfo struct {
    // +kubebuilder:validation:Enum=Manual;Cron;Event;Webhook  ← update existing marker
    Type           string               `json:"type"`
    CronSchedule   string               `json:"cronSchedule,omitempty"`
    TriggeredAt    metav1.Time          `json:"triggeredAt,omitempty"`
    EventResource  *EventResourceInfo   `json:"eventResource,omitempty"`
    WebhookRequest *WebhookRequestInfo  `json:"webhookRequest,omitempty"`  // NEW
}

// WebhookRequestInfo records metadata about the HTTP request that triggered the run.
type WebhookRequestInfo struct {
    // RemoteAddr is the caller's IP address (best-effort; may be proxy IP).
    RemoteAddr string `json:"remoteAddr,omitempty"`
    // RequestID is a unique ID generated per request for tracing.
    RequestID  string `json:"requestId,omitempty"`
}
```

**NOTE:** The existing `// +kubebuilder:validation:Enum=Manual;Cron;Event` marker at `workflowrun_types.go:368` must be updated to `Enum=Manual;Cron;Event;Webhook`. If this marker is not updated, the CRD admission webhook will reject `Type: "Webhook"` even after the struct field is added.

**NOTE:** Adding `WebhookRequest *WebhookRequestInfo` to `TriggerInfo` requires regenerating `zz_generated.deepcopy.go`. The existing `DeepCopyInto` at `zz_generated.deepcopy.go:1604-1612` does not handle this new pointer field — it will not be deep-copied until regenerated. Run `make generate manifests` after any API type changes.

---

## Implementation

### New file: `internal/workflow/controller/webhook_server.go`

The `WebhookServer` is a plain `net/http` server implementing `manager.Runnable`. It does **not** dynamically register routes per workflow — it uses a single catch-all route and looks up the `Workflow` object on every request via `k8sClient`. This avoids the `http.ServeMux` deregistration problem (stdlib mux has no `Deregister`) entirely. This is exactly the approach FluxCD's `ReceiverServer` uses.

**Note:** The full `WebhookServer` struct definition (including rate-limiter fields) is in the [Rate limiting](#rate-limiting) section below. Only the complete 6-field definition should be used; the partial 4-field variant shown in earlier drafts is removed to prevent compile errors when `s.limiterMu` / `s.limiters` are first referenced.

```go
// NewWebhookServer constructs a WebhookServer. The limiters map must be initialized
// here; a nil map panics on the first s.limiters[key] write.
func NewWebhookServer(addr string, logger logr.Logger, kubeClient client.Client, tm *TriggerManager) *WebhookServer {
    return &WebhookServer{
        addr:           addr,
        logger:         logger,
        kubeClient:     kubeClient,
        triggerManager: tm,
        limiters:       make(map[string]webhookLimiterEntry),
    }
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
// WebhookServer must only run on the leader — only the leader creates WorkflowRuns.
// Without this, all replicas open :8083 and create duplicate WorkflowRuns for the
// same inbound request (dedupState is in-process and not shared across replicas).
func (s *WebhookServer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable — called by controller-manager after leader election.
func (s *WebhookServer) Start(ctx context.Context) error {
    mux := http.NewServeMux()
    mux.HandleFunc("/webhooks/", s.handleWebhook)
    mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
    })
    srv := &http.Server{
        Addr:              s.addr,
        Handler:           mux,
        ReadHeaderTimeout: 5 * time.Second,  // prevent Slowloris slow-header attacks
        ReadTimeout:       10 * time.Second,
        WriteTimeout:      10 * time.Second,
        IdleTimeout:       60 * time.Second, // prevent keep-alive connection accumulation
        MaxHeaderBytes:    1 << 16,          // 64 KiB headers max
    }
    go func() {
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            s.logger.Error(err, "webhook server failed")
        }
    }()
    <-ctx.Done()
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    return srv.Shutdown(shutdownCtx)
}
```

**Request handler flow:**

```go
func (s *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
    // 1. Method check — only POST accepted
    if r.Method != http.MethodPost {
        writeError(w, 405, "method not allowed")
        return
    }

    // 2. Parse /{namespace}/{name} from path
    ns, name, ok := parseWebhookPath(r.URL.Path)
    if !ok {
        writeError(w, 400, "invalid path: expected /webhooks/{namespace}/{name}")
        return
    }

    // 3. Verify timestamp first (cheap, O(1), no I/O) — rejects stale replays before
    // any expensive operations (body read, K8s API calls).
    // Correct order: method → path → timestamp → content-type → body → workflow lookup → secret → HMAC.
    ts := r.Header.Get("X-OttoFlow-Timestamp")
    if err := verifyTimestamp(ts, 5*time.Minute); err != nil {
        writeError(w, 401, "unauthorized")
        return
    }

    // 4. Content-Type check — reject non-JSON before reading body.
    if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
        writeError(w, 415, "unsupported media type: expected application/json")
        return
    }

    // 5. Read raw body. LimitReader caps at 1 MiB to prevent payload amplification DoS.
    // Probe one extra byte to detect truncation — a truncated body produces a misleading
    // 401 (HMAC mismatch) instead of the correct 413 (payload too large).
    const maxBodyBytes = 1 << 20 // 1 MiB
    limitedBody, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
    if err != nil {
        writeError(w, 400, "failed to read body")
        return
    }
    if len(limitedBody) > maxBodyBytes {
        writeError(w, 413, "request body exceeds 1 MiB limit")
        return
    }
    body := limitedBody

    // 6. Look up Workflow — return 401 not 404 to avoid namespace/name enumeration
    var wf ottoflowv1alpha1.Workflow
    if err := s.kubeClient.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &wf); err != nil {
        writeError(w, 401, "unauthorized")
        return
    }

    // 7. Find webhook trigger spec — return 401, same enumeration reason
    webhookSpec := findWebhookTrigger(&wf)
    if webhookSpec == nil {
        writeError(w, 401, "unauthorized")
        return
    }

    // 8. Fetch HMAC secret + verify signature.
    // Signed string: "v1:" + timestamp + ":" + path + ":" + body.
    // Including the path prevents cross-endpoint replay when two workflows share a secret.
    secret, err := s.fetchSecret(r.Context(), webhookSpec.SecretRef, ns)
    if err != nil {
        writeError(w, 401, "unauthorized")
        return
    }
    sig := r.Header.Get("X-OttoFlow-Signature")
    if err := verifyHMAC(secret, ts, r.URL.Path, body, sig); err != nil {
        writeError(w, 401, "unauthorized")
        return
    }

    // 9. Rate limit check (per workflow, after auth)
    if !s.webhookRateLimiter(ns+"/"+name, webhookSpec).Allow() {
        writeError(w, 429, "rate limit exceeded")
        return
    }

    // 10. Delegate to TriggerManager
    run, filterResult, err := s.triggerManager.CreateWorkflowRunFromWebhook(
        r.Context(), &wf, webhookSpec, body,
        WebhookRequestMeta{RemoteAddr: r.RemoteAddr, RequestID: generateID()},
    )
    if err != nil {
        // Distinguish server-side errors (k8s API Create failure) from client errors.
        // ErrWorkflowRunCreateFailed wraps tm.client.Create errors → HTTP 500 (retriable).
        // All other errors (bad JSON, CEL expression) → HTTP 400 (client fault).
        if errors.Is(err, ErrWorkflowRunCreateFailed) {
            writeError(w, 500, "internal error creating WorkflowRun")
        } else {
            writeError(w, 400, err.Error())
        }
        return
    }
    switch filterResult {
    case WebhookFiltered:
        // celFilter returned false — acknowledged, no run created
        writeJSON(w, 200, map[string]string{"status": "filtered"})
        return
    case WebhookDeduped:
        // duplicate within dedupWindow — acknowledged, no run created
        writeJSON(w, 200, map[string]string{"status": "deduped"})
        return
    }

    writeJSON(w, 202, WebhookResponse{
        RunName:   run.Name,
        Namespace: run.Namespace,
        Status:    "Pending",
    })
}
```

### HMAC verification (stdlib only, no new deps)

```go
func verifyHMAC(secret []byte, timestamp string, path string, body []byte, sigHeader string) error {
    // sigHeader format: "sha256=<hex>"
    if !strings.HasPrefix(sigHeader, "sha256=") {
        return errors.New("missing sha256= prefix")
    }
    expected, err := hex.DecodeString(strings.TrimPrefix(sigHeader, "sha256="))
    if err != nil {
        return err
    }
    mac := hmac.New(sha256.New, secret)
    mac.Write([]byte("v1:"))
    mac.Write([]byte(timestamp))
    mac.Write([]byte(":"))
    mac.Write([]byte(path))
    mac.Write([]byte(":"))
    mac.Write(body)
    computed := mac.Sum(nil)
    if !hmac.Equal(computed, expected) {   // constant-time comparison
        return errors.New("signature mismatch")
    }
    return nil
}
```

**Why `v1:` prefix?** Slack pioneered this; it enables algorithm rotation — a future `v2:` variant could use a different hash without breaking `v1:` clients.

**Why include the URL path in the signed string?** `"v1:" + timestamp + ":" + path + ":" + body`. The path (`/webhooks/{ns}/{name}`) is part of the signed material, following the approach used by Stripe (endpoint URL) and Shopify (path). If two workflows share the same HMAC secret (operator misconfiguration or testing), a valid signed request for workflow A cannot be replayed at workflow B's endpoint — the signature will not match because the path differs.

**Why `hmac.Equal` not `bytes.Equal`?** `bytes.Equal` short-circuits on first mismatch, leaking timing information that a sufficiently motivated attacker could exploit to forge signatures. `hmac.Equal` runs in constant time regardless of where the mismatch occurs.

### Helper function definitions

All symbols referenced in the handler must be defined. Incomplete definitions will block implementation.

```go
// WebhookRequestMeta carries per-request metadata into CreateWorkflowRunFromWebhook.
type WebhookRequestMeta struct {
    RemoteAddr string
    RequestID  string
}

// WebhookResponse is the 202 response body.
type WebhookResponse struct {
    RunName   string `json:"runName"`
    Namespace string `json:"namespace"`
    Status    string `json:"status"`
}

// parseWebhookPath extracts the {namespace} and {name} segments from a webhook URL path.
// Expected format: /webhooks/{namespace}/{name}
// Returns ok=false if the path has too few or too many segments, or either segment is empty.
func parseWebhookPath(path string) (namespace, name string, ok bool) {
    // Strip leading /webhooks/ prefix and split remainder on "/"
    trimmed := strings.TrimPrefix(path, "/webhooks/")
    if trimmed == path { // prefix not present
        return "", "", false
    }
    parts := strings.SplitN(trimmed, "/", 3) // at most 3: [ns, name, extra]
    if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
        return "", "", false
    }
    return parts[0], parts[1], true
}

// writeError writes a JSON error response: {"error": "<msg>"}.
func writeError(w http.ResponseWriter, code int, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(code)
    _ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON writes a JSON-encoded value with the given HTTP status code.
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(code)
    _ = json.NewEncoder(w).Encode(v)
}

// findWebhookTrigger returns the first WebhookTrigger spec found in the Workflow's
// trigger list, or nil if no webhook trigger is configured.
// The admission webhook ensures at most one webhook trigger per Workflow, so the
// first match is always the correct one.
func findWebhookTrigger(wf *ottoflowv1alpha1.Workflow) *ottoflowv1alpha1.WebhookTrigger {
    for i := range wf.Spec.Triggers {
        if wf.Spec.Triggers[i].Webhook != nil {
            return wf.Spec.Triggers[i].Webhook
        }
    }
    return nil
}

// generateID returns a random 16-char lowercase hex string for request tracing.
// Uses 8 bytes (64 bits of entropy) — sufficient to make birthday collisions negligible
// at realistic request volumes (50% collision probability requires ~4 billion requests).
// Uses crypto/rand — no new imports beyond what HMAC already requires.
func generateID() string {
    b := make([]byte, 8)
    _, _ = rand.Read(b)
    return hex.EncodeToString(b)
}

// verifyTimestamp parses the X-OttoFlow-Timestamp header (Unix epoch seconds)
// and rejects requests outside the replay window in either direction.
func verifyTimestamp(ts string, window time.Duration) error {
    if ts == "" {
        return errors.New("missing X-OttoFlow-Timestamp header")
    }
    epoch, err := strconv.ParseInt(ts, 10, 64)
    if err != nil {
        return fmt.Errorf("invalid timestamp: %w", err)
    }
    diff := time.Since(time.Unix(epoch, 0))
    if diff < -window || diff > window {
        return fmt.Errorf("timestamp out of replay window: %v", diff)
    }
    return nil
}

// fetchSecret reads the HMAC signing key from a Kubernetes Secret.
// The Secret is read only in workflowNamespace; a ref naming another namespace is refused.
// In v1, secretRef.namespace must equal the Workflow's namespace (enforced by admission webhook).
func (s *WebhookServer) fetchSecret(ctx context.Context, ref WebhookSecretRef, workflowNamespace string) ([]byte, error) {
    if ns := ref.Namespace; ns != "" && ns != workflowNamespace {
        return nil, fmt.Errorf("webhook secretRef %s/%s: Secret references are resolved only in the Workflow's namespace %q", ns, ref.Name, workflowNamespace)
    }
    var secret corev1.Secret
    if err := s.kubeClient.Get(ctx, types.NamespacedName{Namespace: workflowNamespace, Name: ref.Name}, &secret); err != nil {
        return nil, err  // not-found → caller returns 401, not 500
    }
    keyName := ref.Key
    if keyName == "" {
        keyName = "hmac-key"
    }
    val, ok := secret.Data[keyName]
    if !ok || len(val) == 0 {
        return nil, fmt.Errorf("secret missing %q key", keyName)
    }
    // 32 bytes (256 bits) is the minimum per NIST SP 800-107 for HMAC-SHA256.
    // The operator guide example already generates 32 bytes via 'openssl rand -base64 32'.
    if len(val) < 32 {
        return nil, errors.New("HMAC secret must be at least 32 bytes (256 bits); use: openssl rand -base64 32")
    }
    return val, nil
}
```

**Note on Secret reads:** The RBAC marker added below grants `verbs=get` only. With `get` alone, the controller-runtime cached client does **not** establish an informer watch for Secrets — `list` and `watch` verbs are required for that. As a result, every call to `fetchSecret` issues a direct (live) API server request, not a cache read. Under the default 60 webhook requests/minute rate limit this is at most 1 API server call/second per workflow — acceptable, but operators should be aware. A future optimization could add `list;watch` to the RBAC marker and register a Secret field indexer to serve reads from cache, but this also grants the controller the ability to list all Secrets in watched namespaces (a wider security footprint). For v1, live reads are the simpler and more conservative choice.

### WorkflowRun naming for webhook-triggered runs

`CreateWorkflowRunFromEvent` uses `{workflow}-{uid4}-{time8hex}` where `uid4` is the first 4 chars of the triggering object's UID. Webhook runs have no triggering K8s object, so use a random 4-char hex prefix instead:

```
{workflowName}-{rand4}-{time8hex}
```

Where `rand4 = hex.EncodeToString(crypto/rand 2 bytes)` and `time8hex = fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)`. This matches the nanosecond-masked pattern used by `CreateWorkflowRunFromEvent` (trigger_manager.go:534), ensuring sub-second uniqueness within a single workflow — two requests within the same second produce different names. (Using Unix seconds would cause a name collision if two requests arrive in the same second.)

### CEL error handling — intentional divergence from event trigger

The event trigger (`CreateWorkflowRunFromEvent`) silently drops events when `celFilter` errors — consistent with its autonomous reconciler model where logging is sufficient. The webhook trigger returns `400` on CEL error — consistent with its synchronous HTTP model where the caller needs a signal. This divergence is **intentional and documented**:

| Trigger type | CEL filter error | inputMapping CEL error | Rationale |
|---|---|---|---|
| `event` | Silent drop + log | Silent skip + log | Autonomous; no caller to notify |
| `webhook` | Return error → handler returns 400 | Silent skip + log warning | celFilter errors indicate expression bugs (caller gets actionable 400); inputMapping errors are per-key and partial results are still useful — WorkflowRun is created with the inputs that evaluated successfully; operator can diagnose via logs |

**inputMapping error handling detail:** When a CEL expression in `inputMapping` fails to evaluate, that input key is omitted from `WorkflowRun.Spec.InputValues` and a log warning is emitted:
```go
for inputName, celExpr := range spec.InputMapping {
    val, err := tm.evaluateTriggerCEL(celExpr, objectData)
    if err != nil {
        tm.logger.Info("inputMapping CEL expression error — input omitted",
            "input", inputName, "expr", celExpr, "err", err)
        continue
    }
    inputs[inputName] = fmt.Sprintf("%v", val)
}
```
This is intentional: a single bad expression should not block a WorkflowRun creation entirely. Operators can diagnose missing inputs via controller logs.

### Dedup semantics when `dedupKey` is not set

When `dedupKey` is omitted:
- If `dedupWindow` is also omitted: all requests create a WorkflowRun (no dedup)
- If `dedupWindow` is set but `dedupKey` is omitted: only one WorkflowRun is created per `dedupWindow` period for that workflow, regardless of payload content (the workflow namespace/name is the dedup scope)

This behavior must be clearly documented in user-facing docs. The common case for webhook triggers is to set an explicit `dedupKey` (e.g. `object.data.run_id`) to deduplicate by payload content rather than by time window alone.

### `TriggerManager.CreateWorkflowRunFromWebhook`

```go
// WebhookFilterResult distinguishes the three outcomes of CreateWorkflowRunFromWebhook
// so the HTTP handler can set the correct response without inspecting (nil, nil).
type WebhookFilterResult int

const (
    WebhookRunCreated  WebhookFilterResult = iota // WorkflowRun was created
    WebhookFiltered                               // celFilter returned false — no run, not an error
    WebhookDeduped                                // duplicate within dedupWindow — no run, not an error
)

// ErrWorkflowRunCreateFailed is returned when tm.client.Create fails with a k8s API error
// (e.g. etcd write failure, leader election in progress). The HTTP handler maps this to
// HTTP 500 so callers treat it as a retriable server error, not a malformed request.
var ErrWorkflowRunCreateFailed = errors.New("WorkflowRun Create failed")

func (tm *TriggerManager) CreateWorkflowRunFromWebhook(
    ctx context.Context,
    workflow *ottoflowv1alpha1.Workflow,
    spec *ottoflowv1alpha1.WebhookTrigger,
    body []byte,
    meta WebhookRequestMeta,
) (*ottoflowv1alpha1.WorkflowRun, WebhookFilterResult, error) {

    // Parse body into map[string]interface{} for CEL evaluation.
    // Design decision: if none of CELFilter, InputMapping, or DedupKey are set,
    // skip JSON parsing — a non-JSON body is accepted in that case (e.g. GitHub ping).
    var objectData map[string]interface{}
    if spec.CELFilter != "" || len(spec.InputMapping) > 0 || spec.DedupKey != "" {
        if err := json.Unmarshal(body, &objectData); err != nil {
            return nil, 0, fmt.Errorf("invalid JSON body: %w", err)
        }
    }

    // Gate 1: CEL filter (reuses evaluateTriggerCEL unchanged)
    // CEL errors → return error so handler can respond 400 (malformed expression).
    // Filter returning false → return WebhookFiltered so handler responds 200.
    if spec.CELFilter != "" {
        raw, err := tm.evaluateTriggerCEL(spec.CELFilter, objectData)
        if err != nil {
            return nil, 0, fmt.Errorf("celFilter expression error: %w", err)
        }
        matched, ok := raw.(bool)
        if !ok || !matched {
            return nil, WebhookFiltered, nil
        }
    }

    // Gate 2: inputMapping (reuses evaluateTriggerCEL unchanged)
    inputs := map[string]string{}
    for inputName, celExpr := range spec.InputMapping {
        val, err := tm.evaluateTriggerCEL(celExpr, objectData)
        if err != nil {
            // Design decision: CEL errors in inputMapping are silent-skipped with a log warning.
            // The WorkflowRun is still created with the inputs that evaluated successfully.
            tm.logger.Info("inputMapping CEL expression error — input omitted",
                "input", inputName,
                "expr", celExpr,
                "err", err,
            )
            continue
        }
        inputs[inputName] = fmt.Sprintf("%v", val)
    }

    // Gate 3: dedup (reuses dedupState / dedupMu unchanged).
    // triggerKey scopes the dedup map entry to this webhook trigger (never reuses requestID).
    // objectKey is what gets deduplicated: either the CEL-extracted dedupKey value, or the
    // workflow namespace/name itself (which gives "one run per dedupWindow regardless of payload").
    triggerKey := fmt.Sprintf("%s/%s-webhook", workflow.Namespace, workflow.Name)
    objectKey  := fmt.Sprintf("%s/%s", workflow.Namespace, workflow.Name)
    if spec.DedupKey != "" {
        raw, err := tm.evaluateTriggerCEL(spec.DedupKey, objectData)
        if err == nil {
            objectKey = fmt.Sprintf("webhook-dedup/%v", raw)
        }
    }

    if spec.DedupWindow != nil && spec.DedupWindow.Duration > 0 {
        tm.dedupMu.Lock()
        state, exists := tm.dedupState[triggerKey]
        if !exists {
            state = &dedupEntry{}
            tm.dedupState[triggerKey] = state
        }
        if state.lastKey == objectKey && time.Since(state.lastSeen) < spec.DedupWindow.Duration {
            tm.dedupMu.Unlock()
            return nil, WebhookDeduped, nil
        }
        // Not a duplicate — record the new key/time AFTER successful Create (see below).
        tm.dedupMu.Unlock()
    }

    // Gate 4: MaxConcurrentRuns.
    // WARNING: countActiveWorkflowRuns as it exists in scheduler.go:194-220 queries with
    // MatchingLabels{"ottoflow.nirmata.io/trigger": "cron"} — it counts only cron-triggered
    // runs. When called for webhook-triggered workflows, it will always return 0, making
    // this gate a no-op. This is a pre-existing bug that also affects event triggers.
    // Before implementing this gate for webhooks, fix countActiveWorkflowRuns to accept a
    // trigger type argument (or count all runs regardless of trigger type):
    //   countActiveWorkflowRuns(ctx, client, workflow, triggerType string) (int, error)
    // The webhook implementation must use the fixed version, not the current broken one.
    //
    // BEHAVIORAL CHANGE NOTE: Fixing countActiveWorkflowRuns to count all runs (all
    // trigger types) changes the semantic of MaxConcurrentRuns from "per trigger type"
    // to "across all trigger types". For workflows with mixed triggers (cron + webhook),
    // this means a cron-triggered run and a webhook-triggered run count against the same
    // MaxConcurrentRuns limit. This is a breaking behavioral change for any workflow that
    // currently relies on cron-only counting. Migration note: operators with mixed-trigger
    // workflows should review their MaxConcurrentRuns values before deploying this fix.

    // Gate 5: build and create WorkflowRun.
    // buildWorkflowRun does not yet exist — it must be extracted from
    // CreateWorkflowRunFromEvent as a shared helper in trigger_manager.go.
    // Signature: buildWorkflowRun(wf *Workflow, inputs map[string]string, info TriggerInfo) *WorkflowRun
    run := buildWorkflowRun(workflow, inputs, ottoflowv1alpha1.TriggerInfo{
        Type:        "Webhook",
        TriggeredAt: metav1.Now(),
        WebhookRequest: &ottoflowv1alpha1.WebhookRequestInfo{
            RemoteAddr: meta.RemoteAddr,
            RequestID:  meta.RequestID,
        },
    })
    run.Labels["ottoflow.nirmata.io/trigger"]    = "webhook"
    run.Labels["ottoflow.nirmata.io/managed-by"] = "ottoflow-webhook-server"
    // NOTE: The "managed-by" label is webhook-specific in v1. Cron and event triggers
    // do not currently set a "managed-by" label. This inconsistency is a known gap —
    // a follow-up task should add "managed-by" to all trigger types (ottoflow-scheduler,
    // ottoflow-trigger-manager) to enable uniform selectors across trigger types.

    // Capture status before Create — the cached client does not reflect the newly
    // created object immediately, so we must use a retry loop (mirroring
    // CreateWorkflowRunFromEvent lines 577-609 of trigger_manager.go).
    statusToSet := run.Status.DeepCopy()
    if err := tm.client.Create(ctx, run); err != nil {
        return nil, 0, fmt.Errorf("%w: %v", ErrWorkflowRunCreateFailed, err)
    }

    // Write dedup state AFTER successful Create — mirrors unregisterEventTrigger pattern.
    if spec.DedupWindow != nil && spec.DedupWindow.Duration > 0 {
        tm.dedupMu.Lock()
        if state, exists := tm.dedupState[triggerKey]; exists {
            state.lastKey  = objectKey
            state.lastSeen = time.Now()
        }
        tm.dedupMu.Unlock()
    }

    // Status update retry loop — the initial Create does not persist Status subresource.
    for i := 0; i < 5; i++ {
        if err := tm.client.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, run); err != nil {
            if apierrors.IsNotFound(err) {
                // WorkflowRun was deleted between Create and Get — stop retrying.
                break
            }
            // Transient error (network, leader election) — log and retry.
            tm.logger.Error(err, "failed to Get WorkflowRun for status update — will retry",
                "run", run.Name, "attempt", i+1)
            continue
        }
        run.Status = *statusToSet
        if err := tm.client.Status().Update(ctx, run); err == nil {
            break
        }
    }

    return run, WebhookRunCreated, nil
}
```

**`buildWorkflowRun` extraction (required pre-work):** Before implementing `CreateWorkflowRunFromWebhook`, the WorkflowRun construction block currently inlined in `CreateWorkflowRunFromEvent` (lines ~537–575 of `trigger_manager.go`) must be extracted into:

```go
func buildWorkflowRun(
    workflow *ottoflowv1alpha1.Workflow,
    inputs map[string]string,
    triggerInfo ottoflowv1alpha1.TriggerInfo,
) *ottoflowv1alpha1.WorkflowRun
```

`buildWorkflowRun` returns a fully populated `*WorkflowRun` struct (including Status) but does **not** call `Create`. The status-update retry loop (lines ~577–609 of `trigger_manager.go`) stays in the caller — both `CreateWorkflowRunFromEvent` and `CreateWorkflowRunFromWebhook` call `buildWorkflowRun`, then `Create`, then the 5-iteration `Get + Status().Update()` retry loop independently. This avoids duplicating the retry logic inside the helper while keeping `buildWorkflowRun` a pure struct builder with no side effects.

This is a pure refactor with no behavioral change; it should be done as the first commit on the implementation branch and covered by the existing cron trigger tests to verify no regression.

### Rate limiting

`golang.org/x/time/rate` is already a direct dependency. Per-workflow limiters stored in a mutex-protected `map[string]webhookLimiterEntry` on `WebhookServer` (not `sync.Map` — while `sync.Map.LoadOrStore` is atomic, the pattern here requires a read-modify-write: we need to check the existing `rpm` value and conditionally replace the entry. This requires `Load` + conditional `Store` which is not atomic as a unit. A plain map + `sync.Mutex` makes the read-modify-write atomic under the lock and is clearer).

```go
// webhookLimiterEntry holds a limiter and the rpm it was created for.
// Comparing rpm on each call detects Workflow spec changes and recreates the limiter.
type webhookLimiterEntry struct {
    limiter *rate.Limiter
    rpm     int
}

type WebhookServer struct {
    addr           string
    logger         logr.Logger
    kubeClient     client.Client
    triggerManager *TriggerManager
    limiterMu      sync.Mutex
    limiters       map[string]webhookLimiterEntry // key: "namespace/name"
}

func (s *WebhookServer) webhookRateLimiter(key string, spec *WebhookTrigger) *rate.Limiter {
    rpm := 60
    burst := 10
    if spec.RateLimit != nil && spec.RateLimit.RequestsPerMinute > 0 {
        rpm = spec.RateLimit.RequestsPerMinute
    }
    if spec.RateLimit != nil && spec.RateLimit.Burst > 0 {
        burst = spec.RateLimit.Burst
    }
    s.limiterMu.Lock()
    defer s.limiterMu.Unlock()
    if entry, ok := s.limiters[key]; ok && entry.rpm == rpm {
        return entry.limiter // reuse if rpm unchanged
    }
    // Create fresh limiter — covers both first-use and rpm-change cases atomically.
    entry := webhookLimiterEntry{
        limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), burst),
        rpm:     rpm,
    }
    s.limiters[key] = entry
    return entry.limiter
}

// RemoveLimiter cleans up the rate limiter for a deleted Workflow.
// Called from WorkflowController.Reconcile on workflow deletion to prevent slow map growth.
func (s *WebhookServer) RemoveLimiter(key string) {
    s.limiterMu.Lock()
    defer s.limiterMu.Unlock()
    delete(s.limiters, key)
}
```

**Dedup state cleanup:** Webhook dedup entries live in `TriggerManager.dedupState` keyed by `{ns}/{name}-webhook`. Without cleanup, this map grows by one entry per webhook-enabled Workflow over the controller's lifetime. The `TriggerManager` must expose a cleanup method called from `WorkflowController.Reconcile` on Workflow deletion:

```go
// CleanupWebhookDedup removes the dedup state entry for a deleted Workflow.
// Mirrors unregisterEventTrigger which prunes dedupState[triggerKey] for event triggers.
// Must be called from WorkflowController.Reconcile when a Workflow is being deleted
// (i.e., !workflow.DeletionTimestamp.IsZero() and the webhook trigger is present).
func (tm *TriggerManager) CleanupWebhookDedup(workflowKey string) {
    triggerKey := workflowKey + "-webhook"
    tm.dedupMu.Lock()
    defer tm.dedupMu.Unlock()
    delete(tm.dedupState, triggerKey)
}
```

**Known limitation — state loss on leader restart:** Both the dedup state and per-workflow rate limiters are in-process memory. If the leader pod is killed or evicted, the new leader starts with empty state:
- A caller that sent a request within the prior `dedupWindow` can resend it and create a duplicate WorkflowRun.
- Per-workflow rate limiters reset, allowing a burst immediately after failover (bounded by the 5-minute HMAC timestamp window).

These are acceptable limitations for v1. Users requiring strict dedup across restarts must use a sufficiently long `dedupWindow` or external idempotency controls (e.g., enforce unique `dedupKey` values from the caller side).

### `cmd/controller/main.go` wiring

```go
// After TriggerManager is created:
webhookServer := workflowcontroller.NewWebhookServer(
    webhookTriggerAddr,   // flag: --webhook-trigger-addr, default ":8083"
    ctrl.Log.WithName("webhook-trigger-server"),
    mgr.GetClient(),
    triggerManager,
)
if err := mgr.Add(webhookServer); err != nil {
    setupLog.Error(err, "unable to add webhook trigger server")
    os.Exit(1)
}
```

`mgr.Add()` ensures:
- Server starts only after leader election (prevents split-brain: two replicas both accepting requests)
- Server receives the manager's `context.Context` for graceful shutdown
- Server lifecycle is tied to the manager's lifecycle

### Helm chart changes

New `Service` port and `Deployment` container port for `:8083`. New flag in `args`. NetworkPolicy update if present.

---

## Security Considerations

| Threat | Mitigation |
|---|---|
| Unauthenticated trigger | HMAC-SHA256 — attacker without the secret cannot forge a valid signature |
| Timing attack on HMAC comparison | `crypto/hmac.Equal` (constant-time) |
| Replay attack | `X-OttoFlow-Timestamp` must be within 5 minutes of server time |
| Oversized payload (DoS) | `io.LimitReader(r.Body, 1<<20)` — 1 MiB cap |
| Request flood (DoS) | Per-workflow `rate.Limiter`; 429 response |
| Secret exposure in logs | Secret is never logged; only the HMAC result is computed |
| Cross-namespace escalation | Workflow lookup is scoped to path `{namespace}/{name}` — a caller cannot trigger a workflow in a namespace they don't know the name of |
| Secret rotation | Rotate the K8s Secret value; no controller restart needed (secret is fetched via a live API call on every request) |
| Secret not found | `fetchSecret` returning not-found yields 401, not 500 — same response as wrong secret |
| Resource enumeration | All pre-auth failures return `401 "unauthorized"` — callers cannot distinguish "workflow doesn't exist" from "wrong secret" |
| Content-Type enforcement | Handler checks `Content-Type: application/json` and returns 415 for other types before reading body |

**TLS note:** Plain HTTP on port `:8083` means payload body content is visible to passive network observers. HMAC provides authentication and integrity but not confidentiality. Production deployments **must** terminate TLS at ingress or a service mesh sidecar. The deployment guide must require this — it is not merely "desirable". A `--webhook-trigger-tls-cert-file` / `--webhook-trigger-tls-key-file` flag pair is reserved for a future version where TLS is handled in-process.

---

## Observability

The webhook server is a network-facing component; operators need counters to detect abuse, misconfiguration, and saturation. Define the following Prometheus metrics using the `controller-runtime` metrics registry (same pattern as existing OttoFlow controllers):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `ottoflow_webhook_requests_total` | Counter | `namespace`, `workflow`, `result` | Total inbound webhook POST requests. `result` values: `created`, `filtered`, `deduped`, `hmac_failure`, `stale_timestamp`, `rate_limited`, `invalid` |
| `ottoflow_webhook_hmac_failures_total` | Counter | `namespace`, `workflow` | HMAC verification failures (subset of `requests_total{result="hmac_failure"}`; surfaced separately for easy alerting) |
| `ottoflow_webhook_runs_created_total` | Counter | `namespace`, `workflow` | WorkflowRuns successfully created via webhook |
| `ottoflow_webhook_request_duration_seconds` | Histogram | `namespace`, `workflow` | End-to-end handler latency |

**Note on `stale_timestamp` vs `hmac_failure`:** These are separate `result` label values so operators can monitor clock skew independently of HMAC key mismatches. A rising `stale_timestamp` rate without a corresponding `hmac_failure` rise indicates clock synchronization issues between callers and the controller pod.

All metrics must be registered in `webhook_server.go` using a `sync.Once` guard inside `NewWebhookServer()`:
```go
var registerMetricsOnce sync.Once

func NewWebhookServer(...) *WebhookServer {
    registerMetricsOnce.Do(func() {
        metrics.Registry.MustRegister(
            webhookRequestsTotal,
            webhookHMACFailuresTotal,
            webhookRunsCreatedTotal,
            webhookRequestDurationSeconds,
        )
    })
    ...
}
```
Using `sync.Once` (not `init()`) avoids double-registration panics when tests call `NewWebhookServer` multiple times in the same process (e.g., Ginkgo parallel test nodes). Using `NewWebhookServer` (not `init()`) keeps metric registration close to the component that owns them.

---

## RBAC Requirements

`WebhookServer.fetchSecret()` reads Kubernetes Secrets via `k8sClient`. The controller's `ClusterRole` must grant this explicitly. Add to `internal/workflow/controller/workflow_controller.go` (alongside existing markers):

```go
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
```

**Cross-namespace `secretRef` — v1 restriction:** In v1, `secretRef.namespace` **must equal** the Workflow's namespace. The admission webhook must reject Workflow specs where `secretRef.namespace` differs from the Workflow's namespace. This restriction:
1. Limits the RBAC grant's blast radius — the controller can only read Secrets in the same namespace as the Workflow that references them.
2. Prevents the privilege escalation path where a namespace-level Workflow author uses `secretRef.namespace` to indirect into secrets in another namespace.

**RBAC note for strict environments:** The `+kubebuilder:rbac:groups="",resources=secrets,verbs=get` marker generates a `ClusterRole` granting `get` on ALL Secrets cluster-wide. The admission webhook's same-namespace restriction is an application-layer control, not an RBAC boundary — it does not narrow the RBAC footprint. If the admission webhook is misconfigured or bypassed, the controller can read any Secret in any namespace. Strict environments should use a separate `ServiceAccount` for the webhook server component, bound to a `Role` (not `ClusterRole`) scoped to the specific namespaces where webhook-enabled Workflows exist.

Cross-namespace `secretRef` (e.g., for shared org-wide signing keys) is reserved for a future version with explicit multi-tenancy controls. Document the v2 escape hatch in a comment in the admission webhook validator.

---

## Validation Markers (Admission Webhook)

All new types need kubebuilder validation markers so the existing admission webhook rejects invalid specs at apply time rather than at runtime:

```go
type WebhookSecretRef struct {
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`
    // +optional
    Namespace string `json:"namespace,omitempty"`
    // +kubebuilder:default=hmac-key
    // +optional
    Key string `json:"key,omitempty"`
}

type WebhookRateLimit struct {
    // +kubebuilder:default=60
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=3600
    RequestsPerMinute int `json:"requestsPerMinute,omitempty"`
    // +kubebuilder:default=10
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=100
    Burst int `json:"burst,omitempty"`
}
```

The existing `Trigger` struct validation must also reject specs where more than one of `cron`, `event`, `webhook` is set simultaneously (oneOf). Add a validator check in `internal/webhook/workflow_webhook.go`.

---

## High-Availability Considerations

`mgr.Add()` ensures the `WebhookServer` only starts on the leader replica. Non-leader pods do not open the listener. With a standard Kubernetes `Service` selecting all controller pods by label, clients may hit a non-leader pod and receive `connection refused`.

**Required mitigations (Helm chart / deployment):**

1. Add a readiness probe on `:8083/healthz` — only the leader pod will respond 200, making non-leaders fail the probe and be removed from the Service endpoint set.
2. OR run the controller with `replicas: 1` (simplest; acceptable for most installations).
3. Document this in the user-facing triggers doc.

**Acceptance criteria for HA:**
- Helm chart includes a `readinessProbe` targeting `:8083/healthz`; non-leader pods fail the probe and are removed from Service endpoints.
- `WebhookServer.RemoveLimiter(ns+"/"+name)` is called from `WorkflowController.Reconcile` on Workflow deletion (alongside `TriggerManager.CleanupWebhookDedup`) to prevent unbounded growth of the `limiters` map.

---

## CEL vs JSONPath for `inputMapping`

An earlier draft proposed JSONPath (`$.data.namespace`). This design uses CEL (`object.data.namespace`) for three reasons:

1. **Consistency** — `event` trigger already uses CEL for `inputMapping` and `celFilter`. Users of both trigger types learn one expression language.
2. **Zero new dependencies** — `evaluateTriggerCEL` and the CEL environment are already present; no JSONPath library needed.
3. **Expressiveness** — CEL supports conditionals, string functions, and type coercions that JSONPath cannot express (e.g. `object.severity == "high" ? "P1" : "P2"`).

`gjson` (already indirect in `go.mod`) would work for simple path extraction but is not needed.

---

## Design Decisions

### Non-JSON payloads

When `celFilter` and `inputMapping` are both empty, there is nothing to evaluate against the JSON body. However, the handler still attempts `json.Unmarshal` in `CreateWorkflowRunFromWebhook`. **Decision:** If both `celFilter` and `inputMapping` are empty, skip JSON parsing and proceed with an empty `objectData`. A non-JSON body is accepted in this case. If either field is set, JSON parsing is required and a non-JSON body returns 400. This decision avoids rejecting simple ping payloads from systems like GitHub that send `{"zen": "..."}` pings.

### One webhook trigger per Workflow

A Workflow's `spec.triggers` is a list; multiple entries each with a `webhook` block are syntactically valid. **Decision:** At most one `webhook` block is allowed across all entries in `spec.triggers` for a single Workflow. The URL path is `{ns}/{name}` — there is no per-trigger routing disambiguation. The admission webhook must validate the entire `spec.triggers` list and reject any Workflow where more than one Trigger entry has a `webhook` field set.

---

## Four-Gate Model

```
Webhook POST arrives
        │
        ▼
┌─────────────────────┐
│  Security Gates     │  HMAC verify + timestamp replay check
│  (authenticate)     │  → 401 on failure
└─────────┬───────────┘
          │ pass
          ▼
┌─────────────────────┐
│  Gate 1             │  celFilter — does this payload matter?
│  (filter noise)     │  e.g. object.severity == "high"
└─────────┬───────────┘
          │ pass
          ▼
┌─────────────────────┐
│  Gate 2             │  inputMapping — extract workflow inputs via CEL
│  (extract data)     │  e.g. object.data.namespace → inputs.namespace
└─────────┬───────────┘
          │
          ▼
┌─────────────────────┐
│  Gate 3             │  deduplication — same dedupKey within dedupWindow?
│  (dedup)            │  same key → drop; new key → proceed
└─────────┬───────────┘
          │ new
          ▼
┌─────────────────────┐
│  Gate 4             │  MaxConcurrentRuns — at capacity?
│  (concurrency)      │  at limit → 429; under limit → proceed
└─────────┬───────────┘
          │
          ▼
    WorkflowRun created
    202 Accepted → {runName, namespace, status}
```

This is a direct parallel to the event trigger's Three-Gate Model; the security layer sits above it.

---

## Testing Strategy

### Unit tests (`controller_unit_test.go`)

- `verifyHMAC` — valid sig, bad sig, wrong timestamp prefix, tampered body
- `verifyTimestamp` — within window, outside window, missing header
- `parseWebhookPath` — valid paths, missing parts, extra segments

### Integration tests (`trigger_test.go`, Ginkgo + envtest + `httptest`)

```
It("should create WorkflowRun on valid webhook request")
It("should return 401 on invalid HMAC signature")
It("should return 401 on stale timestamp")
It("should return 401 for unknown workflow — not 404")
It("should return 401 for workflow with no webhook trigger — not 404")
It("should return 200 (no run) when celFilter is false")
It("should return 400 when celFilter expression is invalid CEL")
It("should map inputMapping CEL expressions to WorkflowRun inputs")
It("should deduplicate requests with the same dedupKey within dedupWindow")
It("should NOT deduplicate requests with different dedupKey values")
It("should suppress all requests within dedupWindow when dedupKey is not set")
It("should return 429 when MaxConcurrentRuns is reached")
It("should return 500 when k8s API Create fails — caller must retry")
```

Pattern: spin up `WebhookServer` with `httptest.NewServer`, use `net/http` client to POST signed requests, assert `k8sClient.List()` for WorkflowRun creation and label presence.

---

## Example: GitHub Actions Integration

```yaml
# .github/workflows/trigger-compliance.yml
- name: Trigger OttoFlow compliance scan
  run: |
    TIMESTAMP=$(date +%s)
    WEBHOOK_PATH="/webhooks/ottoflow/compliance-scan"
    BODY='{"data":{"namespace":"production","cluster_id":"${{ vars.CLUSTER_ID }}","severity":"high"}}'
    # Signed string: "v1:" + timestamp + ":" + path + ":" + body
    # The URL path is included to prevent cross-endpoint replay attacks.
    # Use printf (not echo -n) for portability across shells.
    SIG=$(printf 'v1:%s:%s:%s' "${TIMESTAMP}" "${WEBHOOK_PATH}" "${BODY}" | openssl dgst -sha256 -hmac "${{ secrets.OTTOFLOW_WEBHOOK_SECRET }}" | awk '{print "sha256="$2}')
    curl -s -X POST \
      -H "Content-Type: application/json" \
      -H "X-OttoFlow-Signature: ${SIG}" \
      -H "X-OttoFlow-Timestamp: ${TIMESTAMP}" \
      -d "${BODY}" \
      "https://ottoflow.example.com${WEBHOOK_PATH}"
```

```yaml
# Workflow definition
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: compliance-scan
  namespace: ottoflow
spec:
  triggers:
    - webhook:
        secretRef:
          name: github-actions-webhook-secret
        celFilter: 'object.data.severity == "high"'
        inputMapping:
          namespace: object.data.namespace
          clusterId: object.data.cluster_id
  inputs:
    - name: namespace
    - name: clusterId
  execution:
    steps:
      - name: run-scan
        type: job
        jobTemplate:
          spec:
            template:
              spec:
                containers:
                  - name: scanner
                    image: example.com/compliance-scanner:latest
                    args: ["--namespace", "$(inputs.namespace)"]
```

---

## Comparison with Existing Trigger Types

| | Cron | Event | **Webhook** |
|---|---|---|---|
| Caller requires K8s access | No | Yes (must create/update a CRD) | **No — HTTP only** |
| Trigger source | Time schedule | K8s resource state change | **External HTTP caller** |
| Works from GitHub Actions | No | No | **Yes** |
| Works from Slack bot | No | No | **Yes** |
| Works for SaaS control plane | No | No | **Yes** |
| Authentication | N/A | RBAC | **HMAC-SHA256** |
| Replay protection | N/A | K8s watch dedup | **Timestamp window** |
| Input extraction | K8s Secret values | CEL on K8s object | **CEL on JSON body** |
| Dedup | ConcurrencyPolicy | Revision-based | **Key + time window** |

---

## Next Steps

1. **First commit:** Extract `buildWorkflowRun` helper from `CreateWorkflowRunFromEvent` in `trigger_manager.go`; verify existing cron/event tests still pass
3. Implement `WebhookTrigger` API types; run `make generate manifests` to regenerate `zz_generated.deepcopy.go` and CRD YAML
4. Add `// +kubebuilder:rbac:groups="",resources=secrets,verbs=get` marker; run `make generate manifests` again
5. Add oneOf validator for `Trigger` in `internal/webhook/workflow_webhook.go`
6. Implement `webhook_server.go` with HMAC verification and corrected handler order
7. Extend `TriggerManager` with `CreateWorkflowRunFromWebhook` and `WebhookFilterResult`
8. Wire in `cmd/controller/main.go`; update Helm chart (Service port `:8083`, readiness probe, `--webhook-trigger-addr` flag)
9. Write integration tests (httptest + envtest)
10. Add `docs/user/tasks/triggers.md` webhook section (include HA note about readiness probe)
11. Add sample workflows: `samples/workflows/github-actions-webhook.yaml`, `samples/workflows/generic-ci-webhook.yaml`
