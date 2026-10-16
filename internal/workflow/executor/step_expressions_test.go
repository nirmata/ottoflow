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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

var _ = Describe("Step expressions on action steps", func() {
	run := func(steps []ottoflowv1alpha1.Step) (*ottoflowv1alpha1.WorkflowRun, error) {
		ctx := context.Background()
		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
		wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}, Spec: ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}}}
		ex, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, wr, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"}, Spec: ottoflowv1alpha1.WorkflowSpec{Steps: steps}}
		Expect(c.Create(ctx, wf)).To(Succeed())
		err = ex.ExecuteWorkflow(ctx, wf, wr)
		return wr, err
	}
	rq := func(name string) *ottoflowv1alpha1.StepResourceQuery {
		return &ottoflowv1alpha1.StepResourceQuery{APIVersion: "v1", Resource: "Pod", Namespace: `"default"`, Name: name, Outputs: map[string]string{"phase": "object.status.phase"}}
	}

	It("A: action step can read its own expressions", func() {
		wr, err := run([]ottoflowv1alpha1.Step{{Name: "getPod",
			Expressions:   []ottoflowv1alpha1.Expression{{Name: "podName", Expression: `"my-pod"`}},
			ResourceQuery: rq("expressions.podName")}})
		GinkgoWriter.Printf("A err=%v phase=%s step=%+v\n", err, wr.Status.Phase, wr.Status.StepStatuses["getPod"])
		Expect(err).NotTo(HaveOccurred())
		Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
	})

	It("B: failing expression on an action step is reported", func() {
		wr, err := run([]ottoflowv1alpha1.Step{{Name: "getPod",
			Expressions:   []ottoflowv1alpha1.Expression{{Name: "bad", Expression: `int("not-a-number")`}},
			ResourceQuery: rq(`"my-pod"`)}})
		GinkgoWriter.Printf("B err=%v phase=%s\n", err, wr.Status.Phase)
		Expect(wr.Status.Phase).NotTo(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded), "expression error was silently ignored")
	})

	It("C (today's behaviour, not a bug assertion): expressions from an earlier expression-only step are visible to a later action step", func() {
		wr, err := run([]ottoflowv1alpha1.Step{
			{Name: "build", Expressions: []ottoflowv1alpha1.Expression{{Name: "podName", Expression: `"my-pod"`}}},
			{Name: "getPod", DependsOn: []string{"build"}, ResourceQuery: rq("expressions.podName")}})
		GinkgoWriter.Printf("C err=%v phase=%s step=%+v\n", err, wr.Status.Phase, wr.Status.StepStatuses["getPod"])
		Expect(err).NotTo(HaveOccurred())
		Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
	})

	It("D: forEach child with action can read its own expressions", func() {
		wr, err := run([]ottoflowv1alpha1.Step{{Name: "each",
			ForEach: &ottoflowv1alpha1.StepForEach{Items: `["my-pod"]`, ItemVariable: "p",
				Step: &ottoflowv1alpha1.StepForEachStep{
					Expressions:   []ottoflowv1alpha1.Expression{{Name: "podName", Expression: `variables.p`}},
					ResourceQuery: rq("expressions.podName")}}}})
		GinkgoWriter.Printf("D err=%v phase=%s step=%+v\n", err, wr.Status.Phase, wr.Status.StepStatuses["each"])
		Expect(err).NotTo(HaveOccurred())
		Expect(wr.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
	})
})
