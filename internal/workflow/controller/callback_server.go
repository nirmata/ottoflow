/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/workflow/token"
)

// CallbackServer is an HTTP server that receives external callbacks for waitForCallback steps.
// It listens on a configurable address (default :8084) and exposes:
//
//	POST /api/v1/workflow-runs/{namespace}/{name}/callback/{token}
//	GET  /healthz
//
// The server is leader-elected: only the active leader opens the port.
type CallbackServer struct {
	client        client.Client
	eventRecorder events.EventRecorder
	addr          string
	server        *http.Server
	mux           *http.ServeMux
	// ControllerNamespace is the Workflow lookup fallback, same as WorkflowRunReconciler's.
	ControllerNamespace string
}

// NewCallbackServer creates a CallbackServer that listens on addr.
func NewCallbackServer(c client.Client, recorder events.EventRecorder, addr string) *CallbackServer {
	if addr == "" {
		addr = ":8084"
	}
	cs := &CallbackServer{
		client:        c,
		eventRecorder: recorder,
		addr:          addr,
		mux:           http.NewServeMux(),
	}
	cs.mux.HandleFunc("/api/v1/workflow-runs/", cs.handleCallback)
	cs.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	cs.server = &http.Server{
		Addr:    addr,
		Handler: cs.mux,
	}
	return cs
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
// The callback server should only run on the leader so callbacks hit one endpoint.
func (cs *CallbackServer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. Starts the HTTP server and blocks until ctx is cancelled.
func (cs *CallbackServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", cs.addr)
	if err != nil {
		return fmt.Errorf("callback server listen on %s: %w", cs.addr, err)
	}
	klog.Infof("CallbackServer listening on %s", cs.addr)
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cs.server.Shutdown(shutCtx)
	}()
	if err := cs.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("callback server error: %w", err)
	}
	return nil
}

// handleCallback handles POST /api/v1/workflow-runs/{namespace}/{name}/callback/{token}
func (cs *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCallbackError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: /api/v1/workflow-runs/{namespace}/{name}/callback/{token}
	// After stripping prefix, path is: {namespace}/{name}/callback/{token}
	prefix := "/api/v1/workflow-runs/"
	path := strings.TrimPrefix(r.URL.Path, prefix)
	// path = "{namespace}/{name}/callback/{token}"
	parts := strings.Split(path, "/")
	// parts[0]=namespace, parts[1]=name, parts[2]="callback", parts[3]=token
	if len(parts) != 4 || parts[2] != "callback" || parts[0] == "" || parts[1] == "" || parts[3] == "" {
		writeCallbackError(w, "path must be /api/v1/workflow-runs/{namespace}/{name}/callback/{token}", http.StatusBadRequest)
		return
	}
	namespace, name, callbackToken := parts[0], parts[1], parts[3]

	// Validate token format before any K8s calls
	if !token.ValidateToken(callbackToken) {
		writeCallbackError(w, "invalid token format", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Fetch WorkflowRun
	wr := &ottoflowv1alpha1.WorkflowRun{}
	if err := cs.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, wr); err != nil {
		if apierrors.IsNotFound(err) {
			writeCallbackError(w, "workflow run not found", http.StatusNotFound)
			return
		}
		writeCallbackError(w, "error fetching workflow run", http.StatusInternalServerError)
		return
	}

	// Check pending callback exists
	if wr.Status.PendingCallback == nil {
		writeCallbackError(w, "no pending callback for this workflow run", http.StatusNotFound)
		return
	}

	cb := wr.Status.PendingCallback

	// Hash the incoming token and compare against the stored hash (constant-time, prevents timing attacks)
	incoming := token.HashToken(callbackToken)
	if subtle.ConstantTimeCompare([]byte(cb.TokenHash), []byte(incoming)) != 1 {
		writeCallbackError(w, "invalid or expired callback token", http.StatusUnauthorized)
		return
	}

	// Terminal-phase check: only after token verification, so a caller with the wrong token
	// can't use this response to probe whether a run has reached a terminal phase. A run can be
	// marked terminal (e.g. cron Replace cancellation) while a PendingCallback is still live;
	// once terminal it must never accept a callback that can no longer be consumed.
	if runIsTerminal(wr) {
		writeTerminalCallbackError(w)
		return
	}

	// Expiry check
	if time.Now().Unix() > cb.ExpiresAt {
		writeCallbackError(w, "callback token expired", http.StatusUnauthorized)
		cs.emitEvent(ctx, wr, corev1.EventTypeWarning, "CallbackTimeout",
			fmt.Sprintf("Callback token expired for step %q", cb.StepName))
		return
	}

	// Already processed (idempotency: outputs already set)
	if len(cb.Outputs.Raw) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "already_accepted",
			"runPhase": string(wr.Status.Phase),
		})
		return
	}

	// Read request body (cap at 1 MiB)
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeCallbackError(w, "failed to read request body", http.StatusInternalServerError)
		return
	}
	if len(bodyBytes) == 0 {
		bodyBytes = []byte("{}")
	}

	// Validate JSON
	var payloadMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payloadMap); err != nil {
		writeCallbackError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		cs.emitEvent(ctx, wr, corev1.EventTypeWarning, "CallbackValidationFailed",
			fmt.Sprintf("Callback for step %q rejected: invalid JSON", cb.StepName))
		return
	}

	// Schema validation (if outputSchema is set on the step)
	if err := cs.validateCallbackSchema(ctx, wr, cb.StepName, payloadMap); err != nil {
		writeCallbackError(w, "schema validation failed: "+err.Error(), http.StatusBadRequest)
		cs.emitEvent(ctx, wr, corev1.EventTypeWarning, "CallbackValidationFailed",
			fmt.Sprintf("Callback for step %q rejected: %v", cb.StepName, err))
		return
	}

	// Atomically patch PendingCallback.outputs + clear token (single-use enforcement)
	outputsJSON := apiextensionsv1.JSON{Raw: bodyBytes}
	outputsJSONBytes, err := json.Marshal(outputsJSON)
	if err != nil {
		writeCallbackError(w, "internal serialization error", http.StatusInternalServerError)
		return
	}

	patchStr := fmt.Sprintf(`{"status":{"pendingCallback":{"tokenHash":%q,"stepName":%q,"expiresAt":%d,"createdAt":%d,"outputs":%s}}}`,
		cb.TokenHash, cb.StepName, cb.ExpiresAt, cb.CreatedAt, string(outputsJSONBytes))

	conflictDetected := false
	terminalDetected := false
	patchErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch to get latest resourceVersion for optimistic concurrency
		fresh := &ottoflowv1alpha1.WorkflowRun{}
		if err := cs.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, fresh); err != nil {
			return err
		}
		// Safety: the run may have been marked terminal (e.g. cron Replace cancellation)
		// between the admission check above and this re-fetch.
		if runIsTerminal(fresh) {
			terminalDetected = true
			return nil
		}
		// Safety: another callback may have already written outputs
		if fresh.Status.PendingCallback != nil && len(fresh.Status.PendingCallback.Outputs.Raw) > 0 {
			conflictDetected = true
			return nil
		}
		return cs.client.Status().Patch(ctx, fresh, client.RawPatch(types.MergePatchType, []byte(patchStr)))
	})

	if terminalDetected {
		writeTerminalCallbackError(w)
		return
	}

	if conflictDetected {
		// Another concurrent callback already set outputs — idempotent accept
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "already_accepted",
			"runPhase": string(wr.Status.Phase),
		})
		return
	}

	if patchErr != nil {
		klog.Errorf("CallbackServer: failed to patch WorkflowRun %s/%s status: %v", namespace, name, patchErr)
		writeCallbackError(w, "failed to record callback", http.StatusInternalServerError)
		return
	}

	cs.emitEvent(ctx, wr, corev1.EventTypeNormal, "CallbackReceived",
		fmt.Sprintf("Callback received for step %q; workflow run will resume", cb.StepName))

	klog.Infof("CallbackServer: callback accepted for %s/%s step=%q", namespace, name, cb.StepName)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "accepted",
		"runPhase": string(wr.Status.Phase),
	})
}

// validateCallbackSchema checks the payload against the step's outputSchema if present.
// Returns nil if no schema is defined (all payloads are accepted).
func (cs *CallbackServer) validateCallbackSchema(ctx context.Context, wr *ottoflowv1alpha1.WorkflowRun, stepName string, payload map[string]interface{}) error {
	// Look up the Workflow to find the step's outputSchema
	wf, _, err := resolveWorkflow(ctx, cs.client, cs.ControllerNamespace, wr)
	if err != nil {
		// If we can't load the workflow, skip schema validation (don't block callback)
		klog.Warningf("CallbackServer: could not load Workflow for schema validation, skipping: %v", err)
		return nil
	}

	var step *ottoflowv1alpha1.Step
	for i := range wf.Spec.Steps {
		if wf.Spec.Steps[i].Name == stepName {
			step = &wf.Spec.Steps[i]
			break
		}
	}
	if step == nil || step.WaitForCallback == nil || step.WaitForCallback.OutputSchema == nil {
		return nil // no schema defined — accept all
	}

	// Validate required fields from schema
	return validatePayloadAgainstSchema(step.WaitForCallback.OutputSchema, payload)
}

// validatePayloadAgainstSchema performs basic JSON Schema validation.
// Supports: type=object, required array, properties with type checking.
// For full JSON Schema support, use github.com/xeipuuv/gojsonschema (add as dep if needed).
func validatePayloadAgainstSchema(schema *apiextensionsv1.JSON, payload map[string]interface{}) error {
	if schema == nil || len(schema.Raw) == 0 {
		return nil
	}
	var s map[string]interface{}
	if err := json.Unmarshal(schema.Raw, &s); err != nil {
		return nil // malformed schema — skip validation
	}

	// Validate required fields
	if required, ok := s["required"].([]interface{}); ok {
		for _, r := range required {
			field, ok := r.(string)
			if !ok {
				continue
			}
			if _, exists := payload[field]; !exists {
				return fmt.Errorf("required field %q is missing", field)
			}
		}
	}

	// Validate property types
	if properties, ok := s["properties"].(map[string]interface{}); ok {
		for propName, propDef := range properties {
			propMap, ok := propDef.(map[string]interface{})
			if !ok {
				continue
			}
			expectedType, ok := propMap["type"].(string)
			if !ok {
				continue
			}
			val, exists := payload[propName]
			if !exists {
				continue // not present — required check above handles mandatory
			}
			if err := checkJSONType(propName, val, expectedType); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkJSONType(name string, val interface{}, expected string) error {
	switch expected {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("field %q: expected string, got %T", name, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("field %q: expected boolean, got %T", name, val)
		}
	case "number", "integer":
		switch val.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("field %q: expected number, got %T", name, val)
		}
	case "array":
		if _, ok := val.([]interface{}); !ok {
			return fmt.Errorf("field %q: expected array, got %T", name, val)
		}
	case "object":
		if _, ok := val.(map[string]interface{}); !ok {
			return fmt.Errorf("field %q: expected object, got %T", name, val)
		}
	}
	return nil
}

func (cs *CallbackServer) emitEvent(_ context.Context, wr *ottoflowv1alpha1.WorkflowRun, eventType, reason, message string) {
	if cs.eventRecorder == nil {
		return
	}
	cs.eventRecorder.Eventf(wr, nil, eventType, reason, reason, "%s", message)
}

func writeCallbackError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// runIsTerminal reports whether wr has reached a terminal phase (Succeeded or Failed), at which
// point it must never accept a callback that can no longer be consumed.
func runIsTerminal(wr *ottoflowv1alpha1.WorkflowRun) bool {
	return wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded || wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed
}

// writeTerminalCallbackError writes the 410 Gone response for a callback rejected because the
// WorkflowRun has reached a terminal phase. Shared by the admission check and the retry-loop
// re-check so both surfaces stay in sync.
func writeTerminalCallbackError(w http.ResponseWriter) {
	writeCallbackError(w, "workflow run has reached a terminal phase; no longer accepting callbacks", http.StatusGone)
}
