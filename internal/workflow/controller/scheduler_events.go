/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"errors"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var (
	// errCronInputValuesFrom marks a failure to read a cron trigger's inputValuesFrom Secrets.
	errCronInputValuesFrom = errors.New("cron inputValuesFrom")
	// errCronInputKeyMissing marks a Secret that lacks the referenced key. Its text ends the
	// existing "key %q not found" message, so the message is unchanged.
	errCronInputKeyMissing = errors.New("not found")
)

// maxEventNoteBytes is the events.k8s.io/v1 note limit. The recorder does not truncate, and the
// API server rejects a longer note, so the Event would be dropped.
const maxEventNoteBytes = 1024

// cronFireFailureReason maps a createWorkflowRun error to an Event reason. Under one reason, a
// repeat within a few minutes updates only the existing Event's count and last-seen time, not
// its note, so each failure class gets its own reason.
func cronFireFailureReason(err error) string {
	switch {
	case !errors.Is(err, errCronInputValuesFrom):
		return "CronWorkflowRunCreateFailed"
	case errors.Is(err, errCronInputKeyMissing):
		return "CronInputSecretKeyMissing"
	case apierrors.IsForbidden(err):
		return "CronInputSecretForbidden"
	case apierrors.IsNotFound(err):
		return "CronInputSecretNotFound"
	default:
		return "CronInputSecretInvalid"
	}
}

// recordCronFireFailure records a Warning Event on the Workflow for a failed cron fire.
func (s *Scheduler) recordCronFireFailure(workflow *ottoflowv1alpha1.Workflow, err error) {
	if s.Recorder == nil {
		return
	}
	s.Recorder.Eventf(workflow, nil, corev1.EventTypeWarning, cronFireFailureReason(err), "CreateWorkflowRun",
		"%s", truncateEventNote(err.Error()))
}

// truncateEventNote cuts s to at most maxEventNoteBytes without splitting a UTF-8 sequence.
func truncateEventNote(s string) string {
	if len(s) <= maxEventNoteBytes {
		return s
	}
	cut := maxEventNoteBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
