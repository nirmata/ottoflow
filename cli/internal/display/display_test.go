/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package display

import "testing"

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
