/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/nirmata/ottoflow/internal/secretmount"
)

// reservedRunnerEnvNames are the runner-Job environment variables the controller sets itself —
// WORKFLOW_RUN_NAME, WORKFLOW_RUN_NAMESPACE, JOB_NAME and POD_NAME on every runner Job,
// OTTOFLOW_SECRET_MOUNTS and AGENT_EXECUTOR_NAMESPACE on the runs that get them — ahead of the
// author's spec.execution.job.env entries (buildWorkflowRunnerJob). Neither the API server nor the
// kubelet rejects a repeated env name: the container environment is built from a map keyed by
// name, so the LAST entry silently wins, and an author-supplied duplicate would replace the
// controller's value.
//
// OTTOFLOW_SECRET_MOUNTS is what turns that from a precedence quirk into a privilege escalation.
// It maps each approved (namespace, secret, key) reference to the file the runner reads that
// value from (internal/secretmount), and the runner reads whatever path the map names. An
// author who can replace the map can point an approved credential reference — an MCPServer
// bearer token, an externalAgentRef auth token — at any file in the runner pod, including the
// runner's own ServiceAccount token, and have that value sent wherever the reference is used.
// The remaining names are the runner's identity (which run it executes and reports on, which
// Job and pod it is) and the namespace of the agent-executor it trusts; they are reserved for
// the same reason, so that no controller-owned runner input is author-overridable.
//
// Deliberately NOT reserved: PROMETHEUS_URL (the runner takes its Prometheus URL from its own
// flag or discovery and reads nothing from this variable, and it carries no authority),
// TRACEPARENT/TRACESTATE (appended after the author's entries, so the controller's value already
// wins), and the LLM credential names (NIRMATA_LLM_TOKEN and its legacy aliases), which
// spec.execution.job.env is documented to override.
var reservedRunnerEnvNames = map[string]struct{}{
	secretmount.EnvVar:         {},
	"WORKFLOW_RUN_NAME":        {},
	"WORKFLOW_RUN_NAMESPACE":   {},
	"JOB_NAME":                 {},
	"POD_NAME":                 {},
	"AGENT_EXECUTOR_NAMESPACE": {},
}

// rejectReservedRunnerEnv returns an error when env (spec.execution.job.env) names any variable
// in reservedRunnerEnvNames. buildWorkflowRunnerJob calls it before the author's entries are
// merged, so the run fails terminally (setRunFailed) before any runner Job, ServiceAccount
// binding or Secret copy is created. Fail-closed rather than drop-and-continue: silently
// discarding the entry would leave an author believing the override took effect, and honouring
// it is the escalation described on reservedRunnerEnvNames. Every offending name is reported at
// once, with the full reserved list, so a corrected run does not fail again on the next one.
func rejectReservedRunnerEnv(env []corev1.EnvVar) error {
	var offending []string
	seen := make(map[string]struct{})
	for _, e := range env {
		if _, reserved := reservedRunnerEnvNames[e.Name]; !reserved {
			continue
		}
		if _, dup := seen[e.Name]; dup {
			continue
		}
		seen[e.Name] = struct{}{}
		offending = append(offending, e.Name)
	}
	if len(offending) == 0 {
		return nil
	}
	reserved := make([]string, 0, len(reservedRunnerEnvNames))
	for name := range reservedRunnerEnvNames {
		reserved = append(reserved, name)
	}
	sort.Strings(reserved)
	names := strings.Join(offending, ", ")
	return fmt.Errorf("spec.execution.job.env must not set %s: reserved for the controller, which sets these "+
		"names on the runner Job itself, and a duplicate entry would replace the controller's value "+
		"(the last entry with a given name wins). Remove %s "+
		"from the WorkflowRun's spec.execution.job.env, or from the Workflow's spec.execution if the run "+
		"was created from it. Reserved names: %s",
		names, names, strings.Join(reserved, ", "))
}
