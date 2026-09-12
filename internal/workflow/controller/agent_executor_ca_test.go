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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// Fixtures for ensureAgentExecutorCA: the certificate manager's CA Secret lives in the install
// namespace (a non-default one, so the tests prove RunnerConfig.AgentExecutorNamespace is
// honoured), the WorkflowRun and the ConfigMap it needs live in a tenant namespace that holds no
// Secret and no grant to read one.
const (
	caTestName      = "ca-secret"
	caTestInstallNS = "ottoflow-system"
	caTestRunNS     = "tenant-ns"
	caTestPartOf    = "app.kubernetes.io/part-of"
	caTestKey       = "private-key-that-must-stay-in-the-install-namespace"
)

// caInstallSecret is the CA Secret as the certificate manager writes it: certificate AND private key.
func caInstallSecret(crt string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caTestName, Namespace: caTestInstallNS},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(crt),
			corev1.TLSPrivateKeyKey: []byte(caTestKey),
		},
	}
}

// caRunnerJob is a runner Job carrying the agent-executor CA volume buildWorkflowRunnerJob adds.
func caRunnerJob() *batchv1.Job {
	return &batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{
			Name: agentExecutorCAVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: caTestName},
				Items:                []corev1.KeyToPath{{Key: agentExecutorCAKey, Path: agentExecutorCAKey}},
			}},
		}},
	}}}}
}

func caTenantConfigMap(crt string, owned bool) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: caTestName, Namespace: caTestRunNS},
		Data:       map[string]string{agentExecutorCAKey: crt},
	}
	if owned {
		cm.Labels = map[string]string{caTestPartOf: ottoflowPartOf}
	}
	return cm
}

func caRun() *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: caTestRunNS}}
}

func caReconciler(c client.Client) *WorkflowRunReconciler {
	return &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{AgentExecutorNamespace: caTestInstallNS}}
}

func getTenantCA(t *testing.T, c client.Client) (*corev1.ConfigMap, error) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: caTestRunNS, Name: caTestName}, cm)
	return cm, err
}

func isTransient(err error) bool {
	var te *transientBuildError
	return errors.As(err, &te)
}

// TestEnsureAgentExecutorCA_NoCAVolume_NoOp: a Job without the agent-executor CA volume (no agent
// step, or the mount disabled) needs nothing, and nothing is read or created.
func TestEnsureAgentExecutorCA_NoCAVolume_NoOp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).Build() // not even the CA Secret exists
	job := &batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{Name: "other", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}}}}
	if err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), job); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
	if _, err := getTenantCA(t, c); !apierrors.IsNotFound(err) {
		t.Fatalf("no ConfigMap must be published for a Job without the CA volume, got err=%v", err)
	}
}

// TestEnsureAgentExecutorCA_PublishesCertificateOnly: the common case. No ConfigMap exists in the
// tenant namespace, so one is created holding exactly the certificate under ca.crt, labelled as
// OttoFlow-managed, with no owner reference, and the private key never leaves the install
// namespace.
func TestEnsureAgentExecutorCA_PublishesCertificateOnly(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(caInstallSecret("crt-v1")).Build()
	if err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob()); err != nil {
		t.Fatalf("ensureAgentExecutorCA: %v", err)
	}
	cm, err := getTenantCA(t, c)
	if err != nil {
		t.Fatalf("expected the ConfigMap to be created: %v", err)
	}
	if len(cm.Data) != 1 || cm.Data[agentExecutorCAKey] != "crt-v1" {
		t.Errorf("Data = %v, want only %s=crt-v1", cm.Data, agentExecutorCAKey)
	}
	for k, v := range cm.Data {
		if strings.Contains(v, caTestKey) || k == corev1.TLSPrivateKeyKey {
			t.Errorf("private key material published under %q", k)
		}
	}
	if len(cm.BinaryData) != 0 {
		t.Errorf("BinaryData must be empty, got %v", cm.BinaryData)
	}
	if cm.Labels[caTestPartOf] != ottoflowPartOf {
		t.Errorf("Labels = %v, want %s=%s", cm.Labels, caTestPartOf, ottoflowPartOf)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("the shared ConfigMap must carry no owner reference, got %v", cm.OwnerReferences)
	}
}

// TestEnsureAgentExecutorCA_RefreshesOwnedStaleConfigMap: an OttoFlow-managed ConfigMap whose
// ca.crt is not the current CA is updated, so a rotated CA reaches the namespace on its next run.
func TestEnsureAgentExecutorCA_RefreshesOwnedStaleConfigMap(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithObjects(caInstallSecret("crt-v2"), caTenantConfigMap("crt-v1", true)).Build()
	if err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob()); err != nil {
		t.Fatalf("ensureAgentExecutorCA: %v", err)
	}
	cm, err := getTenantCA(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[agentExecutorCAKey] != "crt-v2" {
		t.Errorf("ca.crt = %q, want the current CA crt-v2", cm.Data[agentExecutorCAKey])
	}
}

// TestEnsureAgentExecutorCA_CurrentConfigMap_NotRewritten: a ConfigMap already holding the current
// CA is left alone (no Update call; the fake client bumps resourceVersion on every write).
func TestEnsureAgentExecutorCA_CurrentConfigMap_NotRewritten(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithObjects(caInstallSecret("crt-v1"), caTenantConfigMap("crt-v1", true)).Build()
	before, err := getTenantCA(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob()); err != nil {
		t.Fatalf("ensureAgentExecutorCA: %v", err)
	}
	after, err := getTenantCA(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion changed %q -> %q: a current ConfigMap must not be rewritten", before.ResourceVersion, after.ResourceVersion)
	}
}

// TestEnsureAgentExecutorCA_UnownedConfigMapRejected: a ConfigMap of the well-known name without
// the OttoFlow label — e.g. pre-created by someone with configmaps create in the namespace,
// holding a CA of their choosing — is neither trusted nor overwritten. The error is terminal (not
// a transientBuildError) and names the problem.
func TestEnsureAgentExecutorCA_UnownedConfigMapRejected(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithObjects(caInstallSecret("crt-v1"), caTenantConfigMap("someone-elses-ca", false)).Build()
	err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
	if err == nil || !strings.Contains(err.Error(), "not managed by OttoFlow") {
		t.Fatalf("want an error naming the unowned ConfigMap, got %v", err)
	}
	if isTransient(err) {
		t.Errorf("an unowned ConfigMap is configuration to fix, not a transient error: %v", err)
	}
	cm, getErr := getTenantCA(t, c)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if cm.Data[agentExecutorCAKey] != "someone-elses-ca" || cm.Labels[caTestPartOf] != "" {
		t.Errorf("the unowned ConfigMap must be left untouched, got data=%v labels=%v", cm.Data, cm.Labels)
	}
}

// TestEnsureAgentExecutorCA_CASecretMissing_Terminal: the certificate manager fills the CA Secret
// before the manager starts, so a missing Secret at reconcile time is a deleted one; the run
// fails with a message saying how to get it back, and no ConfigMap is published from nothing.
func TestEnsureAgentExecutorCA_CASecretMissing_Terminal(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "restart the controller") {
		t.Fatalf("want a not-found error with the recovery step, got %v", err)
	}
	if isTransient(err) {
		t.Errorf("a missing CA Secret must fail the run, not requeue it forever: %v", err)
	}
	if _, getErr := getTenantCA(t, c); !apierrors.IsNotFound(getErr) {
		t.Errorf("no ConfigMap must be published without a CA, got err=%v", getErr)
	}
}

// TestEnsureAgentExecutorCA_CASecretWithoutCertificate_Terminal: a CA Secret with no tls.crt must
// never become an empty ca.crt in a tenant namespace.
func TestEnsureAgentExecutorCA_CASecretWithoutCertificate_Terminal(t *testing.T) {
	empty := caInstallSecret("")
	delete(empty.Data, corev1.TLSCertKey)
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(empty).Build()
	err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
	if err == nil || !strings.Contains(err.Error(), "has no tls.crt") {
		t.Fatalf("want an error about the missing certificate, got %v", err)
	}
	if isTransient(err) {
		t.Errorf("expected a terminal error, got transient: %v", err)
	}
	if _, getErr := getTenantCA(t, c); !apierrors.IsNotFound(getErr) {
		t.Errorf("no ConfigMap must be published from an empty CA, got err=%v", getErr)
	}
}

// TestEnsureAgentExecutorCA_CASecretForbidden_SecretAccessDenied: a Forbidden read of the CA
// Secret in the install namespace is the one Secret denial this path can meet; it classifies as
// SecretAccessDenied and the message points at the certmanager Role rather than at the tenant
// namespace, where no Secret is involved.
func TestEnsureAgentExecutorCA_CASecretForbidden_SecretAccessDenied(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, caTestName, errors.New("no rule"))
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(caInstallSecret("crt-v1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return forbidden
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
	if err == nil || !strings.Contains(err.Error(), "certmanager Role") {
		t.Fatalf("want a Forbidden error naming the certmanager Role, got %v", err)
	}
	if got := secretAccessDeniedReason(err); got != ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied {
		t.Errorf("failure reason = %q, want SecretAccessDenied", got)
	}
	if isTransient(err) {
		t.Errorf("a Forbidden Secret read is configuration to fix, not transient: %v", err)
	}
}

// TestEnsureAgentExecutorCA_ConfigMapCreateForbidden_Terminal: an operator who tightened the
// controller's ConfigMap grant gets a message naming the verbs to restore, and not
// SecretAccessDenied — no Secret was denied.
func TestEnsureAgentExecutorCA_ConfigMapCreateForbidden_Terminal(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, caTestName, errors.New("no rule"))
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(caInstallSecret("crt-v1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isCM := obj.(*corev1.ConfigMap); isCM {
					return forbidden
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
	if err == nil || !strings.Contains(err.Error(), "create, get and update on configmaps") {
		t.Fatalf("want a Forbidden error naming the configmaps verbs, got %v", err)
	}
	if got := secretAccessDeniedReason(err); got != "" {
		t.Errorf("failure reason = %q, want none: a denied ConfigMap write is not a Secret denial", got)
	}
	if isTransient(err) {
		t.Errorf("expected a terminal error, got transient: %v", err)
	}
}

// TestEnsureAgentExecutorCA_ConfigMapUpdateConflict_Transient: an ordinary API error on the write
// (here a Conflict) is wrapped as transientBuildError so the reconcile requeues instead of
// failing the run.
func TestEnsureAgentExecutorCA_ConfigMapUpdateConflict_Transient(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, caTestName, errors.New("stale"))
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithObjects(caInstallSecret("crt-v2"), caTenantConfigMap("crt-v1", true)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return conflict
			},
		}).Build()
	err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
	if !isTransient(err) {
		t.Fatalf("want a transientBuildError for a Conflict, got %v", err)
	}
}

// TestEnsureAgentExecutorCA_LosesCreateRace_ReconcilesWinner: the Get says NotFound, the Create
// says AlreadyExists (someone else got there first), and the winner is then held to the same
// ownership and content rules as a pre-existing ConfigMap — an owned stale one is refreshed, an
// unowned one is rejected.
func TestEnsureAgentExecutorCA_LosesCreateRace_ReconcilesWinner(t *testing.T) {
	cases := []struct {
		name      string
		winner    *corev1.ConfigMap
		wantErr   string
		wantCACrt string
	}{
		{name: "owned stale winner is refreshed", winner: caTenantConfigMap("crt-v1", true), wantCACrt: "crt-v2"},
		{name: "unowned winner is rejected", winner: caTenantConfigMap("someone-elses-ca", false), wantErr: "not managed by OttoFlow", wantCACrt: "someone-elses-ca"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			firstGet := true
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).
				WithObjects(caInstallSecret("crt-v2"), tc.winner).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isCM := obj.(*corev1.ConfigMap); isCM && firstGet {
							firstGet = false
							return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}).Build()
			err := caReconciler(c).ensureAgentExecutorCA(context.Background(), caRun(), caRunnerJob())
			if tc.wantErr == "" && err != nil {
				t.Fatalf("ensureAgentExecutorCA: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			cm, getErr := getTenantCA(t, c)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if cm.Data[agentExecutorCAKey] != tc.wantCACrt {
				t.Errorf("ca.crt = %q, want %q", cm.Data[agentExecutorCAKey], tc.wantCACrt)
			}
		})
	}
}

// agentRunFixture returns an Agent, a Workflow with one agentRef step, and a WorkflowRun for it,
// all in caTestRunNS, for reconcile-level tests that reach runner Job creation.
func agentRunFixture() (*ottoflowv1alpha1.Agent, *ottoflowv1alpha1.Workflow, *ottoflowv1alpha1.WorkflowRun) {
	agent := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: caTestRunNS, Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "do stuff", ModelProvider: "openai"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: caTestRunNS},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: caTestRunNS},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: caTestRunNS}},
	}
	return agent, wf, wr
}

func caReconcileConfig() RunnerConfig {
	return RunnerConfig{RunnerClusterRole: "runner-role", AgentExecutorCASecret: caTestName, AgentExecutorNamespace: caTestInstallNS}
}

// TestWorkflowRunReconciler_Reconcile_AgentStep_PublishesCAThenCreatesJob drives a full reconcile
// for an agent-step run in a namespace holding no Secret: the CA ConfigMap is published and the
// runner Job is created mounting it, with no Secret-backed volume anywhere on the pod.
func TestWorkflowRunReconciler_Reconcile_AgentStep_PublishesCAThenCreatesJob(t *testing.T) {
	ctx := context.Background()
	agent, wf, wr := agentRunFixture()
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(agent, wf, wr, caInstallSecret("crt-v1")).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: caReconcileConfig()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cm, err := getTenantCA(t, c)
	if err != nil {
		t.Fatalf("CA ConfigMap not published: %v", err)
	}
	if cm.Data[agentExecutorCAKey] != "crt-v1" {
		t.Errorf("ca.crt = %q", cm.Data[agentExecutorCAKey])
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: caTestRunNS, Name: workflowRunnerJobName(wr.Name)}, job); err != nil {
		t.Fatalf("runner Job not created: %v", err)
	}
	var caVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		v := &job.Spec.Template.Spec.Volumes[i]
		if v.Secret != nil {
			t.Errorf("runner pod must carry no Secret-backed volume, got %+v", v)
		}
		if v.Name == agentExecutorCAVolumeName {
			caVol = v
		}
	}
	if caVol == nil || caVol.ConfigMap == nil || caVol.ConfigMap.Name != caTestName {
		t.Fatalf("agent-executor CA volume missing or not ConfigMap-backed: %+v", caVol)
	}
}

// TestWorkflowRunReconciler_Reconcile_AgentStep_UnownedCAConfigMap_FailsRun: with an unowned
// ConfigMap in the way, the run is failed terminally with the message and no Job is created.
func TestWorkflowRunReconciler_Reconcile_AgentStep_UnownedCAConfigMap_FailsRun(t *testing.T) {
	ctx := context.Background()
	agent, wf, wr := agentRunFixture()
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(agent, wf, wr, caInstallSecret("crt-v1"), caTenantConfigMap("someone-elses-ca", false)).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: caReconcileConfig()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err == nil {
		t.Fatal("expected Reconcile to return the error")
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %q, want Failed", updated.Status.Phase)
	}
	if !strings.HasPrefix(updated.Status.Message, "Failed to publish the agent-executor CA: ") || !strings.Contains(updated.Status.Message, "not managed by OttoFlow") {
		t.Errorf("Message = %q", updated.Status.Message)
	}
	if updated.Status.FailureReason != "" {
		t.Errorf("FailureReason = %q, want none", updated.Status.FailureReason)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: caTestRunNS, Name: workflowRunnerJobName(wr.Name)}, job); !apierrors.IsNotFound(err) {
		t.Errorf("no runner Job must be created, got err=%v", err)
	}
}

// TestWorkflowRunReconciler_Reconcile_AgentStep_CASecretForbidden_SecretAccessDenied: a denied
// read of the install-namespace CA Secret fails the run with failureReason SecretAccessDenied.
func TestWorkflowRunReconciler_Reconcile_AgentStep_CASecretForbidden_SecretAccessDenied(t *testing.T) {
	ctx := context.Background()
	agent, wf, wr := agentRunFixture()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, caTestName, errors.New("no rule"))
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(agent, wf, wr, caInstallSecret("crt-v1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return forbidden
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: caReconcileConfig()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err == nil {
		t.Fatal("expected Reconcile to return the error")
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.FailureReason != ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied {
		t.Errorf("FailureReason = %q, want SecretAccessDenied", updated.Status.FailureReason)
	}
	if _, getErr := getTenantCA(t, c); !apierrors.IsNotFound(getErr) {
		t.Errorf("no ConfigMap must be published from a denied read, got err=%v", getErr)
	}
}

// TestWorkflowRunReconciler_Reconcile_AgentStep_TransientCAError_Requeues: a transient error
// publishing the CA is returned for requeue and leaves the run un-failed.
func TestWorkflowRunReconciler_Reconcile_AgentStep_TransientCAError_Requeues(t *testing.T) {
	ctx := context.Background()
	agent, wf, wr := agentRunFixture()
	timeout := apierrors.NewTimeoutError("etcd", 1)
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(agent, wf, wr, caInstallSecret("crt-v1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isCM := obj.(*corev1.ConfigMap); isCM {
					return timeout
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: caReconcileConfig()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err == nil {
		t.Fatal("expected Reconcile to return the transient error for requeue")
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("a transient error must not fail the run: %+v", updated.Status)
	}
}
