/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	kyvernohttp "github.com/kyverno/sdk/extensions/cel/libs/http"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("jsonNormalizingHTTPClient", func() {
	It("wraps non-JSON 2xx response as {\"ok\":true,\"body\":\"...\"}", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))
		defer server.Close()

		c := &jsonNormalizingHTTPClient{client: http.DefaultClient}
		req, err := http.NewRequest("GET", server.URL, nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := c.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(Equal("application/json"))
		body, _ := io.ReadAll(resp.Body)
		var out map[string]interface{}
		Expect(json.Unmarshal(body, &out)).To(Succeed())
		Expect(out["ok"]).To(BeTrue())
		Expect(out["body"]).To(Equal("ok"))
	})

	It("passes through valid JSON 2xx unchanged", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"key":"value"}`))
		}))
		defer server.Close()

		c := &jsonNormalizingHTTPClient{client: http.DefaultClient}
		req, err := http.NewRequest("GET", server.URL, nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := c.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		body, _ := io.ReadAll(resp.Body)
		Expect(strings.TrimSpace(string(body))).To(Equal(`{"key":"value"}`))
	})

	It("passes through non-2xx response unchanged", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		}))
		defer server.Close()

		c := &jsonNormalizingHTTPClient{client: http.DefaultClient}
		req, err := http.NewRequest("GET", server.URL, nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := c.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		body, _ := io.ReadAll(resp.Body)
		Expect(string(body)).To(Equal("not found"))
	})
})

var _ = Describe("celHTTPContext", func() {
	It("returns {\"ok\":true,\"body\":\"ok\",\"statusCode\":200} for a plain-text ok response via Get and Post", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))
		defer server.Close()

		ctx := NewCELHTTPContext()

		getResult, err := ctx.Get(server.URL, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(getResult).To(Equal(map[string]any{"ok": true, "body": "ok", "statusCode": 200}))

		postResult, err := ctx.Post(server.URL, map[string]any{"x": 1}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(postResult).To(Equal(map[string]any{"ok": true, "body": "ok", "statusCode": 200}))
	})

	It("preserves JSON object keys and injects statusCode for a JSON 200 response", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"key":"value","count":3}`))
		}))
		defer server.Close()

		ctx := NewCELHTTPContext()

		result, err := ctx.Get(server.URL, nil)
		Expect(err).NotTo(HaveOccurred())
		resultMap, ok := result.(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(resultMap["key"]).To(Equal("value"))
		Expect(resultMap["count"]).To(Equal(float64(3)))
		Expect(resultMap["statusCode"]).To(Equal(200))
	})

	It("Client errors on an invalid PEM CA bundle", func() {
		ctx := NewCELHTTPContext()
		_, err := ctx.Client("not-a-pem")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to parse PEM CA bundle"))
	})

	It("Client wraps the same context for an empty CA bundle", func() {
		ctx := NewCELHTTPContext()
		derived, err := ctx.Client("")
		Expect(err).NotTo(HaveOccurred())
		Expect(derived).To(BeIdenticalTo(kyvernohttp.Context{ContextInterface: ctx}))
	})

	It("Client(validPEM) returns a context that still normalizes non-JSON bodies", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))
		defer server.Close()

		caPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: server.Certificate().Raw,
		})

		ctx := NewCELHTTPContext()
		derived, err := ctx.Client(string(caPEM))
		Expect(err).NotTo(HaveOccurred())

		result, err := derived.Get(server.URL, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(map[string]any{"ok": true, "body": "ok", "statusCode": 200}))
	})
})

var _ = Describe("CEL http.Client(caBundle) member calls", func() {
	var evaluator *CELEvaluator

	BeforeEach(func() {
		var err error
		evaluator, err = NewCELEvaluator(nil, nil)
		Expect(err).NotTo(HaveOccurred())
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"job-1"}`))
		}
	})

	It("Post with headers through a pinned client returns the JSON body", func() {
		server := httptest.NewTLSServer(handler)
		defer server.Close()
		ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
		vars := map[string]interface{}{"inputs": map[string]interface{}{"ca": ca, "url": server.URL}}

		res, err := evaluator.EvaluateExpression(context.Background(),
			`http.Client(inputs.ca).Post(inputs.url + "/scan", {"a": "b"}, {"Content-Type": "application/json"}).id`, vars)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal("job-1"))
	})

	It("an empty caBundle falls back to the default client and supports member calls", func() {
		server := httptest.NewServer(handler)
		defer server.Close()
		vars := map[string]interface{}{"inputs": map[string]interface{}{"url": server.URL}}

		res, err := evaluator.EvaluateExpression(context.Background(), `http.Client("").Get(inputs.url).statusCode`, vars)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(BeEquivalentTo(200))
	})
})
