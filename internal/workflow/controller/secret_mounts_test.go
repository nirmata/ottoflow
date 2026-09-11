/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

const plaintextCAValue = "-----BEGIN CERTIFICATE-----\nsecret-ca-plaintext-marker\n-----END CERTIFICATE-----"
const plaintextTokenValue = "super-secret-bearer-token-plaintext-marker"
const plaintextKubeconfigValue = "apiVersion: v1\nkind: Config\n# kubeconfig-plaintext-marker"

func TestBuildWorkflowRunnerJob_NoPlaintextSecretValuesInJobSpec(t *testing.T) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:         "https://agent.example.com",
						Prompt:      "'hi'",
						CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "agent-ca"},
						Auth: &ottoflowv1alpha1.ExternalAgentAuth{
							SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"},
						},
					},
				},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
			ClusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target-kubeconfig"},
			},
		},
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-ca", Namespace: ns},
		Data:       map[string][]byte{"ca.crt": []byte(plaintextCAValue)},
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte(plaintextTokenValue)},
	}
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "target-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{"config": []byte(plaintextKubeconfigValue)},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithObjects(wf, wr, caSecret, tokenSecret, kubeconfigSecret).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}

	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	rawStr := string(raw)
	for _, plaintext := range []string{plaintextCAValue, plaintextTokenValue, plaintextKubeconfigValue} {
		if strings.Contains(rawStr, plaintext) {
			t.Errorf("Job spec contains a plaintext secret value %q", plaintext)
		}
	}
}

func TestBuildWorkflowRunnerJob_KubeconfigIsWholeSecretVolume(t *testing.T) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns}}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
			ClusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target-kubeconfig"},
			},
		},
	}
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "target-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{"config": []byte(plaintextKubeconfigValue)},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, kubeconfigSecret).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}

	var found *corev1.Volume
	for i, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "target-kubeconfig" {
			found = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if found == nil {
		t.Fatal("expected a Secret volume for the kubeconfig secret")
	}
	if len(found.Secret.Items) != 0 {
		t.Errorf("kubeconfig volume must mount the WHOLE secret (no Items filter); got Items=%v", found.Secret.Items)
	}
	var mountPath string
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == found.Name {
			mountPath = m.MountPath
		}
	}
	if mountPath != secretmount.KubeconfigDir {
		t.Errorf("kubeconfig mount path = %q, want %q", mountPath, secretmount.KubeconfigDir)
	}
}

// TestBuildWorkflowRunnerJob_SecretVolumeNameCollision proves that when an
// operator-supplied spec.execution.job.volumes entry happens to collide with a volume
// name buildSecretMounts would otherwise mint, buildWorkflowRunnerJob fails with a clear
// error naming the colliding volume, rather than letting the collision reach the
// Kubernetes API as an opaque "duplicate volume name" Job-creation error.
func TestBuildWorkflowRunnerJob_SecretVolumeNameCollision(t *testing.T) {
	ns := defaultNamespace

	newWorkflowRun := func() *ottoflowv1alpha1.WorkflowRun {
		return &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
				ClusterRef: &ottoflowv1alpha1.ClusterRef{
					KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target-kubeconfig"},
				},
			},
		}
	}
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "target-kubeconfig", Namespace: ns},
		Data:       map[string][]byte{"config": []byte(plaintextKubeconfigValue)},
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte(plaintextTokenValue)},
	}
	// Both a kubeconfig ref AND an externalAgentRef Secret ref, so the test can trigger a
	// collision on either the "ottoflow-secret-kubeconfig" name or the "ottoflow-secret-0"
	// indexed name buildSecretMounts mints.
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "https://agent.example.com",
						Prompt: "'hi'",
						Auth:   &ottoflowv1alpha1.ExternalAgentAuth{SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"}},
					},
				},
			},
		},
	}

	tests := []struct {
		name           string
		collidingVol   string
		wantErrContain string
	}{
		{name: "kubeconfig volume name", collidingVol: "ottoflow-secret-kubeconfig", wantErrContain: "ottoflow-secret-kubeconfig"},
		{name: "indexed secret volume name", collidingVol: "ottoflow-secret-0", wantErrContain: "ottoflow-secret-0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wr := newWorkflowRun()
			wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Volumes: []corev1.Volume{{Name: tc.collidingVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, kubeconfigSecret, tokenSecret).Build()
			r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

			_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
			if err == nil {
				t.Fatal("expected a volume-name collision error")
			}
			if !strings.Contains(err.Error(), tc.wantErrContain) {
				t.Errorf("error %q does not name the colliding volume %q", err.Error(), tc.wantErrContain)
			}
		})
	}
}

// TestBuildWorkflowRunnerJob_AgentExecutorCAVolumeNameCollision extends the collision guard to
// the "agent-executor-ca" volume: unlike the ottoflow-secret-* volumes buildSecretMounts mints
// (guarded by its reserved-prefix check), this one is appended directly by
// buildWorkflowRunnerJob, so without its own check an operator-supplied
// spec.execution.job.volumes entry of that exact name would reach the Kubernetes API as an
// opaque "duplicate volume name" Job-creation error instead of an actionable one.
func TestBuildWorkflowRunnerJob_AgentExecutorCAVolumeNameCollision(t *testing.T) {
	ns := defaultNamespace
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "do stuff", ModelProvider: "openai"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Volumes: []corev1.Volume{{Name: "agent-executor-ca", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, agentCRD).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{AgentExecutorCASecret: "ca-secret"}}

	_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err == nil {
		t.Fatal("expected a volume-name collision error")
	}
	if !strings.Contains(err.Error(), "agent-executor-ca") {
		t.Errorf("error %q does not name the colliding volume %q", err.Error(), "agent-executor-ca")
	}
}

func TestBuildWorkflowRunnerJob_CrossNamespaceRefRejected(t *testing.T) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "https://agent.example.com",
						Prompt: "'hi'",
						CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
							Name:      "agent-ca",
							Namespace: "other-ns",
						},
					},
				},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err == nil {
		t.Fatal("expected an error rejecting the cross-namespace secret ref")
	}
	// This ref needs to be mounted into the runner Job (externalAgentRef
	// caSecretRef), so the error must explain that a native Secret volume is
	// same-namespace-only and must NOT suggest the operator allowlist — it can never
	// help this reference kind, see validateSecretRefPolicy's NeedsRunnerMount branch.
	if !strings.Contains(err.Error(), "native Kubernetes Secret volume") {
		t.Errorf("expected error to explain the same-namespace-only constraint, got: %v", err)
	}
	if strings.Contains(err.Error(), "grant the runner ServiceAccount a Role+RoleBinding") {
		t.Errorf("error must not suggest the allowlist remediation for a NeedsRunnerMount ref, got: %v", err)
	}
}

// TestBuildWorkflowRunnerJob_CrossNamespaceSubWorkflowMCPAuthRejected proves the policy walk
// crosses a workflowRef boundary: a sub-workflow in another namespace whose mcpToolCall step
// carries MCPServer auth creds resolves those creds in the SUB-workflow's namespace, and —
// because the sub-workflow executes inline in the parent's runner Job (NeedsRunnerMount) —
// validateSecretRefPolicy must reject the run with the native-volume explanation rather than
// letting it fail later or, worse, mount nothing silently.
func TestBuildWorkflowRunnerJob_CrossNamespaceSubWorkflowMCPAuthRejected(t *testing.T) {
	ns := defaultNamespace
	subWF := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-wf", Namespace: "other-ns"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "callTool",
					MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "bearer-server", Tool: "t"},
				},
			},
		},
	}
	mcpServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "bearer-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "http://x"},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type:      "bearer",
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "bearer-secret", Key: "token"},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "runSub",
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub-wf", Namespace: "other-ns"},
				},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, subWF, mcpServer, wr).Build()
	// The allowlist deliberately includes other-ns: a NeedsRunnerMount ref must be rejected
	// even when the namespace is allowlisted, because no allowlist can make a native Secret
	// volume reach another namespace.
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme,
		RunnerConfig: RunnerConfig{SecretRefAllowedNamespaces: map[string]struct{}{"other-ns": {}}}}

	_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err == nil {
		t.Fatal("expected an error rejecting the sub-workflow's cross-namespace MCP auth ref")
	}
	if !strings.Contains(err.Error(), "native Kubernetes Secret volume") {
		t.Errorf("expected the same-namespace-only explanation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "workflowRef") {
		t.Errorf("expected the error to mention the workflowRef remediation, got: %v", err)
	}
}

// TestBuildWorkflowRunnerJob_MCPEnvCredIsMountedNotPodEnvVar proves an MCPServer Spec.Env
// SecretKeyRef credential is delivered to the runner as a mounted file (via
// OTTOFLOW_SECRET_MOUNTS), NOT as a pod env var. The runner resolves MCP env creds only from
// the mount map (resolveEnvValue, useAPISecretAccess=false), so a pod SecretKeyRef env var
// would be clobbered by the runner-built empty value and the credential would arrive empty.
func TestBuildWorkflowRunnerJob_MCPEnvCredIsMountedNotPodEnvVar(t *testing.T) {
	ns := defaultNamespace
	mcpServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: ns},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"gh-mcp"}},
			Env: []corev1.EnvVar{
				{
					Name: "GITHUB_TOKEN",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "github-secret"},
							Key:                  "token",
						},
					},
				},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "callGH", MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "gh", Tool: "doThing"}},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, mcpServer).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}

	containerEnv := job.Spec.Template.Spec.Containers[0].Env
	var mountsJSON string
	for _, e := range containerEnv {
		if e.Name == "GITHUB_TOKEN" {
			t.Errorf("MCP env cred GITHUB_TOKEN must NOT be a pod env var; got %+v", e)
		}
		if e.Name == secretmount.EnvVar {
			mountsJSON = e.Value
		}
	}
	if mountsJSON == "" {
		t.Fatalf("expected %s env var carrying the MCP env-cred mount, got none", secretmount.EnvVar)
	}
	var m secretmount.Mounts
	if err := json.Unmarshal([]byte(mountsJSON), &m); err != nil {
		t.Fatalf("unmarshal mounts: %v", err)
	}
	if _, ok := m[secretmount.Key(ns, "github-secret", "token")]; !ok {
		t.Errorf("expected a mount-map entry for the MCP env cred, got %v", m)
	}
}

// buildGHMCPFixture builds the MCPServer/Workflow/WorkflowRun/fake-client fixture shared by
// TestBuildWorkflowRunnerJob_SecretVolumeOriginsAnnotation and
// TestBuildWorkflowRunnerJob_GeneratedMountsNeedNoRunnerSecretRBAC: a single "callGH" mcpToolCall
// step against an MCPServer with a bearer auth.secretRef. withEnvCred additionally gives the
// MCPServer an env SecretKeyRef credential, which the RBAC test needs to assert is mounted as a
// file, never a pod env var; the origins-annotation test has no use for it and passes false.
func buildGHMCPFixture(ns string, withEnvCred bool) (*WorkflowRunReconciler, *ottoflowv1alpha1.Workflow, *ottoflowv1alpha1.WorkflowRun) {
	mcpServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: ns},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"gh-mcp"}},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type:      "bearer",
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "gh-auth", Key: "token"},
			},
		},
	}
	if withEnvCred {
		mcpServer.Spec.Env = []corev1.EnvVar{
			{
				Name: "GITHUB_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "gh-env-secret"},
						Key:                  "token",
					},
				},
			},
		}
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "callGH", MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "gh", Tool: "doThing"}},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, mcpServer).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	return r, wf, wr
}

// TestBuildWorkflowRunnerJob_SecretVolumeOriginsAnnotation proves buildWorkflowRunnerJob sets
// generatedSecretVolumeOriginsAnnotation on the Job, mapping each minted volume name back to
// the resolvedSecretRef.Origin that produced it.
//
// Revert -> red: drop the annotation-setting block -> the annotation is absent.
func TestBuildWorkflowRunnerJob_SecretVolumeOriginsAnnotation(t *testing.T) {
	r, wf, wr := buildGHMCPFixture(defaultNamespace, false)

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}

	originsJSON, ok := job.Annotations[generatedSecretVolumeOriginsAnnotation]
	if !ok || originsJSON == "" {
		t.Fatalf("expected annotation %q to be set on the Job, got annotations: %v", generatedSecretVolumeOriginsAnnotation, job.Annotations)
	}
	var origins map[string]string
	if err := json.Unmarshal([]byte(originsJSON), &origins); err != nil {
		t.Fatalf("unmarshal origins annotation: %v", err)
	}
	origin, ok := origins["ottoflow-secret-0"]
	if !ok {
		t.Fatalf("expected origins to map ottoflow-secret-0, got %v", origins)
	}
	if !strings.Contains(origin, "callGH") || !strings.Contains(origin, "auth.secretRef") {
		t.Errorf("expected the origin to name the MCPServer auth.secretRef step, got %q", origin)
	}
}

// TestBuildWorkflowRunnerJob_GeneratedMountsNeedNoRunnerSecretRBAC is the unit-level form of
// the runner-RBAC guarantee: every Secret ref the controller collects for
// the runner Job — an MCP auth.secretRef and an MCP env credential here — must reach the pod as
// a Secret VOLUME with a KeyToPath item and an OTTOFLOW_SECRET_MOUNTS entry, and NEVER as a
// container EnvVar with a SecretKeyRef: a volume is resolved by the kubelet under node
// credentials (no RBAC on the runner ServiceAccount needed); a SecretKeyRef env var would
// require the runner's own Secret RBAC, which the runner is designed to run without (see
// docs/user/rbac-secret-access.md).
//
// Revert -> red: emit a SecretKeyRef EnvVar for a collected ref instead of routing it through
// the mount map.
func TestBuildWorkflowRunnerJob_GeneratedMountsNeedNoRunnerSecretRBAC(t *testing.T) {
	ns := defaultNamespace
	r, wf, wr := buildGHMCPFixture(ns, true)

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	for _, e := range container.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("no container EnvVar may carry a SecretKeyRef (runner-SA Secret RBAC would be required); got %+v", e)
		}
	}

	var mountsJSON string
	for _, e := range container.Env {
		if e.Name == secretmount.EnvVar {
			mountsJSON = e.Value
		}
	}
	if mountsJSON == "" {
		t.Fatalf("expected %s to be set", secretmount.EnvVar)
	}
	var m secretmount.Mounts
	if err := json.Unmarshal([]byte(mountsJSON), &m); err != nil {
		t.Fatalf("unmarshal mounts: %v", err)
	}
	for _, key := range []string{
		secretmount.Key(ns, "gh-auth", "token"),
		secretmount.Key(ns, "gh-env-secret", "token"),
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected a mount-map entry for %q, got %v", key, m)
		}
	}

	var sawAuthVol, sawEnvVol bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret == nil {
			continue
		}
		switch v.Secret.SecretName {
		case "gh-auth":
			sawAuthVol = true
		case "gh-env-secret":
			sawEnvVol = true
		}
		if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "token" {
			t.Errorf("expected a single KeyToPath item for volume %q, got %+v", v.Name, v.Secret.Items)
		}
	}
	if !sawAuthVol || !sawEnvVol {
		t.Errorf("expected both the MCP auth Secret and the MCP env-cred Secret as volumes, sawAuthVol=%v sawEnvVol=%v", sawAuthVol, sawEnvVol)
	}
}

// TestBuildWorkflowRunnerJob_SecretVolumeOriginsAnnotationCapped proves the 8 KiB annotation cap
// (maxSecretVolumeOriginsAnnotationBytes, secret_refs.go): when the marshaled origins map would
// exceed it, buildWorkflowRunnerJob omits the annotation entirely rather than truncating it — but
// the Secret volumes and mounts (the runner-critical output) are unaffected. buildSecretMounts
// itself returns the raw origins map uncapped; buildWorkflowRunnerJob applies the cap at the
// point it marshals the map onto the Job annotation, so this is where the revert property lives.
//
// Revert -> red: drop the cap check when writing the annotation -> the annotation gets set
// unconditionally and this test starts failing.
func TestBuildWorkflowRunnerJob_SecretVolumeOriginsAnnotationCapped(t *testing.T) {
	ns := defaultNamespace
	const n = 150 // enough distinct MCPServer auth.secretRef origins to exceed the 8 KiB cap
	objs := make([]client.Object, 0, n+2)
	steps := make([]ottoflowv1alpha1.Step, 0, n)
	for i := 0; i < n; i++ {
		serverName := fmt.Sprintf("gh%d", i)
		objs = append(objs, &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: ns},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"gh-mcp"}},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: fmt.Sprintf("gh-auth-%d", i), Key: "token"},
				},
			},
		})
		steps = append(steps, ottoflowv1alpha1.Step{
			Name:        fmt.Sprintf("call%d", i),
			MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: serverName, Tool: "doThing"},
		})
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	objs = append(objs, wf, wr)
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(objs...).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}

	if v, ok := job.Annotations[generatedSecretVolumeOriginsAnnotation]; ok {
		t.Errorf("expected annotation %q to be omitted over the %d-byte cap, got %d bytes",
			generatedSecretVolumeOriginsAnnotation, maxSecretVolumeOriginsAnnotationBytes, len(v))
	}

	if len(job.Spec.Template.Spec.Volumes) < n {
		t.Fatalf("the origins cap must not affect the Secret volumes, got %d volumes for %d refs",
			len(job.Spec.Template.Spec.Volumes), n)
	}
	var sawMountsEnv bool
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == secretmount.EnvVar && e.Value != "" {
			sawMountsEnv = true
		}
	}
	if !sawMountsEnv {
		t.Error("the origins cap must not affect the OTTOFLOW_SECRET_MOUNTS env var")
	}
}
