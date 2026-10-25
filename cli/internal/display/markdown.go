/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package display

import (
	"io"
	"strings"

	"github.com/charmbracelet/glamour"
)

// MarkdownOutputNames contains output names that are typically formatted reports.
var MarkdownOutputNames = map[string]bool{
	"report":           true,
	"formattedReport":  true,
	"formatted_report": true,
}

// LooksLikeMarkdown returns true if the string appears to contain markdown.
func LooksLikeMarkdown(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return false
	}
	// Check for common markdown patterns
	return strings.Contains(s, "# ") ||
		strings.Contains(s, "## ") ||
		strings.Contains(s, "**") ||
		strings.Contains(s, "\n- ") ||
		strings.Contains(s, "\n* ")
}

// RenderMarkdown renders markdown to terminal-friendly formatted output.
// Returns the rendered string, or the original if rendering fails.
func RenderMarkdown(md string, out io.Writer) (string, error) {
	// WithAutoStyle picks dark or light from os.Stdout's background (plain text if stdout is not a TTY)
	tr, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)
	if err != nil {
		return md, err
	}
	rendered, err := tr.Render(md)
	if err != nil {
		return md, err
	}
	return rendered, nil
}

// FormatOutputValue formats an output value for display. If the value is a string
// that looks like markdown (or has a known markdown output name), it is rendered
// as formatted output. Otherwise returns the JSON-indented representation.
func FormatOutputValue(name string, value interface{}, out io.Writer) string {
	if str, ok := value.(string); ok {
		str = strings.TrimSpace(str)
		if LooksLikeMarkdown(str) || MarkdownOutputNames[name] {
			rendered, err := RenderMarkdown(str, out)
			if err == nil {
				return rendered
			}
			// Fall back to plain string on render error
		}
		return str
	}
	return ""
}
