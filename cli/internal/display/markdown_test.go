/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package display

import (
	"io"
	"strings"
	"testing"
)

func TestLooksLikeMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"shorter than three after trim", "  ** ", false},
		{"plain text", "just some words", false},
		{"h1", "# Title", true},
		{"h2", "text\n## Section", true},
		{"bold", "this is **bold**", true},
		{"dash list", "intro\n- item", true},
		{"star list", "intro\n* item", true},
		{"dash without newline", "a - b", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeMarkdown(tt.in); got != tt.want {
				t.Fatalf("LooksLikeMarkdown(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatOutputValue(t *testing.T) {
	tests := []struct {
		name     string
		outName  string
		value    interface{}
		wantWord []string
		wantExac string
	}{
		{name: "non-string", value: 42},
		{name: "nil", value: nil},
		{name: "plain string is trimmed", value: "  plain words \n", wantExac: "plain words"},
		{name: "markdown by content", value: "# Heading\n\nsome body text", wantWord: []string{"Heading", "body", "text"}},
		{name: "markdown by output name", outName: "report", value: "plain report words", wantWord: []string{"plain", "report", "words"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatOutputValue(tt.outName, tt.value, io.Discard)
			if tt.wantWord == nil && got != tt.wantExac {
				t.Fatalf("FormatOutputValue() = %q, want %q", got, tt.wantExac)
			}
			if tt.wantWord != nil && got == strings.TrimSpace(tt.value.(string)) {
				t.Fatalf("FormatOutputValue() = %q, want rendered output, not the trimmed input", got)
			}
			for _, w := range tt.wantWord {
				if !strings.Contains(got, w) {
					t.Fatalf("FormatOutputValue() = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}
