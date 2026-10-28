package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func cronTestTrigger() ottoflowv1alpha1.Trigger {
	return ottoflowv1alpha1.Trigger{Cron: &ottoflowv1alpha1.CronTrigger{Schedule: "0 * * * *"}}
}

func cronTestSetup(t *testing.T, wfs ...*ottoflowv1alpha1.Workflow) (*Scheduler, *TriggerManager, *WorkflowReconciler) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	s := NewScheduler(c, logr.Discard())
	tm := NewTriggerManagerWithWatcherFactory(c, unitTestScheme, s, &fakeWatcherFactory{})
	for _, wf := range wfs {
		if err := tm.RegisterWorkflow(context.Background(), wf); err != nil {
			t.Fatal(err)
		}
	}
	return s, tm, &WorkflowReconciler{Client: c, Scheme: unitTestScheme, TriggerManager: tm}
}

func cronTestNotFound(t *testing.T, r *WorkflowReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func TestCronSchedule_NotFound_RemovesSchedule(t *testing.T) {
	s, _, r := cronTestSetup(t, eventTestWorkflow("wf", cronTestTrigger()))
	cronTestNotFound(t, r, "wf")
	if s.HasSchedule("default/wf-cron-0") {
		t.Fatalf("cron schedule left after NotFound: %v", s.entries)
	}
}

func TestCronSchedule_ShrunkTriggers_RemovesRemovedIndex(t *testing.T) {
	s, tm, _ := cronTestSetup(t, eventTestWorkflow("wf", cronTestTrigger(), cronTestTrigger()))
	if err := tm.RegisterWorkflow(context.Background(), eventTestWorkflow("wf", cronTestTrigger())); err != nil {
		t.Fatal(err)
	}
	if s.HasSchedule("default/wf-cron-1") || !s.HasSchedule("default/wf-cron-0") {
		t.Fatalf("want only cron-0 after shrink: %v", s.entries)
	}
}

func TestCronSchedule_ReplacedByEvent_RemovesSchedule(t *testing.T) {
	s, tm, _ := cronTestSetup(t, eventTestWorkflow("wf", cronTestTrigger()))
	if err := tm.RegisterWorkflow(context.Background(), eventTestWorkflow("wf", podEventTrigger())); err != nil {
		t.Fatal(err)
	}
	if s.HasSchedule("default/wf-cron-0") {
		t.Fatalf("cron-0 left after trigger 0 became event: %v", s.entries)
	}
}

func TestCronSchedule_NotFound_KeepsPrefixNeighbour(t *testing.T) {
	s, _, r := cronTestSetup(t, eventTestWorkflow("wf", cronTestTrigger()), eventTestWorkflow("wf-cron-0", cronTestTrigger()))
	cronTestNotFound(t, r, "wf")
	if s.HasSchedule("default/wf-cron-0") || !s.HasSchedule("default/wf-cron-0-cron-0") {
		t.Fatalf("want wf's own key gone and neighbour kept: %v", s.entries)
	}
}
