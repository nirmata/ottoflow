package executor

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("openReport probe with split control/target clients", func() {
	It("creates the Report when the CRD exists on the target cluster only", func() {
		ctx := context.Background()
		s := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
		utilruntime.Must(clientgoscheme.AddToScheme(s))
		// control = manifest-backed fake (what cli/internal/executor/local_executor.go builds)
		control := fake.NewClientBuilder().WithScheme(s).Build()
		// target = the real cluster the run points at, with OpenReports installed
		target := fake.NewClientBuilder().WithScheme(s).Build()
		crd := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]interface{}{"name": openReportsCRDName}}}
		Expect(target.Create(ctx, crd)).To(Succeed())

		wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
		exec, err := NewWorkflowExecutorWithClientsAndAgentExecutor(control, target, nil, nil, nil, wr, nil, nil, true, 0, 5, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(exec.contextManager.InitializeContext(ctx, &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default"}}, nil)).To(Succeed())

		out, err := exec.executeOpenReportStep(ctx, wr, ottoflowv1alpha1.Step{Name: "publishReport", OpenReport: &ottoflowv1alpha1.StepOpenReport{
			ReportName: "rep", ResultsExpression: `[{"policy":"p","result":"pass"}]`}})
		Expect(err).NotTo(HaveOccurred())
		Expect(out["reportResult"].(map[string]interface{})["mode"]).To(Equal("crd"))
	})
})
