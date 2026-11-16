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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

var _ = Describe("Mutate Step Execution", func() {
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

	It("should apply ApplyConfiguration patch (merge labels)", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cm", Namespace: "default"},
			Data:       map[string]string{"key": "value"},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "addLabel",
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target: ottoflowv1alpha1.StepMutateTarget{
								APIVersion: "v1",
								Resource:   "ConfigMap",
								Namespace:  `"default"`,
								Name:       `"my-cm"`,
							},
							PatchType: "ApplyConfiguration",
							ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{
								Expression: `{"metadata": {"labels": {"managed-by": "ottoflow"}}}`,
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

		updated := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-cm"}, updated)).To(Succeed())
		Expect(updated.Labels).NotTo(BeNil())
		Expect(updated.Labels["managed-by"]).To(Equal("ottoflow"))
	})

	It("should apply JSONPatch (add annotation)", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "patch-cm", Namespace: "default"},
			Data:       map[string]string{"a": "b"},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "addAnnotation",
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target: ottoflowv1alpha1.StepMutateTarget{
								APIVersion: "v1",
								Resource:   "ConfigMap",
								Namespace:  `"default"`,
								Name:       `"patch-cm"`,
							},
							PatchType: "JSONPatch",
							JSONPatch: &ottoflowv1alpha1.MutateJSONPatch{
								Operations: []ottoflowv1alpha1.MutateJSONPatchOp{
									{Op: "add", Path: "/metadata/annotations", Value: &apiextensionsv1.JSON{Raw: []byte(`{"ottoflow/key":"value"}`)}},
								},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())

		updated := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "patch-cm"}, updated)).To(Succeed())
		Expect(updated.Annotations).NotTo(BeNil())
		Expect(updated.Annotations["ottoflow/key"]).To(Equal("value"))
	})

	It("step-level outputs: should evaluate alongside mutate.outputs", func() {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "my-cm", Namespace: "default"}}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "addLabel",
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target: ottoflowv1alpha1.StepMutateTarget{
								APIVersion: "v1", Resource: "ConfigMap", Namespace: `"default"`, Name: `"my-cm"`,
							},
							PatchType: "ApplyConfiguration",
							ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{
								Expression: `{"metadata": {"labels": {"managed-by": "ottoflow"}}}`,
							},
							Outputs: map[string]string{"label": `object.metadata.labels["managed-by"]`},
						},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "derived", Expression: `variables.label + "-x"`},
							{Name: "name", Expression: `object.metadata.name`},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["label"]).To(Equal("ottoflow"))
		Expect(variables["derived"]).To(Equal("ottoflow-x"))
		Expect(variables["name"]).To(Equal("my-cm"))
	})

	It("step-level outputs: should be written when mutate.outputs is absent", func() {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "my-cm", Namespace: "default"}}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "addLabel",
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target: ottoflowv1alpha1.StepMutateTarget{
								APIVersion: "v1", Resource: "ConfigMap", Namespace: `"default"`, Name: `"my-cm"`,
							},
							PatchType: "ApplyConfiguration",
							ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{
								Expression: `{"metadata": {"labels": {"managed-by": "ottoflow"}}}`,
							},
						},
						Outputs: []ottoflowv1alpha1.Output{{Name: "name", Expression: `object.metadata.name`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(contextData["variables"].(map[string]interface{})).To(HaveKeyWithValue("name", "my-cm"))
	})

	It("should fail when mutate target resource does not exist", func() {
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
						Name: "mutateMissing",
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target: ottoflowv1alpha1.StepMutateTarget{
								APIVersion: "v1",
								Resource:   "ConfigMap",
								Namespace:  `"default"`,
								Name:       `"missing"`,
							},
							PatchType: "ApplyConfiguration",
							ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{
								Expression: `{"metadata": {"labels": {"x": "y"}}}`,
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
	})

	It("should apply JSONPatch from CEL expression", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "patch-me", Namespace: "default"},
			Data:       map[string]string{"key": "value"},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "jsonPatchExpr",
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target: ottoflowv1alpha1.StepMutateTarget{
								APIVersion: "v1",
								Resource:   "ConfigMap",
								Namespace:  `"default"`,
								Name:       `"patch-me"`,
							},
							PatchType: "JSONPatch",
							JSONPatch: &ottoflowv1alpha1.MutateJSONPatch{
								// CEL expression returning list of ops; value is string so type is consistent
								Expression: `[{"op": "replace", "path": "/data/key", "value": "from-cel"}]`,
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		updated := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "patch-me", Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Data["key"]).To(Equal("from-cel"))
	})
})

var _ = Describe("deepMergeMap", func() {
	It("merges top-level keys from patch into base", func() {
		base := map[string]interface{}{"a": "1", "b": "2"}
		patch := map[string]interface{}{"b": "two", "c": "3"}
		out := deepMergeMap(base, patch)
		Expect(out["a"]).To(Equal("1"))
		Expect(out["b"]).To(Equal("two"))
		Expect(out["c"]).To(Equal("3"))
	})

	It("recursively merges nested maps", func() {
		base := map[string]interface{}{"meta": map[string]interface{}{"a": "1", "b": "2"}}
		patch := map[string]interface{}{"meta": map[string]interface{}{"b": "B", "c": "3"}}
		out := deepMergeMap(base, patch)
		meta := out["meta"].(map[string]interface{})
		Expect(meta["a"]).To(Equal("1"))
		Expect(meta["b"]).To(Equal("B"))
		Expect(meta["c"]).To(Equal("3"))
	})

	It("overwrites non-map values with patch", func() {
		base := map[string]interface{}{"x": "old"}
		patch := map[string]interface{}{"x": "new"}
		out := deepMergeMap(base, patch)
		Expect(out["x"]).To(Equal("new"))
	})
})
