/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package v1alpha1

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func int32Ptr(n int32) *int32 { return &n }
func int64Ptr(n int64) *int64 { return &n }

func TestWorkflowRunJobSpec_Validate(t *testing.T) {
	tests := []struct {
		name     string
		spec     *WorkflowRunJobSpec
		wantErr  bool
		contains []string
	}{
		{
			name:    "nil spec",
			spec:    nil,
			wantErr: false,
		},
		{
			name:    "valid empty spec",
			spec:    &WorkflowRunJobSpec{},
			wantErr: false,
		},
		{
			name: "valid with image and service account",
			spec: &WorkflowRunJobSpec{
				Image:              "myrunner:tag",
				ServiceAccountName: "my-sa",
			},
			wantErr: false,
		},
		{
			name: "invalid backoffLimit negative",
			spec: &WorkflowRunJobSpec{
				BackoffLimit: int32Ptr(-1),
			},
			wantErr:  true,
			contains: []string{"backoffLimit", ">= 0"},
		},
		{
			name: "invalid ttlSecondsAfterFinished negative",
			spec: &WorkflowRunJobSpec{
				TTLSecondsAfterFinished: int32Ptr(-10),
			},
			wantErr:  true,
			contains: []string{"ttlSecondsAfterFinished", ">= 0"},
		},
		{
			name: "invalid activeDeadlineSeconds negative",
			spec: &WorkflowRunJobSpec{
				ActiveDeadlineSeconds: int64Ptr(-5),
			},
			wantErr:  true,
			contains: []string{"activeDeadlineSeconds", ">= 0"},
		},
		{
			name: "invalid serviceAccountName",
			spec: &WorkflowRunJobSpec{
				ServiceAccountName: "invalid name!",
			},
			wantErr:  true,
			contains: []string{"serviceAccountName"},
		},
		{
			name: "volume with empty name",
			spec: &WorkflowRunJobSpec{
				Volumes: []corev1.Volume{{Name: ""}},
			},
			wantErr:  true,
			contains: []string{"volumes[0].name"},
		},
		{
			name: "duplicate volume names",
			spec: &WorkflowRunJobSpec{
				Volumes: []corev1.Volume{{Name: "v1"}, {Name: "v1"}},
			},
			wantErr:  true,
			contains: []string{"duplicate volume name"},
		},
		{
			name: "volumeMount without matching volume",
			spec: &WorkflowRunJobSpec{
				Volumes:      []corev1.Volume{{Name: "v1"}},
				VolumeMounts: []corev1.VolumeMount{{Name: "v2", MountPath: "/data"}},
			},
			wantErr:  true,
			contains: []string{"volumeMounts[0].name", "must refer to a volume"},
		},
		{
			name: "volumeMount with empty mountPath",
			spec: &WorkflowRunJobSpec{
				Volumes:      []corev1.Volume{{Name: "v1"}},
				VolumeMounts: []corev1.VolumeMount{{Name: "v1", MountPath: ""}},
			},
			wantErr:  true,
			contains: []string{"volumeMounts[0].mountPath"},
		},
		{
			name: "env with empty name",
			spec: &WorkflowRunJobSpec{
				Env: []corev1.EnvVar{{Name: "", Value: "x"}},
			},
			wantErr:  true,
			contains: []string{"env[0].name"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && len(tt.contains) > 0 {
				msg := err.Error()
				for _, sub := range tt.contains {
					if !strings.Contains(msg, sub) {
						t.Errorf("Validate() error = %q, want to contain %q", msg, sub)
					}
				}
			}
		})
	}
}

// TestWorkflowRunFailureReason_EnumMarkersMatchConstants guards against the field's and the
// type's +kubebuilder:validation:Enum markers drifting apart from each other or from the
// declared WorkflowRunFailureReason constants — controller-gen renders whichever marker it
// finds, so a stale one would silently narrow the generated CRD's accepted values without any
// compile-time signal. Reads the raw source rather than reflecting on the type, since Go struct
// tags carry no equivalent of the marker comment.
func TestWorkflowRunFailureReason_EnumMarkersMatchConstants(t *testing.T) {
	src, err := os.ReadFile("workflowrun_types.go")
	if err != nil {
		t.Fatalf("read workflowrun_types.go: %v", err)
	}
	text := string(src)

	fieldMarker := enumMarkerBefore(t, text, `FailureReason WorkflowRunFailureReason `+"`json:\"failureReason,omitempty\"`")
	typeMarker := enumMarkerBefore(t, text, "type WorkflowRunFailureReason string")

	want := strings.Join([]string{
		string(WorkflowRunFailureReasonSecretAccessDenied),
		string(WorkflowRunFailureReasonRunnerRefUnresolved),
	}, ";")

	if fieldMarker != want {
		t.Errorf("field +kubebuilder:validation:Enum=%s does not match the declared constants (%s)", fieldMarker, want)
	}
	if typeMarker != want {
		t.Errorf("type +kubebuilder:validation:Enum=%s does not match the declared constants (%s)", typeMarker, want)
	}
}

// enumMarkerBefore returns the value of the nearest "+kubebuilder:validation:Enum=" comment
// line preceding the first occurrence of anchor in src.
func enumMarkerBefore(t *testing.T, src, anchor string) string {
	t.Helper()
	idx := strings.Index(src, anchor)
	if idx < 0 {
		t.Fatalf("anchor %q not found in workflowrun_types.go", anchor)
	}
	const markerPrefix = "+kubebuilder:validation:Enum="
	before := src[:idx]
	pos := strings.LastIndex(before, markerPrefix)
	if pos < 0 {
		t.Fatalf("no %s marker found before anchor %q", markerPrefix, anchor)
	}
	value := before[pos+len(markerPrefix):]
	if nl := strings.IndexAny(value, "\r\n"); nl >= 0 {
		value = value[:nl]
	}
	return value
}
