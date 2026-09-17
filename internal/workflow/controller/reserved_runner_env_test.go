/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

// controllerOwnedRunnerEnv spells the reserved set out by hand. reservedRunnerEnvNames is the
// implementation; this list is the specification, so a name silently dropped from the set (which
// would reopen the override for that variable) fails TestReservedRunnerEnvNames_IsExactlyTheControllerOwnedSet
// instead of going unnoticed.
var controllerOwnedRunnerEnv = []string{
	"OTTOFLOW_SECRET_MOUNTS",
	"WORKFLOW_RUN_NAME",
	"WORKFLOW_RUN_NAMESPACE",
	"JOB_NAME",
	"POD_NAME",
	"AGENT_EXECUTOR_NAMESPACE",
}

func TestReservedRunnerEnvNames_IsExactlyTheControllerOwnedSet(t *testing.T) {
	got := make([]string, 0, len(reservedRunnerEnvNames))
	for name := range reservedRunnerEnvNames {
		got = append(got, name)
	}
	slices.Sort(got)
	want := slices.Clone(controllerOwnedRunnerEnv)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("reservedRunnerEnvNames = %v, want %v", got, want)
	}
	// The secret-mount variable is referenced through its constant so a rename on the runner side
	// is followed here; pin the constant's value so that rename cannot silently change WHICH
	// author-supplied name is refused.
	if secretmount.EnvVar != "OTTOFLOW_SECRET_MOUNTS" {
		t.Errorf("secretmount.EnvVar = %q, want OTTOFLOW_SECRET_MOUNTS", secretmount.EnvVar)
	}
}

func TestRejectReservedRunnerEnv_RejectsEachReservedName(t *testing.T) {
	for _, name := range controllerOwnedRunnerEnv {
		t.Run(name, func(t *testing.T) {
			// Only the name matters: a literal value and a valueFrom reference are both refused,
			// since either would replace the controller's entry in the container environment.
			entries := []corev1.EnvVar{
				{Name: name, Value: "author-supplied"},
				{Name: name, ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k",
					},
				}},
			}
			for _, entry := range entries {
				err := rejectReservedRunnerEnv([]corev1.EnvVar{{Name: "HARMLESS", Value: "x"}, entry})
				if err == nil {
					t.Fatalf("rejectReservedRunnerEnv accepted %+v", entry)
				}
				for _, want := range []string{name, "reserved", "spec.execution.job.env", "Remove " + name} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error must contain %q so the author knows what to change; got: %v", want, err)
					}
				}
			}
		})
	}
}

func TestRejectReservedRunnerEnv_ReportsEveryOffendingNameOnce(t *testing.T) {
	err := rejectReservedRunnerEnv([]corev1.EnvVar{
		{Name: "JOB_NAME", Value: "a"},
		{Name: "TEAM_SETTING", Value: "on"},
		{Name: "OTTOFLOW_SECRET_MOUNTS", Value: "b"},
		{Name: "JOB_NAME", Value: "c"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "must not set JOB_NAME, OTTOFLOW_SECRET_MOUNTS:") {
		t.Errorf("error must list each offending name once, in order of appearance; got: %s", msg)
	}
	if strings.Contains(msg, "TEAM_SETTING") {
		t.Errorf("error must not name a non-reserved entry; got: %s", msg)
	}
	// The full reserved list is part of the message so a corrected run does not fail again on
	// the next reserved name.
	for _, name := range controllerOwnedRunnerEnv {
		if !strings.Contains(msg, name) {
			t.Errorf("error must list every reserved name, missing %q; got: %s", name, msg)
		}
	}
}

func TestRejectReservedRunnerEnv_AcceptsNonReservedNames(t *testing.T) {
	cases := map[string][]corev1.EnvVar{
		"nil":   nil,
		"empty": {},
		"application setting": {
			{Name: "TEAM_SETTING", Value: "on"},
		},
		// Published by the controller but not reserved: nothing in the runner reads it from the
		// environment and it carries no authority.
		"PROMETHEUS_URL": {
			{Name: "PROMETHEUS_URL", Value: "http://prometheus.tenant:9090"},
		},
		// The documented override path for LLM credentials.
		"NIRMATA_LLM_TOKEN": {
			{Name: "NIRMATA_LLM_TOKEN", Value: "explicit"},
		},
		// Env names are case-sensitive and match exactly: these are different variables.
		"lower-case look-alike": {
			{Name: "ottoflow_secret_mounts", Value: "x"},
		},
		"prefixed look-alike": {
			{Name: "OTTOFLOW_SECRET_MOUNTS_EXTRA", Value: "x"},
			{Name: "XPOD_NAME", Value: "x"},
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := rejectReservedRunnerEnv(env); err != nil {
				t.Errorf("rejectReservedRunnerEnv(%v) = %v, want nil", env, err)
			}
		})
	}
}

// TestReservedRunnerEnvNames_CoverEveryControllerEnvAheadOfAuthorEntries derives the reserved set
// from what buildWorkflowRunnerJob actually publishes rather than from a list: with every
// RunnerConfig field that produces runner env configured and a Secret reference in the workflow,
// each env name the controller places AHEAD of the author's entries must be reserved (an author
// duplicate would win) unless it is in the explicit carries-no-authority list, and each reserved
// name must really be published there. A new controller env var added ahead of the author's
// entries — once this fixture configures whatever gates it — or a reserved name the controller no
// longer sets, fails here.
func TestReservedRunnerEnvNames_CoverEveryControllerEnvAheadOfAuthorEntries(t *testing.T) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{
				Name: "callAgent",
				ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
					URL:    "https://agent.example.com",
					Prompt: "'hi'",
					Auth: &ottoflowv1alpha1.ExternalAgentAuth{
						SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"},
					},
				},
			}},
		},
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	const marker = "AUTHOR_MARKER"
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{Env: []corev1.EnvVar{{Name: marker, Value: "1"}}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, tokenSecret, wr).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
		PrometheusURL:          "http://prometheus.monitoring:9090",
		AgentExecutorNamespace: "ottoflow",
	}}

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	markerAt := slices.IndexFunc(env, func(e corev1.EnvVar) bool { return e.Name == marker })
	if markerAt < 0 {
		t.Fatalf("author entry %s did not reach the Job env: %v", marker, env)
	}

	// Controller-published names that are NOT reserved, each with the reason. Extending this list
	// is a deliberate decision that the variable carries no authority.
	unreservedByDesign := map[string]string{
		"PROMETHEUS_URL": "nothing in the runner reads it from the environment; it carries no authority",
	}
	var ahead []string
	for _, e := range env[:markerAt] {
		ahead = append(ahead, e.Name)
		if _, reserved := reservedRunnerEnvNames[e.Name]; reserved {
			continue
		}
		if _, ok := unreservedByDesign[e.Name]; ok {
			continue
		}
		t.Errorf("buildWorkflowRunnerJob publishes %q ahead of spec.execution.job.env, so an author "+
			"duplicate would replace it, but it is neither reserved nor listed as unreserved by design", e.Name)
	}
	for name := range reservedRunnerEnvNames {
		if !slices.Contains(ahead, name) {
			t.Errorf("%q is reserved but buildWorkflowRunnerJob did not publish it ahead of the author's "+
				"entries with a full configuration (published: %v); fix the fixture or drop the name", name, ahead)
		}
	}
}
