/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// Prometheus metrics for the webhook server. Declared at package level so the
// Once guard can call MustRegister without re-creating them on each test run.
var (
	webhookRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ottoflow_webhook_requests_total",
			Help: "Total inbound webhook POST requests. result label values: created, filtered, deduped, hmac_failure, stale_timestamp, rate_limited, invalid",
		},
		[]string{"namespace", "workflow", "result"},
	)
	webhookHMACFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ottoflow_webhook_hmac_failures_total",
			Help: "HMAC verification failures (subset of requests_total{result=hmac_failure}); surfaced separately for easy alerting",
		},
		[]string{"namespace", "workflow"},
	)
	webhookRunsCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ottoflow_webhook_runs_created_total",
			Help: "WorkflowRuns successfully created via webhook trigger",
		},
		[]string{"namespace", "workflow"},
	)
	webhookRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ottoflow_webhook_request_duration_seconds",
			Help:    "End-to-end webhook handler latency in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		[]string{"namespace", "workflow"},
	)

	// registerMetricsOnce prevents double-registration panics when tests call
	// NewWebhookServer multiple times in the same process (e.g. Ginkgo parallel specs).
	registerMetricsOnce sync.Once
)

// webhookLimiterEntry holds a rate limiter plus the parameters it was created with.
// Both rpm and burst are compared on each request so Workflow spec changes are picked up.
type webhookLimiterEntry struct {
	limiter *rate.Limiter
	rpm     int
	burst   int
}

// WebhookRequestMeta carries per-request metadata into CreateWorkflowRunFromWebhook.
type WebhookRequestMeta struct {
	RemoteAddr string
	RequestID  string
}

// WebhookResponse is the JSON body of a 202 Accepted response.
type WebhookResponse struct {
	RunName   string `json:"runName"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
}

// WebhookServer is a plain net/http server that accepts signed POST requests and
// creates WorkflowRuns via TriggerManager.
//
// Implements manager.Runnable and manager.LeaderElectionRunnable — only the
// leader replica opens the port. Without NeedLeaderElection()=true, all replicas
// accept requests and create duplicate WorkflowRuns (dedup state is in-process).
type WebhookServer struct {
	addr           string
	logger         logr.Logger
	kubeClient     client.Client
	triggerManager *TriggerManager
	limiterMu      sync.Mutex
	limiters       map[string]webhookLimiterEntry // key: "namespace/name"
}

// NewWebhookServer constructs a WebhookServer and registers Prometheus metrics once.
// limiters is initialized here; a nil map panics on first write.
func NewWebhookServer(addr string, logger logr.Logger, kubeClient client.Client, tm *TriggerManager) *WebhookServer {
	registerMetricsOnce.Do(func() {
		ctrlmetrics.Registry.MustRegister(
			webhookRequestsTotal,
			webhookHMACFailuresTotal,
			webhookRunsCreatedTotal,
			webhookRequestDurationSeconds,
		)
	})
	return &WebhookServer{
		addr:           addr,
		logger:         logger,
		kubeClient:     kubeClient,
		triggerManager: tm,
		limiters:       make(map[string]webhookLimiterEntry),
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
// Must return true — non-leaders must not open the listener.
func (s *WebhookServer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. Called by the controller manager after leader election.
func (s *WebhookServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/", s.handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // prevent Slowloris slow-header attacks
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second, // prevent keep-alive connection accumulation
		MaxHeaderBytes:    1 << 16,          // 64 KiB headers max
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("webhook trigger server listen on %s: %w", s.addr, err)
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	s.logger.Info("webhook trigger server started", "addr", s.addr)
	select {
	case err := <-errCh:
		_ = srv.Close()
		return fmt.Errorf("webhook trigger server error: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// handleWebhook processes POST /webhooks/{namespace}/{name}.
// Handler order: method → path → timestamp → body → workflow lookup →
//
//	trigger spec → content-type (if CEL) → secret → HMAC → rate limit → TriggerManager
func (s *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// 1. Method check — only POST accepted.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// 2. Parse /{namespace}/{name} from path.
	ns, name, ok := parseWebhookPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path: expected /webhooks/{namespace}/{name}")
		return
	}

	// Record end-to-end latency on all subsequent exits (ns/name resolved above).
	start := time.Now()
	defer func() {
		webhookRequestDurationSeconds.WithLabelValues(ns, name).Observe(time.Since(start).Seconds())
	}()

	// 3. Verify timestamp (cheap, O(1), no I/O) — rejects stale replays before any
	// expensive operation (body read, K8s API calls).
	ts := r.Header.Get("X-OttoFlow-Timestamp")
	if err := verifyTimestamp(ts); err != nil {
		webhookRequestsTotal.WithLabelValues(ns, name, "stale_timestamp").Inc()
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// 4. Read raw body. LimitReader caps at 1 MiB to prevent payload-amplification DoS.
	// Probe one extra byte to detect truncation: a truncated body produces a misleading
	// 401 (HMAC mismatch) instead of the correct 413 (payload too large).
	const maxBodyBytes = 1 << 20 // 1 MiB
	limited, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		webhookRequestsTotal.WithLabelValues(ns, name, "invalid").Inc()
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	if len(limited) > maxBodyBytes {
		webhookRequestsTotal.WithLabelValues(ns, name, "invalid").Inc()
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB limit")
		return
	}
	body := limited

	// 5. Look up Workflow — return 401, not 404, to prevent namespace/name enumeration.
	var wf ottoflowv1alpha1.Workflow
	if err := s.kubeClient.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &wf); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// 6. Find webhook trigger spec — return 401 for the same enumeration reason.
	webhookSpec := findWebhookTrigger(&wf)
	if webhookSpec == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// 7. Reject non-JSON only when the workflow uses CEL that requires a parsed object.
	// Plain webhooks with no filter/mapping/dedup accept any content type.
	if webhookSpec.CELFilter != "" || len(webhookSpec.InputMapping) > 0 || webhookSpec.DedupKey != "" {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			webhookRequestsTotal.WithLabelValues(ns, name, "invalid").Inc()
			writeError(w, http.StatusUnsupportedMediaType, "unsupported media type: expected application/json")
			return
		}
	}

	// 8. Fetch HMAC secret and verify signature.
	// Signed string: "v1:" + timestamp + ":" + path + ":" + body.
	// Including the path prevents cross-endpoint replay when two workflows share a secret.
	secret, err := s.fetchSecret(r.Context(), webhookSpec.SecretRef, ns)
	if err != nil {
		webhookHMACFailuresTotal.WithLabelValues(ns, name).Inc()
		webhookRequestsTotal.WithLabelValues(ns, name, "hmac_failure").Inc()
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sig := r.Header.Get("X-OttoFlow-Signature")
	if err := verifyHMAC(secret, ts, r.URL.Path, body, sig); err != nil {
		webhookHMACFailuresTotal.WithLabelValues(ns, name).Inc()
		webhookRequestsTotal.WithLabelValues(ns, name, "hmac_failure").Inc()
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// 9. Per-workflow rate limit (applied after auth — unauthenticated callers get 401 above).
	if !s.webhookRateLimiter(ns+"/"+name, webhookSpec).Allow() {
		webhookRequestsTotal.WithLabelValues(ns, name, "rate_limited").Inc()
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	// 10. Delegate to TriggerManager.
	run, filterResult, err := s.triggerManager.CreateWorkflowRunFromWebhook(
		r.Context(), &wf, webhookSpec, body,
		WebhookRequestMeta{RemoteAddr: r.RemoteAddr, RequestID: generateID()},
	)
	if err != nil {
		if errors.Is(err, ErrWorkflowRunCreateFailed) {
			webhookRequestsTotal.WithLabelValues(ns, name, "invalid").Inc()
			writeError(w, http.StatusInternalServerError, "internal error creating WorkflowRun")
		} else {
			webhookRequestsTotal.WithLabelValues(ns, name, "invalid").Inc()
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	switch filterResult {
	case WebhookFiltered:
		webhookRequestsTotal.WithLabelValues(ns, name, "filtered").Inc()
		writeJSON(w, http.StatusOK, map[string]string{"status": "filtered"})
		return
	case WebhookDeduped:
		webhookRequestsTotal.WithLabelValues(ns, name, "deduped").Inc()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deduped"})
		return
	case WebhookConcurrencyLimited:
		webhookRequestsTotal.WithLabelValues(ns, name, "rate_limited").Inc()
		writeError(w, http.StatusTooManyRequests, "max concurrent runs reached")
		return
	}

	webhookRequestsTotal.WithLabelValues(ns, name, "created").Inc()
	webhookRunsCreatedTotal.WithLabelValues(ns, name).Inc()
	writeJSON(w, http.StatusAccepted, WebhookResponse{
		RunName:   run.Name,
		Namespace: run.Namespace,
		Status:    "Pending",
	})
}

// parseWebhookPath extracts {namespace} and {name} from /webhooks/{namespace}/{name}.
// Returns ok=false for any other path shape.
func parseWebhookPath(path string) (namespace, name string, ok bool) {
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

// webhookTimestampWindow is the replay-protection window for X-OttoFlow-Timestamp.
// Requests outside this window (in either direction) are rejected with 401.
const webhookTimestampWindow = 5 * time.Minute

// verifyTimestamp parses X-OttoFlow-Timestamp (Unix epoch seconds) and rejects
// requests outside webhookTimestampWindow in either direction.
func verifyTimestamp(ts string) error {
	if ts == "" {
		return errors.New("missing X-OttoFlow-Timestamp header")
	}
	epoch, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	diff := time.Since(time.Unix(epoch, 0))
	if diff < -webhookTimestampWindow || diff > webhookTimestampWindow {
		return fmt.Errorf("timestamp out of replay window: %v", diff)
	}
	return nil
}

// verifyHMAC checks X-OttoFlow-Signature against HMAC-SHA256 of "v1:ts:path:body".
// The v1: prefix enables algorithm rotation. Path inclusion prevents cross-endpoint replay.
// hmac.Equal ensures constant-time comparison (bytes.Equal short-circuits on first mismatch).
func verifyHMAC(secret []byte, timestamp, path string, body []byte, sigHeader string) error {
	if !strings.HasPrefix(sigHeader, "sha256=") {
		return errors.New("missing sha256= prefix in X-OttoFlow-Signature")
	}
	expected, err := hex.DecodeString(strings.TrimPrefix(sigHeader, "sha256="))
	if err != nil {
		return fmt.Errorf("invalid hex in signature: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v1:"))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(":"))
	mac.Write([]byte(path))
	mac.Write([]byte(":"))
	mac.Write(body)
	computed := mac.Sum(nil)
	if !hmac.Equal(computed, expected) {
		return errors.New("signature mismatch")
	}
	return nil
}

// fetchSecret reads the HMAC signing key from a Kubernetes Secret.
// The Secret is read only in workflowNamespace; a ref naming another namespace is refused.
// All errors map to 401 in the caller — callers must not distinguish not-found
// from wrong-secret to avoid secret existence leakage.
func (s *WebhookServer) fetchSecret(ctx context.Context, ref ottoflowv1alpha1.WebhookSecretRef, workflowNamespace string) ([]byte, error) {
	if ns := ref.Namespace; ns != "" && ns != workflowNamespace {
		return nil, fmt.Errorf("webhook secretRef %s/%s: Secret references are resolved only in the Workflow's namespace %q", ns, ref.Name, workflowNamespace)
	}
	var secret corev1.Secret
	if err := s.kubeClient.Get(ctx, types.NamespacedName{Namespace: workflowNamespace, Name: ref.Name}, &secret); err != nil {
		return nil, err
	}
	keyName := ref.Key
	if keyName == "" {
		keyName = "hmac-key"
	}
	val, ok := secret.Data[keyName]
	if !ok || len(val) == 0 {
		return nil, fmt.Errorf("secret missing %q key", keyName)
	}
	// NIST SP 800-107: minimum 32 bytes (256 bits) for HMAC-SHA256.
	if len(val) < 32 {
		return nil, errors.New("HMAC secret must be at least 32 bytes; use: openssl rand -base64 32")
	}
	return val, nil
}

// findWebhookTrigger returns the first WebhookTrigger in the Workflow's trigger list, or nil.
// The admission webhook ensures at most one webhook trigger per Workflow.
func findWebhookTrigger(wf *ottoflowv1alpha1.Workflow) *ottoflowv1alpha1.WebhookTrigger {
	for i := range wf.Spec.Triggers {
		if wf.Spec.Triggers[i].Webhook != nil {
			return wf.Spec.Triggers[i].Webhook
		}
	}
	return nil
}

// generateID returns a random 16-char lowercase hex string for per-request tracing.
// 8 bytes (64 bits) makes birthday collisions negligible at realistic request volumes.
func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeError writes {"error": msg} with the given HTTP status code.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON encodes v as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// webhookRateLimiter returns (or creates) the per-workflow rate.Limiter.
// Recreated when rpm changes to pick up Workflow spec updates without a restart.
func (s *WebhookServer) webhookRateLimiter(key string, spec *ottoflowv1alpha1.WebhookTrigger) *rate.Limiter {
	rpm, burst := 60, 10
	if spec.RateLimit != nil && spec.RateLimit.RequestsPerMinute > 0 {
		rpm = spec.RateLimit.RequestsPerMinute
	}
	if spec.RateLimit != nil && spec.RateLimit.Burst > 0 {
		burst = spec.RateLimit.Burst
	}
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	if entry, ok := s.limiters[key]; ok && entry.rpm == rpm && entry.burst == burst {
		return entry.limiter
	}
	entry := webhookLimiterEntry{
		limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), burst),
		rpm:     rpm,
		burst:   burst,
	}
	s.limiters[key] = entry
	return entry.limiter
}

// RemoveLimiter cleans up the rate limiter for a deleted Workflow.
// Called from WorkflowReconciler.Reconcile on Workflow deletion.
func (s *WebhookServer) RemoveLimiter(key string) {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	delete(s.limiters, key)
}
