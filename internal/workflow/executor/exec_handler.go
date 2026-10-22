/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// nirmataProvider is the model provider name for Nirmata-hosted LLMs.
const nirmataProvider = "nirmata"

// ExecHandler handles POST /api/exec/{namespace}/{agentName}.
//
// It handles POST /api/exec/{namespace}/{agentName} for internal Agent CRD execution.
// It does not use taskupdate.Manager and avoids O(N²) memory growth from accumulating task state.
type ExecHandler struct {
	agentExecutor *OttoFlowAgentExecutor
}

// NewExecHandler creates an ExecHandler backed by the given OttoFlowAgentExecutor.
func NewExecHandler(agentExecutor *OttoFlowAgentExecutor) *ExecHandler {
	return &ExecHandler{agentExecutor: agentExecutor}
}

// ServeHTTP handles POST /{namespace}/{agentName} (after /api/exec prefix is stripped).
func (h *ExecHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse /{namespace}/{agentName} from the (already stripped) path.
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeExecError(w, "path must be /{namespace}/{agentName}", http.StatusBadRequest)
		return
	}
	namespace, agentName := parts[0], parts[1]

	// Decode request body (cap at 32 MiB to prevent memory exhaustion).
	const maxBodyBytes = 32 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		code := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			code = http.StatusRequestEntityTooLarge
		}
		writeExecError(w, "invalid request body: "+err.Error(), code)
		return
	}

	// Look up Agent CRD.
	agentCRD := &ottoflowv1alpha1.Agent{}
	if err := h.agentExecutor.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: agentName}, agentCRD); err != nil {
		writeExecError(w, fmt.Sprintf("agent %s/%s not found: %v", namespace, agentName, err), http.StatusNotFound)
		return
	}

	// Validate LLM credentials for Nirmata provider.
	llmEnv := ParseLLMEnvHeader(r.Header.Get(XLLMEnvHeader))
	provider := agentCRD.Spec.ModelProvider
	if provider == "" {
		// Empty is retained only for backward compatibility with Agent objects
		// stored before modelProvider became required (CRD required is
		// enforced at admission, not retroactively).
		provider = nirmataProvider
	}
	if provider == nirmataProvider {
		token := ""
		if llmEnv != nil {
			token = llmEnv["NIRMATA_LLM_TOKEN"]
			if token == "" {
				token = llmEnv["NIRMATA_LLM_SERVICEACCOUNT_TOKEN"]
			}
			if token == "" {
				token = llmEnv["NIRMATA_LLM_APIKEY"]
			}
		}
		if token == "" {
			writeExecError(w, "Nirmata LLM credentials required: set NIRMATA_LLM_TOKEN (or NIRMATA_LLM_SERVICEACCOUNT_TOKEN / NIRMATA_LLM_APIKEY) in spec.execution.job.env for the workflow run", http.StatusBadRequest)
			return
		}
	}

	// Apply LLM env overrides to execution context.
	execCtx := r.Context()
	if len(llmEnv) > 0 {
		execCtx = agent.WithLLMEnvOverride(execCtx, llmEnv)
	}

	// Execute agent and extract outputs.
	response, tokenUsage, extractedOutputs, err := executeAndExtract(execCtx, h.agentExecutor.agentExecutor, agentCRD, req.Prompt, req.Context, namespace)
	if err != nil {
		writeExecError(w, "agent execution failed: "+err.Error()+secretAccessHint(err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ExecResponse{
		Content:          response,
		ExtractedOutputs: extractedOutputs,
		InputTokens:      tokenUsage.InputTokens,
		OutputTokens:     tokenUsage.OutputTokens,
	}); err != nil {
		klog.V(2).InfoS("Failed to encode exec response", "error", err)
	}
}

// secretAccessHint names the Helm grant, but only for a denied Secret read.
func secretAccessHint(err error) string {
	var st apierrors.APIStatus
	if !errors.As(err, &st) || !apierrors.IsForbidden(err) {
		return ""
	}
	if d := st.Status().Details; d == nil || d.Kind != "secrets" || d.Group != "" {
		return ""
	}
	return ". To let the agent-executor read this Secret, set rbac.secretAccess.agentExecutor in the Helm values (a Secret in the install namespace needs a hand-written Role instead); see docs/user/rbac-secret-access.md"
}

// executeAndExtract calls ExecuteAgent then runs output extraction if configured.
// It is the shared implementation for the CLI local path and the HTTP exec handler.
func executeAndExtract(
	ctx context.Context,
	exec agent.AgentExecutor,
	agentCRD *ottoflowv1alpha1.Agent,
	prompt string,
	contextData map[string]interface{},
	namespace string,
) (string, agent.AgentTokenUsage, map[string]interface{}, error) {
	response, tokenUsage, err := exec.ExecuteAgent(ctx, agentCRD, prompt, contextData, namespace)
	if err != nil {
		return "", agent.AgentTokenUsage{}, nil, err
	}
	var outputs map[string]interface{}
	if agentCRD.Spec.OutputExtraction != nil {
		outputs, err = agent.NewDefaultOutputExtractor().Extract(response, agentCRD.Spec.OutputExtraction)
		if err != nil {
			klog.V(2).InfoS("Failed to extract outputs",
				"namespace", namespace, "agent", agentCRD.Name, "error", err)
		}
	}
	return response, tokenUsage, outputs, nil
}

// writeExecError writes an ExecErrorResponse with the given HTTP status code.
func writeExecError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ExecErrorResponse{Error: msg})
}

// ParseLLMEnvHeader decodes the X-LLM-Env header (base64-encoded JSON map of
// string→string). Returns nil when the header is absent or malformed.
func ParseLLMEnvHeader(header string) map[string]string {
	if header == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		klog.V(4).InfoS("Invalid X-LLM-Env header (base64)", "error", err)
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(decoded, &m); err != nil {
		klog.V(4).InfoS("Invalid X-LLM-Env header (JSON)", "error", err)
		return nil
	}
	return m
}
