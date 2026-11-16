package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestEnsureRunnerSecrets_TLSCopyFence(t *testing.T) {
	const runNS, ctrlNS, aeNS, otherNS = "tenant-a", "ctrl-ns", "ae-ns", "team-ns"
	tests := []struct {
		name       string
		srcNS      string
		secretType corev1.SecretType
		wantCopied bool
		aeFlag     string
	}{
		{"TLS in controller namespace refused", ctrlNS, corev1.SecretTypeTLS, false, aeNS},
		{"TLS in agent-executor namespace refused", aeNS, corev1.SecretTypeTLS, false, aeNS},
		{"TLS in other namespace copied", otherNS, corev1.SecretTypeTLS, true, aeNS},
		{"Opaque in controller namespace copied", ctrlNS, corev1.SecretTypeOpaque, true, aeNS},
		{"Opaque in agent-executor namespace copied", aeNS, corev1.SecretTypeOpaque, true, aeNS},
		{"TLS in default agent-executor namespace refused when unset", defaultAgentExecutorNamespace, corev1.SecretTypeTLS, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: runNS, UID: "run-uid"}}
			job := &batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "my-cert"}}}},
			}}}}
			src := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-cert", Namespace: tc.srcNS}, Type: tc.secretType, Data: map[string][]byte{"k": []byte("v")}}
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wr, src).Build()
			r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, ControllerNamespace: ctrlNS, RunnerConfig: RunnerConfig{AgentExecutorNamespace: tc.aeFlag}}

			err := r.ensureRunnerSecrets(context.Background(), wr, job, tc.srcNS)
			getErr := c.Get(context.Background(), client.ObjectKey{Namespace: runNS, Name: "my-cert"}, &corev1.Secret{})
			if tc.wantCopied {
				if err != nil || getErr != nil {
					t.Fatalf("want copied; ensure err: %v, get err: %v", err, getErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `"my-cert"`) || !strings.Contains(err.Error(), runNS) {
				t.Fatalf("want refusal naming the Secret and run namespace, got: %v", err)
			}
			if getErr == nil {
				t.Fatal("refused Secret was copied")
			}
		})
	}
}
