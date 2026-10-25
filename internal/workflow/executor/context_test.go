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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("ContextManager", func() {
	var (
		ctx            context.Context
		contextManager *ContextManager
		workflowRun    *ottoflowv1alpha1.WorkflowRun
	)

	BeforeEach(func() {
		ctx = context.Background()
		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-run",
				Namespace: "default",
			},
		}
		contextManager = NewContextManager(workflowRun)
	})

	Describe("InitializeContext", func() {
		It("should initialize context with input values", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Inputs: []ottoflowv1alpha1.Input{
						{Name: "name", Default: "default"},
						{Name: "value", Required: true},
					},
				},
			}

			inputValues := map[string]string{
				"value": "test-value",
			}

			err := contextManager.InitializeContext(ctx, workflow, inputValues)
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(contextData).NotTo(BeNil())
			Expect(contextData["inputs"]).NotTo(BeNil())

			inputs := contextData["inputs"].(map[string]interface{})
			Expect(inputs["name"]).To(Equal("default"))
			Expect(inputs["value"]).To(Equal("test-value"))
		})

		It("should fail when required input is missing", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Inputs: []ottoflowv1alpha1.Input{
						{Name: "required", Required: true},
					},
				},
			}

			inputValues := map[string]string{}

			err := contextManager.InitializeContext(ctx, workflow, inputValues)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("required"))
		})

		It("should apply the default when a required input with a default is not provided", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Inputs: []ottoflowv1alpha1.Input{
						{Name: "name", Required: true, Default: "default-value"},
					},
				},
			}

			err := contextManager.InitializeContext(ctx, workflow, map[string]string{})
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			inputs := contextData["inputs"].(map[string]interface{})
			Expect(inputs["name"]).To(Equal("default-value"))
		})

		It("should use default values when input not provided", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Inputs: []ottoflowv1alpha1.Input{
						{Name: "name", Default: "default-value"},
					},
				},
			}

			inputValues := map[string]string{}

			err := contextManager.InitializeContext(ctx, workflow, inputValues)
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			inputs := contextData["inputs"].(map[string]interface{})
			Expect(inputs["name"]).To(Equal("default-value"))
		})

		It("should apply empty default so optional inputs are always present for CEL", func() {
			workflow := &ottoflowv1alpha1.Workflow{
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Inputs: []ottoflowv1alpha1.Input{
						{Name: "namespace", Required: false, Default: ""},
						{Name: "severity", Required: false, Default: "medium"},
					},
				},
			}

			// Partial inputValues: only severity provided, namespace omitted
			inputValues := map[string]string{
				"severity": "high",
			}

			err := contextManager.InitializeContext(ctx, workflow, inputValues)
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			inputs := contextData["inputs"].(map[string]interface{})
			Expect(inputs).To(HaveKey("namespace"))
			Expect(inputs["namespace"]).To(Equal(""))
			Expect(inputs["severity"]).To(Equal("high"))
		})
	})

	Describe("WriteOutput", func() {
		BeforeEach(func() {
			workflow := &ottoflowv1alpha1.Workflow{
				Spec: ottoflowv1alpha1.WorkflowSpec{
					Inputs: []ottoflowv1alpha1.Input{},
				},
			}
			err := contextManager.InitializeContext(ctx, workflow, map[string]string{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should write step outputs directly to variables map", func() {
			outputs := map[string]interface{}{
				"result": "test-result",
				"count":  42,
			}

			err := contextManager.WriteStepOutputs(ctx, "step1", outputs)
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(contextData["variables"]).NotTo(BeNil())

			// Outputs are written directly to variables map (no namespacing)
			variables := contextData["variables"].(map[string]interface{})
			Expect(variables["result"]).To(Equal("test-result"))
			Expect(variables["count"]).To(Equal(42))
		})

		It("should allow reading variables from previous steps", func() {
			// Write first step output
			err := contextManager.WriteStepOutputs(ctx, "step1", map[string]interface{}{
				"value": "step1-value",
			})
			Expect(err).NotTo(HaveOccurred())

			// Write second step output (overwrites if same name, or adds new)
			err = contextManager.WriteStepOutputs(ctx, "step2", map[string]interface{}{
				"combined": "step2-step1-value", // Simplified - in real CEL this would reference variables.value
			})
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Variables are flat: variables.<outputName>
			variables := contextData["variables"].(map[string]interface{})
			Expect(variables["value"]).To(Equal("step1-value"))          // From step1
			Expect(variables["combined"]).To(Equal("step2-step1-value")) // From step2
		})

		It("should write outputs directly to variables (step names are already camelCase)", func() {
			outputs := map[string]interface{}{
				"podData": map[string]interface{}{
					"name": "test-pod",
				},
			}

			// Step name is already camelCase (validated by CRD)
			err := contextManager.WriteStepOutputs(ctx, "collectPodData", outputs)
			Expect(err).NotTo(HaveOccurred())

			contextData, err := contextManager.ReadContext(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Outputs are written directly to variables map
			variables := contextData["variables"].(map[string]interface{})
			Expect(variables["podData"]).NotTo(BeNil())

			podData := variables["podData"].(map[string]interface{})
			Expect(podData["name"]).To(Equal("test-pod"))
		})

		It("GetContextFrom returns scoped context when ctx has scopedContextKey", func() {
			scopedMap := map[string]interface{}{
				"variables": map[string]interface{}{"scoped": "value"},
			}
			ctxWithScope := context.WithValue(ctx, scopedContextKey, scopedMap)
			got := contextManager.GetContextFrom(ctxWithScope)
			Expect(got).To(Equal(scopedMap))
			Expect(got["variables"].(map[string]interface{})["scoped"]).To(Equal("value"))
		})

		It("WriteStepOutputs writes to scoped variables when ctx has scopedContextKey", func() {
			variablesMap := map[string]interface{}{}
			scopedMap := map[string]interface{}{"variables": variablesMap}
			ctxWithScope := context.WithValue(ctx, scopedContextKey, scopedMap)
			err := contextManager.WriteStepOutputs(ctxWithScope, "childStep", map[string]interface{}{"item": "x"})
			Expect(err).NotTo(HaveOccurred())
			Expect(variablesMap["item"]).To(Equal("x"))
		})
	})

	Describe("RestoreContext", func() {
		It("sets IsInitialized to true and makes the snapshot readable", func() {
			snapshot := map[string]interface{}{
				"inputs":      map[string]interface{}{"foo": "bar"},
				"variables":   map[string]interface{}{"x": float64(42)},
				"expressions": map[string]interface{}{},
				"steps":       map[string]interface{}{},
			}
			wr := &ottoflowv1alpha1.WorkflowRun{}
			cm := NewContextManager(wr)

			// Before restore: not initialized
			Expect(cm.IsInitialized()).To(BeFalse())

			cm.RestoreContext(snapshot)

			// After restore: initialized
			Expect(cm.IsInitialized()).To(BeTrue())

			// ReadContext returns the restored data
			data, err := cm.ReadContext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(data["inputs"]).To(Equal(map[string]interface{}{"foo": "bar"}))
			Expect(data["variables"]).To(Equal(map[string]interface{}{"x": float64(42)}))
		})

		It("overwrites existing context when called on an already-initialized manager", func() {
			wr := &ottoflowv1alpha1.WorkflowRun{}
			wf := &ottoflowv1alpha1.Workflow{}
			cm := NewContextManager(wr)
			_ = cm.InitializeContext(context.Background(), wf, map[string]string{"k": "v"})

			newSnapshot := map[string]interface{}{
				"inputs":      map[string]interface{}{"restored": "yes"},
				"variables":   map[string]interface{}{},
				"expressions": map[string]interface{}{},
				"steps":       map[string]interface{}{},
			}
			cm.RestoreContext(newSnapshot)

			data, _ := cm.ReadContext(context.Background())
			Expect(data["inputs"]).To(Equal(map[string]interface{}{"restored": "yes"}))
		})
	})
})
