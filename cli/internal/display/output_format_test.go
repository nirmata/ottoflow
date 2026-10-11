/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package display

import "testing"

func TestValidateOutputFormat(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		if err := ValidateOutputFormat(format); err != nil {
			t.Errorf("ValidateOutputFormat(%q) = %v, want nil", format, err)
		}
	}

	want := `invalid --output "xml": must be one of table, json, yaml`
	if err := ValidateOutputFormat("xml"); err == nil {
		t.Fatal(`ValidateOutputFormat("xml") = nil, want error`)
	} else if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}
