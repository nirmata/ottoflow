/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

var _ = Describe("ResourceQuery Step Execution", func() {
	var (
		ctx              context.Context
		k8sClient        client.Client
		scheme           *runtime.Scheme
		workflowRun      *ottoflowv1alpha1.WorkflowRun
		workflowExecutor *WorkflowExecutor
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))

		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "test-wf"}},
		}
	})

	It("should execute single resource GET and write outputs to variables", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
				ContainerStatuses: []corev1.ContainerStatus{
					{RestartCount: 3},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "getPod",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Namespace:  `"default"`,
							Name:       `"my-pod"`,
							Outputs: map[string]string{
								"phase":        "object.status.phase",
								"restartCount": "size(object.status.containerStatuses) > 0 ? int(object.status.containerStatuses[0].restartCount) : 0",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))

		stepStatus := workflowRun.Status.StepStatuses["getPod"]
		Expect(stepStatus.Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["phase"]).To(Equal("Running"))
		Expect(variables["restartCount"]).To(Equal(int64(3)))
	})

	It("should execute list query with label selector and write outputs", func() {
		pod1 := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		pod2 := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "default", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod1, pod2).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "listPods",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Namespace:  `"default"`,
							LabelSelector: map[string]string{
								"app": `"web"`,
							},
							Outputs: map[string]string{
								"count": "size(items)",
								"names": "items.map(i, i.metadata.name)",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))

		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["count"]).To(Equal(int64(2)))
		names, ok := variables["names"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(names).To(HaveLen(2))
	})

	It("should list all items when Limit is 0 (default, no cap)", func() {
		// The fake client does not implement server-side pagination, so Limit enforcement
		// is verified in integration tests against a real API server. Here we verify that
		// Limit=0 (no cap) still returns all objects correctly through the pagination loop.
		pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}}
		pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}}
		pod3 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-c", Namespace: "default"}}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod1, pod2, pod3).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "listPods",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Namespace:  `"default"`,
							Limit:      0, // no cap — returns all items
							Outputs: map[string]string{
								"count": "size(items)",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))

		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["count"]).To(Equal(int64(3)))
	})

	It("should apply Limit before outputs, so a filter over items misses matches past the limit", func() {
		// The fake client ignores the page-size hint, but the executor stops appending at
		// Limit itself. The CEL reference and the Limit API comment document this behaviour.
		pods := make([]client.Object, 0, 3)
		for _, name := range []string{"pod-a", "pod-b", "pod-c"} {
			pods = append(pods, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			})
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pods...).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "listPods",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Namespace:  `"default"`,
							Limit:      2,
							Outputs: map[string]string{
								"running": `size(items.filter(i, i.status.phase == "Running"))`,
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))

		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["running"]).To(Equal(int64(2)), "all three pods are Running, but outputs see only the first 2")
	})

	It("should fail when resource does not exist", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "getPod",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Namespace:  `"default"`,
							Name:       `"missing-pod"`,
							Outputs:    map[string]string{"phase": "resource.status.phase"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseFailed))
	})

	It("should fail on invalid apiVersion", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "badQuery",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "invalid/",
							Resource:   "Pod",
							Name:       `"x"`,
							Outputs:    map[string]string{"x": "resource.metadata.name"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("ResourceQuery step-level outputs", func() {
	// Regression: a resourceQuery step used to return as soon as it had written the
	// query's own outputs, so any step-level `outputs:` were silently dropped while the
	// step still reported Succeeded. Every other step type evaluates both.
	It("evaluates step-level outputs in addition to resourceQuery outputs", func() {
		ctx := context.Background()
		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "rq-step-outputs"}},
		}
		exec, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "rq-step-outputs", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "query",
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Namespace:  `"default"`,
							Outputs:    map[string]string{"queryCount": "size(items)"},
						},
						Outputs: []ottoflowv1alpha1.Output{
							// Step-level outputs must also see the query's outputs by name.
							{Name: "doubled", Expression: "variables.queryCount * 2"},
						},
					},
				},
			},
		}

		Expect(exec.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())
		Expect(exec.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())

		vars := exec.contextManager.GetContext()["variables"].(map[string]interface{})
		Expect(vars).To(HaveKeyWithValue("queryCount", int64(1)))
		Expect(vars).To(HaveKeyWithValue("doubled", int64(2)))
	})
})
