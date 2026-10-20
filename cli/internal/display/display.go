/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package display

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// PrintStepStatusLine prints a single step status line (e.g. "  getAllPods: Succeeded (20ms)").
// Used for incremental progress during local execution.
func PrintStepStatusLine(stepName string, s *ottoflowv1alpha1.StepStatus) {
	phase := string(s.Phase)
	if phase == "" {
		phase = "Pending"
	}
	icon := getPhaseIcon(s.Phase)
	var suffix string
	if s.Message != "" {
		suffix = " " + s.Message
	}
	if s.StartTime != nil {
		if s.CompletionTime != nil {
			d := s.CompletionTime.Sub(s.StartTime.Time)
			suffix = suffix + " (" + formatDuration(d) + ")"
		} else {
			d := time.Since(s.StartTime.Time)
			suffix = suffix + " (" + formatDuration(d) + " so far)"
		}
	}
	fmt.Printf("  %s: %s %s%s\n", stepName, icon, phase, suffix)
}

// ValidOutputFormats lists the output formats accepted by PrintWorkflowStatus.
var ValidOutputFormats = []string{"table", "json", "yaml"}

// ValidateOutputFormat returns an error if format is not one of ValidOutputFormats.
func ValidateOutputFormat(format string) error {
	for _, f := range ValidOutputFormats {
		if format == f {
			return nil
		}
	}
	return fmt.Errorf("invalid --output %q: must be one of %s", format, strings.Join(ValidOutputFormats, ", "))
}

// PrintWorkflowStatus prints workflow status in the specified format.
// Output is appended to the terminal (no clear screen) so users can scroll and troubleshoot.
// includeInputs controls whether spec.inputValues (which may contain secrets) are included in json/yaml output.
func PrintWorkflowStatus(workflowRun *ottoflowv1alpha1.WorkflowRun, format string, includeInputs bool) {
	switch format {
	case "json":
		printJSON(workflowRun, includeInputs)
	case "yaml":
		printYAML(workflowRun, includeInputs)
	default:
		printTable(workflowRun)
	}
}

func printTable(workflowRun *ottoflowv1alpha1.WorkflowRun) {
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("WorkflowRun: %s/%s\n", workflowRun.Namespace, workflowRun.Name)
	phase := string(workflowRun.Status.Phase)
	if phase == "" {
		phase = "Pending"
	}
	fmt.Printf("Phase: %s %s\n", getPhaseIcon(phase), phase)
	if workflowRun.Status.Message != "" {
		fmt.Printf("Message: %s\n", workflowRun.Status.Message)
	}
	if workflowRun.Status.StartTime != nil {
		fmt.Printf("Started: %s\n", formatTime(workflowRun.Status.StartTime.Time))
	}
	if workflowRun.Status.CompletionTime != nil {
		fmt.Printf("Completed: %s\n", formatTime(workflowRun.Status.CompletionTime.Time))
		if workflowRun.Status.StartTime != nil {
			duration := workflowRun.Status.CompletionTime.Sub(workflowRun.Status.StartTime.Time)
			fmt.Printf("Duration: %s\n", duration.Round(time.Second))
		}
	}
	fmt.Println()

	// Print step statuses
	if len(workflowRun.Status.StepStatuses) > 0 {
		fmt.Println("Steps:")
		fmt.Println(strings.Repeat("─", 80))
		fmt.Printf("%-30s %-15s %-20s %s\n", "STEP", "PHASE", "MESSAGE", "TIME")
		fmt.Println(strings.Repeat("─", 80))

		for stepName, stepStatus := range workflowRun.Status.StepStatuses {
			phaseIcon := getPhaseIcon(stepStatus.Phase)
			phaseStr := string(stepStatus.Phase)
			message := stepStatus.Message
			if len(message) > 40 {
				message = message[:37] + "..."
			}
			timeStr := ""
			if stepStatus.StartTime != nil {
				if stepStatus.CompletionTime != nil {
					// Step completed - show duration
					duration := stepStatus.CompletionTime.Sub(stepStatus.StartTime.Time)
					timeStr = formatDuration(duration)
				} else {
					// Step still running - show elapsed time
					duration := time.Since(stepStatus.StartTime.Time)
					timeStr = formatDuration(duration)
				}
			}
			fmt.Printf("%-30s %-15s %-20s %s\n", stepName, phaseIcon+" "+phaseStr, message, timeStr)
		}
		fmt.Println()
	}

	// Print outputs
	if len(workflowRun.Status.Outputs) > 0 {
		fmt.Println("Outputs:")
		fmt.Println(strings.Repeat("─", 80))
		for name, value := range workflowRun.Status.Outputs {
			var outputValue interface{}
			if err := json.Unmarshal(value.Raw, &outputValue); err == nil {
				if formatted := FormatOutputValue(name, outputValue, os.Stdout); formatted != "" {
					fmt.Printf("  %s:\n%s\n", name, formatted)
				} else {
					outputJSON, _ := json.MarshalIndent(outputValue, "  ", "  ")
					fmt.Printf("  %s:\n%s\n", name, string(outputJSON))
				}
			} else {
				fmt.Printf("  %s: %s\n", name, string(value.Raw))
			}
		}
		fmt.Println()
	}
}

// BuildOutputMap builds a JSON-serializable map from a WorkflowRun.
// Used by both printJSON (terminal display) and output saving.
func BuildOutputMap(workflowRun *ottoflowv1alpha1.WorkflowRun, includeInputs bool) map[string]interface{} {
	output := map[string]interface{}{
		"name":      workflowRun.Name,
		"namespace": workflowRun.Namespace,
		"phase":     workflowRun.Status.Phase,
		"message":   workflowRun.Status.Message,
		"steps":     workflowRun.Status.StepStatuses,
		"outputs":   workflowRun.Status.Outputs,
	}
	if includeInputs {
		output["inputValues"] = workflowRun.Spec.InputValues
	}

	if workflowRun.Status.StartTime != nil {
		output["startTime"] = workflowRun.Status.StartTime.Format(time.RFC3339)
	}
	if workflowRun.Status.CompletionTime != nil {
		output["completionTime"] = workflowRun.Status.CompletionTime.Format(time.RFC3339)
	}
	return output
}

func printJSON(workflowRun *ottoflowv1alpha1.WorkflowRun, includeInputs bool) {
	output := BuildOutputMap(workflowRun, includeInputs)
	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fmt.Printf("Error marshaling JSON: %v\n", err)
		return
	}
	fmt.Println(string(jsonBytes))
}

func printYAML(workflowRun *ottoflowv1alpha1.WorkflowRun, includeInputs bool) {
	obj := workflowRun.DeepCopyObject()
	wr, ok := obj.(*ottoflowv1alpha1.WorkflowRun)
	if ok && !includeInputs {
		wr.Spec.InputValues = nil
	}
	yamlBytes, err := yaml.Marshal(obj)
	if err != nil {
		fmt.Printf("Error marshaling YAML: %v\n", err)
		return
	}
	fmt.Println(string(yamlBytes))
}

func getPhaseIcon(phase interface{}) string {
	phaseStr := fmt.Sprintf("%v", phase)
	switch phaseStr {
	case "Succeeded":
		return "✅"
	case "Failed":
		return "❌"
	case "Running":
		return "🔄"
	case "Pending":
		return "⏳"
	case "Skipped":
		return "⏭️"
	default:
		return "  "
	}
}

func formatTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

func formatDuration(d time.Duration) string {
	if d < time.Microsecond {
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.2fm", d.Minutes())
	}
	return fmt.Sprintf("%.2fh", d.Hours())
}
