/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func runnerJobBuildFixture(steps ...ottoflowv1alpha1.Step) (*ottoflowv1alpha1.Workflow, *ottoflowv1alpha1.WorkflowRun) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	return wf, wr
}

func TestBuildRunnerJob_RejectsReservedCAVolumeNameForEveryJob(t *testing.T) {
	wf, wr := runnerJobBuildFixture()
	wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
		Volumes: []corev1.Volume{{
			Name:         agentExecutorCAVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "tenant-chosen"}}},
		}},
	}}
	r := &WorkflowRunReconciler{}

	_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err == nil || !strings.Contains(err.Error(), "reserved for the agent-executor CA") {
		t.Fatalf("a tenant volume named %q must be rejected, got: %v", agentExecutorCAVolumeName, err)
	}
}

func TestBuildRunnerJob_WalkersReadThroughTheDirectReader(t *testing.T) {
	wf, wr := runnerJobBuildFixture(ottoflowv1alpha1.Step{
		Name: "s1", StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "tpl"},
	})
	tpl := &ottoflowv1alpha1.StepTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: defaultNamespace}}
	r := &WorkflowRunReconciler{
		Client:       fake.NewClientBuilder().WithScheme(unitTestScheme).Build(),
		APIReader:    fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(tpl).Build(),
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{AgentExecutorCASecret: "ottoflow-agent-executor.ottoflow.svc.tls-ca"},
	}

	if _, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf); err != nil {
		t.Fatalf("a StepTemplate the cache has not seen yet must be found through the direct reader: %v", err)
	}
}

func TestBuildRunnerJob_CredentialsPreflightReadsThroughTheDirectReaderAndRequeues(t *testing.T) {
	wf, wr := runnerJobBuildFixture(ottoflowv1alpha1.Step{
		Name: "s1", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"},
	})
	agent := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent1", Namespace: defaultNamespace},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	direct := interceptor.NewClient(fake.NewClientBuilder().WithScheme(unitTestScheme).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *corev1.Secret:
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("denied"))
			case *ottoflowv1alpha1.Agent:
				return errors.New("connection reset by peer")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	r := &WorkflowRunReconciler{
		Client:       fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(agent).Build(),
		APIReader:    direct,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{LLMCredentialsSecret: "ottoflow-llm-credentials"},
	}

	_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if !isTransient(err) {
		t.Fatalf("a transient Agent read error on the denied-credentials path must requeue the build, got: %v", err)
	}
}

func TestBuildRunnerJob_CredentialsPreflightFindsAStepTemplateThroughTheDirectReader(t *testing.T) {
	wf, wr := runnerJobBuildFixture(ottoflowv1alpha1.Step{
		Name: "s1", StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "tpl"},
	})
	tpl := &ottoflowv1alpha1.StepTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: defaultNamespace}}
	direct := secretGetErrorReader(fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(tpl).Build(), nil,
		func(key client.ObjectKey) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("denied"))
		})
	r := &WorkflowRunReconciler{
		Client:       fake.NewClientBuilder().WithScheme(unitTestScheme).Build(),
		APIReader:    direct,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{LLMCredentialsSecret: "ottoflow-llm-credentials"},
	}

	if _, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf); err != nil {
		t.Fatalf("a StepTemplate the cache has not seen yet must be found through the direct reader on the denied-credentials path: %v", err)
	}
}
