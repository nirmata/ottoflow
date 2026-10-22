/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package display

import (
	"testing"
	"time"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestValidateOutputFormat(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		wantErr bool
	}{
		{name: "table is valid", format: "table"},
		{name: "json is valid", format: "json"},
		{name: "yaml is valid", format: "yaml"},
		{name: "empty is invalid", format: "", wantErr: true},
		{name: "unknown value is invalid", format: "xml", wantErr: true},
		{name: "case-sensitive mismatch is invalid", format: "JSON", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOutputFormat(tt.format)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateOutputFormat(%q) = nil, want error", tt.format)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateOutputFormat(%q) = %v, want nil", tt.format, err)
			}
		})
	}
}

func TestGetPhaseIcon(t *testing.T) {
	tests := []struct {
		phase interface{}
		want  string
	}{
		{"Succeeded", "✅"},
		{"Failed", "❌"},
		{"Running", "🔄"},
		{"Pending", "⏳"},
		{"Skipped", "⏭️"},
		{"Unknown", "  "},
		{"", "  "},
		{nil, "  "},
		{ottoflowv1alpha1.StepPhaseSucceeded, "✅"},
	}

	for _, tt := range tests {
		if got := getPhaseIcon(tt.phase); got != tt.want {
			t.Errorf("getPhaseIcon(%v) = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Nanosecond, "500ns"},
		{time.Microsecond, "1µs"},
		{1500 * time.Nanosecond, "1µs"},
		{time.Millisecond, "1ms"},
		{250 * time.Millisecond, "250ms"},
		{time.Second, "1.00s"},
		{1500 * time.Millisecond, "1.50s"},
		{time.Minute, "1.00m"},
		{90 * time.Second, "1.50m"},
		{time.Hour, "1.00h"},
		{150 * time.Minute, "2.50h"},
	}

	for _, tt := range tests {
		if got := formatDuration(tt.d); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
