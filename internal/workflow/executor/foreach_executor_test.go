/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// Helper function to get map keys for debugging
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

var _ = Describe("ForEach Executor", func() {
	var (
		ctx              context.Context
		k8sClient        client.Client
		workflowExecutor *WorkflowExecutor
		workflowRun      *ottoflowv1alpha1.WorkflowRun
		scheme           *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		_ = ottoflowv1alpha1.AddToScheme(scheme)
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-run",
				Namespace: "default",
			},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{
					Name:      "test-workflow",
					Namespace: "default",
				},
				InputValues: map[string]string{},
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{
				Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning,
			},
		}

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient,
			nil, // metricsClient
			nil, // customMetricsClient
			nil, // prometheusClient
			workflowRun,
			nil,   // agentExecutor
			false, // localExecutionMode
			0,     // celCacheSize
			5,     // maxWorkers
			nil,   // eventRecorder
		)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should invoke progress callback during forEach execution", func() {
		var callbackInvocations int
		workflowExecutor.SetProgressCallback(func(_ *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
			callbackInvocations++
		})
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Variables: []ottoflowv1alpha1.Variable{{Name: "items", Expression: `["a", "b"]`}},
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "fe",
						ForEach: &ottoflowv1alpha1.StepForEach{
							Items: `variables.items`,
							Step: &ottoflowv1alpha1.StepForEachStep{
								Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}},
							},
						},
					},
				},
			},
		}
		err := workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
		Expect(err).NotTo(HaveOccurred())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(callbackInvocations).To(BeNumerically(">=", 1))
	})

	Context("When executing a forEach step with inline step definition", func() {
		It("should process items concurrently and collect results", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-workflow",
					Namespace: "default",
				},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{
							Name:       "items",
							Expression: `["item1", "item2", "item3"]`,
						},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "processItems",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									Expressions: []ottoflowv1alpha1.Expression{
										{
											Name:       "processed",
											Expression: `variables.item + "-processed"`,
										},
									},
									Outputs: []ottoflowv1alpha1.Output{
										{
											Name:       "result",
											Expression: `expressions.processed`,
										},
									},
								},
								MaxConcurrency:    3,
								ItemFailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
							},
							Outputs: []ottoflowv1alpha1.Output{
								{
									Name:       "totalProcessed",
									Expression: `size(steps.processItems.results)`,
								},
							},
						},
					},
				},
			}

			// Initialize context
			err := workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
			Expect(err).NotTo(HaveOccurred())

			// Execute workflow
			err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			if err != nil {
				// Print error details for debugging
				fmt.Printf("Workflow execution error: %v\n", err)
				if workflowRun.Status.Message != "" {
					fmt.Printf("Workflow status message: %s\n", workflowRun.Status.Message)
				}
			}
			Expect(err).NotTo(HaveOccurred())

			// Verify workflow completed successfully
			Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))

			// Verify step status
			stepStatus, exists := workflowRun.Status.StepStatuses["processItems"]
			Expect(exists).To(BeTrue())
			Expect(stepStatus.Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

			// Verify results - use GetContext() to get the actual in-memory context (not a copy)
			inMemoryContext := workflowExecutor.contextManager.GetContext()
			Expect(inMemoryContext).NotTo(BeNil())

			// Debug: Check if variables.items exists
			variables := inMemoryContext["variables"].(map[string]interface{})
			itemsVal, itemsExists := variables["items"]
			if !itemsExists {
				fmt.Printf("DEBUG: variables.items does not exist. Variables keys: %v\n", getMapKeys(variables))
			} else {
				fmt.Printf("DEBUG: variables.items exists, type: %T, value: %v\n", itemsVal, itemsVal)
			}

			// Check steps map
			stepsMap, ok := inMemoryContext["steps"].(map[string]interface{})
			if !ok {
				fmt.Printf("Steps map not found in context. Context keys: %v\n", getMapKeys(inMemoryContext))
			}
			Expect(ok).To(BeTrue())
			Expect(stepsMap).NotTo(BeNil())

			processItemsDataVal, exists := stepsMap["processItems"]
			if !exists {
				fmt.Printf("processItems not found in steps map. Steps map keys: %v\n", getMapKeys(stepsMap))
				Expect(exists).To(BeTrue())
				return
			}
			processItemsData, ok := processItemsDataVal.(map[string]interface{})
			if !ok {
				fmt.Printf("processItems is not map[string]interface{}, got type: %T, value: %v\n", processItemsDataVal, processItemsDataVal)
				Expect(ok).To(BeTrue())
				return
			}

			// Verify results
			resultsVal, exists := processItemsData["results"]
			if !exists {
				fmt.Printf("results key not found in processItemsData. Keys: %v\n", getMapKeys(processItemsData))
				Expect(exists).To(BeTrue())
				return
			}
			results, ok := resultsVal.([]interface{})
			if !ok {
				fmt.Printf("results is not []interface{}, got type: %T, value: %v\n", resultsVal, resultsVal)
				Expect(ok).To(BeTrue())
				return
			}
			if len(results) == 0 {
				fmt.Printf("results is empty. Full processItemsData: %+v\n", processItemsData)
			}
			Expect(len(results)).To(Equal(3))

			succeeded, ok := processItemsData["succeeded"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(len(succeeded)).To(Equal(3))

			// Verify each result (results may be in any order due to concurrency)
			expectedItems := map[string]bool{
				"item1": false,
				"item2": false,
				"item3": false,
			}
			for _, result := range results {
				resultMap := result.(map[string]interface{})
				Expect(resultMap["status"]).To(Equal("succeeded"))
				item := resultMap["item"].(string)
				Expect(expectedItems).To(HaveKey(item))
				expectedItems[item] = true

				outputs := resultMap["outputs"].(map[string]interface{})
				Expect(outputs["result"]).To(Equal(item + "-processed"))
			}
			// Verify all items were processed
			for item, found := range expectedItems {
				Expect(found).To(BeTrue(), "Expected item %s to be processed", item)
			}

			// Verify step output (variables already declared above)
			Expect(variables["totalProcessed"]).To(Equal(int64(3)))
		})

		It("should handle empty items list", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-workflow",
					Namespace: "default",
				},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{
							Name:       "items",
							Expression: `[]`,
						},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "processItems",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									Expressions: []ottoflowv1alpha1.Expression{
										{
											Name:       "processed",
											Expression: `variables.item + "-processed"`,
										},
									},
									Outputs: []ottoflowv1alpha1.Output{
										{
											Name:       "result",
											Expression: `expressions.processed`,
										},
									},
								},
							},
						},
					},
				},
			}

			// Initialize context
			err := workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
			Expect(err).NotTo(HaveOccurred())

			// Execute workflow
			err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).NotTo(HaveOccurred())

			// Verify empty results - use GetContext() to get the actual in-memory context
			inMemoryContext := workflowExecutor.contextManager.GetContext()
			Expect(inMemoryContext).NotTo(BeNil())

			stepsMap := inMemoryContext["steps"].(map[string]interface{})
			processItemsData := stepsMap["processItems"].(map[string]interface{})

			results := processItemsData["results"].([]interface{})
			Expect(len(results)).To(Equal(0))
		})

		It("should respect maxConcurrency limit", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-workflow",
					Namespace: "default",
				},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{
							Name:       "items",
							Expression: `["item1", "item2", "item3", "item4", "item5"]`,
						},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "processItems",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									Expressions: []ottoflowv1alpha1.Expression{
										{
											Name:       "processed",
											Expression: `variables.item + "-processed"`,
										},
									},
									Outputs: []ottoflowv1alpha1.Output{
										{
											Name:       "result",
											Expression: `expressions.processed`,
										},
									},
								},
								MaxConcurrency:    2, // Limit to 2 concurrent workers
								ItemFailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
							},
						},
					},
				},
			}

			// Initialize context
			err := workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
			Expect(err).NotTo(HaveOccurred())

			// Execute workflow
			err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).NotTo(HaveOccurred())

			// Verify all items were processed - use GetContext() to get the actual in-memory context
			inMemoryContext := workflowExecutor.contextManager.GetContext()
			Expect(inMemoryContext).NotTo(BeNil())

			stepsMap := inMemoryContext["steps"].(map[string]interface{})
			processItemsData := stepsMap["processItems"].(map[string]interface{})

			results := processItemsData["results"].([]interface{})
			Expect(len(results)).To(Equal(5))

			succeeded := processItemsData["succeeded"].([]interface{})
			Expect(len(succeeded)).To(Equal(5))
		})
	})

	Context("When executing a forEach step with StepTemplateRef", func() {
		It("should instantiate template per item and collect results", func() {
			tpl := &ottoflowv1alpha1.StepTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "echo-tpl", Namespace: "default"},
				Spec: ottoflowv1alpha1.StepTemplateSpec{
					Parameters: []ottoflowv1alpha1.StepTemplateParameter{
						{Name: "value", Required: true},
					},
					Step: ottoflowv1alpha1.StepTemplateStep{
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "result", Expression: `"{{.value}}"`},
						},
					},
				},
			}
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tpl).Build()
			var err error
			workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
				k8sClient, nil, nil, nil, workflowRun, nil, false, 0, 5, nil)
			Expect(err).NotTo(HaveOccurred())

			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["a", "b", "c"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "loop",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								StepTemplateRef: &ottoflowv1alpha1.StepForEachTemplateRef{
									Name: "echo-tpl",
									Arguments: map[string]string{
										"value": "variables.item",
									},
								},
							},
						},
					},
				},
			}
			err = workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
			Expect(err).NotTo(HaveOccurred())

			err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRun.Status.StepStatuses["loop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

			inMemoryContext := workflowExecutor.contextManager.GetContext()
			stepsMap := inMemoryContext["steps"].(map[string]interface{})
			loopData := stepsMap["loop"].(map[string]interface{})
			results := loopData["results"].([]interface{})
			Expect(results).To(HaveLen(3))
			resultValues := make([]string, 0, len(results))
			for _, r := range results {
				m := r.(map[string]interface{})
				outputs := m["outputs"].(map[string]interface{})
				resultValues = append(resultValues, outputs["result"].(string))
			}
			Expect(resultValues).To(ContainElements("a", "b", "c"))
		})
	})

	Context("When executing a forEach step with ResourceQuery (inline step)", func() {
		It("should run resourceQuery per item and collect outputs", func() {
			schemeWithCore := runtime.NewScheme()
			utilruntime.Must(ottoflowv1alpha1.AddToScheme(schemeWithCore))
			utilruntime.Must(clientgoscheme.AddToScheme(schemeWithCore))
			cm1 := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "cm-one", Namespace: "default"},
				Data:       map[string]string{"id": "one"},
			}
			cm2 := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "cm-two", Namespace: "default"},
				Data:       map[string]string{"id": "two"},
			}
			k8sClient = fake.NewClientBuilder().WithScheme(schemeWithCore).WithObjects(cm1, cm2).Build()
			var err error
			workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
				k8sClient, nil, nil, nil, workflowRun, nil, false, 0, 5, nil)
			Expect(err).NotTo(HaveOccurred())

			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["cm-one", "cm-two"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "queryEach",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
										APIVersion: "v1",
										Resource:   "ConfigMap",
										Namespace:  `"default"`,
										Name:       "variables.item",
										Outputs: map[string]string{
											"dataId": "object.data.id",
										},
									},
								},
							},
						},
					},
				},
			}
			err = workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
			Expect(err).NotTo(HaveOccurred())

			err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
			Expect(workflowRun.Status.StepStatuses["queryEach"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

			inMemoryContext := workflowExecutor.contextManager.GetContext()
			stepsMap := inMemoryContext["steps"].(map[string]interface{})
			queryData := stepsMap["queryEach"].(map[string]interface{})
			results := queryData["results"].([]interface{})
			Expect(results).To(HaveLen(2))
			ids := make([]string, 0, 2)
			for _, r := range results {
				m := r.(map[string]interface{})
				Expect(m["status"]).To(Equal("succeeded"))
				outputs := m["outputs"].(map[string]interface{})
				ids = append(ids, outputs["dataId"].(string))
			}
			Expect(ids).To(ContainElements("one", "two"))
		})
	})

	Context("When executing a forEach step with Mutate (inline step)", func() {
		It("should run mutate per item and collect outputs", func() {
			schemeWithCore := runtime.NewScheme()
			utilruntime.Must(ottoflowv1alpha1.AddToScheme(schemeWithCore))
			utilruntime.Must(clientgoscheme.AddToScheme(schemeWithCore))
			cm1 := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "mut-a", Namespace: "default"},
				Data:       map[string]string{"x": "1"},
			}
			cm2 := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "mut-b", Namespace: "default"},
				Data:       map[string]string{"x": "2"},
			}
			k8sClient = fake.NewClientBuilder().WithScheme(schemeWithCore).WithObjects(cm1, cm2).Build()
			var err error
			workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
				k8sClient, nil, nil, nil, workflowRun, nil, false, 0, 5, nil)
			Expect(err).NotTo(HaveOccurred())

			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["mut-a", "mut-b"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "mutateEach",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									Mutate: &ottoflowv1alpha1.StepMutate{
										PatchType: "JSONPatch",
										Target: ottoflowv1alpha1.StepMutateTarget{
											APIVersion: "v1",
											Resource:   "ConfigMap",
											Namespace:  `"default"`,
											Name:       "variables.item",
										},
										JSONPatch: &ottoflowv1alpha1.MutateJSONPatch{
											Operations: []ottoflowv1alpha1.MutateJSONPatchOp{
												{Op: "add", Path: "/metadata/labels", Value: &apiextensionsv1.JSON{Raw: []byte(`{"patched":"true"}`)}},
											},
										},
										Outputs: map[string]string{"name": "object.metadata.name"},
									},
								},
							},
						},
					},
				},
			}
			err = workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
			Expect(err).NotTo(HaveOccurred())

			err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).NotTo(HaveOccurred())
			Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
			Expect(workflowRun.Status.StepStatuses["mutateEach"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

			inMemoryContext := workflowExecutor.contextManager.GetContext()
			stepsMap := inMemoryContext["steps"].(map[string]interface{})
			mutateData := stepsMap["mutateEach"].(map[string]interface{})
			results := mutateData["results"].([]interface{})
			Expect(results).To(HaveLen(2))
			names := make([]string, 0, 2)
			for _, r := range results {
				m := r.(map[string]interface{})
				Expect(m["status"]).To(Equal("succeeded"))
				outputs := m["outputs"].(map[string]interface{})
				names = append(names, outputs["name"].(string))
			}
			Expect(names).To(ContainElements("mut-a", "mut-b"))

			// Verify ConfigMaps were patched
			var got1, got2 corev1.ConfigMap
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "mut-a"}, &got1)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "mut-b"}, &got2)).To(Succeed())
			Expect(got1.Labels).To(HaveKeyWithValue("patched", "true"))
			Expect(got2.Labels).To(HaveKeyWithValue("patched", "true"))
		})
	})

	Context("When items fail", func() {
		// forEachWorkflow builds a workflow with a single forEach step over ["a", "b", "c"]:
		// childExpr is evaluated once per item (as MaxConcurrency=1, so results stay
		// deterministic regardless of completion order) under the given itemFailurePolicy.
		forEachWorkflow := func(policy, childExpr string) *ottoflowv1alpha1.Workflow {
			return &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["a", "b", "c"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "loop",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									Expressions: []ottoflowv1alpha1.Expression{
										{Name: "boom", Expression: childExpr},
									},
								},
								MaxConcurrency:    1,
								ItemFailurePolicy: policy,
							},
						},
					},
				},
			}
		}

		// alwaysFailingForEach builds a workflow whose every forEach item fails: the child
		// expression indexes a field on a string item, which CEL rejects at evaluation.
		alwaysFailingForEach := func(policy string) *ottoflowv1alpha1.Workflow {
			return forEachWorkflow(policy, `variables.item.noSuchField`)
		}

		It("fails the step when itemFailurePolicy is Fail", func() {
			workflow := alwaysFailingForEach(ottoflowv1alpha1.FailurePolicyFail)
			Expect(workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())

			err := workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("itemFailurePolicy=Fail"))
			Expect(workflowRun.Status.StepStatuses["loop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))
		})

		It("fails the step when every item fails under itemFailurePolicy=Continue", func() {
			workflow := alwaysFailingForEach(ottoflowv1alpha1.FailurePolicyContinue)
			Expect(workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())

			// Continue tolerates partial failure, but a loop where every item failed must
			// never report success -- that would be a false green. It must not look identical
			// to a clean run.
			err := workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("all 3 item(s) failed"))
			Expect(workflowRun.Status.StepStatuses["loop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))

			// Failures remain available in context for debugging even though the step failed.
			failed := workflowExecutor.contextManager.GetContext()["steps"].(map[string]interface{})["loop"].(map[string]interface{})["failed"].([]interface{})
			Expect(failed).To(HaveLen(3))
		})

		It("stays Succeeded with an accurate tally when only some items fail under itemFailurePolicy=Continue", func() {
			// Only item "b" fails (field access on a string); "a" and "c" succeed.
			workflow := forEachWorkflow(ottoflowv1alpha1.FailurePolicyContinue, `variables.item == "b" ? variables.item.noSuchField : variables.item`)
			Expect(workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())

			Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
			Expect(workflowRun.Status.StepStatuses["loop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))
			Expect(workflowRun.Status.StepStatuses["loop"].Message).To(Equal("2/3 items succeeded, 1 failed"))

			failed := workflowExecutor.contextManager.GetContext()["steps"].(map[string]interface{})["loop"].(map[string]interface{})["failed"].([]interface{})
			Expect(failed).To(HaveLen(1))
		})

		It("still writes step outputs when every item fails under a step-level failurePolicy: Continue, so a downstream step can read them", func() {
			// Distinct from itemFailurePolicy (which governs individual items within the
			// forEach): step.FailurePolicy is the outer, step-level escape hatch that lets the
			// *workflow* proceed past this step even though it ends up Failed. The forEach step
			// declares its own output (a count of failed items) and a downstream step reads it.
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["a", "b", "c"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "loop",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								Step: &ottoflowv1alpha1.StepForEachStep{
									Expressions: []ottoflowv1alpha1.Expression{
										{Name: "boom", Expression: `variables.item.noSuchField`},
									},
								},
								MaxConcurrency:    1,
								ItemFailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
							},
							Outputs: []ottoflowv1alpha1.Output{
								{Name: "failedCount", Expression: `size(steps.loop.failed)`},
							},
							FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
						},
						{
							Name:      "afterLoop",
							DependsOn: []string{"loop"},
							Outputs: []ottoflowv1alpha1.Output{
								// Step outputs land in the flat variables map (no per-step
								// namespace -- see ContextManager.WriteStepOutputs), so this
								// reads the forEach step's declared output as variables.failedCount.
								{Name: "echoedFailedCount", Expression: `variables.failedCount`},
							},
						},
					},
				},
			}
			Expect(workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())

			// step-level failurePolicy: Continue means ExecuteWorkflow itself must not error,
			// even though the forEach step is Failed.
			Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())

			Expect(workflowRun.Status.StepStatuses["loop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))
			Expect(workflowRun.Status.StepStatuses["afterLoop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

			variables := workflowExecutor.contextManager.GetContext()["variables"].(map[string]interface{})
			Expect(variables["failedCount"]).To(Equal(int64(3)))
			Expect(variables["echoedFailedCount"]).To(Equal(int64(3)))
		})

		It("fails the step when stepTemplateRef resolution fails for every item", func() {
			// No StepTemplate named "does-not-exist" is registered with the fake client, so
			// every item fails during template resolution before the child step ever runs.
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["a", "b"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "loop",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items: `variables.items`,
								StepTemplateRef: &ottoflowv1alpha1.StepForEachTemplateRef{
									Name: "does-not-exist",
								},
							},
						},
					},
				},
			}
			Expect(workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())

			err := workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("all 2 item(s) failed"))
			Expect(workflowRun.Status.StepStatuses["loop"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))
		})
	})

	Context("When referencing the current item", func() {
		It("binds the bare item variable as well as variables.<itemVariable>", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Variables: []ottoflowv1alpha1.Variable{
						{Name: "items", Expression: `["x", "y"]`},
					},
					Steps: []ottoflowv1alpha1.Step{
						{
							Name: "loop",
							ForEach: &ottoflowv1alpha1.StepForEach{
								Items:        `variables.items`,
								ItemVariable: "entry",
								Step: &ottoflowv1alpha1.StepForEachStep{
									Expressions: []ottoflowv1alpha1.Expression{
										// `item` must resolve even though the alias is "entry".
										{Name: "viaBare", Expression: `item + "!"`},
										{Name: "viaAlias", Expression: `variables.entry + "?"`},
									},
									Outputs: []ottoflowv1alpha1.Output{
										{Name: "bare", Expression: `expressions.viaBare`},
										{Name: "alias", Expression: `expressions.viaAlias`},
									},
								},
								MaxConcurrency: 1,
							},
						},
					},
				},
			}
			Expect(workflowExecutor.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)).To(Succeed())
			Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())

			results := workflowExecutor.contextManager.GetContext()["steps"].(map[string]interface{})["loop"].(map[string]interface{})["results"].([]interface{})
			Expect(results).To(HaveLen(2))
			bare := make([]string, 0, len(results))
			alias := make([]string, 0, len(results))
			for _, r := range results {
				outputs := r.(map[string]interface{})["outputs"].(map[string]interface{})
				bare = append(bare, outputs["bare"].(string))
				alias = append(alias, outputs["alias"].(string))
			}
			Expect(bare).To(ConsistOf("x!", "y!"))
			Expect(alias).To(ConsistOf("x?", "y?"))
		})
	})
})
