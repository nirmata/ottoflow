package executor

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("openReport CRD probe", func() {
	var (
		ctx context.Context
		s   *runtime.Scheme
		wr  *ottoflowv1alpha1.WorkflowRun
	)
	BeforeEach(func() {
		ctx = context.Background()
		s = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
		utilruntime.Must(clientgoscheme.AddToScheme(s))
		wr = &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
	})
	withCRD := func() client.Client {
		c := fake.NewClientBuilder().WithScheme(s).Build()
		crd := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]interface{}{"name": openReportsCRDName}}}
		Expect(c.Create(ctx, crd)).To(Succeed())
		return c
	}
	empty := func() client.Client { return fake.NewClientBuilder().WithScheme(s).Build() }
	crdGetErr := func(err error) client.Client {
		return interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).Build(), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == openReportsCRDGVK {
					return err
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}
	newExec := func(control, target client.Client) *WorkflowExecutor {
		exec, err := NewWorkflowExecutorWithClientsAndAgentExecutor(control, target, nil, nil, nil, wr, nil, nil, true, 0, 5, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(exec.contextManager.InitializeContext(ctx, &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default"}}, nil)).To(Succeed())
		return exec
	}
	step := ottoflowv1alpha1.Step{Name: "publishReport", OpenReport: &ottoflowv1alpha1.StepOpenReport{
		ReportName: "rep", ResultsExpression: `[{"policy":"p","result":"pass"}]`}}
	mode := func(out map[string]interface{}) string {
		return out["reportResult"].(map[string]interface{})["mode"].(string)
	}

	It("1 split clients: CRD on target only -> crd mode, Report written to target", func() {
		target := withCRD()
		exec := newExec(empty(), target)
		out, err := exec.executeOpenReportStep(ctx, wr, step)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode(out)).To(Equal("crd"))
		rep := &unstructured.Unstructured{}
		rep.SetGroupVersionKind(openReportsGVK)
		Expect(target.Get(ctx, client.ObjectKey{Name: "rep", Namespace: "default"}, rep)).To(Succeed())
	})
	It("2 CRD on control only (old probe would say yes) -> data mode, no failed Create", func() {
		exec := newExec(withCRD(), empty())
		out, err := exec.executeOpenReportStep(ctx, wr, step)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode(out)).To(Equal("data"))
		Expect(exec.openReportsCRDReason).To(ContainSubstring("not installed"))
	})
	It("3 nil target -> data mode, cannot-determine reason, no panic", func() {
		exec := newExec(empty(), nil)
		out, err := exec.executeOpenReportStep(ctx, wr, step)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode(out)).To(Equal("data"))
		Expect(exec.openReportsCRDReason).To(ContainSubstring("Cannot determine"))
	})
	It("4 Forbidden on target CRD get -> data mode, not-permitted reason", func() {
		forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, openReportsCRDName, errors.New("rbac denied"))
		exec := newExec(empty(), crdGetErr(forbidden))
		out, err := exec.executeOpenReportStep(ctx, wr, step)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode(out)).To(Equal("data"))
		Expect(exec.openReportsCRDReason).To(ContainSubstring("not permitted"))
		Expect(exec.openReportsCRDReason).To(ContainSubstring("get customresourcedefinitions"))
		Expect(exec.openReportsCRDReason).To(ContainSubstring("reports.openreports.io"))
		Expect(exec.openReportsCRDReason).To(ContainSubstring("Report write was not attempted"))
	})
	It("5 other error on target CRD get -> step fails", func() {
		exec := newExec(empty(), crdGetErr(apierrors.NewInternalError(errors.New("boom"))))
		_, err := exec.executeOpenReportStep(ctx, wr, step)
		Expect(err).To(MatchError(ContainSubstring("failed to check CRD availability")))
	})
	It("6 two executors in one process with different targets do not share the cached answer", func() {
		a := newExec(empty(), withCRD())
		b := newExec(empty(), empty())
		outA, err := a.executeOpenReportStep(ctx, wr, step)
		Expect(err).NotTo(HaveOccurred())
		outB, err := b.executeOpenReportStep(ctx, wr, step)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode(outA)).To(Equal("crd"))
		Expect(mode(outB)).To(Equal("data"))
	})
	It("7 Forbidden/nil answer is cached for the executor (second step does not re-probe)", func() {
		calls := 0
		c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).Build(), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == openReportsCRDGVK {
					calls++
					return apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("x"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		exec := newExec(empty(), c)
		for i := 0; i < 3; i++ {
			_, err := exec.executeOpenReportStep(ctx, wr, step)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(calls).To(Equal(1))
	})
})
