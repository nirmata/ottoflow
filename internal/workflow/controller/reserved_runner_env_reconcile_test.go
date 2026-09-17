/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// The tests in this file pin the reserved-name behaviour through buildWorkflowRunnerJob and
// Reconcile alone. They name the variables literally and reference no helper of the check, so
// they stay meaningful if the implementation is refactored, and they fail if the check is moved
// after the runner Job is created or a name stops being rejected.

// runnerServiceAccountTokenPath is the file an author would aim an approved credential reference
// at: the runner pod mounts its own ServiceAccount token there.
const runnerServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// reservedEnvFixture returns a Workflow whose externalAgentRef step authenticates with a
// Secret-backed bearer token — so the controller publishes its own OTTOFLOW_SECRET_MOUNTS map for
// every run of it — the token Secret, and a WorkflowRun of that Workflow whose
// spec.execution.job.env is env. Everything lives in defaultNamespace.
func reservedEnvFixture(env []corev1.EnvVar) (*ottoflowv1alpha1.Workflow, *corev1.Secret, *ottoflowv1alpha1.WorkflowRun) {
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
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("approved-token")},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{Env: env},
			},
		},
		Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	return wf, secret, wr
}

// describeEnv reports every container env entry named name, with its position, so a failure
// shows what the runner would have received and in which order.
func describeEnv(job *batchv1.Job, name string) string {
	if job == nil {
		return "no Job"
	}
	var parts []string
	for i, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name != name {
			continue
		}
		if e.Value == "" && e.ValueFrom != nil {
			parts = append(parts, fmt.Sprintf("env[%d]=<valueFrom>", i))
			continue
		}
		parts = append(parts, fmt.Sprintf("env[%d]=%q", i, e.Value))
	}
	if len(parts) == 0 {
		return "no entry named " + name
	}
	return strings.Join(parts, ", ")
}

func TestBuildWorkflowRunnerJob_RejectsEachReservedRunnerEnvName(t *testing.T) {
	for _, name := range []string{
		"OTTOFLOW_SECRET_MOUNTS",
		"WORKFLOW_RUN_NAME",
		"WORKFLOW_RUN_NAMESPACE",
		"JOB_NAME",
		"POD_NAME",
		"AGENT_EXECUTOR_NAMESPACE",
	} {
		t.Run(name, func(t *testing.T) {
			wf, secret, wr := reservedEnvFixture([]corev1.EnvVar{{Name: name, Value: "author-supplied"}})
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, secret, wr).Build()
			r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme,
				RunnerConfig: RunnerConfig{AgentExecutorNamespace: "ottoflow"}}

			job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
			if err == nil {
				t.Fatalf("buildWorkflowRunnerJob accepted a spec.execution.job.env entry named %s; the runner "+
					"Job carries %s and the last entry wins", name, describeEnv(job, name))
			}
			if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error must name the reserved variable and say it is reserved; got: %v", err)
			}
		})
	}
}

func TestBuildWorkflowRunnerJob_KeepsNonReservedAuthorEnv(t *testing.T) {
	author := []corev1.EnvVar{
		{Name: "TEAM_SETTING", Value: "on"},
		{Name: "PROMETHEUS_URL", Value: "http://prometheus.tenant:9090"},
	}
	wf, secret, wr := reservedEnvFixture(author)
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, secret, wr).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme,
		RunnerConfig: RunnerConfig{PrometheusURL: "http://prometheus.monitoring:9090", AgentExecutorNamespace: "ottoflow"}}

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob rejected non-reserved author env: %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	for _, want := range author {
		var last *corev1.EnvVar
		for i := range env {
			if env[i].Name == want.Name {
				last = &env[i]
			}
		}
		if last == nil {
			t.Errorf("author entry %s did not reach the Job env", want.Name)
			continue
		}
		// The author's entry is appended after the controller's, so for a non-reserved name it is
		// the effective value; that is the documented precedence and it must keep working.
		if last.Value != want.Value {
			t.Errorf("effective %s = %q, want the author's %q (entries: %s)", want.Name, last.Value, want.Value, describeEnv(job, want.Name))
		}
	}
}

// TestWorkflowRunReconciler_Reconcile_ReservedRunnerEnv_FailsRunBeforeJobCreation drives the
// escalation through Reconcile: the run sets OTTOFLOW_SECRET_MOUNTS to a map that aims the
// workflow's approved bearer-token reference at the runner's own ServiceAccount token. The run
// must end Failed with a message the author can act on, and nothing may have been created for it
// — no runner Job (the entry must never reach a pod), and no ServiceAccount or binding either,
// which pins the check ahead of the runner-access writes in reconcileJobExecution as well.
func TestWorkflowRunReconciler_Reconcile_ReservedRunnerEnv_FailsRunBeforeJobCreation(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	authorMap := fmt.Sprintf(`{"%s/agent-token/token":%q}`, ns, runnerServiceAccountTokenPath)
	wf, secret, wr := reservedEnvFixture([]corev1.EnvVar{{Name: "OTTOFLOW_SECRET_MOUNTS", Value: authorMap}})
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(wf, secret, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
		RunnerServiceAccount:   "controller-manager",
		RunnerClusterRole:      "ottoflow-role",
		AgentExecutorNamespace: "ottoflow",
	}}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})

	job := &batchv1.Job{}
	switch err := c.Get(ctx, types.NamespacedName{Name: "run-1-runner", Namespace: ns}, job); {
	case err == nil:
		t.Errorf("a runner Job was created for a run whose spec.execution.job.env sets OTTOFLOW_SECRET_MOUNTS; "+
			"its container env carries %s — the last entry wins, so the runner would resolve the approved "+
			"bearer-token reference to %s", describeEnv(job, "OTTOFLOW_SECRET_MOUNTS"), runnerServiceAccountTokenPath)
	case !apierrors.IsNotFound(err):
		t.Fatalf("get runner Job: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	switch err := c.Get(ctx, types.NamespacedName{Name: "controller-manager", Namespace: ns}, sa); {
	case err == nil:
		t.Error("the runner ServiceAccount was created before the reserved-name check ran; the check must precede every write")
	case !apierrors.IsNotFound(err):
		t.Fatalf("get runner ServiceAccount: %v", err)
	}
	crb := &rbacv1.ClusterRoleBinding{}
	switch err := c.Get(ctx, types.NamespacedName{Name: workflowRunnerRoleBindingName(ns, "controller-manager")}, crb); {
	case err == nil:
		t.Error("the runner ClusterRoleBinding was created before the reserved-name check ran; the check must precede every write")
	case !apierrors.IsNotFound(err):
		t.Fatalf("get runner ClusterRoleBinding: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %q, want Failed: the run must fail terminally, not wait for a Job that must never exist", updated.Status.Phase)
	}
	for _, want := range []string{"OTTOFLOW_SECRET_MOUNTS", "reserved", "spec.execution.job.env"} {
		if !strings.Contains(updated.Status.Message, want) {
			t.Errorf("Message must contain %q so the author knows which entry to remove; got: %q", want, updated.Status.Message)
		}
	}
	if updated.Status.FailureReason != "" {
		t.Errorf("FailureReason = %q, want empty: a reserved-name rejection is an author-side spec error, "+
			"not one of the classified operator-side causes", updated.Status.FailureReason)
	}
	if updated.Status.Execution != nil && updated.Status.Execution.JobName != "" {
		t.Errorf("status records runner Job %q for a run that must never get one", updated.Status.Execution.JobName)
	}
}

// TestWorkflowRunReconciler_Reconcile_NonReservedRunnerEnv_ReachesRunnerJob is the control for
// the test above: the same fixture with a harmless author entry still gets its runner Job, the
// entry reaches the container, and the controller's own OTTOFLOW_SECRET_MOUNTS is published
// exactly once — which is what makes the sibling test's override real rather than hypothetical.
func TestWorkflowRunReconciler_Reconcile_NonReservedRunnerEnv_ReachesRunnerJob(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf, secret, wr := reservedEnvFixture([]corev1.EnvVar{{Name: "TEAM_SETTING", Value: "on"}})
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(wf, secret, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
		RunnerServiceAccount:   "controller-manager",
		RunnerClusterRole:      "ottoflow-role",
		AgentExecutorNamespace: "ottoflow",
	}}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: "run-1-runner", Namespace: ns}, job); err != nil {
		t.Fatalf("expected a runner Job for a run with only non-reserved env: %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	count := func(name string) int {
		n := 0
		for _, e := range env {
			if e.Name == name {
				n++
			}
		}
		return n
	}
	if n := count("TEAM_SETTING"); n != 1 {
		t.Errorf("TEAM_SETTING appears %d times in the runner env, want 1 (%s)", n, describeEnv(job, "TEAM_SETTING"))
	}
	if n := count("OTTOFLOW_SECRET_MOUNTS"); n != 1 {
		t.Errorf("OTTOFLOW_SECRET_MOUNTS appears %d times in the runner env, want exactly the controller's one (%s)",
			n, describeEnv(job, "OTTOFLOW_SECRET_MOUNTS"))
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("run failed for a non-reserved author entry: %s", updated.Status.Message)
	}
}
