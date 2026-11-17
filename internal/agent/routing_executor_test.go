/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("RoutingAgentExecutor", func() {
	var (
		ctx         context.Context
		nirmataMock *MockAgentExecutor
		defaultMock *MockAgentExecutor
		router      *RoutingAgentExecutor
	)

	BeforeEach(func() {
		ctx = context.Background()
		nirmataMock = NewMockAgentExecutor()
		nirmataMock.SetDefaultResponse("from nirmata")
		defaultMock = NewMockAgentExecutor()
		defaultMock.SetDefaultResponse("from default")
		router = NewRoutingAgentExecutorFromExecutors(nirmataMock, defaultMock)
	})

	DescribeTable("routes ModelProvider nirmata and empty to the Nirmata delegate",
		func(provider string) {
			agentCRD := &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: provider},
			}
			response, _, err := router.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
			Expect(err).NotTo(HaveOccurred())
			Expect(response).To(Equal("from nirmata"))
			Expect(nirmataMock.GetCallCount()).To(Equal(1))
			Expect(defaultMock.GetCallCount()).To(Equal(0))
		},
		Entry("nirmata", "nirmata"),
		Entry("empty", ""),
	)

	DescribeTable("routes every other ModelProvider to the default delegate",
		func(provider string) {
			agentCRD := &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: provider},
			}
			response, _, err := router.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
			Expect(err).NotTo(HaveOccurred())
			Expect(response).To(Equal("from default"))
			Expect(defaultMock.GetCallCount()).To(Equal(1))
			Expect(nirmataMock.GetCallCount()).To(Equal(0))
		},
		Entry("openai", "openai"),
		Entry("anthropic", "anthropic"),
		Entry("azure-openai", "azure-openai"),
		Entry("google", "google"),
		Entry("gemini", "gemini"),
		Entry("local", "local"),
		Entry("unknown", "some-unrecognized-provider"),
	)

	disabled := &ottoflowv1alpha1.AgentThinking{Type: "disabled"}
	DescribeTable("refuses a non-empty spec.thinking on every provider but anthropic, before any delegate runs",
		func(provider string, thinking *ottoflowv1alpha1.AgentThinking) {
			agentCRD := &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec: ottoflowv1alpha1.AgentSpec{
					ModelProvider: provider,
					Thinking:      thinking,
				},
			}
			_, _, err := router.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
			Expect(err).To(MatchError(ContainSubstring("spec.thinking is supported only for modelProvider anthropic")))
			Expect(nirmataMock.GetCallCount() + defaultMock.GetCallCount()).To(Equal(0))
		},
		Entry("nirmata", "nirmata", disabled),
		Entry("empty", "", disabled),
		Entry("openai", "openai", disabled),
		Entry("azure-openai", "azure-openai", disabled),
		Entry("google", "google", disabled),
		Entry("gemini", "gemini", disabled),
		Entry("local", "local", disabled),
		Entry("effort only on openai", "openai", &ottoflowv1alpha1.AgentThinking{Effort: "low"}),
	)

	DescribeTable("passes spec.thinking to the default delegate",
		func(provider string, thinking *ottoflowv1alpha1.AgentThinking) {
			agentCRD := &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: provider, Thinking: thinking},
			}
			_, _, err := router.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
			Expect(err).NotTo(HaveOccurred())
			Expect(defaultMock.GetCallCount()).To(Equal(1))
		},
		Entry("anthropic with effort", "anthropic", &ottoflowv1alpha1.AgentThinking{Effort: "low"}),
		Entry("an empty block on openai, which sends nothing", "openai", &ottoflowv1alpha1.AgentThinking{}),
	)

	It("constructs real delegates that both satisfy AgentExecutor", func() {
		real := NewRoutingAgentExecutor(nil)
		var _ AgentExecutor = real
		Expect(real).NotTo(BeNil())
	})

	DescribeTable("real router reports the Nirmata provider as enterprise-only",
		func(provider string) {
			real := NewRoutingAgentExecutor(nil)
			agentCRD := &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: provider},
			}
			_, _, err := real.ExecuteAgent(context.Background(), agentCRD, "prompt", nil, "default")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("enterprise plugin"))
		},
		Entry("nirmata", "nirmata"),
		Entry("empty", ""),
	)
})
