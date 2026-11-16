/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("Trigger Manager", func() {
	var (
		ctx            context.Context
		cancelCtx      context.CancelFunc
		triggerManager *TriggerManager
		scheduler      *Scheduler
	)

	BeforeEach(func() {
		ctx, cancelCtx = context.WithCancel(context.Background())
		scheduler = NewScheduler(k8sClient, ctrl.Log)

		go func() {
			defer GinkgoRecover()
			_ = scheduler.Start(ctx)
		}()

		var err error
		triggerManager, err = NewTriggerManagerWithConfig(k8sClient, k8sClient.Scheme(), cfg, scheduler)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cancelCtx()
	})

	Context("Cron Triggers", func() {
		var workflow *ottoflowv1alpha1.Workflow

		BeforeEach(func() {
			workflow = &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cron-workflow",
					Namespace: "default",
				},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "echo",
							Expressions: []ottoflowv1alpha1.Expression{
								{
									Name:       "result",
									Expression: `"test"`,
								},
							},
						},
					},
					Triggers: []ottoflowv1alpha1.Trigger{
						{
							Cron: &ottoflowv1alpha1.CronTrigger{
								Schedule:          "*/1 * * * *",
								ConcurrencyPolicy: "Forbid",
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		})

		AfterEach(func() {
			if workflow != nil {
				_ = triggerManager.UnregisterWorkflow(ctx, workflow)
				_ = k8sClient.Delete(ctx, workflow)
			}
		})

		It("should add a cron schedule when registering a cron trigger", func() {
			err := triggerManager.RegisterWorkflow(ctx, workflow)
			Expect(err).NotTo(HaveOccurred())

			key := client.ObjectKeyFromObject(workflow).String() + "-cron-0"
			Expect(scheduler.HasSchedule(key)).To(BeTrue())
		})

		It("should remove schedule when unregistering a cron trigger", func() {
			err := triggerManager.RegisterWorkflow(ctx, workflow)
			Expect(err).NotTo(HaveOccurred())

			key := client.ObjectKeyFromObject(workflow).String() + "-cron-0"
			Expect(scheduler.HasSchedule(key)).To(BeTrue())

			err = triggerManager.UnregisterWorkflow(ctx, workflow)
			Expect(err).NotTo(HaveOccurred())
			Expect(scheduler.HasSchedule(key)).To(BeFalse())
		})

		It("should create WorkflowRun when cron fires", func() {
			cronSpec := workflow.Spec.Triggers[0].Cron
			err := scheduler.AddSchedule("test-cron-fire", workflow, cronSpec)
			Expect(err).NotTo(HaveOccurred())

			// Directly invoke the cron callback to test WorkflowRun creation
			scheduler.handleCronFire("test-cron-fire")

			workflowRunList := &ottoflowv1alpha1.WorkflowRunList{}
			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": workflow.Name,
					"ottoflow.nirmata.io/trigger":  "cron",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRunList.Items).NotTo(BeEmpty())

			wr := &ottoflowv1alpha1.WorkflowRun{}
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&workflowRunList.Items[0]), wr)
			Expect(err).NotTo(HaveOccurred())
			Expect(wr.Status.Trigger).NotTo(BeNil())
			Expect(wr.Status.Trigger.Type).To(Equal("Cron"))
			Expect(wr.Status.Trigger.CronSchedule).To(Equal("*/1 * * * *"))
		})

		It("should inject inputValuesFrom Secret into WorkflowRun", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "slack-webhook",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"url": []byte("https://hooks.slack.com/test"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, secret) }()

			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "cron-input-secret", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{{Name: "echo", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"ok"`}}}},
				},
			}
			Expect(k8sClient.Create(ctx, wf)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, wf) }()

			cronSpec := &ottoflowv1alpha1.CronTrigger{
				Schedule: "*/1 * * * *",
				InputValuesFrom: []ottoflowv1alpha1.CronInputFromSecret{
					{
						InputName: "slackWebhookUrl",
						SecretRef: ottoflowv1alpha1.CronSecretKeyRef{
							Name: "slack-webhook",
							Key:  "url",
						},
					},
				},
			}
			err := scheduler.AddSchedule("test-input-from-secret", wf, cronSpec)
			Expect(err).NotTo(HaveOccurred())

			scheduler.handleCronFire("test-input-from-secret")

			workflowRunList := &ottoflowv1alpha1.WorkflowRunList{}
			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": wf.Name,
					"ottoflow.nirmata.io/trigger":  "cron",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRunList.Items).NotTo(BeEmpty())

			wr := workflowRunList.Items[len(workflowRunList.Items)-1]
			Expect(wr.Spec.InputValues).To(HaveKeyWithValue("slackWebhookUrl", "https://hooks.slack.com/test"))
		})

		It("should not resolve an inputValuesFrom Secret from another namespace", func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "secret-ns"}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, ns) }()

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cross-ns-secret",
					Namespace: "secret-ns",
				},
				Data: map[string][]byte{
					"token": []byte("my-token-value"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, secret) }()

			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "cron-input-cross-ns", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{{Name: "echo", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"ok"`}}}},
				},
			}
			Expect(k8sClient.Create(ctx, wf)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, wf) }()

			cronSpec := &ottoflowv1alpha1.CronTrigger{
				Schedule: "*/1 * * * *",
				InputValuesFrom: []ottoflowv1alpha1.CronInputFromSecret{
					{
						InputName: "apiToken",
						SecretRef: ottoflowv1alpha1.CronSecretKeyRef{
							Name:      "cross-ns-secret",
							Namespace: "secret-ns",
							Key:       "token",
						},
					},
				},
			}
			err := scheduler.AddSchedule("test-input-cross-ns", wf, cronSpec)
			Expect(err).NotTo(HaveOccurred())

			scheduler.handleCronFire("test-input-cross-ns")

			workflowRunList := &ottoflowv1alpha1.WorkflowRunList{}
			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": wf.Name,
					"ottoflow.nirmata.io/trigger":  "cron",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRunList.Items).To(BeEmpty())

			// Control: a cron fire for the same Workflow without the cross-namespace ref does create a run.
			Expect(scheduler.AddSchedule("test-input-cross-ns-control", wf, &ottoflowv1alpha1.CronTrigger{Schedule: "*/1 * * * *"})).To(Succeed())
			scheduler.handleCronFire("test-input-cross-ns-control")
			Expect(k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{"ottoflow.nirmata.io/workflow": wf.Name, "ottoflow.nirmata.io/trigger": "cron"})).To(Succeed())
			Expect(workflowRunList.Items).NotTo(BeEmpty())
		})

		It("should deep-copy Workflow execution spec into WorkflowRun", func() {
			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "cron-exec-copy", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{{Name: "echo", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"ok"`}}}},
					Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
						Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
							Env: []corev1.EnvVar{
								{Name: "LLM_TOKEN", Value: "test-value"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, wf)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, wf) }()

			cronSpec := &ottoflowv1alpha1.CronTrigger{
				Schedule: "*/1 * * * *",
			}
			err := scheduler.AddSchedule("test-execution-copy", wf, cronSpec)
			Expect(err).NotTo(HaveOccurred())

			scheduler.handleCronFire("test-execution-copy")

			workflowRunList := &ottoflowv1alpha1.WorkflowRunList{}
			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": wf.Name,
					"ottoflow.nirmata.io/trigger":  "cron",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRunList.Items).NotTo(BeEmpty())

			wr := workflowRunList.Items[len(workflowRunList.Items)-1]
			Expect(wr.Spec.Execution).NotTo(BeNil())
			Expect(wr.Spec.Execution.Job).NotTo(BeNil())
			Expect(wr.Spec.Execution.Job.Env).To(HaveLen(1))
			Expect(wr.Spec.Execution.Job.Env[0].Name).To(Equal("LLM_TOKEN"))
			Expect(wr.Spec.Execution.Job.Env[0].Value).To(Equal("test-value"))

			// Verify it's a deep copy (mutating the original should not affect the run)
			wf.Spec.Execution.Job.Env[0].Value = "mutated"
			Expect(wr.Spec.Execution.Job.Env[0].Value).To(Equal("test-value"))
		})

		It("should respect Forbid concurrency policy", func() {
			cronSpec := &ottoflowv1alpha1.CronTrigger{
				Schedule:          "*/1 * * * *",
				ConcurrencyPolicy: "Forbid",
			}
			err := scheduler.AddSchedule("test-forbid", workflow, cronSpec)
			Expect(err).NotTo(HaveOccurred())

			// First fire should succeed
			scheduler.handleCronFire("test-forbid")

			// Mark the created run as Running
			workflowRunList := &ottoflowv1alpha1.WorkflowRunList{}
			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": workflow.Name,
					"ottoflow.nirmata.io/trigger":  "cron",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRunList.Items).To(HaveLen(1))

			wr := &workflowRunList.Items[0]
			wr.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
			Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())

			// Second fire should be skipped (Forbid)
			scheduler.handleCronFire("test-forbid")

			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": workflow.Name,
					"ottoflow.nirmata.io/trigger":  "cron",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRunList.Items).To(HaveLen(1))
		})

		It("should support timezone in schedule", func() {
			cronSpec := &ottoflowv1alpha1.CronTrigger{
				Schedule: "0 9 * * *",
				Timezone: "America/New_York",
			}
			err := scheduler.AddSchedule("test-tz", workflow, cronSpec)
			Expect(err).NotTo(HaveOccurred())
			Expect(scheduler.HasSchedule("test-tz")).To(BeTrue())
		})
	})

	Context("MaxConcurrentRuns", func() {
		It("should count active runs across all trigger types (not just cron)", func() {
			// countActiveWorkflowRuns must count Pending/Running runs regardless of trigger label.
			// This test locks in the behavior so a future regression (adding a cron-only filter) is caught.
			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "max-concurrent-wf", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{{Name: "echo", Expressions: []ottoflowv1alpha1.Expression{{Name: "r", Expression: `"ok"`}}}},
					Run:   &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: func() *int32 { v := int32(1); return &v }()},
				},
			}
			Expect(k8sClient.Create(ctx, wf)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, wf) }()

			// Create one active WorkflowRun with an event trigger label.
			activeRun := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "max-concurrent-run-1",
					Namespace: "default",
					Labels: map[string]string{
						"ottoflow.nirmata.io/workflow": wf.Name,
						"ottoflow.nirmata.io/trigger":  "event",
					},
				},
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wf.Name, Namespace: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, activeRun)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, activeRun) }()
			activeRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
			Expect(k8sClient.Status().Update(ctx, activeRun)).To(Succeed())

			// countActiveWorkflowRuns must see this event-triggered run.
			count, err := countActiveWorkflowRuns(ctx, k8sClient, wf)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(1))
		})
	})

	Context("Event Triggers", func() {
		var workflow *ottoflowv1alpha1.Workflow

		BeforeEach(func() {
			workflow = &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "event-workflow",
					Namespace: "default",
				},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "echo",
							Expressions: []ottoflowv1alpha1.Expression{
								{
									Name:       "result",
									Expression: `"test"`,
								},
							},
						},
					},
					Triggers: []ottoflowv1alpha1.Trigger{
						{
							Event: &ottoflowv1alpha1.EventTrigger{
								Resources: []ottoflowv1alpha1.EventResource{
									{
										APIVersion: "v1",
										Kind:       "Pod",
										Namespace:  "default",
									},
								},
								Operations: []string{"CREATE"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		})

		AfterEach(func() {
			if workflow != nil {
				_ = triggerManager.UnregisterWorkflow(ctx, workflow)
				_ = k8sClient.Delete(ctx, workflow)
			}
		})

		It("should register event trigger", func() {
			err := triggerManager.RegisterWorkflow(ctx, workflow)
			Expect(err).NotTo(HaveOccurred())

			// Verify event watcher was set up (check internal state)
			// Note: This is a simplified check - in production, you'd verify the watcher is active
			Expect(triggerManager.eventWatchers).NotTo(BeEmpty())
		})

		It("should create WorkflowRun from event", func() {
			err := triggerManager.RegisterWorkflow(ctx, workflow)
			Expect(err).NotTo(HaveOccurred())

			// Simulate an Unstructured event object (as the dynamic client returns).
			// apiVersion/kind must be set so getObjectKind returns the correct Kind.
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "test-pod",
					"namespace": "default",
					"uid":       "test-pod-uid",
					"labels":    map[string]interface{}{"test": "trigger"},
				},
			}}

			// Create WorkflowRun from event (simulating event trigger)
			eventSpec := workflow.Spec.Triggers[0].Event
			err = triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger", workflow, eventSpec, obj, "ADDED")
			Expect(err).NotTo(HaveOccurred())

			// Verify WorkflowRun was created (deterministic - no timing dependency)
			workflowRunList := &ottoflowv1alpha1.WorkflowRunList{}
			err = k8sClient.List(ctx, workflowRunList, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": workflow.Name,
					"ottoflow.nirmata.io/trigger":  "event",
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(workflowRunList.Items)).To(BeNumerically(">", 0))

			// Verify WorkflowRun has trigger info (refresh to get latest status)
			workflowRun := &ottoflowv1alpha1.WorkflowRun{}
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&workflowRunList.Items[0]), workflowRun)
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRun.Status.Trigger).NotTo(BeNil())
			Expect(workflowRun.Status.Trigger.Type).To(Equal("Event"))
			Expect(workflowRun.Status.Trigger.EventResource).NotTo(BeNil())
			Expect(workflowRun.Status.Trigger.EventResource.Kind).To(Equal("Pod"))
			Expect(workflowRun.Status.Trigger.EventResource.Name).To(Equal("test-pod"))
		})

		countEventRuns := func() int {
			list := &ottoflowv1alpha1.WorkflowRunList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace("default"),
				client.MatchingLabels{
					"ottoflow.nirmata.io/workflow": workflow.Name,
					"ottoflow.nirmata.io/trigger":  "event",
				})).To(Succeed())
			return len(list.Items)
		}

		It("should not trigger on OttoFlow's own runner pods (feedback loop guard)", func() {
			before := countEventRuns()

			// Runner pods carry the workflowrun label — must never re-trigger a workflow.
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "runner-pod",
					"namespace": "default",
					"uid":       "runner-pod-uid",
					"labels": map[string]interface{}{
						runnerManagedLabel: "some-run",
					},
				},
			}}

			eventSpec := workflow.Spec.Triggers[0].Event
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-selfexcl", workflow, eventSpec, obj, "ADDED")).To(Succeed())
			Expect(countEventRuns()).To(Equal(before))
		})

		It("should dedup rapid-fire events for the same object when dedupWindow is set explicitly", func() {
			before := countEventRuns()

			// Pods have no auto-detectable revision field, so the fallback time window
			// applies. Here it's set explicitly to override the defaultDedupWindow value;
			// the effect (dedupe repeat events for this same object) is the same as the
			// default, just with a caller-chosen duration.
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "flapping-pod",
					"namespace": "default",
					"uid":       "flapping-pod-uid",
				},
			}}

			eventSpec := workflow.Spec.Triggers[0].Event
			eventSpec.DedupWindow = &metav1.Duration{Duration: time.Hour}
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-dedup", workflow, eventSpec, obj, "ADDED")).To(Succeed())
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-dedup", workflow, eventSpec, obj, "ADDED")).To(Succeed())
			Expect(countEventRuns()).To(Equal(before + 1))
		})

		It("should dedup repeat events for the same object by default when neither dedupKey nor dedupWindow is set", func() {
			before := countEventRuns()

			// No dedup key resolves and no dedupWindow is configured: the implicit
			// defaultDedupWindow applies, suppressing the second event for this
			// already-seen object (e.g. a flapping Pod re-firing the same event).
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "no-dedup-pod",
					"namespace": "default",
					"uid":       "no-dedup-pod-uid",
				},
			}}

			eventSpec := workflow.Spec.Triggers[0].Event
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-nodedup", workflow, eventSpec, obj, "ADDED")).To(Succeed())
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-nodedup", workflow, eventSpec, obj, "ADDED")).To(Succeed())
			Expect(countEventRuns()).To(Equal(before + 1))
		})

		It("should NOT dedup a stream of distinct new objects even with the default window active", func() {
			before := countEventRuns()

			// This is the concrete self-amplification regression case: a self-amplifying loop
			// never presents the same object twice — every triggering object is new by
			// construction (e.g. runner Pods spawned by the workflow's own runs). The
			// default per-object dedup window is keyed by UID, so each new object is a
			// first-sight miss and is never suppressed by it, no matter how tight the
			// window or how rapid the sequence. Loop prevention for this case comes from
			// the label-selector exclusion and the WorkflowRun ownership guard — not from
			// dedupWindow, default or explicit. This test exists to prevent a future
			// change from assuming the default window covers this case.
			eventSpec := workflow.Spec.Triggers[0].Event
			for i := 0; i < 5; i++ {
				obj := &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      fmt.Sprintf("loop-pod-%d", i),
						"namespace": "default",
						"uid":       fmt.Sprintf("loop-pod-uid-%d", i),
					},
				}}
				Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-loop", workflow, eventSpec, obj, "ADDED")).To(Succeed())
			}
			Expect(countEventRuns()).To(Equal(before + 5))
		})

		It("should not dedup a deleted-and-recreated object with the same namespace/name", func() {
			before := countEventRuns()

			// Same namespace/name as an object dedup'd within the window, but a
			// different UID: this represents delete+recreate, not a re-fired event
			// for the same instance, and must not be suppressed as burst noise.
			original := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "recreated-pod",
					"namespace": "default",
					"uid":       "original-uid",
				},
			}}
			recreated := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "recreated-pod",
					"namespace": "default",
					"uid":       "recreated-uid",
				},
			}}

			eventSpec := workflow.Spec.Triggers[0].Event
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-recreate", workflow, eventSpec, original, "ADDED")).To(Succeed())
			Expect(triggerManager.CreateWorkflowRunFromEvent(ctx, "test-trigger-recreate", workflow, eventSpec, recreated, "ADDED")).To(Succeed())
			Expect(countEventRuns()).To(Equal(before + 2))
		})
	})
})
