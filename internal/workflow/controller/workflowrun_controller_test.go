/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/workflow/executor"
	"github.com/nirmata/ottoflow/internal/workflow/token"
)

const defaultNamespace = "default"

var _ = Describe("WorkflowRun Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		workflowrun := &ottoflowv1alpha1.WorkflowRun{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind WorkflowRun")
			err := k8sClient.Get(ctx, typeNamespacedName, workflowrun)
			if err != nil && errors.IsNotFound(err) {
				// Create a Workflow first
				workflow := &ottoflowv1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-workflow",
						Namespace: "default",
					},
					Spec: ottoflowv1alpha1.WorkflowSpec{
						Steps: []ottoflowv1alpha1.Step{
							{
								Name: "testStep",
								Expressions: []ottoflowv1alpha1.Expression{
									{
										Name:       "result",
										Expression: `"test"`,
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

				resource := &ottoflowv1alpha1.WorkflowRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: ottoflowv1alpha1.WorkflowRunSpec{
						WorkflowRef: ottoflowv1alpha1.WorkflowRef{
							Name:      "test-workflow",
							Namespace: "default",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// Cleanup WorkflowRun
			resource := &ottoflowv1alpha1.WorkflowRun{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleanup the specific resource instance WorkflowRun")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}

			// Cleanup Workflow
			workflow := &ottoflowv1alpha1.Workflow{}
			err = k8sClient.Get(ctx, client.ObjectKey{Name: "test-workflow", Namespace: "default"}, workflow)
			if err == nil {
				Expect(k8sClient.Delete(ctx, workflow)).To(Succeed())
			}
		})
		It("should create a runner Job for the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &WorkflowRunReconciler{
				Client:              k8sClient,
				Scheme:              k8sClient.Scheme(),
				MetricsClient:       nil,
				CustomMetricsClient: nil,
				PrometheusClient:    nil,
				RunnerConfig:        RunnerConfig{RunnerClusterRole: "ottoflow-runner-role"},
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying WorkflowRun status and step propagation")
			updated := &ottoflowv1alpha1.WorkflowRun{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhasePending))
			Expect(updated.Status.Execution).NotTo(BeNil())
			Expect(updated.Status.Execution.JobName).To(Equal("test-resource-runner"))
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-resource-runner", Namespace: "default"}, job)).To(Succeed())
		})
	})

	Context("Run policy (retention and maxAllowed)", func() {
		ctx := context.Background()
		ns := defaultNamespace

		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))

		It("should delete completed runs when retentionMinutes is set", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "retention-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}},
					},
					Run: &ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](1)},
				},
			}
			oldCompletion := metav1.NewTime(time.Now().Add(-2 * time.Hour))
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-old", Namespace: ns},
				Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "retention-wf", Namespace: ns}},
				Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &oldCompletion},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(wr).WithObjects(workflow, wr).Build()
			reconciler := &WorkflowRunReconciler{Client: fakeClient, Scheme: scheme}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			err = fakeClient.Get(ctx, types.NamespacedName{Name: wr.Name, Namespace: ns}, &ottoflowv1alpha1.WorkflowRun{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "old run should be deleted by retention")
		})

		It("should delete oldest completed runs when maxAllowed is exceeded", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "max-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}},
					},
					Run: &ottoflowv1alpha1.RunPolicy{MaxAllowed: ptr.To[int32](2)},
				},
			}
			baseTime := time.Now()
			objs := []client.Object{workflow}
			for i := 0; i < 3; i++ {
				t := metav1.NewTime(baseTime.Add(time.Duration(i) * time.Minute))
				name := "run-max-" + string(rune('a'+i))
				wr := &ottoflowv1alpha1.WorkflowRun{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
					Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "max-wf", Namespace: ns}},
					Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &t},
				}
				objs = append(objs, wr)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(objs[1], objs[2], objs[3]).WithObjects(objs...).Build()
			reconciler := &WorkflowRunReconciler{Client: fakeClient, Scheme: scheme}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "run-max-a", Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			var list ottoflowv1alpha1.WorkflowRunList
			Expect(fakeClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
			completed := 0
			for _, r := range list.Items {
				if r.Spec.WorkflowRef.Name == "max-wf" && (r.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded || r.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed) {
					completed++
				}
			}
			Expect(completed).To(Equal(2), "maxAllowed=2 should leave exactly 2 completed runs")
		})
	})

	Context("Runner Job execution", func() {
		ctx := context.Background()
		ns := defaultNamespace

		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))

		It("should create a runner Job by default", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "restart-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "restartStep", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"done"`}}},
					},
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "restart-run", Namespace: ns},
				Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "restart-wf", Namespace: ns}},
				Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(wr).WithObjects(workflow, wr).Build()
			reconciler := &WorkflowRunReconciler{Client: fakeClient, Scheme: scheme, RunnerConfig: RunnerConfig{RunnerClusterRole: "ottoflow-runner-role"}}

			By("Reconciling a run")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: wr.Name, Namespace: ns}, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhasePending))
			Expect(wr.Status.Execution).NotTo(BeNil())
			Expect(wr.Status.Execution.JobName).To(Equal("restart-run-runner"))

			job := &batchv1.Job{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "restart-run-runner", Namespace: ns}, job)).To(Succeed())
		})

		It("should mark WorkflowRun failed when the runner Job fails", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "restart-fail-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "restartStep", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"done"`}}},
					},
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "restart-fail-run", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "restart-fail-wf", Namespace: ns},
				},
				Status: ottoflowv1alpha1.WorkflowRunStatus{
					Phase: ottoflowv1alpha1.WorkflowRunPhasePending,
					Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{
						JobName: "restart-fail-run-runner",
					},
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "restart-fail-run-runner", Namespace: ns},
				Status:     batchv1.JobStatus{Failed: 1},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(wr).WithObjects(workflow, wr, job).Build()
			reconciler := &WorkflowRunReconciler{Client: fakeClient, Scheme: scheme}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: wr.Name, Namespace: ns}, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseFailed))
			Expect(wr.Status.Execution).NotTo(BeNil())
			Expect(wr.Status.Execution.JobName).To(Equal("restart-fail-run-runner"))
			Expect(wr.Status.Execution.Phase).To(Equal(string(ottoflowv1alpha1.WorkflowRunPhaseFailed)))
		})

		deadlineRun := func(name string, checkpointing bool, condType batchv1.JobConditionType, extra ...client.Object) client.Client {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"done"`}}},
					},
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: workflow.Name, Namespace: ns},
				},
				Status: ottoflowv1alpha1.WorkflowRunStatus{
					Phase:     ottoflowv1alpha1.WorkflowRunPhasePending,
					Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: workflowRunnerJobName(name)},
				},
			}
			if checkpointing {
				wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
				}
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: workflowRunnerJobName(name), Namespace: ns},
				Status: batchv1.JobStatus{
					Failed: 1,
					Conditions: []batchv1.JobCondition{
						{Type: condType, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded},
					},
				},
			}
			objs := append([]client.Object{workflow, wr, job}, extra...)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(wr).WithObjects(objs...).Build()
			reconciler := &WorkflowRunReconciler{Client: fakeClient, Scheme: scheme}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseFailed))
			Expect(wr.Status.Message).To(ContainSubstring("DeadlineExceeded"))
			Expect(wr.Status.Execution).NotTo(BeNil())
			Expect(wr.Status.Execution.LastTerminationReason).To(Equal("DeadlineExceeded"))
			return fakeClient
		}

		It("should record DeadlineExceeded when the Job has a Failed condition with that reason", func() {
			deadlineRun("deadline-failed-run", false, batchv1.JobFailed)
		})

		It("should record DeadlineExceeded when only the FailureTarget condition is set", func() {
			deadlineRun("deadline-target-run", false, batchv1.JobFailureTarget)
		})

		It("should not retry a deadline-killed Job with checkpointing on, and should delete the checkpoint", func() {
			name := "deadline-cp-run"
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-cp-" + strings.ToLower(name), Namespace: ns}}
			fakeClient := deadlineRun(name, true, batchv1.JobFailed, cm)
			err := fakeClient.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "checkpoint ConfigMap should be deleted")
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: workflowRunnerJobName(name), Namespace: ns}, &batchv1.Job{})).To(Succeed(), "Job should not be deleted")
		})

		It("should still retry a checkpointed run whose Job failed with BackoffLimitExceeded", func() {
			name := "backoff-cp-run"
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"done"`}}},
					},
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: workflow.Name, Namespace: ns},
					Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
						Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
					},
				},
				Status: ottoflowv1alpha1.WorkflowRunStatus{
					Phase:     ottoflowv1alpha1.WorkflowRunPhasePending,
					Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: workflowRunnerJobName(name)},
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: workflowRunnerJobName(name), Namespace: ns},
				Status: batchv1.JobStatus{
					Failed: 1,
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded},
					},
				},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(wr).WithObjects(workflow, wr, job).Build()
			reconciler := &WorkflowRunReconciler{Client: fakeClient, Scheme: scheme}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhasePending))
			Expect(wr.Status.Execution.Attempts).To(Equal(int32(1)))
			err = fakeClient.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "Job should be deleted for the retry")
		})
	})

	Context("waitForCallback resume", func() {
		// This Context uses the real envtest API server (the shared package-level k8sClient),
		// not the fake client, because the regression being guarded against only reproduces
		// against real API-server Job status/finalizer semantics (see forceDeleteJob below).
		ctx := context.Background()
		ns := defaultNamespace

		// checkpointCMNameForTest mirrors executor.checkpointConfigMapName (unexported) for
		// the short, hyphen-only run names used in these tests, where no truncation or hash
		// suffix is ever applied.
		checkpointCMNameForTest := func(runName string) string {
			return "ottoflow-cp-" + strings.ToLower(runName)
		}

		// forceDeleteJob works around envtest's lack of a garbage-collector controller. A
		// foreground-propagation Delete sets a deletionTimestamp plus a "foregroundDeletion"
		// finalizer that only a running GC controller would normally remove once dependent
		// Pods are gone. Stripping the finalizer here simulates that cleanup completing so
		// the object is actually removed and the run can be recreated under the same name.
		forceDeleteJob := func(key types.NamespacedName) {
			job := &batchv1.Job{}
			if err := k8sClient.Get(ctx, key, job); err != nil {
				Expect(errors.IsNotFound(err)).To(BeTrue())
				return
			}
			if len(job.Finalizers) > 0 {
				job.Finalizers = nil
				Expect(k8sClient.Update(ctx, job)).To(Succeed())
			}
			if err := k8sClient.Get(ctx, key, job); err == nil {
				if dErr := k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); dErr != nil && !errors.IsNotFound(dErr) {
					GinkgoWriter.Printf("forceDeleteJob: delete error for %s: %v\n", key, dErr)
				}
			}
			Eventually(func() bool {
				getErr := k8sClient.Get(ctx, key, &batchv1.Job{})
				if getErr != nil && !errors.IsNotFound(getErr) {
					GinkgoWriter.Printf("forceDeleteJob: get error for %s: %v\n", key, getErr)
				}
				return errors.IsNotFound(getErr)
			}, "5s", "100ms").Should(BeTrue())
		}

		buildWorkflowAndRun := func(wfName, runName string) (*ottoflowv1alpha1.Workflow, *ottoflowv1alpha1.WorkflowRun) {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: wfName, Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{
							Name:    "pre",
							Outputs: []ottoflowv1alpha1.Output{{Name: "preResult", Expression: `"pre-done"`}},
						},
						{
							Name:      "gate",
							DependsOn: []string{"pre"},
							WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
								Timeout: "1h",
								OutputSchema: &apiextensionsv1.JSON{
									Raw: []byte(`{"type":"object","required":["approved"],"properties":{"approved":{"type":"boolean"}}}`),
								},
							},
						},
					},
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: runName, Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wfName, Namespace: ns},
					Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
						Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
					},
				},
			}
			return workflow, wr
		}

		// writeCheckpoint persists a checkpoint ConfigMap in exactly the shape checkpoint.go
		// expects, recording "pre" as already succeeded so a resumed runner would restore it
		// instead of re-running it. WorkflowRunUID must match the created run's UID or Load
		// treats it as a stale checkpoint and returns nil.
		writeCheckpoint := func(runName string, runUID types.UID) {
			snapshot := executor.CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    string(runUID),
				LastCompletedStep: "pre",
				StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
					"pre": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
				},
				Context: map[string]interface{}{
					"inputs":    map[string]interface{}{},
					"variables": map[string]interface{}{"preResult": "pre-done"},
					"steps":     map[string]interface{}{},
				},
			}
			raw, err := json.Marshal(snapshot)
			Expect(err).NotTo(HaveOccurred())
			cmObj := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: checkpointCMNameForTest(runName), Namespace: ns},
				Data:       map[string]string{"checkpoint": string(raw)},
			}
			Expect(k8sClient.Create(ctx, cmObj)).To(Succeed())
		}

		// markJobComplete simulates the paused runner Job having exited 0 (waitForCallback
		// steps exit clean and wait for the controller to recreate the Job on resume).
		markJobComplete := func(jobKey types.NamespacedName) {
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
			}
			job.Status.Succeeded = 1
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		}

		cleanupAll := func(wfName, runName string) {
			jobKey := types.NamespacedName{Name: workflowRunnerJobName(runName), Namespace: ns}
			if err := k8sClient.Get(ctx, jobKey, &batchv1.Job{}); err == nil {
				forceDeleteJob(jobKey)
			}
			_ = k8sClient.Delete(ctx, &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: runName, Namespace: ns}})
			_ = k8sClient.Delete(ctx, &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: wfName, Namespace: ns}})
			_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: checkpointCMNameForTest(runName), Namespace: ns}})
		}

		It("recreates the runner Job and preserves PendingCallback when outputs are delivered by a direct status update", func() {
			wfName, runName := "cb-resume-wf-a", "cb-resume-run-a"
			defer cleanupAll(wfName, runName)

			workflow, wr := buildWorkflowAndRun(wfName, runName)
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
			Expect(k8sClient.Create(ctx, wr)).To(Succeed())

			wrKey := types.NamespacedName{Name: runName, Namespace: ns}
			jobKey := types.NamespacedName{Name: workflowRunnerJobName(runName), Namespace: ns}
			reconciler := &WorkflowRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerConfig: RunnerConfig{RunnerClusterRole: "ottoflow-runner-role"}}

			By("initial reconcile creates the runner Job")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhasePending))
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())

			By("simulating the pause: pending callback set, checkpoint written, old Job completed")
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
				TokenHash: token.HashToken(validTestToken()),
				StepName:  "gate",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
				CreatedAt: time.Now().Unix(),
			}
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())
			writeCheckpoint(runName, wr.UID)
			markJobComplete(jobKey)

			By("delivering the callback outputs via a direct status update")
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			wr.Status.PendingCallback.Outputs = apiextensionsv1.JSON{Raw: []byte(`{"approved":true}`)}
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())

			By("reconcile deletes the old (completed) runner Job and requeues")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			job := &batchv1.Job{}
			getErr := k8sClient.Get(ctx, jobKey, job)
			if getErr == nil {
				Expect(job.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(errors.IsNotFound(getErr)).To(BeTrue())
			}

			By("simulating garbage collection of the deleted Job (envtest has no GC controller)")
			forceDeleteJob(jobKey)

			By("reconcile recreates the runner Job and preserves PendingCallback for the executor to consume")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())

			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseRunning))
			Expect(wr.Status.PendingCallback).NotTo(BeNil())
			Expect(wr.Status.PendingCallback.Outputs.Raw).NotTo(BeEmpty())

			By("further reconciles converge: Job stays present, Phase stays Running, no repeated deletes")
			for i := 0; i < 3; i++ {
				_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())
				Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
				Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseRunning))
			}
		})

		It("recreates the runner Job when outputs are delivered via the HTTP callback server", func() {
			wfName, runName := "cb-resume-wf-b", "cb-resume-run-b"
			defer cleanupAll(wfName, runName)

			workflow, wr := buildWorkflowAndRun(wfName, runName)
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
			Expect(k8sClient.Create(ctx, wr)).To(Succeed())

			wrKey := types.NamespacedName{Name: runName, Namespace: ns}
			jobKey := types.NamespacedName{Name: workflowRunnerJobName(runName), Namespace: ns}
			reconciler := &WorkflowRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerConfig: RunnerConfig{RunnerClusterRole: "ottoflow-runner-role"}}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())

			tok := validTestToken()
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
				TokenHash: token.HashToken(tok),
				StepName:  "gate",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
				CreatedAt: time.Now().Unix(),
			}
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())
			writeCheckpoint(runName, wr.UID)
			markJobComplete(jobKey)

			By("delivering the callback outputs via the HTTP callback server")
			cs := NewCallbackServer(k8sClient, nil, ":0")
			path := fmt.Sprintf("/api/v1/workflow-runs/%s/%s/callback/%s", ns, runName, tok)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"approved":true}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			cs.mux.ServeHTTP(w, req)
			Expect(w.Code).To(Equal(http.StatusOK))

			By("reconcile deletes the old (completed) runner Job and requeues")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			forceDeleteJob(jobKey)

			By("reconcile recreates the runner Job and preserves PendingCallback for the executor to consume")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())

			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseRunning))
			Expect(wr.Status.PendingCallback).NotTo(BeNil())
			Expect(wr.Status.PendingCallback.Outputs.Raw).NotTo(BeEmpty())
		})

		It("terminally fails a resume run without leaving PendingCallback set when the recreated Job fails", func() {
			wfName, runName := "cb-resume-wf-c", "cb-resume-run-c"
			defer cleanupAll(wfName, runName)

			workflow, wr := buildWorkflowAndRun(wfName, runName)
			zeroRetries := int32(0)
			wr.Spec.Execution.Checkpointing.MaxRestartAttempts = &zeroRetries
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
			Expect(k8sClient.Create(ctx, wr)).To(Succeed())

			wrKey := types.NamespacedName{Name: runName, Namespace: ns}
			jobKey := types.NamespacedName{Name: workflowRunnerJobName(runName), Namespace: ns}
			reconciler := &WorkflowRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerConfig: RunnerConfig{RunnerClusterRole: "ottoflow-runner-role"}}

			By("initial reconcile creates the runner Job")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())

			By("simulating the pause: pending callback set, checkpoint written, old Job completed")
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
				TokenHash: token.HashToken(validTestToken()),
				StepName:  "gate",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
				CreatedAt: time.Now().Unix(),
			}
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())
			writeCheckpoint(runName, wr.UID)
			markJobComplete(jobKey)

			By("delivering the callback outputs via a direct status update")
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			wr.Status.PendingCallback.Outputs = apiextensionsv1.JSON{Raw: []byte(`{"approved":true}`)}
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())

			By("reconcile deletes the old (completed) runner Job and requeues")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			forceDeleteJob(jobKey)

			By("reconcile recreates the runner Job and preserves PendingCallback for the executor to consume")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseRunning))
			Expect(wr.Status.PendingCallback).NotTo(BeNil())

			By("the recreated resume Job terminally fails (backoffLimit exhausted)")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			By("reconcile routes the failed resume Job through the terminal-failure path")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseFailed))
			Expect(wr.Status.PendingCallback).To(BeNil())
		})

		It("clears PendingCallback when the referenced Workflow cannot be resolved", func() {
			wfName, runName := "cb-resolve-fail-wf", "cb-resolve-fail-run"
			defer cleanupAll(wfName, runName)

			// Deliberately do NOT create the Workflow so getReferencedWorkflow fails. This
			// exercises the pre-gate failure path in reconcileJobExecution: a run carrying a live
			// pending callback whose Workflow has been deleted must be marked Failed WITHOUT
			// leaving a stale PendingCallback, or its callback endpoint would keep accepting POSTs
			// for a dead run (the callback server never checks Phase).
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: runName, Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wfName, Namespace: ns},
				},
			}
			Expect(k8sClient.Create(ctx, wr)).To(Succeed())

			wrKey := types.NamespacedName{Name: runName, Namespace: ns}
			By("setting a live pending callback on the run")
			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
				TokenHash: token.HashToken(validTestToken()),
				StepName:  "gate",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
				CreatedAt: time.Now().Unix(),
			}
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())

			reconciler := &WorkflowRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerConfig: RunnerConfig{RunnerClusterRole: "ottoflow-runner-role"}}

			By("reconcile fails to resolve the Workflow and marks the run Failed")
			// A NotFound Workflow reference is a terminal condition, not a transient one: the
			// reconciler marks the run Failed and returns a nil error so controller-runtime does
			// not keep re-queuing a run that can never succeed.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wrKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, wrKey, wr)).To(Succeed())
			Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseFailed))
			Expect(wr.Status.PendingCallback).To(BeNil())
		})
	})

	Context("SetupWithManager", func() {
		It("should register WorkflowRunReconciler with manager", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := &WorkflowRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
	})

	Context("Runner RBAC (least-privilege)", func() {
		ctx := context.Background()
		ns := defaultNamespace

		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))

		It("binds a default runner Job to a per-workflow ServiceAccount and the narrowed runner ClusterRole", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "rbac-wf", Namespace: ns},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}},
					},
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "rbac-run", Namespace: ns},
				Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "rbac-wf", Namespace: ns}},
				Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(wr).WithObjects(workflow, wr).Build()
			// RunnerServiceAccount and RunnerClusterRole reflect the chart's new defaults:
			// no controller-SA fallback, and runner Jobs bind to the narrowed runner role.
			reconciler := &WorkflowRunReconciler{
				Client: fakeClient,
				Scheme: scheme,
				RunnerConfig: RunnerConfig{
					RunnerServiceAccount: "",
					RunnerClusterRole:    "ottoflow-runner-role",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "rbac-run-runner", Namespace: ns}, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("rbac-wf-runner"))
			Expect(job.Spec.Template.Spec.ServiceAccountName).NotTo(Equal("controller-manager"))

			crbName := workflowRunnerRoleBindingName(ns, job.Spec.Template.Spec.ServiceAccountName)
			crb := &rbacv1.ClusterRoleBinding{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: crbName}, crb)).To(Succeed())
			Expect(crb.RoleRef.Name).To(Equal("ottoflow-runner-role"))
			Expect(crb.RoleRef.Name).NotTo(Equal("ottoflow-role"))
		})

		It("migrates an existing runner ClusterRoleBinding to the narrowed role via delete-and-recreate", func() {
			saName := "rbac-migrate-sa"

			sa := &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
			}
			Expect(k8sClient.Create(ctx, sa)).To(Succeed())

			crbName := workflowRunnerRoleBindingName(ns, saName)
			crb := &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: crbName, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-role"},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: ns}},
			}
			Expect(k8sClient.Create(ctx, crb)).To(Succeed())

			defer func() {
				_ = k8sClient.Delete(ctx, crb)
				_ = k8sClient.Delete(ctx, sa)
			}()

			reconciler := &WorkflowRunReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				RunnerConfig: RunnerConfig{
					RunnerClusterRole: "ottoflow-runner-role",
				},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}

			// This runs against the envtest admin k8sClient, which unconditionally holds every
			// RBAC verb, so it validates the delete/recreate migration LOGIC only — not that the
			// controller's own ServiceAccount actually has the "delete" and "bind" grants added
			// to -role:core. Those are covered by the helm-template rule assertions and the e2e
			// suite, which run under the real, narrower controller-manager identity.
			Expect(reconciler.ensureRunnerAccess(ctx, wr, nil, saName, false)).To(Succeed())

			reloaded := &rbacv1.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crbName}, reloaded)).To(Succeed())
			Expect(reloaded.RoleRef.Name).To(Equal("ottoflow-runner-role"))
		})
	})
})
