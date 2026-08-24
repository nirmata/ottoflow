/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"context"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	namespace  string
	kubeconfig string
)

// version, gitCommit, and buildTime are set at build time via ldflags.
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "ottoflow",
	Short: "OttoFlow CLI - Execute and manage workflows",
	Long: `OttoFlow is a CLI tool for executing and managing workflows in Kubernetes clusters.

OttoFlow allows you to:
- Execute workflows against a Kubernetes cluster
- Monitor workflow execution progress
- View workflow results and outputs
- Manage workflow runs`,
	Version: version,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// The root context is cancelled on SIGINT (Ctrl+C) so long-running operations (e.g. watch) can exit promptly.
func Execute() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
	}()
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "",
		"Kubernetes namespace (defaults to current kubeconfig context namespace, then \"ottoflow\")")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "",
		"Path to kubeconfig file (defaults to $HOME/.kube/config)")
}

// getNamespace returns the namespace to use: the --namespace flag, then the current
// kubeconfig context namespace (clientcmd itself falls back to "default" when the context
// sets none, so no further fallback is needed here). Local execution modes (-f/--workflow-dir)
// resolve workflows by name across whatever namespace(s) were actually loaded -- see
// LocalWorkflowExecutor.ResolveWorkflow -- so this value is only ever used as a hint to
// disambiguate an otherwise-ambiguous match, not a hard requirement.
func getNamespace() string {
	if namespace != "" {
		return namespace
	}
	return namespaceFromContext()
}

// namespaceFromContext reads the namespace from the active kubeconfig context.
func namespaceFromContext() string {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	ns, _, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).Namespace()
	if err != nil {
		return ""
	}
	return ns
}
