/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// mockPrometheusResult implements PrometheusResult for tests.
type mockPrometheusResult struct {
	typ     string
	samples []PrometheusSample
	scalar  float64
}

func (m *mockPrometheusResult) Type() string                  { return m.typ }
func (m *mockPrometheusResult) GetVector() []PrometheusSample { return m.samples }
func (m *mockPrometheusResult) GetScalar() float64            { return m.scalar }

// mockPrometheusSample implements PrometheusSample for tests.
type mockPrometheusSample struct {
	metric map[string]string
	value  float64
	ts     time.Time
}

func (m *mockPrometheusSample) Metric() map[string]string { return m.metric }
func (m *mockPrometheusSample) Value() float64            { return m.value }
func (m *mockPrometheusSample) Timestamp() time.Time      { return m.ts }

// mockPrometheusClient returns a fixed result for tests.
type mockPrometheusClient struct {
	result PrometheusResult
	err    error
}

func (m *mockPrometheusClient) Query(ctx context.Context, query string, ts time.Time) (PrometheusResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

var _ = Describe("PrometheusQuery Step Execution", func() {
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
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "test-wf"}},
		}
	})

	It("should execute prometheusQuery step and write outputs", func() {
		mockResult := &mockPrometheusResult{
			typ: "vector",
			samples: []PrometheusSample{
				&mockPrometheusSample{
					metric: map[string]string{"pod": "my-pod", "namespace": "default"},
					value:  0.5,
					ts:     time.Now(),
				},
			},
		}
		mockClient := &mockPrometheusClient{result: mockResult}

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, mockClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "queryCpu",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "container_cpu_usage_seconds_total{namespace=\"default\"}",
							TimeRange: "5m",
							Outputs: map[string]string{
								"sampleCount": "size(result.samples)",
								"firstValue":  "size(result.samples) > 0 ? result.samples[0].value : 0.0",
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

		stepStatus := workflowRun.Status.StepStatuses["queryCpu"]
		Expect(stepStatus.Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["sampleCount"]).To(Equal(int64(1)))
		Expect(variables["firstValue"]).To(Equal(0.5))
	})

	It("should fail when prometheus client is nil", func() {
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "queryCpu",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "up",
							TimeRange: "5m",
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("prometheus client not configured"))
	})

	It("should substitute Variables into Query and evaluate outputs", func() {
		var capturedQuery string
		mockClient := &mockPrometheusClient{
			result: &mockPrometheusResult{
				typ:     "vector",
				samples: []PrometheusSample{&mockPrometheusSample{metric: map[string]string{"ns": "default"}, value: 1.0, ts: time.Now()}},
			},
		}
		// Wrap to capture query (mockPrometheusClient doesn't store it; use a wrapper that records then delegates)
		recordQueryClient := &recordQueryPrometheusClient{mockClient, &capturedQuery}

		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, recordQueryClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Variables: []ottoflowv1alpha1.Variable{
					{Name: "ns", Expression: `"default"`},
				},
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "queryWithVars",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     `metric{namespace="{{.namespace}}"}`,
							TimeRange: "1h",
							Variables: map[string]string{
								"namespace": "variables.ns",
							},
							Outputs: map[string]string{
								"count": "size(result.samples)",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(capturedQuery).To(Equal(`metric{namespace="default"}`))
		contextData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["count"]).To(Equal(int64(1)))
	})

	It("should accept timeRange with days (7d)", func() {
		mockClient := &mockPrometheusClient{
			result: &mockPrometheusResult{typ: "vector", samples: []PrometheusSample{}},
		}
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, mockClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "query7d",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "up",
							TimeRange: "7d",
							Outputs:   map[string]string{"sampleCount": "size(result.samples)"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
	})

	It("should write default result when Outputs is empty", func() {
		mockClient := &mockPrometheusClient{
			result: &mockPrometheusResult{
				typ: "scalar", scalar: 42.0,
			},
		}
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, mockClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "scalarQuery",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "count(up)",
							TimeRange: "5m",
							// no Outputs -> default "result" map
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		contextData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables).To(HaveKey("result"))
		result := variables["result"].(map[string]interface{})
		Expect(result["type"]).To(Equal("scalar"))
		Expect(result["value"]).To(Equal(42.0))
	})

	It("step-level outputs: should see prometheusQuery.outputs as variables", func() {
		mockClient := &mockPrometheusClient{
			result: &mockPrometheusResult{
				typ: "vector",
				samples: []PrometheusSample{
					&mockPrometheusSample{metric: map[string]string{"pod": "p"}, value: 0.5, ts: time.Now()},
				},
			},
		}
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, mockClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "q",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "up",
							TimeRange: "5m",
							Outputs:   map[string]string{"count": "size(result.samples)"},
						},
						Outputs: []ottoflowv1alpha1.Output{{Name: "doubled", Expression: `variables.count * 2`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables["count"]).To(BeEquivalentTo(1))
		Expect(variables["doubled"]).To(BeEquivalentTo(2))
	})

	It("step-level outputs: should be written alongside the default result", func() {
		mockClient := &mockPrometheusClient{result: &mockPrometheusResult{typ: "vector"}}
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, mockClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "q",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "up",
							TimeRange: "5m",
						},
						Outputs: []ottoflowv1alpha1.Output{{Name: "kind", Expression: `result.type`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		Expect(workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		contextData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := contextData["variables"].(map[string]interface{})
		Expect(variables).To(HaveKey("result"))
		Expect(variables["kind"]).To(Equal("vector"))
	})

	It("should fail on invalid timeRange", func() {
		mockClient := &mockPrometheusClient{result: &mockPrometheusResult{typ: "vector", samples: nil}}
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, mockClient, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "badRange",
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "up",
							TimeRange: "invalid",
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("timeRange"))
	})
})

// recordQueryPrometheusClient captures the query string then delegates to the inner client.
type recordQueryPrometheusClient struct {
	inner PrometheusClient
	query *string
}

func (r *recordQueryPrometheusClient) Query(ctx context.Context, query string, ts time.Time) (PrometheusResult, error) {
	*r.query = query
	return r.inner.Query(ctx, query, ts)
}

// mockPrometheusQueryAPI implements prometheusQueryAPI for tests.
type mockPrometheusQueryAPI struct {
	value model.Value
	err   error
}

func (m *mockPrometheusQueryAPI) Query(ctx context.Context, query string, ts time.Time, opts ...prometheusv1.Option) (model.Value, prometheusv1.Warnings, error) {
	if m.err != nil {
		return nil, nil, m.err
	}
	return m.value, nil, nil
}

var _ = Describe("HTTPPrometheusClient", func() {
	It("NewHTTPPrometheusClient with invalid URL returns error", func() {
		_, err := NewHTTPPrometheusClient("://invalid-url")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Prometheus"))
	})

	It("Query with mock API returns vector result and GetVector/GetScalar/Type", func() {
		vec := model.Vector{
			{Metric: model.Metric{"job": "a"}, Value: 1.5, Timestamp: model.Time(1000)},
		}
		api := &mockPrometheusQueryAPI{value: vec}
		client := NewHTTPPrometheusClientWithAPI(api)
		result, err := client.Query(context.Background(), "up", time.Unix(1, 0))
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
		Expect(result.Type()).To(Equal("vector"))
		Expect(result.GetScalar()).To(Equal(0.0))
		samples := result.GetVector()
		Expect(samples).To(HaveLen(1))
		Expect(samples[0].Value()).To(Equal(1.5))
		Expect(samples[0].Metric()).To(Equal(map[string]string{"job": "a"}))
		Expect(samples[0].Timestamp().UnixMilli()).To(Equal(int64(1000)))
	})

	It("Query with mock API returns scalar result", func() {
		sc := model.Scalar{Value: 42.5, Timestamp: 2000}
		api := &mockPrometheusQueryAPI{value: &sc}
		client := NewHTTPPrometheusClientWithAPI(api)
		result, err := client.Query(context.Background(), "scalar(1)", time.Unix(2, 0))
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
		Expect(result.Type()).To(Equal("scalar"))
		Expect(result.GetScalar()).To(Equal(42.5))
		Expect(result.GetVector()).To(BeNil())
	})

	It("Query with mock API error returns error", func() {
		api := &mockPrometheusQueryAPI{err: fmt.Errorf("connection refused")}
		client := NewHTTPPrometheusClientWithAPI(api)
		result, err := client.Query(context.Background(), "up", time.Now())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("connection refused"))
		Expect(result).To(BeNil())
	})
})

var _ = Describe("NoOp Prometheus and CustomMetrics clients", func() {
	It("NoOpPrometheusClient.Query returns error", func() {
		client := &NoOpPrometheusClient{}
		result, err := client.Query(context.Background(), "up", time.Now())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not configured"))
		Expect(result).To(BeNil())
	})

	It("NoOpCustomMetricsClient.GetMetric returns error", func() {
		client := &NoOpCustomMetricsClient{}
		val, err := client.GetMetric(context.Background(), "v1", "Pod", "default", "x", "metric")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not configured"))
		Expect(val).To(BeNil())
	})
})
