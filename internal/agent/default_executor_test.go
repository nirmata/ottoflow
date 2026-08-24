/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("DefaultAgentExecutor createLLMClient", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("returns error for unknown provider", func() {
		e := NewDefaultAgentExecutor(nil)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: "unknown-provider"},
		}
		_, err := e.createLLMClient(ctx, agentCRD)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown provider"))
	})

	It("returns error for nirmata and empty provider (routing handles those, not this executor)", func() {
		e := NewDefaultAgentExecutor(nil)
		for _, provider := range []string{"", "nirmata"} {
			agentCRD := &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: provider},
			}
			_, err := e.createLLMClient(ctx, agentCRD)
			Expect(err).To(HaveOccurred(), "provider %q", provider)
		}
	})

	It("uses LLMClientFactory when set and returns its error", func() {
		wantErr := errors.New("factory error")
		factory := &mockLLMClientFactory{err: wantErr}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: "openai"},
		}
		_, err := e.createLLMClient(ctx, agentCRD)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("factory error"))
		Expect(factory.called).To(BeTrue())
		Expect(factory.providerID).To(Equal("openai"))
	})

	It("uses LLMClientFactory when set and returns client", func() {
		mockClient := &mockGollmClient{}
		factory := &mockLLMClientFactory{client: mockClient}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: "openai"},
		}
		client, err := e.createLLMClient(ctx, agentCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).To(Equal(mockClient))
		Expect(factory.providerID).To(Equal("openai"))
	})

	It("applies custom endpoint from config when valid", func() {
		factory := &mockLLMClientFactory{client: &mockGollmClient{}}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec: ottoflowv1alpha1.AgentSpec{
				ModelProvider: "openai",
				Config:        map[string]string{"endpoint": "https://api.example.com/v1"},
			},
		}
		_, err := e.createLLMClient(ctx, agentCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(factory.optsCount).To(BeNumerically(">", 0))
	})

	It("applies skipVerifySSL when config has skipVerifySSL=true", func() {
		factory := &mockLLMClientFactory{client: &mockGollmClient{}}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec: ottoflowv1alpha1.AgentSpec{
				ModelProvider: "openai",
				Config:        map[string]string{"skipVerifySSL": "true"},
			},
		}
		_, err := e.createLLMClient(ctx, agentCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(factory.optsCount).To(BeNumerically(">", 0))
	})

	It("skips invalid endpoint URL and still creates client", func() {
		factory := &mockLLMClientFactory{client: &mockGollmClient{}}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec: ottoflowv1alpha1.AgentSpec{
				ModelProvider: "openai",
				Config:        map[string]string{"endpoint": "://invalid"},
			},
		}
		client, err := e.createLLMClient(ctx, agentCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())
	})
})

var _ = Describe("DefaultAgentExecutor ExecuteAgent", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("returns error when the chat stream fails", func() {
		streamErr := errors.New("stream error")
		mockClient := &mockGollmClientWithFailingStream{streamErr: streamErr}
		factory := &mockLLMClientFactory{client: mockClient}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: "openai", ModelName: "gpt-4o"},
		}
		_, _, err := e.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("agent execution failed"))
		Expect(err.Error()).To(ContainSubstring("stream error"))
	})

	It("returns success when the chat stream yields one final-answer response", func() {
		mockClient := &mockGollmClientWithStreamResponse{responseText: "agent said hello"}
		factory := &mockLLMClientFactory{client: mockClient}
		e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: "openai", ModelName: "gpt-4o"},
		}
		response, usage, err := e.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(response).To(Equal("agent said hello"))
		Expect(usage).To(Equal(AgentTokenUsage{}))
	})

	It("returns success with MCP tools registered when provider supplies client", func() {
		mockClient := &mockGollmClientWithStreamResponse{responseText: "done"}
		factory := &mockLLMClientFactory{client: mockClient}
		mcpProvider := &mockMCPProviderForBuildSessionTools{
			client: &mockMCPClientForBuildSessionTools{
				tools: []MCPToolMeta{
					{Name: "tool1", Description: "A tool", InputSchema: nil},
				},
			},
		}
		e := NewDefaultAgentExecutorWithLLMFactory(mcpProvider, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec: ottoflowv1alpha1.AgentSpec{
				ModelProvider: "openai",
				ModelName:     "gpt-4o",
				MCPTools:      []string{"srv:tool1"},
			},
		}
		response, _, err := e.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(response).To(Equal("done"))
	})

	It("returns error when building MCP session tools fails", func() {
		mockClient := &mockGollmClient{}
		factory := &mockLLMClientFactory{client: mockClient}
		mcpProvider := &mockMCPProviderForBuildSessionTools{err: errors.New("get client failed")}
		e := NewDefaultAgentExecutorWithLLMFactory(mcpProvider, factory)
		agentCRD := &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec: ottoflowv1alpha1.AgentSpec{
				ModelProvider: "openai",
				ModelName:     "gpt-4o",
				MCPTools:      []string{"srv:tool1"},
			},
		}
		_, _, err := e.ExecuteAgent(ctx, agentCRD, "prompt", nil, "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("building MCP session tools"))
	})
})

var _ = Describe("DefaultAgentExecutor mapProviderToID", func() {
	It("maps known providers to real, registered gollm provider IDs", func() {
		e := &DefaultAgentExecutor{}
		Expect(e.mapProviderToID("openai")).To(Equal("openai"))
		Expect(e.mapProviderToID("anthropic")).To(Equal("anthropic"))
		Expect(e.mapProviderToID("azure-openai")).To(Equal("azopenai"))
		Expect(e.mapProviderToID("google")).To(Equal("gemini"))
		Expect(e.mapProviderToID("gemini")).To(Equal("gemini"))
		Expect(e.mapProviderToID("local")).To(Equal("llamacpp"))
	})

	It("returns empty string for nirmata and empty provider (handled by routing, not this executor)", func() {
		e := &DefaultAgentExecutor{}
		Expect(e.mapProviderToID("nirmata")).To(Equal(""))
		Expect(e.mapProviderToID("")).To(Equal(""))
	})

	It("returns empty string for unknown provider", func() {
		e := &DefaultAgentExecutor{}
		Expect(e.mapProviderToID("unknown")).To(Equal(""))
	})
})

var _ = Describe("DefaultAgentExecutor getDefaultModel", func() {
	It("returns a generic default model for each known provider", func() {
		e := &DefaultAgentExecutor{}
		Expect(e.getDefaultModel("openai")).To(Equal("gpt-4o"))
		Expect(e.getDefaultModel("anthropic")).To(Equal("claude-opus-5"))
		Expect(e.getDefaultModel("azure-openai")).To(Equal("gpt-4o"))
		Expect(e.getDefaultModel("google")).To(Equal("gemini-2.5-flash"))
		Expect(e.getDefaultModel("gemini")).To(Equal("gemini-2.5-flash"))
	})

	It("returns empty string for local/unknown/nirmata rather than guessing", func() {
		e := &DefaultAgentExecutor{}
		Expect(e.getDefaultModel("local")).To(Equal(""))
		Expect(e.getDefaultModel("unknown")).To(Equal(""))
		Expect(e.getDefaultModel("nirmata")).To(Equal(""))
	})
})
