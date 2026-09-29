/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/cli/internal/display"
)

var (
	statusOutputFormat  string
	statusIncludeInputs bool
)

// statusCmd represents the status command
var statusCmd = &cobra.Command{
	Use:          "status [workflow-run-name]",
	Short:        "Get status of a workflow run",
	SilenceUsage: true, // Don't print usage on error
	Long: `Get the current status of a workflow run.

Examples:
  # Get status of a workflow run
  ottoflow status my-workflow-run-1234567890

  # Get status in JSON format
  ottoflow status my-workflow-run-1234567890 --output json

  # Get status in YAML format
  ottoflow status my-workflow-run-1234567890 --output yaml`,
	Args: cobra.ExactArgs(1),
	RunE: getStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().StringVarP(&statusOutputFormat, "output", "o", "table", "Output format: table, json, yaml")
	statusCmd.Flags().BoolVar(&statusIncludeInputs, "include-inputs", false,
		"Include spec.inputValues in json/yaml output (may contain secrets)")
}

func getStatus(cmd *cobra.Command, args []string) error {
	if err := display.ValidateOutputFormat(statusOutputFormat); err != nil {
		return err
	}

	ctx := context.Background()
	workflowRunName := args[0]

	// Get kubeconfig and create client
	config, err := getKubeConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	k8sClient, err := createK8sClient(config)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Fetch WorkflowRun
	workflowRun := &ottoflowv1alpha1.WorkflowRun{}
	key := types.NamespacedName{
		Name:      workflowRunName,
		Namespace: getNamespace(),
	}

	if err := k8sClient.Get(ctx, key, workflowRun); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Errorf("WorkflowRun '%s' not found in namespace '%s'", workflowRunName, getNamespace())
		}
		return fmt.Errorf("failed to get WorkflowRun: %w", err)
	}

	// Display status
	display.PrintWorkflowStatus(workflowRun, statusOutputFormat, statusIncludeInputs)
	return nil
}
