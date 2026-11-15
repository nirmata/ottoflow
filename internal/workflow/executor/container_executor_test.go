/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

const testKyvernoImage = "ghcr.io/kyverno/kyverno-cli"

func TestImageAllowed(t *testing.T) {
	allowed := []string{testKyvernoImage, "registry.example.com/tools/"}
	cases := map[string]bool{
		testKyvernoImage:                        true,
		testKyvernoImage + ":v1.15.0":           true,
		testKyvernoImage + "@sha256:abc":        true,
		testKyvernoImage + "-evil:v1":           false,
		testKyvernoImage + "/sub:v1":            false,
		"ghcr.io/kyverno/kyverno:v1":            false,
		"registry.example.com/tools/scanner:1":  true,
		"registry.example.com/tools/":           false,
		"registry.example.com/toolsx/scanner:1": false,
		"":                                      false,
		testKyvernoImage + ":v1 --privileged":   false,
		"docker.io/" + strings.TrimPrefix(testKyvernoImage, "ghcr.io/"): false,
	}
	for image, want := range cases {
		if got := ImageAllowed(image, allowed); got != want {
			t.Errorf("ImageAllowed(%q) = %v, want %v", image, got, want)
		}
	}
	if ImageAllowed(testKyvernoImage, nil) {
		t.Error("empty allowlist must allow nothing")
	}
}

func TestParseAllowedImagesAndEnv(t *testing.T) {
	if got := ParseAllowedImages(" a , ,b,"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ParseAllowedImages = %v", got)
	}
	t.Setenv(ContainerStepAllowedImagesEnv, "a,b")
	t.Setenv(ContainerStepMaxOutputBytesEnv, "42")
	cfg, err := ContainerStepConfigFromEnv()
	if err != nil || len(cfg.AllowedImages) != 2 || cfg.MaxOutputBytes != 42 {
		t.Fatalf("ContainerStepConfigFromEnv = %+v, %v", cfg, err)
	}
	t.Setenv(ContainerStepMaxOutputBytesEnv, "-1")
	if _, err := ContainerStepConfigFromEnv(); err == nil {
		t.Fatal("negative max output bytes must be rejected")
	}
}

func TestContainerTemplateExpressions(t *testing.T) {
	c := &ottoflowv1alpha1.StepContainer{
		Image: "{{ inputs.image }}",
		Args:  []string{"apply", "--cluster={{inputs.cluster}}", "{{ steps.prepare.dir }}/x"},
		Env:   []ottoflowv1alpha1.StepContainerEnvVar{{Name: "A", Value: "{{ variables.a }}"}},
		Files: &ottoflowv1alpha1.StepContainerFiles{ConfigMapRef: "{{ inputs.cm }}", MountPath: "/in"},
	}
	got := strings.Join(ContainerTemplateExpressions(c), "|")
	want := "inputs.image|inputs.cluster|steps.prepare.dir|variables.a|inputs.cm"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func testRun() *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID("run-uid")},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "what-if"},
			InputValues: map[string]string{"image": testKyvernoImage + ":v1.15.0", "cm": "run-1-inputs"},
			ClusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target-kubeconfig"},
			},
		},
	}
}

func TestBuildContainerStepJobHardening(t *testing.T) {
	r := &resolvedContainer{
		Image: testKyvernoImage + ":v1", Args: []string{"apply"}, ConfigMap: "inputs", MountPath: "/in",
		Kubeconfig: true, Timeout: 90 * time.Second, Env: []corev1.EnvVar{{Name: "X", Value: "y"}},
	}
	job := buildContainerStepJob(testRun(), "evaluateStep", r, "target-kubeconfig", "value")

	if job.Namespace != "team-a" || !strings.HasPrefix(job.GenerateName, "run-1-evaluatestep-") {
		t.Fatalf("unexpected name/namespace %q %q", job.Namespace, job.GenerateName)
	}
	for k, v := range map[string]string{partOfLabel: "ottoflow", workflowRunLabel: "run-1", ottoflowv1alpha1.WorkflowLabelKey: "what-if", containerStepLabel: "evaluateStep"} {
		if job.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, job.Labels[k], v)
		}
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != "run-uid" || job.OwnerReferences[0].Kind != "WorkflowRun" ||
		job.OwnerReferences[0].Controller != nil {
		t.Errorf("owner references = %+v", job.OwnerReferences)
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 90 || job.Spec.TTLSecondsAfterFinished == nil {
		t.Errorf("job spec limits = %+v", job.Spec)
	}
	ps := job.Spec.Template.Spec
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken || ps.HostNetwork || ps.HostPID || ps.HostIPC ||
		ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod spec not hardened: %+v", ps)
	}
	if !*ps.SecurityContext.RunAsNonRoot || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", ps.SecurityContext)
	}
	c := ps.Containers[0]
	sc := c.SecurityContext
	if *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || *sc.Privileged ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Errorf("container security context = %+v", sc)
	}
	if c.Image != r.Image || c.Args[0] != "apply" || len(c.Command) != 0 {
		t.Errorf("container = %+v", c)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}
	if m := mounts["kubeconfig"]; m.MountPath != "/kube" || !m.ReadOnly {
		t.Errorf("kubeconfig mount = %+v", m)
	}
	if m := mounts["files"]; m.MountPath != "/in" || !m.ReadOnly {
		t.Errorf("files mount = %+v", m)
	}
	for _, v := range ps.Volumes {
		if v.Name == "kubeconfig" {
			if v.Secret.SecretName != "target-kubeconfig" || v.Secret.Items[0].Key != "value" || v.Secret.Items[0].Path != "config" {
				t.Errorf("kubeconfig volume = %+v", v.Secret)
			}
		}
		if v.HostPath != nil {
			t.Errorf("hostPath volume %s", v.Name)
		}
	}
}

func TestContainerKubeconfigSecret(t *testing.T) {
	run := testRun()
	if _, _, err := containerKubeconfigSecret(run, ""); err == nil {
		t.Error("no key and nothing mounted must fail")
	}
	if name, key, err := containerKubeconfigSecret(run, "config"); err != nil || name != "target-kubeconfig" || key != "config" {
		t.Errorf("mounted key: %s %s %v", name, key, err)
	}
	run.Spec.ClusterRef.KubeConfigSecretRef.Key = "value"
	if _, key, _ := containerKubeconfigSecret(run, "config"); key != "value" {
		t.Errorf("explicit key must win, got %s", key)
	}
	run.Spec.ClusterRef.KubeConfigSecretRef.Namespace = "other"
	if _, _, err := containerKubeconfigSecret(run, ""); err == nil {
		t.Error("a Secret in another namespace must be rejected")
	}
	run.Spec.ClusterRef = nil
	if _, _, err := containerKubeconfigSecret(run, ""); err == nil {
		t.Error("kubeconfig without clusterRef must fail")
	}
}

func TestReadCapped(t *testing.T) {
	if s, err := readCapped(strings.NewReader("abcd"), 4); err != nil || s != "abcd" {
		t.Errorf("at cap: %q %v", s, err)
	}
	if _, err := readCapped(strings.NewReader("abcde"), 4); err == nil {
		t.Error("over cap must fail, not truncate")
	}
}

// fakeJobCluster makes a fake clientset behave enough like the Job controller: it names created Jobs,
// and finish() marks the Job done and creates its terminated pod.
type fakeJobCluster struct {
	cs      *k8sfake.Clientset
	created atomic.Pointer[batchv1.Job]
	deleted atomic.Int32
}

func newFakeJobCluster(t *testing.T, onCreate func(f *fakeJobCluster, job *batchv1.Job)) *fakeJobCluster {
	t.Helper()
	f := &fakeJobCluster{cs: k8sfake.NewClientset()}
	f.cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		job.Name = job.GenerateName + "abcde"
		job.UID = types.UID("job-uid")
		f.created.Store(job.DeepCopy())
		if onCreate != nil {
			go onCreate(f, job.DeepCopy())
		}
		return false, nil, nil
	})
	f.cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		f.deleted.Add(1)
		return false, nil, nil
	})
	return f
}

func finishJob(exitCode int32, condition batchv1.JobConditionType, reason string) func(*fakeJobCluster, *batchv1.Job) {
	return func(f *fakeJobCluster, job *batchv1.Job) {
		ctx := context.Background()
		time.Sleep(50 * time.Millisecond)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace,
				Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  containerStepContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: "boom"}},
			}}},
		}
		_, _ = f.cs.CoreV1().Pods(job.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		cur, err := f.cs.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return
		}
		cur.Status.Conditions = append(cur.Status.Conditions, batchv1.JobCondition{Type: condition, Status: corev1.ConditionTrue, Reason: reason})
		_, _ = f.cs.BatchV1().Jobs(job.Namespace).UpdateStatus(ctx, cur, metav1.UpdateOptions{})
	}
}

func newContainerTestExecutor(t *testing.T, run *ottoflowv1alpha1.WorkflowRun, cfg ContainerStepConfig) *WorkflowExecutor {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	e, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.SetContainerStepConfig(cfg)
	return e
}

func containerWorkflow(step ottoflowv1alpha1.Step) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "what-if", Namespace: "team-a"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Inputs: []ottoflowv1alpha1.Input{{Name: "image"}, {Name: "cm"}},
			Steps:  []ottoflowv1alpha1.Step{step},
		},
	}
}

func evaluateStep() ottoflowv1alpha1.Step {
	return ottoflowv1alpha1.Step{
		Name: "evaluate",
		Container: &ottoflowv1alpha1.StepContainer{
			Image:      "{{ inputs.image }}",
			Args:       []string{"apply", "--values={{ inputs.cm }}"},
			Files:      &ottoflowv1alpha1.StepContainerFiles{ConfigMapRef: "{{ inputs.cm }}", MountPath: "/in"},
			Kubeconfig: true,
			Timeout:    "5s",
		},
		Outputs: []ottoflowv1alpha1.Output{
			{Name: "report", Expression: "step.stdout"},
			{Name: "code", Expression: "step.exitCode"},
		},
	}
}

func TestExecuteContainerStepSucceeds(t *testing.T) {
	run := testRun()
	f := newFakeJobCluster(t, finishJob(0, batchv1.JobComplete, ""))
	e := newContainerTestExecutor(t, run, ContainerStepConfig{AllowedImages: []string{testKyvernoImage}, Clientset: f.cs, KubeconfigKey: "config"})

	if err := e.ExecuteWorkflow(context.Background(), containerWorkflow(evaluateStep()), run); err != nil {
		t.Fatalf("ExecuteWorkflow: %v (%s)", err, run.Status.Message)
	}
	if run.Status.StepStatuses["evaluate"].Phase != ottoflowv1alpha1.StepPhaseSucceeded {
		t.Fatalf("step status = %+v", run.Status.StepStatuses["evaluate"])
	}
	data, _ := e.GetContextManager().ReadContext(context.Background())
	vars := data["variables"].(map[string]interface{})
	if vars["report"] != "fake logs" || vars["code"] != int64(0) {
		t.Errorf("outputs = %v", vars)
	}
	job := f.created.Load()
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != testKyvernoImage+":v1.15.0" || c.Args[1] != "--values=run-1-inputs" {
		t.Errorf("templates not rendered: %s %v", c.Image, c.Args)
	}
	if f.deleted.Load() != 1 {
		t.Errorf("Job deleted %d times, want 1", f.deleted.Load())
	}
}

func TestExecuteContainerStepNonZeroExit(t *testing.T) {
	run := testRun()
	f := newFakeJobCluster(t, finishJob(1, batchv1.JobFailed, "BackoffLimitExceeded"))
	e := newContainerTestExecutor(t, run, ContainerStepConfig{AllowedImages: []string{testKyvernoImage}, Clientset: f.cs, KubeconfigKey: "config"})

	if err := e.ExecuteWorkflow(context.Background(), containerWorkflow(evaluateStep()), run); err == nil {
		t.Fatal("exit code 1 must fail the step by default")
	}
	if st := run.Status.StepStatuses["evaluate"]; st.Phase != ottoflowv1alpha1.StepPhaseFailed || !strings.Contains(st.Message, "exited with code 1") {
		t.Errorf("step status = %+v", st)
	}
	if f.deleted.Load() != 1 {
		t.Errorf("Job deleted %d times, want 1", f.deleted.Load())
	}

	run = testRun()
	f = newFakeJobCluster(t, finishJob(1, batchv1.JobFailed, "BackoffLimitExceeded"))
	e = newContainerTestExecutor(t, run, ContainerStepConfig{AllowedImages: []string{testKyvernoImage}, Clientset: f.cs, KubeconfigKey: "config"})
	step := evaluateStep()
	step.Container.SuccessExitCodes = []int32{0, 1}
	if err := e.ExecuteWorkflow(context.Background(), containerWorkflow(step), run); err != nil {
		t.Fatalf("exit code 1 listed in successExitCodes must succeed: %v", err)
	}
}

func TestExecuteContainerStepRejectsImage(t *testing.T) {
	run := testRun()
	run.Spec.InputValues["image"] = "docker.io/library/busybox:latest"
	f := newFakeJobCluster(t, nil)
	e := newContainerTestExecutor(t, run, ContainerStepConfig{AllowedImages: []string{testKyvernoImage}, Clientset: f.cs, KubeconfigKey: "config"})
	_ = e.ExecuteWorkflow(context.Background(), containerWorkflow(evaluateStep()), run)
	if st := run.Status.StepStatuses["evaluate"]; st.Phase != ottoflowv1alpha1.StepPhaseFailed || !strings.Contains(st.Message, "allowlist") {
		t.Errorf("step status = %+v", st)
	}
	if f.created.Load() != nil {
		t.Error("no Job may be created for a rejected image")
	}
}

func TestExecuteContainerStepDisabled(t *testing.T) {
	run := testRun()
	e := newContainerTestExecutor(t, run, ContainerStepConfig{})
	_, err := e.executeContainerStep(context.Background(), run, evaluateStep())
	if !errors.Is(err, ErrContainerStepsDisabled) {
		t.Errorf("err = %v, want ErrContainerStepsDisabled", err)
	}
}

func TestRunContainerJobTimeoutDeletesJob(t *testing.T) {
	f := newFakeJobCluster(t, nil)
	r := &resolvedContainer{Image: testKyvernoImage, Timeout: time.Second}
	job := buildContainerStepJob(testRun(), "evaluate", r, "", "")

	old := containerStepWaitGrace
	containerStepWaitGrace = 0
	defer func() { containerStepWaitGrace = old }()

	_, err := runContainerJob(context.Background(), f.cs, job, r.Timeout, 0)
	if !errors.Is(err, errContainerStepTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
	if f.deleted.Load() != 1 {
		t.Errorf("Job deleted %d times, want 1", f.deleted.Load())
	}
}

func TestRunContainerJobDeadlineExceeded(t *testing.T) {
	f := newFakeJobCluster(t, finishJob(137, batchv1.JobFailed, batchv1.JobReasonDeadlineExceeded))
	r := &resolvedContainer{Image: testKyvernoImage, Timeout: 5 * time.Second}
	_, err := runContainerJob(context.Background(), f.cs, buildContainerStepJob(testRun(), "evaluate", r, "", ""), r.Timeout, 0)
	if !errors.Is(err, errContainerStepTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
	if f.deleted.Load() != 1 {
		t.Errorf("Job deleted %d times, want 1", f.deleted.Load())
	}
}

func TestStepTypeContainer(t *testing.T) {
	if got := stepType(evaluateStep()); got != "container" {
		t.Errorf("stepType = %q", got)
	}
}
