package executor

import (
	"strings"
	"testing"
)

// TestForeachFailureError pins the message chosen for each relationship between the number
// of items requested and the number that recorded a failure. The middle case -- some items
// failed and the rest never recorded a result because the context was cancelled mid-loop --
// is the one a review flagged: it must not claim "all N failed".
func TestForeachFailureError(t *testing.T) {
	tests := []struct {
		name         string
		requested    int
		failed       int
		firstFailure string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "every item ran and failed",
			requested:    3,
			failed:       3,
			firstFailure: "boom",
			wantContains: []string{"all 3 item(s) failed", "itemFailurePolicy=Continue", "boom"},
			wantAbsent:   []string{"did not complete"},
		},
		{
			name:         "some failed, rest cancelled mid-loop",
			requested:    3,
			failed:       1,
			firstFailure: "boom",
			wantContains: []string{"1 of 3 item(s) failed", "2 did not complete", "loop cancelled or incomplete", "boom"},
			wantAbsent:   []string{"all 3 item(s) failed"},
		},
		{
			name:         "nothing recorded a result",
			requested:    3,
			failed:       0,
			firstFailure: "",
			wantContains: []string{"no items completed successfully", "3 item(s) requested"},
			wantAbsent:   []string{"did not complete", "item(s) failed"},
		},
		{
			name:         "single item, ran and failed",
			requested:    1,
			failed:       1,
			firstFailure: "kaboom",
			wantContains: []string{"all 1 item(s) failed", "kaboom"},
			wantAbsent:   []string{"did not complete"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := foreachFailureError(tt.requested, tt.failed, "Continue", tt.firstFailure)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			msg := err.Error()
			for _, want := range tt.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q missing expected substring %q", msg, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(msg, absent) {
					t.Errorf("message %q should not contain %q", msg, absent)
				}
			}
		})
	}
}
