/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/cli/internal/display"
)

// WorkflowExecutor handles workflow execution
type WorkflowExecutor struct {
	client client.Client
}

// NewWorkflowExecutor creates a new workflow executor
func NewWorkflowExecutor(k8sClient client.Client) *WorkflowExecutor {
	return &WorkflowExecutor{
		client: k8sClient,
	}
}

// CreateWorkflowRun creates a new WorkflowRun
func (e *WorkflowExecutor) CreateWorkflowRun(ctx context.Context, workflowName, namespace string, inputValues map[string]string) (*ottoflowv1alpha1.WorkflowRun, error) {
	// Generate unique name (nanosecond precision to avoid collisions within same second)
	runName := fmt.Sprintf("%s-%d", workflowName, time.Now().UnixNano())

	workflowRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: namespace,
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{
				Name: workflowName,
			},
			InputValues: inputValues,
		},
	}

	if err := e.client.Create(ctx, workflowRun); err != nil {
		return nil, fmt.Errorf("failed to create WorkflowRun: %w", err)
	}

	return workflowRun, nil
}

// LoadWorkflowRunFromFile loads a WorkflowRun from a YAML file
func (e *WorkflowExecutor) LoadWorkflowRunFromFile(ctx context.Context, filePath string) (*ottoflowv1alpha1.WorkflowRun, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Parse YAML
	workflowRun := &ottoflowv1alpha1.WorkflowRun{}
	if err := yaml.Unmarshal(data, workflowRun); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Create the WorkflowRun if it doesn't exist
	if workflowRun.Name == "" {
		return nil, fmt.Errorf("WorkflowRun name is required in YAML file")
	}

	// Check if it already exists
	existing := &ottoflowv1alpha1.WorkflowRun{}
	key := types.NamespacedName{
		Name:      workflowRun.Name,
		Namespace: workflowRun.Namespace,
	}
	if err := e.client.Get(ctx, key, existing); err != nil {
		// Doesn't exist, create it
		if err := e.client.Create(ctx, workflowRun); err != nil {
			return nil, fmt.Errorf("failed to create WorkflowRun: %w", err)
		}
		return workflowRun, nil
	}

	// Already exists, return it
	return existing, nil
}

// WatchWorkflow watches a workflow execution and displays progress
func (e *WorkflowExecutor) WatchWorkflow(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, timeout time.Duration, outputFormat string, includeInputs bool) error {
	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second

	key := types.NamespacedName{
		Name:      workflowRun.Name,
		Namespace: workflowRun.Namespace,
	}

	fmt.Println("Watching workflow execution...")
	fmt.Println()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Fetch current status
		current := &ottoflowv1alpha1.WorkflowRun{}
		if err := e.client.Get(ctx, key, current); err != nil {
			return fmt.Errorf("failed to get WorkflowRun: %w", err)
		}

		// Display status
		display.PrintWorkflowStatus(current, outputFormat, includeInputs)

		// Check if completed
		if current.Status.Phase.IsTerminal() {
			fmt.Println()
			if current.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
				fmt.Println("✅ Workflow completed successfully!")
			} else {
				fmt.Println("❌ Workflow failed!")
			}
			return nil
		}

		// Wait before next poll, or exit on cancellation (e.g. Ctrl+C)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return fmt.Errorf("workflow did not complete within timeout period")
}
