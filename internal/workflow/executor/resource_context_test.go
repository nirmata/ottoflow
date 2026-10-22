/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// pagedListClient serves total ConfigMaps in pages honouring Limit/Continue, counting List calls.
// Pages are sliced from one pre-built slice so a read-ahead producer outruns the consumer.
func pagedListClient(scheme *runtime.Scheme, total int, pages *atomic.Int64) client.Client {
	all := make([]unstructured.Unstructured, total)
	for i := range all {
		all[i].SetAPIVersion("v1")
		all[i].SetKind("ConfigMap")
		all[i].SetName(fmt.Sprintf("cm-%d", i))
		all[i].SetNamespace("ns")
	}
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			start, end := 0, total
			if lo.Continue != "" {
				start, _ = strconv.Atoi(lo.Continue)
			}
			if lo.Limit > 0 && start+int(lo.Limit) < total {
				end = start + int(lo.Limit)
			}
			pages.Add(1)
			ul := list.(*unstructured.UnstructuredList)
			ul.Items = all[start:end]
			if end < total {
				ul.SetContinue(strconv.Itoa(end))
			}
			return nil
		},
	})
}

var _ = Describe("resourceContext", func() {
	var (
		k8sClient client.Client
		scheme    *runtime.Scheme
		rc        *resourceContext
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	})

	It("GetResource returns a single resource by name", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		obj, err := rc.GetResource("v1", "pods", "default", "my-pod")
		Expect(err).NotTo(HaveOccurred())
		Expect(obj).NotTo(BeNil())
		Expect(obj.GetNamespace()).To(Equal("default"))
		Expect(obj.GetName()).To(Equal("my-pod"))
		phase, _, _ := unstructured.NestedString(obj.UnstructuredContent(), "status", "phase")
		Expect(phase).To(Equal("Running"))
	})

	It("GetResource uses default namespace when empty", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cm", Namespace: "default"},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		obj, err := rc.GetResource("v1", "configmaps", "", "my-cm")
		Expect(err).NotTo(HaveOccurred())
		Expect(obj.GetNamespace()).To(Equal("default"))
	})

	It("GetResource returns error when resource does not exist", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		_, err := rc.GetResource("v1", "pods", "default", "missing")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to get resource"))
	})

	It("GetResource returns error for invalid apiVersion", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		_, err := rc.GetResource("invalid/version/here", "pods", "default", "x")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid apiVersion"))
	})

	It("ListResources returns list with label selector", func() {
		pod1 := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "foo"}},
		}
		pod2 := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default", Labels: map[string]string{"app": "foo"}},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod1, pod2).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		list, err := rc.ListResources("v1", "pods", "default", map[string]string{"app": "foo"})
		Expect(err).NotTo(HaveOccurred())
		Expect(list).NotTo(BeNil())
		Expect(list.Items).To(HaveLen(2))
	})

	It("ListResources returns list in namespace", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		list, err := rc.ListResources("v1", "pods", "default", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).NotTo(BeNil())
		Expect(list.Items).To(BeEmpty())
	})

	It("PostResource returns not implemented", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		rc = &resourceContext{client: k8sClient, namespace: "default"}

		_, err := rc.PostResource("v1", "pods", "default", nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not implemented"))
	})

	It("GetResource returns an error, not a panic, when the client is nil", func() {
		rc = &resourceContext{client: nil, namespace: "default"}

		_, err := rc.GetResource("v1", "pods", "default", "my-pod")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("kubernetes client not available"))
	})

	It("ListResources returns an error, not a panic, when the client is nil", func() {
		rc = &resourceContext{client: nil, namespace: "default"}

		_, err := rc.ListResources("v1", "pods", "default", nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("kubernetes client not available"))
	})

	It("ToGVR parses apiVersion and kind", func() {
		rc = &resourceContext{client: nil, namespace: "default"}
		gvr, err := rc.ToGVR("apps/v1", "Deployment")
		Expect(err).NotTo(HaveOccurred())
		Expect(gvr.Group).To(Equal("apps"))
		Expect(gvr.Version).To(Equal("v1"))
		Expect(gvr.Resource).To(Equal("deployments"))
	})

	It("convertResourceNameToKind handles Kind and resource name", func() {
		Expect(convertResourceNameToKind("Deployment")).To(Equal("Deployment"))
		Expect(convertResourceNameToKind("deployments")).To(Equal("Deployment"))
		Expect(convertResourceNameToKind("pods")).To(Equal("Pod"))
		Expect(convertResourceNameToKind("configmaps")).To(Equal("ConfigMap"))
	})

	It("convertResourceNameToKind uses map for services and fallback for unknown plurals", func() {
		Expect(convertResourceNameToKind("services")).To(Equal("Service"))
		Expect(convertResourceNameToKind("jobs")).To(Equal("Job")) // generic trailing-s
		Expect(convertResourceNameToKind("job")).To(Equal("Job"))  // fallback capitalize
		Expect(convertResourceNameToKind("")).To(Equal(""))
		// generic "ingresses" -> strip 's' and capitalize -> "Ingresse" (no special-case for "ingress")
		Expect(convertResourceNameToKind("ingresses")).To(Equal("Ingresse"))
	})

	It("ListResources returns exactly listMaxItems items without error", func() {
		var pages atomic.Int64
		rc = &resourceContext{client: pagedListClient(scheme, listMaxItems, &pages), namespace: "ns"}

		list, err := rc.ListResources("v1", "configmaps", "ns", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(HaveLen(listMaxItems))
	})

	It("ListResources fails closed above listMaxItems and bounds the pages fetched", func() {
		var pages atomic.Int64
		total := listMaxItems + 11*listPageSize
		rc = &resourceContext{client: pagedListClient(scheme, total, &pages), namespace: "ns"}

		list, err := rc.ListResources("v1", "configmaps", "ns", nil)
		Expect(err).To(MatchError(ContainSubstring("more than 10000 items")))
		Expect(list).To(BeNil())
		Expect(pages.Load()).To(BeNumerically("<=", int(math.Ceil(float64(listMaxItems+1)/listPageSize))+1))
	})

	It("resource.List() fails the CEL expression above listMaxItems", func() {
		var pages atomic.Int64
		ev, err := NewCELEvaluator(pagedListClient(scheme, listMaxItems+1, &pages), nil)
		Expect(err).NotTo(HaveOccurred())

		_, err = ev.EvaluateExpression(context.Background(),
			`size(dyn(resource.List("v1", "configmaps", "ns", {})).items) > 0`, nil)
		Expect(err).To(MatchError(And(ContainSubstring("failed to list resource"), ContainSubstring("more than 10000 items"))))
	})
})
