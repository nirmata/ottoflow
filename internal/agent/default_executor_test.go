/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
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

	Describe("spec.config.endpoint / skipVerifySSL: operator allowlist gate", func() {
		allProviders := []string{"anthropic", "azure-openai", "openai", "google", "gemini", "local"}

		newAgent := func(provider string, config map[string]string) *ottoflowv1alpha1.Agent {
			return &ottoflowv1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "tenant"},
				Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: provider, Config: config},
			}
		}

		// factory.called == false proves no client was constructed, so no credential was
		// attached anywhere: NewClient sets called before it touches opts.
		assertRefused := func(providers []string, config map[string]string, wantSubstring string) {
			for _, provider := range providers {
				factory := &mockLLMClientFactory{client: &mockGollmClient{}}
				e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
				_, err := e.createLLMClient(ctx, newAgent(provider, config))
				Expect(err).To(HaveOccurred(), provider)
				Expect(err.Error()).To(ContainSubstring(wantSubstring), provider)
				Expect(factory.called).To(BeFalse(), provider)
			}
		}

		admittedURL := func(provider, endpoint string) string {
			factory := &mockLLMClientFactory{client: &mockGollmClient{}}
			e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
			_, err := e.createLLMClient(ctx, newAgent(provider, map[string]string{"endpoint": endpoint}))
			Expect(err).NotTo(HaveOccurred())
			Expect(factory.called).To(BeTrue())
			Expect(factory.opts).To(HaveLen(1))
			co := &gollm.ClientOptions{}
			factory.opts[0](co)
			Expect(co.URL).NotTo(BeNil())
			return co.URL.String()
		}

		BeforeEach(func() {
			// Isolate from the shell: Setenv restores the original value after each spec.
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "")
		})

		It("default closed: an unset or empty-ish allowlist refuses every provider", func() {
			for _, envVal := range []string{"", " ", ",", " , ", ",,", "\t\n"} {
				GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, envVal)
				assertRefused(allProviders, map[string]string{"endpoint": "https://attacker.example.com"},
					"allowlisted no LLM endpoints")
			}
		})

		It("admits an allowlisted origin and passes it to the client unchanged", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
			Expect(admittedURL("azure-openai", "https://llm.corp.example")).To(Equal("https://llm.corp.example"))
		})

		It("normalizes an uppercase host, trailing dot and default port on the tenant endpoint", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
			admittedURL("azure-openai", "https://LLM.Corp.Example.:443")
		})

		It("accepts an allowlist entry whose path is exactly \"/\"", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example/")
			admittedURL("azure-openai", "https://llm.corp.example")
		})

		DescribeTable("refuses an endpoint whose origin differs from the allowlisted one",
			func(endpoint string) {
				GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
				assertRefused(allProviders, map[string]string{"endpoint": endpoint}, "not in the operator's allowlist")
			},
			Entry("host suffix", "https://llm.corp.example.evil.example"),
			Entry("scheme", "http://llm.corp.example"),
			Entry("port", "https://llm.corp.example:8443"),
			Entry("userinfo swaps the effective host", "https://llm.corp.example@evil.example"),
		)

		It("refuses userinfo on an allowlisted host", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
			assertRefused(allProviders, map[string]string{"endpoint": "https://evil.example@llm.corp.example"},
				"userinfo is not permitted")
		})

		It("refuses a path, query or fragment on an allowlisted origin", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
			for _, ep := range []string{"https://llm.corp.example/v1", "https://llm.corp.example?x=1", "https://llm.corp.example#f"} {
				assertRefused(allProviders, map[string]string{"endpoint": ep}, "path/query/fragment are not permitted")
			}
		})

		It("refuses a non-origin allowlist entry at startup and on every run", func() {
			for _, entry := range []string{
				"https://llm.corp.example/teams/platform",
				"https://llm.corp.example?x=1",
				"https://llm.corp.example#f",
				"https://u:p@llm.corp.example",
			} {
				GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, entry)
				err := ValidateLLMEndpointAllowlist()
				Expect(err).To(HaveOccurred(), entry)
				Expect(err.Error()).To(ContainSubstring("origin only"), entry)
				assertRefused(allProviders, map[string]string{"endpoint": "https://llm.corp.example"}, "origin only")
			}
		})

		It("refuses an allowlist entry with no scheme, naming the variable", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "llm.corp.example")
			Expect(ValidateLLMEndpointAllowlist()).To(MatchError(ContainSubstring("must include scheme and host")))
			assertRefused(allProviders, map[string]string{"endpoint": "https://llm.corp.example"}, AgentLLMEndpointAllowlistEnv)
		})

		It("refuses a non-listed origin for every allowlist spelling that lists something", func() {
			for _, envVal := range []string{
				"https://llm.corp.example",
				"https://llm.corp.example,,",
				",https://llm.corp.example",
				" https://llm.corp.example , , ",
			} {
				GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, envVal)
				assertRefused(allProviders, map[string]string{"endpoint": "https://definitely-not-listed.invalid"},
					"not in the operator's allowlist")
			}
		})

		It("refuses a non-empty but unparseable endpoint rather than ignoring it", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
			assertRefused(allProviders, map[string]string{"endpoint": "://invalid"}, "endpoint rejected")
		})

		It("refuses an allowlisted http endpoint for azure-openai, which rewrites the scheme to https", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "http://gateway.example,http://gateway.example:80")
			for _, ep := range []string{"http://gateway.example", "http://gateway.example:80"} {
				assertRefused([]string{"azure-openai"}, map[string]string{"endpoint": ep}, "rewrites the endpoint scheme to https")
			}
		})

		It("refuses skipVerifySSL=true for every provider, with or without an allowlisted endpoint", func() {
			GinkgoT().Setenv(AgentLLMEndpointAllowlistEnv, "https://llm.corp.example")
			assertRefused(allProviders, map[string]string{"skipVerifySSL": "true"}, "no longer honored")
			assertRefused(allProviders, map[string]string{
				"endpoint":      "https://llm.corp.example",
				"skipVerifySSL": "true",
			}, "no longer honored")
		})

		It("keeps skipVerifySSL=\"false\" legal", func() {
			factory := &mockLLMClientFactory{client: &mockGollmClient{}}
			e := NewDefaultAgentExecutorWithLLMFactory(nil, factory)
			_, err := e.createLLMClient(ctx, newAgent("openai", map[string]string{"skipVerifySSL": "false"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(factory.opts).To(BeEmpty())
		})
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
		Expect(e.getDefaultModel("google")).To(Equal("gemini-3.6-flash"))
		Expect(e.getDefaultModel("gemini")).To(Equal("gemini-3.6-flash"))
	})

	It("returns empty string for local/unknown/nirmata rather than guessing", func() {
		e := &DefaultAgentExecutor{}
		Expect(e.getDefaultModel("local")).To(Equal(""))
		Expect(e.getDefaultModel("unknown")).To(Equal(""))
		Expect(e.getDefaultModel("nirmata")).To(Equal(""))
	})
})
