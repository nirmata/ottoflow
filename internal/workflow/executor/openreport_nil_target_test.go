package executor

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("openReport with nil target client", func() {
	It("falls back to data mode without panicking", func() {
		ctx := context.Background()
		s := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
		utilruntime.Must(clientgoscheme.AddToScheme(s))
		control := fake.NewClientBuilder().WithScheme(s).Build()
		wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
		exec, err := NewWorkflowExecutorWithClientsAndAgentExecutor(control, nil, nil, nil, nil, wr, nil, nil, true, 0, 5, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(exec.contextManager.InitializeContext(ctx, &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default"}}, nil)).To(Succeed())
		out, err := exec.executeOpenReportStep(ctx, wr, ottoflowv1alpha1.Step{Name: "s", OpenReport: &ottoflowv1alpha1.StepOpenReport{
			ReportName: "rep", ResultsExpression: `[{"policy":"p","result":"pass"}]`}})
		Expect(err).NotTo(HaveOccurred())
		Expect(out["reportResult"].(map[string]interface{})["mode"]).To(Equal("data"))
	})
})
