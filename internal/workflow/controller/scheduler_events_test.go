/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	cronEventSecretName  = "cron-cred"
	cronEventSecretKey   = "token"
	cronEventSecretValue = "s3cr3t-value"
)

// fireCron fires one cron entry for wf on a Scheduler with a fake recorder and returns the
// recorded events.
func fireCron(t *testing.T, c client.Client, wf *ottoflowv1alpha1.Workflow, cronSpec *ottoflowv1alpha1.CronTrigger) []string {
	t.Helper()
	rec := events.NewFakeRecorder(10)
	s := &Scheduler{
		client:    c,
		logger:    logr.Discard(),
		ctx:       context.Background(),
		workflows: map[string]*ottoflowv1alpha1.Workflow{"key": wf},
		cronSpecs: map[string]*ottoflowv1alpha1.CronTrigger{"key": cronSpec},
		Recorder:  rec,
	}
	s.handleCronFire("key")
	var out []string
	for len(rec.Events) > 0 {
		out = append(out, <-rec.Events)
	}
	return out
}

func cronEventWorkflow() *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: defaultNamespace},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
		},
	}
}

func cronWithSecretInput() *ottoflowv1alpha1.CronTrigger {
	return &ottoflowv1alpha1.CronTrigger{
		Schedule: "*/5 * * * *",
		InputValuesFrom: []ottoflowv1alpha1.CronInputFromSecret{{
			InputName: "token",
			SecretRef: ottoflowv1alpha1.CronSecretKeyRef{Name: cronEventSecretName, Key: cronEventSecretKey},
		}},
	}
}

func failSecretGet(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// A cron fire whose createWorkflowRun fails records one Warning on the Workflow, with a
// reason per failure class, so a failure in one class is never hidden under another class's reason.
func TestHandleCronFire_RecordsWarningPerFailureClass(t *testing.T) {
	secretsGR := schema.GroupResource{Resource: "secrets"}
	otherKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cronEventSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{"other": []byte(cronEventSecretValue)},
	}
	cases := []struct {
		name       string
		objects    []client.Object
		funcs      interceptor.Funcs
		cronSpec   *ottoflowv1alpha1.CronTrigger
		wantReason string
		wantInNote string
	}{
		{
			name:       "secret read forbidden",
			funcs:      failSecretGet(apierrors.NewForbidden(secretsGR, cronEventSecretName, errors.New("RBAC denied"))),
			cronSpec:   cronWithSecretInput(),
			wantReason: "CronInputSecretForbidden",
			wantInNote: "forbidden",
		},
		{
			name:       "secret not found",
			cronSpec:   cronWithSecretInput(),
			wantReason: "CronInputSecretNotFound",
			wantInNote: "not found",
		},
		{
			name:       "secret key missing",
			objects:    []client.Object{otherKeySecret},
			cronSpec:   cronWithSecretInput(),
			wantReason: "CronInputSecretKeyMissing",
			wantInNote: `key "token" not found`,
		},
		{
			name:       "secret read fails otherwise",
			funcs:      failSecretGet(apierrors.NewTimeoutError("read timed out", 1)),
			cronSpec:   cronWithSecretInput(),
			wantReason: "CronInputSecretInvalid",
			wantInNote: "read timed out",
		},
		{
			// A Forbidden on the WorkflowRun create is a create failure, not a Secret failure.
			name: "workflowrun create rejected",
			funcs: interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*ottoflowv1alpha1.WorkflowRun); ok {
						return apierrors.NewForbidden(schema.GroupResource{Group: "ottoflow.nirmata.io", Resource: "workflowruns"}, obj.GetName(), errors.New("RBAC denied"))
					}
					return c.Create(ctx, obj, opts...)
				},
			},
			cronSpec:   &ottoflowv1alpha1.CronTrigger{Schedule: "*/5 * * * *"},
			wantReason: "CronWorkflowRunCreateFailed",
			wantInNote: "workflowruns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := cronEventWorkflow()
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).
				WithObjects(append(tc.objects, wf)...).
				WithInterceptorFuncs(tc.funcs).Build()

			evs := fireCron(t, c, wf, tc.cronSpec)

			if len(evs) != 1 {
				t.Fatalf("expected exactly one event, got %d: %v", len(evs), evs)
			}
			if want := "Warning " + tc.wantReason + " "; !strings.HasPrefix(evs[0], want) {
				t.Errorf("event = %q, want prefix %q", evs[0], want)
			}
			if !strings.Contains(evs[0], tc.wantInNote) {
				t.Errorf("event = %q, want the error text containing %q", evs[0], tc.wantInNote)
			}
			if strings.Contains(evs[0], cronEventSecretValue) {
				t.Errorf("event must carry names only, got %q", evs[0])
			}
		})
	}
}

// A cron fire that creates its WorkflowRun records no Warning.
func TestHandleCronFire_SuccessRecordsNoWarning(t *testing.T) {
	wf := cronEventWorkflow()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cronEventSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{cronEventSecretKey: []byte(cronEventSecretValue)},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).
		WithObjects(wf, secret).Build()

	evs := fireCron(t, c, wf, cronWithSecretInput())

	if len(evs) != 0 {
		t.Errorf("expected no events on a successful fire, got %v", evs)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	if err := c.List(context.Background(), &list, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected one WorkflowRun, got %d", len(list.Items))
	}
}

// The events.k8s.io note limit is 1024 bytes, the recorder does not truncate, and the API server
// rejects a longer note, so the note is cut to 1024 bytes on a UTF-8 boundary before recording.
func TestHandleCronFire_TruncatesNoteTo1024BytesUTF8(t *testing.T) {
	t.Run("cut lands inside a rune", func(t *testing.T) {
		in := "a" + strings.Repeat("é", 700) // byte 1024 is the second byte of an "é"
		got := truncateEventNote(in)
		if len(got) > 1024 || !utf8.ValidString(got) || !strings.HasPrefix(in, got) {
			t.Fatalf("truncateEventNote: len=%d valid=%v prefix=%v", len(got), utf8.ValidString(got), strings.HasPrefix(in, got))
		}
		if len(got) != 1023 {
			t.Errorf("len = %d, want 1023 (the longest valid prefix)", len(got))
		}
	})
	t.Run("short note unchanged", func(t *testing.T) {
		if got := truncateEventNote("short"); got != "short" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("recorded note from a cron fire", func(t *testing.T) {
		wf := cronEventWorkflow()
		long := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, cronEventSecretName, errors.New(strings.Repeat("é", 700)))
		c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).
			WithInterceptorFuncs(failSecretGet(long)).Build()

		evs := fireCron(t, c, wf, cronWithSecretInput())

		if len(evs) != 1 {
			t.Fatalf("expected exactly one event, got %v", evs)
		}
		note := strings.TrimPrefix(evs[0], "Warning CronInputSecretForbidden ")
		if len(note) > 1024 || !utf8.ValidString(note) {
			t.Errorf("recorded note len=%d valid UTF-8=%v, want <= 1024 and valid", len(note), utf8.ValidString(note))
		}
	})
}
