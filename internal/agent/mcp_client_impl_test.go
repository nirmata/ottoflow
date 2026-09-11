/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

var _ = Describe("parseToolResult", func() {
	It("returns empty string for empty input", func() {
		out, err := parseToolResult("")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(""))
	})

	It("returns original string when trimmed is empty", func() {
		out, err := parseToolResult("  \n\t  ")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("  \n\t  "))
	})

	It("parses JSON object", func() {
		out, err := parseToolResult(`{"a":1,"b":"x"}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(BeAssignableToTypeOf(map[string]interface{}(nil)))
		m := out.(map[string]interface{})
		Expect(m["a"]).To(BeEquivalentTo(1))
		Expect(m["b"]).To(Equal("x"))
	})

	It("parses JSON array", func() {
		out, err := parseToolResult(`[1,2,"three"]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(BeAssignableToTypeOf([]interface{}(nil)))
		Expect(out.([]interface{})).To(HaveLen(3))
	})

	It("parses number", func() {
		out, err := parseToolResult("42.5")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(BeEquivalentTo(42.5))
	})

	It("parses boolean", func() {
		out, err := parseToolResult("true")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(true))
	})

	It("returns raw string when not JSON or number or bool", func() {
		out, err := parseToolResult("hello world")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("hello world"))
	})
})

var _ = Describe("buildMCPClientConfig", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("returns error for stdio without command", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio"},
			},
		}
		_, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("command"))
	})

	It("builds stdio config with command and args", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"/bin/echo", "-n", "hi"},
				},
			},
		}
		cfg, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Name).To(Equal("s"))
		Expect(cfg.Command).To(Equal("/bin/echo"))
		Expect(cfg.Args).To(Equal([]string{"-n", "hi"}))
		Expect(cfg.Timeout).To(Equal(90))
	})

	It("builds stdio config with timeout", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
				Timeout: "30s",
			},
		}
		cfg, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Timeout).To(Equal(30))
	})

	It("returns error for http/sse without address", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http"},
			},
		}
		_, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("address"))
	})

	It("builds http config with address and headers", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "http",
					Address: "https://mcp.example.com",
					Headers: map[string]string{"X-Custom": "val"},
				},
			},
		}
		cfg, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.URL).To(Equal("https://mcp.example.com"))
		Expect(cfg.UseStreaming).To(BeFalse())
		Expect(cfg.Headers).To(Equal(map[string]string{"X-Custom": "val"}))
	})

	It("sets UseStreaming for sse transport", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "sse",
					Address: "https://mcp.example.com",
				},
			},
		}
		cfg, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.UseStreaming).To(BeTrue())
	})

	It("returns error for unsupported transport type", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "grpc"},
			},
		}
		_, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported"))
	})

	It("builds http config with auth from secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("bearer-token")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://mcp.example.com"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "token"},
				},
			},
		}
		cfg, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Auth).NotTo(BeNil())
		Expect(cfg.Auth.Token).To(Equal("bearer-token"))
	})

	It("builds stdio config with env from ValueFrom secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "env-secret", Namespace: "default"},
			Data:       map[string][]byte{"api_key": []byte("secret-key")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
				Env: []corev1.EnvVar{
					{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "env-secret"},
							Key:                  "api_key",
						},
					}},
				},
			},
		}
		cfg, err := buildMCPClientConfig(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Env).To(ContainElement("API_KEY=secret-key"))
	})
})

var _ = Describe("resolveEnvValue", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(corev1.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("returns Value when set", func() {
		ev := &corev1.EnvVar{Name: "FOO", Value: "bar"}
		val, present, err := resolveEnvValue(ctx, k8sClient, "default", ev, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeTrue())
		Expect(val).To(Equal("bar"))
	})

	It("returns secret value when ValueFrom.SecretKeyRef is set", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("secret-val")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		ev := &corev1.EnvVar{
			Name: "TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s1"}, Key: "token"},
			},
		}
		val, present, err := resolveEnvValue(ctx, k8sClient, "default", ev, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeTrue())
		Expect(val).To(Equal("secret-val"))
	})

	// A missing credential must propagate an error rather than silently resolving to "":
	// launching the stdio MCP server with an empty credential surfaces only later as an
	// opaque upstream 401.
	It("returns an error when the secret is not found", func() {
		ev := &corev1.EnvVar{
			Name: "TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "token"},
			},
		}
		val, present, err := resolveEnvValue(ctx, k8sClient, "default", ev, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("TOKEN"))
		Expect(val).To(Equal(""))
		// A NON-optional ref that cannot be resolved must stay an error, never a silent omission.
		Expect(present).To(BeFalse())
	})

	It("leaves an OPTIONAL env unset when the secret is not found", func() {
		optional := true
		ev := &corev1.EnvVar{
			Name: "TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "missing"},
					Key:                  "token",
					Optional:             &optional,
				},
			},
		}
		val, present, err := resolveEnvValue(ctx, k8sClient, "default", ev, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeFalse())
		Expect(val).To(Equal(""))
	})
})

var _ = Describe("resolveAuthConfigs", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("returns nil when auth is nil", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec:       ottoflowv1alpha1.MCPServerSpec{Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"}},
		}
		ac, oauth, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(ac).To(BeNil())
		Expect(oauth).To(BeNil())
	})

	It("returns error for bearer auth without secretRef", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth:      &ottoflowv1alpha1.AuthConfig{Type: "bearer"},
			},
		}
		_, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("secretRef"))
	})

	It("resolves bearer auth from secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("bearer-token")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "token"},
				},
			},
		}
		ac, oauth, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(oauth).To(BeNil())
		Expect(ac).NotTo(BeNil())
		Expect(ac.Type).To(Equal("bearer"))
		Expect(ac.Token).To(Equal("bearer-token"))
	})

	It("resolves apiKey auth and sets ApiKey", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"},
			Data:       map[string][]byte{"apikey": []byte("my-api-key")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "apiKey",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "apikey"},
				},
			},
		}
		ac, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(ac.Type).To(Equal("api-key"))
		Expect(ac.Token).To(Equal("my-api-key"))
		Expect(ac.ApiKey).To(Equal("my-api-key"))
	})

	It("returns error for basic auth without username/password in secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"},
			Data:       map[string][]byte{"username": []byte("u")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "basic",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "x"},
				},
			},
		}
		_, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		// The error names the specific failing key and carries the underlying cause.
		Expect(err.Error()).To(ContainSubstring("resolving password"))
		Expect(err.Error()).To(ContainSubstring(`key "password" not found`))
	})

	It("resolves basic auth from secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"},
			Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "basic",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "x"},
				},
			},
		}
		ac, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(ac.Username).To(Equal("u"))
		Expect(ac.Password).To(Equal("p"))
	})

	It("returns error for oauth2 without config", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth:      &ottoflowv1alpha1.AuthConfig{Type: "oauth2"},
			},
		}
		_, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("oauth2 config"))
	})

	It("resolves oauth2 with ClientCredentialsRef", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: "default"},
			Data:       map[string][]byte{"client_id": []byte("cid"), "client_secret": []byte("csec")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type: "oauth2",
					OAuth2: &ottoflowv1alpha1.OAuth2Config{
						TokenURL:             "https://auth.example.com/token",
						Scopes:               []string{"read"},
						ClientCredentialsRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "oauth", Namespace: "default"},
					},
				},
			},
		}
		ac, oauth, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(ac).NotTo(BeNil())
		Expect(oauth).NotTo(BeNil())
		Expect(oauth.TokenURL).To(Equal("https://auth.example.com/token"))
		Expect(oauth.Scopes).To(Equal([]string{"read"}))
		Expect(oauth.ClientID).To(Equal("cid"))
		Expect(oauth.ClientSecret).To(Equal("csec"))
	})

	It("resolves oauth2 with ClientID and ClientSecretRef", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oauth-secret", Namespace: "default"},
			Data:       map[string][]byte{"secret": []byte("my-client-secret")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type: "oauth2",
					OAuth2: &ottoflowv1alpha1.OAuth2Config{
						TokenURL: "https://auth.example.com/token",
						ClientID: "my-client-id",
						ClientSecretRef: &ottoflowv1alpha1.SecretReference{
							Name: "oauth-secret", Key: "secret",
						},
					},
				},
			},
		}
		_, oauth, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(oauth).NotTo(BeNil())
		Expect(oauth.ClientID).To(Equal("my-client-id"))
		Expect(oauth.ClientSecret).To(Equal("my-client-secret"))
	})

	It("returns error when auth secret key not found", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"},
			Data:       map[string][]byte{"other": []byte("x")},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "token"},
				},
			},
		}
		_, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found in secret"))
	})

	It("returns error when auth secret get fails", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "missing", Key: "token"},
				},
			},
		}
		_, _, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to get auth secret"))
	})
})

var _ = Describe("DefaultMCPClientFactory CreateClient", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("returns error for unsupported transport type", func() {
		f := NewDefaultMCPClientFactory(k8sClient, true)
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "grpc"},
			},
		}
		_, err := f.CreateClient(ctx, mcpServer)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported"))
	})
})

// noAPIClient returns a fake client whose Get fails the spec if it is ever invoked, so a
// spec can prove that the mounted-file path (useAPISecretAccess=false) never falls back to a
// live API read.
func noAPIClient(scheme *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			Fail("unexpected API Get for " + key.String() + ": useAPISecretAccess=false must not touch the Kubernetes API")
			return nil
		},
	}).Build()
}

// mountValues writes each value to its own file under a per-spec temp dir and points
// OTTOFLOW_SECRET_MOUNTS at them for the rest of the spec. Map keys are secretmount.Key(...).
func mountValues(values map[string]string) {
	dir := GinkgoT().TempDir()
	mounts := secretmount.Mounts{}
	i := 0
	for k, v := range values {
		path := filepath.Join(dir, fmt.Sprintf("mount-%d", i))
		i++
		Expect(os.WriteFile(path, []byte(v), 0o600)).To(Succeed())
		mounts[k] = path
	}
	raw, err := json.Marshal(mounts)
	Expect(err).NotTo(HaveOccurred())
	GinkgoT().Setenv(secretmount.EnvVar, string(raw))
}

// capturingBuilder records the mcp.ClientConfig the factory hands to Build.
type capturingBuilder struct {
	cfg mcp.ClientConfig
}

func (b *capturingBuilder) Build(_ context.Context, _ string, _ time.Duration, cfg interface{}) (MCPClient, error) {
	b.cfg = cfg.(mcp.ClientConfig)
	return &mockMCPClientForBuildSessionTools{}, nil
}

func secretEnv(name, secretName, key string, optional *bool) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             optional,
			},
		},
	}
}

var _ = Describe("mounted-file secret access (useAPISecretAccess=false)", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
		// secretmount.Load() memoizes OTTOFLOW_SECRET_MOUNTS for the process lifetime (it
		// never changes in the real runner process); each spec below sets it to a different
		// value, so reset the memoization before every case.
		secretmount.ResetForTest()
	})

	It("resolveEnvValue reads the mounted file and never calls the API", func() {
		mountValues(map[string]string{secretmount.Key("default", "s1", "token"): "mounted-value"})

		ev := secretEnv("TOKEN", "s1", "token", nil)
		val, present, err := resolveEnvValue(ctx, noAPIClient(scheme), "default", &ev, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeTrue())
		Expect(val).To(Equal("mounted-value"))
	})

	// An unmounted required credential must propagate an error without calling the API:
	// the runner has no live fallback, and an empty credential would only fail later inside
	// the MCP server.
	It("resolveEnvValue returns an error when the key is not mounted, without calling the API", func() {
		GinkgoT().Setenv(secretmount.EnvVar, "{}")
		ev := secretEnv("TOKEN", "s1", "token", nil)
		val, present, err := resolveEnvValue(ctx, noAPIClient(scheme), "default", &ev, false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not mounted"))
		Expect(val).To(Equal(""))
		Expect(present).To(BeFalse())
	})

	It("resolveEnvValue leaves an OPTIONAL env unset when its key is not mounted, without calling the API", func() {
		GinkgoT().Setenv(secretmount.EnvVar, "{}")
		optional := true
		ev := secretEnv("TOKEN", "s1", "token", &optional)
		val, present, err := resolveEnvValue(ctx, noAPIClient(scheme), "default", &ev, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeFalse())
		Expect(val).To(Equal(""))
	})

	// Optional means "unset when absent", not "swallow every failure": a broken mount map is
	// a broken runtime and must surface even for an optional ref.
	It("resolveEnvValue surfaces a malformed OTTOFLOW_SECRET_MOUNTS even for an OPTIONAL env", func() {
		GinkgoT().Setenv(secretmount.EnvVar, "{not json")
		optional := true
		ev := secretEnv("TOKEN", "s1", "token", &optional)
		_, _, err := resolveEnvValue(ctx, noAPIClient(scheme), "default", &ev, false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(secretmount.EnvVar))
	})

	It("buildMCPClientConfig delivers a mounted env credential and omits an unmounted optional one", func() {
		mountValues(map[string]string{secretmount.Key("default", "creds", "api_key"): "mounted-key"})
		optional := true
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
				Env: []corev1.EnvVar{
					{Name: "PLAIN", Value: "literal"},
					secretEnv("API_KEY", "creds", "api_key", nil),
					secretEnv("MAYBE", "creds", "absent", &optional),
				},
			},
		}
		cfg, err := buildMCPClientConfig(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Env).To(ConsistOf("PLAIN=literal", "API_KEY=mounted-key"))
	})

	It("buildMCPClientConfig fails the whole config when a required env credential is not mounted", func() {
		GinkgoT().Setenv(secretmount.EnvVar, "{}")
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
				Env:       []corev1.EnvVar{secretEnv("API_KEY", "creds", "api_key", nil)},
			},
		}
		_, err := buildMCPClientConfig(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`MCPServer default/s env "API_KEY"`))
		Expect(err.Error()).To(ContainSubstring("not mounted"))
	})

	It("resolveAuthConfigs resolves bearer auth from the mounted file and never calls the API", func() {
		mountValues(map[string]string{secretmount.Key("default", "auth", "token"): "mounted-bearer"})
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "token"},
				},
			},
		}
		ac, oauth, err := resolveAuthConfigs(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(oauth).To(BeNil())
		Expect(ac).NotTo(BeNil())
		Expect(ac.Token).To(Equal("mounted-bearer"))
	})

	It("resolveAuthConfigs errors when the auth secret is not mounted, without calling the API", func() {
		GinkgoT().Setenv(secretmount.EnvVar, "{}")
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type:      "bearer",
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "auth", Key: "token"},
				},
			},
		}
		_, _, err := resolveAuthConfigs(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to get auth secret"))
		Expect(err.Error()).To(ContainSubstring("not mounted"))
	})

	It("resolveAuthConfigs resolves basic auth from mounted username and password files", func() {
		mountValues(map[string]string{
			secretmount.Key("team-a", "basic", "username"): "u",
			secretmount.Key("team-a", "basic", "password"): "p",
		})
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type: "basic",
					// An explicit namespace on the ref must be honoured when looking up the mount.
					SecretRef: &ottoflowv1alpha1.SecretReference{Name: "basic", Namespace: "team-a", Key: "unused"},
				},
			},
		}
		ac, _, err := resolveAuthConfigs(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(ac.Username).To(Equal("u"))
		Expect(ac.Password).To(Equal("p"))
	})

	It("resolveAuthConfigs resolves oauth2 clientCredentialsRef from mounted client_id and client_secret files", func() {
		mountValues(map[string]string{
			secretmount.Key("default", "oauth", "client_id"):     "cid",
			secretmount.Key("default", "oauth", "client_secret"): "csec",
		})
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type: "oauth2",
					OAuth2: &ottoflowv1alpha1.OAuth2Config{
						TokenURL:             "https://auth.example.com/token",
						ClientCredentialsRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "oauth"},
					},
				},
			},
		}
		_, oauth, err := resolveAuthConfigs(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(oauth).NotTo(BeNil())
		Expect(oauth.ClientID).To(Equal("cid"))
		Expect(oauth.ClientSecret).To(Equal("csec"))
	})

	It("resolveAuthConfigs resolves oauth2 clientSecretRef from the mounted file", func() {
		mountValues(map[string]string{secretmount.Key("default", "oauth-secret", "secret"): "my-client-secret"})
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "https://x"},
				Auth: &ottoflowv1alpha1.AuthConfig{
					Type: "oauth2",
					OAuth2: &ottoflowv1alpha1.OAuth2Config{
						TokenURL:        "https://auth.example.com/token",
						ClientID:        "my-client-id",
						ClientSecretRef: &ottoflowv1alpha1.SecretReference{Name: "oauth-secret", Key: "secret"},
					},
				},
			},
		}
		_, oauth, err := resolveAuthConfigs(ctx, noAPIClient(scheme), mcpServer, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(oauth).NotTo(BeNil())
		Expect(oauth.ClientID).To(Equal("my-client-id"))
		Expect(oauth.ClientSecret).To(Equal("my-client-secret"))
	})

	// The factory is the seam the runner's MCP manager goes through, so the flag must reach
	// buildMCPClientConfig from there, not only when the helpers are called directly.
	It("a factory built with useAPISecretAccess=false hands the builder a config resolved from the mounts", func() {
		mountValues(map[string]string{secretmount.Key("default", "creds", "api_key"): "mounted-key"})
		builder := &capturingBuilder{}
		f := NewDefaultMCPClientFactoryWithBuilder(noAPIClient(scheme), builder, false)
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
				Env:       []corev1.EnvVar{secretEnv("API_KEY", "creds", "api_key", nil)},
			},
		}
		_, err := f.CreateClient(ctx, mcpServer)
		Expect(err).NotTo(HaveOccurred())
		Expect(builder.cfg.Env).To(ConsistOf("API_KEY=mounted-key"))
	})
})
