/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/cli/internal/display"
	cliexec "github.com/nirmata/ottoflow/cli/internal/executor"
	clioutput "github.com/nirmata/ottoflow/cli/internal/output"
)

var (
	workflowName     string
	workflowDir      string
	runFile          string
	allowInsecureURL bool
	inputValues      map[string]string
	timeout          string
	watch            bool
	outputFormat     string
	includeInputs    bool
	maxWorkers       int
	prometheusURL    string
	outputDir        string
	providerOverride string
	modelOverride    string
)

// runCmd represents the run command
var runCmd = &cobra.Command{
	Use:          "run [workflow-name|workflow-file.yaml]",
	Short:        "Create and watch a WorkflowRun",
	SilenceUsage: true, // Don't print usage on error
	Long: `Run a workflow: locally (with --workflow-dir, --file, or a plain file path) or in-cluster (default).

When --workflow-dir is set, workflows are loaded from that directory and executed in-process
(local execution). When --file/-f is set, a single manifest is loaded from a file, an http(s)
URL, or stdin ("-") and executed the same way, with no directory and no cluster required for
cluster-independent steps. Passing a bare file path with no flag does the same thing -- no
--file/--local flag needed -- unless the file contains a WorkflowRun, which is applied in-cluster
for backward compatibility. Otherwise, a WorkflowRun is created in the cluster and the
controller runs the workflow in a Job.

Examples:
  # Local execution: run a single workflow manifest by path, no flags needed
  ottoflow run samples/workflows/production/cluster-overview.yaml

  # Local execution: load and run a single workflow from a directory
  ottoflow run my-workflow --workflow-dir samples/workflows

  # Local execution: run all workflows in a directory
  ottoflow run --workflow-dir samples/workflows

  # Local execution: run a manifest piped in on stdin
  cat my-workflow.yaml | ottoflow run -f -

  # Local execution: run a manifest fetched from an http(s) URL
  ottoflow run -f https://example.com/my-workflow.yaml

  # Cluster: create a WorkflowRun (Workflow must exist in cluster)
  ottoflow run my-workflow --input name=value

  # Cluster: create and watch until completion
  ottoflow run my-workflow --input name="World"

  # Cluster: create without watching
  ottoflow run my-workflow --watch=false

  # Local: override LLM provider and model for all agent steps
  ottoflow run my-workflow --workflow-dir samples/workflows --provider openai --model gpt-4`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkflow,
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&workflowName, "workflow", "w", "", "Name of the workflow to execute")
	runCmd.Flags().StringVar(&workflowDir, "workflow-dir", "",
		"Load workflows from directory and run locally (in-process); if set, cluster path is not used")
	runCmd.Flags().StringVarP(&runFile, "file", "f", "",
		"Run a manifest locally, in-process, from a file, an http(s) URL, or '-' for stdin (no cluster/controller required)")
	runCmd.Flags().BoolVar(&allowInsecureURL, "allow-insecure-url", false,
		"Permit http:// (non-TLS) URLs with -f or a bare http(s) URL argument")
	runCmd.Flags().StringToStringVarP(&inputValues, "input", "i", map[string]string{},
		"Input values as key=value pairs (can be specified multiple times)")
	runCmd.Flags().StringVar(&timeout, "timeout", "10m", "Maximum time to wait for workflow completion (cluster watch)")
	runCmd.Flags().BoolVarP(&watch, "watch", "W", true, "Watch workflow execution progress (cluster mode only)")
	runCmd.Flags().StringVarP(&outputFormat, "output", "o", "table", "Output format: table, json, yaml")
	runCmd.Flags().BoolVar(&includeInputs, "include-inputs", false,
		"Include spec.inputValues in json/yaml output (may contain secrets; use only when needed)")
	runCmd.Flags().IntVar(&maxWorkers, "max-workers", 5, "Max concurrent workers for forEach steps (local mode only)")
	runCmd.Flags().StringVar(&prometheusURL, "prometheus-url", "",
		"Prometheus server URL for CEL/prometheus steps (local mode only)")
	runCmd.Flags().StringVar(&outputDir, "output-dir", "",
		"Save run output (JSON + Markdown) to directory (created if needed)")
	runCmd.Flags().StringVar(&providerOverride, "provider", "",
		"Override LLM provider for all agent steps (local mode only); e.g. openai, anthropic, google")
	runCmd.Flags().StringVar(&modelOverride, "model", "",
		"Override LLM model for all agent steps (local mode only); e.g. gpt-4, gemini-flash-latest")
}

func runWorkflow(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if runFile != "" && workflowDir != "" {
		return fmt.Errorf("--file and --workflow-dir are mutually exclusive")
	}
	// A bare file path or http(s) URL (e.g. `ottoflow run samples/foo.yaml` or
	// `ottoflow run https://.../foo.yaml`) with no --file/--workflow-dir is ambiguous: it's
	// historically been treated as a WorkflowRun to apply in-cluster, but a user pointing run
	// at a Workflow manifest almost always wants it executed locally, the same as `-f <ref>`.
	// classifyRunSource fetches it once (the same file/URL/stdin logic -f uses) and peeks at its
	// Kind to route it correctly instead of requiring a flag.
	var preloadedManifest []byte
	var preloadedRun *ottoflowv1alpha1.WorkflowRun
	if runFile == "" && workflowDir == "" && len(args) == 1 && (looksLikeFilePath(args[0]) || looksLikeURL(args[0])) {
		manifest, run, err := classifyRunSource(cmd, ctx, args[0])
		if err != nil {
			return err
		}
		if run != nil {
			preloadedRun = run
		} else {
			preloadedManifest = manifest
			runFile = args[0]
			args = nil
		}
	}
	if runFile != "" || workflowDir != "" {
		// --watch only has meaning against the cluster's async watch-and-poll loop; local
		// execution always runs synchronously to completion, so the flag does nothing here.
		if cmd.Flags().Changed("watch") {
			fmt.Fprintln(os.Stderr, "Warning: --watch only applies to cluster mode; local execution always runs to completion")
		}
		localCtx, cancel, err := applyLocalTimeout(ctx)
		if err != nil {
			return err
		}
		defer cancel()
		ctx = localCtx
	}
	if runFile != "" {
		return runWorkflowFromStream(cmd, ctx, args, preloadedManifest)
	}
	config, err := getKubeConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	k8sClient, err := createK8sClient(config)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	if workflowDir != "" {
		return runWorkflowLocal(ctx, k8sClient, config, args)
	}
	if providerOverride != "" || modelOverride != "" {
		fmt.Fprintln(os.Stderr, "Warning: --provider and --model flags only apply to local mode (--workflow-dir)")
	}
	return runWorkflowInCluster(ctx, k8sClient, args, preloadedRun)
}

// applyLocalTimeout wraps ctx with a deadline from --timeout for local execution
// (-f/--workflow-dir), which previously ignored the flag entirely -- only the cluster watch
// loop honored it. Always returns a non-nil cancel func, safe to defer unconditionally, even
// on error.
func applyLocalTimeout(ctx context.Context) (context.Context, context.CancelFunc, error) {
	d, err := time.ParseDuration(timeout)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("invalid timeout: %w", err)
	}
	if d <= 0 {
		return ctx, func() {}, fmt.Errorf("timeout must be positive (got %v)", d)
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	return ctx, cancel, nil
}

// runWorkflowLocal loads workflows from workflowDir and runs the named workflow in-process.
func runWorkflowLocal(ctx context.Context, k8sClient client.Client, config *rest.Config, args []string) error {
	name, err := resolveWorkflowNameForLocal(args)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("workflow name is required for local execution (use --workflow or pass name as argument)")
	}

	exec := cliexec.NewLocalWorkflowExecutor(k8sClient, prometheusURL, maxWorkers, providerOverride, modelOverride)

	// resourceMetrics() needs a metrics.k8s.io client, built from the same config as the
	// target client. NewForConfig does no I/O -- it only fails when a client cannot be
	// constructed from this config, not when metrics-server is absent or unreachable (that
	// surfaces later, on the call). Carry on without it: only workflows that call
	// resourceMetrics() are affected, and those report it themselves.
	if mc, err := metricsclientset.NewForConfig(config); err != nil {
		klog.V(2).InfoS("metrics client unavailable, resourceMetrics() will be disabled", "error", err)
	} else {
		exec.SetMetricsClient(mc)
	}

	if err := exec.LoadFromDirectory(workflowDir); err != nil {
		return fmt.Errorf("load workflow dir: %w", err)
	}

	// Resolve by name across whatever namespace the workflow actually loaded into (e.g. a
	// namespace-less Workflow normalizes to "default") instead of assuming it lives in
	// getNamespace()'s namespace, which defaults to "ottoflow" and would otherwise miss it.
	resolvedName, ns, err := exec.ResolveWorkflow(ctx, name, getNamespace())
	if err != nil {
		return err
	}

	return runLoadedWorkflow(ctx, exec, resolvedName, ns)
}

// runWorkflowFromStream implements `ottoflow run -f <ref>` and the bare file-path/URL form: it
// loads a single manifest from a file, an http(s) URL, or stdin ("-") and executes it locally,
// in-process. Unlike --workflow-dir this path never requires a kubeconfig -- a Kubernetes client
// is built on a best-effort basis so cluster-independent workflows (expressions, agent steps)
// run with zero setup, while steps that do need a cluster (resourceQuery, mutate) fail with a
// clear error. preloadedData, when non-nil, is already-fetched manifest bytes (the bare
// path/URL form in runWorkflow fetches once to classify the Kind and passes the result here
// instead of fetching the same URL twice); otherwise it's read from runFile.
func runWorkflowFromStream(cmd *cobra.Command, ctx context.Context, args []string, preloadedData []byte) error {
	data := preloadedData
	if data == nil {
		d, err := readRunSource(cmd, ctx, runFile)
		if err != nil {
			return err
		}
		data = d
	}

	config, k8sClient, err := resolveOptionalKubeClient()
	if err != nil {
		return err
	}

	exec := cliexec.NewLocalWorkflowExecutor(k8sClient, prometheusURL, maxWorkers, providerOverride, modelOverride)
	if config != nil {
		if mc, err := metricsclientset.NewForConfig(config); err != nil {
			klog.V(2).InfoS("metrics client unavailable, resourceMetrics() will be disabled", "error", err)
		} else {
			exec.SetMetricsClient(mc)
		}
	}

	if err := exec.LoadFromReader(bytes.NewReader(data)); err != nil {
		return err
	}

	// -f already names the manifest source, so a positional arg here is unambiguously the
	// requested workflow name -- unlike resolveWorkflowName's cluster-mode use of
	// looksLikeFilePath, where the positional could otherwise be a WorkflowRun file path.
	requested := workflowName
	if requested == "" && len(args) > 0 {
		requested = args[0]
	}
	// Only use a namespace hint to auto-select among multiple loaded workflows when the user
	// explicitly passed -n/--namespace. getNamespace()'s ambient fallback (kubeconfig context
	// namespace, or "default") has nothing to do with what was actually loaded from the
	// manifest, and would otherwise let it silently pick which workflow runs.
	nsHint := ""
	if namespace != "" {
		nsHint = namespace
	}
	name, ns, err := exec.ResolveWorkflow(ctx, requested, nsHint)
	if err != nil {
		return err
	}
	return runLoadedWorkflow(ctx, exec, name, ns)
}

// runLoadedWorkflow executes a workflow already loaded into exec, printing progress and the
// final result the same way regardless of how the manifests were loaded (directory or stream).
func runLoadedWorkflow(ctx context.Context, exec *cliexec.LocalWorkflowExecutor, name, ns string) error {
	// Print one line per step when its phase or message changes; full table and outputs at the end.
	lastStepState := make(map[string]string) // stepName -> "phase\tmessage" for dedup
	exec.SetProgressCallback(func(run *ottoflowv1alpha1.WorkflowRun, wf *ottoflowv1alpha1.Workflow) {
		if run == nil || wf == nil || outputFormat != "table" {
			return
		}
		for _, step := range wf.Spec.Steps {
			status, ok := run.Status.StepStatuses[step.Name]
			if !ok {
				continue
			}
			phase := string(status.Phase)
			if phase == "" {
				phase = "Pending"
			}
			state := phase + "\t" + status.Message
			if lastStepState[step.Name] != state {
				lastStepState[step.Name] = state
				display.PrintStepStatusLine(step.Name, &status)
			}
		}
	})

	run, err := exec.ExecuteWorkflow(ctx, name, ns, inputValues)
	if err != nil {
		if run != nil {
			display.PrintWorkflowStatus(run, outputFormat, includeInputs)
			maybeSaveOutput(run)
		}
		return err
	}
	if run != nil {
		display.PrintWorkflowStatus(run, outputFormat, includeInputs)
		maybeSaveOutput(run)
	}
	return nil
}

// maxManifestBytes caps how much manifest data readRunSource will read from either a remote
// URL or stdin, so a slow/oversized/malicious source can't exhaust memory or hang the process.
const maxManifestBytes = 10 << 20 // 10 MiB

// readRunSource reads the manifest bytes referenced by ref: "-" for stdin, an http(s) URL for a
// remote fetch, or anything else as a local file path.
func readRunSource(cmd *cobra.Command, ctx context.Context, ref string) ([]byte, error) {
	if ref == "-" {
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxManifestBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(data) > maxManifestBytes {
			return nil, fmt.Errorf("stdin input exceeds 10 MiB limit")
		}
		if err := checkNotHTML("stdin", "", data); err != nil {
			return nil, err
		}
		return data, nil
	}
	if lower := strings.ToLower(ref); strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return fetchURL(ctx, ref)
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref, err)
	}
	return data, nil
}

// checkNotHTML returns a clear, actionable error when body looks like an HTML page rather than
// a YAML manifest -- e.g. a GitHub "blob" view URL was used instead of its raw-content
// equivalent, or a login/error page came back in place of the file. Without this, an HTML body
// reaches the YAML parser and fails with an opaque error far removed from the actual cause.
// contentType may be "" (e.g. for stdin, which has none); source names the input in the error.
func checkNotHTML(source, contentType string, body []byte) error {
	isHTMLContentType := false
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		isHTMLContentType = mediaType == "text/html"
	}
	looksLikeHTMLBody := bytes.HasPrefix(bytes.TrimSpace(body), []byte("<"))
	if !isHTMLContentType && !looksLikeHTMLBody {
		return nil
	}
	return fmt.Errorf(
		"%s looks like an HTML page, not a YAML manifest -- if this is a GitHub (or similar) "+
			"URL, use the raw file URL instead (e.g. raw.githubusercontent.com/...)",
		source,
	)
}

// fetchURL downloads rawURL with a bounded timeout, a redirect cap, and a response size limit,
// so a slow, redirect-looping, or oversized remote manifest can't hang or exhaust memory.
// Plain http:// is rejected unless --allow-insecure-url is set, including on redirect.
func fetchURL(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if parsed.Scheme == "http" && !allowInsecureURL {
		return nil, fmt.Errorf("refusing to fetch insecure http:// URL %q; pass --allow-insecure-url to permit it", rawURL)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpClient := &http.Client{
		CheckRedirect: checkRedirectPolicy,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %q: %w", rawURL, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", rawURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %q: unexpected status %s", rawURL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response from %q: %w", rawURL, err)
	}
	if len(body) > maxManifestBytes {
		return nil, fmt.Errorf("fetch %q: response exceeds 10 MiB limit", rawURL)
	}
	if err := checkNotHTML(rawURL, resp.Header.Get("Content-Type"), body); err != nil {
		return nil, err
	}

	return body, nil
}

// resolveWorkflowNameForLocal returns the workflow name for local execution (--workflow or first arg).
// Returns an error when the argument looks like a file path, guiding the user to the correct usage.
func resolveWorkflowNameForLocal(args []string) (string, error) {
	if workflowName != "" {
		return workflowName, nil
	}
	if len(args) > 0 {
		if looksLikeFilePath(args[0]) {
			return "", fmt.Errorf(
				"%q looks like a file path, not a workflow name\n"+
					"  Use: ottoflow run <workflow-name> --workflow-dir <directory>",
				args[0],
			)
		}
		return args[0], nil
	}
	return "", nil
}

// clusterRunOptions holds options for creating and watching a WorkflowRun in-cluster.
type clusterRunOptions struct {
	workflowName  string
	inputValues   map[string]string
	watch         bool
	timeout       string
	outputFormat  string
	includeInputs bool
	getNamespace  func() string
}

// runWorkflowInCluster creates and (by default) watches a WorkflowRun. preloadedRun, when
// non-nil, is a WorkflowRun already parsed from a bare file-path/URL argument in runWorkflow
// (which fetched it once to classify its Kind) and is applied as-is; otherwise the WorkflowRun
// is built from args the normal way (a local WorkflowRun file path, or a workflow name).
func runWorkflowInCluster(
	ctx context.Context, k8sClient client.Client, args []string, preloadedRun *ottoflowv1alpha1.WorkflowRun,
) error {
	opts := clusterRunOptions{
		workflowName:  workflowName,
		inputValues:   inputValues,
		watch:         watch,
		timeout:       timeout,
		outputFormat:  outputFormat,
		includeInputs: includeInputs,
		getNamespace:  getNamespace,
	}
	var runObj *ottoflowv1alpha1.WorkflowRun
	if preloadedRun != nil {
		runObj = preloadedRun.DeepCopy()
	} else {
		var err error
		runObj, err = resolveWorkflowRunSpec(opts, args)
		if err != nil {
			return err
		}
	}
	normalizeWorkflowRunSpec(runObj, opts)

	// Validate that the referenced Workflow exists before creating the WorkflowRun so we fail fast with a clear error
	ref := runObj.Spec.WorkflowRef
	wfNamespace := ref.Namespace
	if wfNamespace == "" {
		wfNamespace = runObj.Namespace
	}
	wfKey := types.NamespacedName{Name: ref.Name, Namespace: wfNamespace}
	wf := &ottoflowv1alpha1.Workflow{}
	if err := k8sClient.Get(ctx, wfKey, wf); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("workflow %q not found in namespace %q", ref.Name, wfNamespace)
		}
		return fmt.Errorf("failed to get workflow %q: %w", ref.Name, err)
	}

	if err := k8sClient.Create(ctx, runObj); err != nil {
		return fmt.Errorf("failed to create WorkflowRun: %w", err)
	}
	fmt.Printf("Created WorkflowRun: %s/%s\n", runObj.Namespace, runObj.Name)
	if !opts.watch {
		return nil
	}
	timeoutDuration, err := time.ParseDuration(opts.timeout)
	if err != nil {
		return fmt.Errorf("invalid timeout: %w", err)
	}
	if timeoutDuration <= 0 {
		return fmt.Errorf("timeout must be positive (got %v)", timeoutDuration)
	}
	return watchWorkflowRunToCompletion(
		ctx, k8sClient, client.ObjectKeyFromObject(runObj), timeoutDuration, opts.outputFormat, opts.includeInputs)
}

// resolveWorkflowRunSpec builds a WorkflowRun from a workflow name. A file-path/URL argument
// naming a WorkflowRun directly is handled earlier, in runWorkflow (preloadedRun) -- by the time
// this runs, args (per cobra.MaximumNArgs(1)) is either empty or a single non-file-like name.
func resolveWorkflowRunSpec(opts clusterRunOptions, args []string) (*ottoflowv1alpha1.WorkflowRun, error) {
	name := resolveWorkflowName(opts, args)
	if name == "" {
		return nil, fmt.Errorf("workflow name is required (use --workflow or provide as argument)")
	}
	ns := opts.getNamespace()
	generateName := fmt.Sprintf("%s-", name)
	if len(generateName) > 253 {
		generateName = generateName[:253]
	}
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generateName,
			Namespace:    ns,
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: name, Namespace: ns},
			InputValues: opts.inputValues,
		},
	}, nil
}

// parseWorkflowRunDoc scans data (already-loaded manifest bytes, from a file, URL, or stdin) for
// a WorkflowRun document, splitting on YAML document separators the same way
// loadWorkflowRunFromFile always has. Returns the first WorkflowRun found and ok=true, or
// ok=false if data contains no WorkflowRun (e.g. a Workflow, Agent, or other Kind) -- callers
// use that to route the source to local execution instead.
func parseWorkflowRunDoc(data []byte) (wr *ottoflowv1alpha1.WorkflowRun, ok bool) {
	documents := strings.Split("\n"+string(data), "\n---")
	for _, doc := range documents {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		candidate := &ottoflowv1alpha1.WorkflowRun{}
		if err := yaml.Unmarshal([]byte(doc), candidate); err == nil && candidate.Kind == "WorkflowRun" {
			return candidate, true
		}
	}
	return nil, false
}

// classifyRunSource fetches ref (a file path, http(s) URL, or "-" for stdin -- the same sources
// readRunSource accepts for -f) and classifies its content: a WorkflowRun document returns
// (nil, wr, nil) for the caller to apply in-cluster; anything else (typically a Workflow) returns
// (manifest, nil, nil) for local execution, with manifest holding the already-fetched bytes so
// the caller never has to fetch the same URL a second time.
func classifyRunSource(
	cmd *cobra.Command, ctx context.Context, ref string,
) ([]byte, *ottoflowv1alpha1.WorkflowRun, error) {
	data, err := readRunSource(cmd, ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if wr, ok := parseWorkflowRunDoc(data); ok {
		return nil, wr, nil
	}
	return data, nil, nil
}

// looksLikeURL reports whether arg is an http(s) URL, the same schemes readRunSource/fetchURL
// accept for -f.
func looksLikeURL(arg string) bool {
	lower := strings.ToLower(arg)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func looksLikeFilePath(arg string) bool {
	return strings.Contains(arg, string(filepath.Separator)) ||
		strings.HasSuffix(arg, ".yaml") ||
		strings.HasSuffix(arg, ".yml")
}

func resolveWorkflowName(opts clusterRunOptions, args []string) string {
	if opts.workflowName != "" {
		return opts.workflowName
	}
	if len(args) > 0 && !looksLikeFilePath(args[0]) {
		return args[0]
	}
	return ""
}

// normalizeWorkflowRunSpec fills default namespace and merges input values before create.
func normalizeWorkflowRunSpec(runObj *ottoflowv1alpha1.WorkflowRun, opts clusterRunOptions) {
	ns := opts.getNamespace()
	if runObj.Namespace == "" {
		runObj.Namespace = ns
	}
	if runObj.Spec.WorkflowRef.Namespace == "" {
		runObj.Spec.WorkflowRef.Namespace = runObj.Namespace
	}
	if len(opts.inputValues) == 0 {
		return
	}
	if runObj.Spec.InputValues == nil {
		runObj.Spec.InputValues = map[string]string{}
	}
	for k, v := range opts.inputValues {
		runObj.Spec.InputValues[k] = v
	}
}

// watchWorkflowRunToCompletion polls WorkflowRun status until a terminal phase or timeout.
// timeoutDuration must be positive; otherwise the deadline would be in the past and the loop would exit immediately.
func watchWorkflowRunToCompletion(
	ctx context.Context,
	k8sClient client.Client,
	key client.ObjectKey,
	timeoutDuration time.Duration,
	outputFormat string,
	includeInputs bool,
) error {
	if timeoutDuration <= 0 {
		return fmt.Errorf("timeout must be positive (got %v)", timeoutDuration)
	}
	deadline := time.Now().Add(timeoutDuration)
	pollInterval := 2 * time.Second
	for {
		current := &ottoflowv1alpha1.WorkflowRun{}
		if err := k8sClient.Get(ctx, key, current); err != nil {
			return fmt.Errorf("failed to get WorkflowRun status: %w", err)
		}
		if current.Status.Execution != nil && current.Status.Execution.JobName != "" {
			fmt.Printf("Runner Job: %s\n", current.Status.Execution.JobName)
		}
		switch current.Status.Phase {
		case ottoflowv1alpha1.WorkflowRunPhaseSucceeded, ottoflowv1alpha1.WorkflowRunPhaseFailed:
			display.PrintWorkflowStatus(current, outputFormat, includeInputs)
			maybeSaveOutput(current)
			if current.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
				return fmt.Errorf("workflow execution failed")
			}
			return nil
		}
		if time.Now().After(deadline) {
			display.PrintWorkflowStatus(current, outputFormat, includeInputs)
			maybeSaveOutput(current)
			return fmt.Errorf("timed out waiting for workflow completion")
		}
		// Wait before next poll, or exit on cancellation (e.g. Ctrl+C)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// maybeSaveOutput saves run output to outputDir if the flag was set.
// Errors are logged to stderr but don't affect the command's exit code.
func maybeSaveOutput(workflowRun *ottoflowv1alpha1.WorkflowRun) {
	if outputDir == "" {
		return
	}
	jsonPath, mdPath, err := clioutput.SaveRunOutput(workflowRun, outputDir, includeInputs)
	if jsonPath != "" {
		fmt.Fprintf(os.Stderr, "Saved: %s\n", jsonPath)
	}
	if mdPath != "" {
		fmt.Fprintf(os.Stderr, "Saved: %s\n", mdPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
}

// checkRedirectPolicy is the http.Client.CheckRedirect policy for fetchURL: it caps the
// redirect chain at 10 hops so a redirect loop can't hang the fetch, and refuses to follow a
// redirect to a plain http:// URL unless --allow-insecure-url was passed, so a compromised or
// misconfigured server can't downgrade an https fetch to plaintext transport mid-request.
func checkRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme == "http" && !allowInsecureURL {
		return errors.New("refusing redirect to insecure http URL (use --allow-insecure-url)")
	}
	return nil
}

// resolveOptionalKubeClient builds a best-effort Kubernetes client for the -f/stdin path, which
// never requires a cluster. A kubeconfig that is genuinely absent (no --kubeconfig flag, no file
// at the default path, and not running in-cluster) is tolerated: it returns a nil config and nil
// client with no error, so cluster-independent workflows still run with zero setup. Any other
// failure -- an explicit --kubeconfig flag that fails to load, a kubeconfig file that exists but
// won't parse, or client construction failing against a config that DID load -- is a real error
// and must not be silently swallowed as "no kubeconfig".
func resolveOptionalKubeClient() (*rest.Config, client.Client, error) {
	config, cfgErr := getKubeConfig()
	if cfgErr != nil {
		if kubeconfigGenuinelyAbsent(cfgErr) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("failed to load kubeconfig: %w", cfgErr)
	}
	k8sClient, err := createK8sClient(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	return config, k8sClient, nil
}

// kubeconfigLoadingRules returns the clientcmd loading rules used to resolve the kubeconfig
// path: --kubeconfig if set, otherwise clientcmd's own precedence chain, which honors the
// $KUBECONFIG environment variable before falling back to $HOME/.kube/config.
func kubeconfigLoadingRules() *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
		return rules
	}
	// clientcmd.RecommendedHomeFile is a package-level var resolved against $HOME once, when
	// the clientcmd package is first imported -- never an issue for a short-lived CLI process,
	// but it means NewDefaultClientConfigLoadingRules's own $HOME/.kube/config fallback (used
	// only when $KUBECONFIG is unset) won't reflect a $HOME changed later in this process.
	// Recompute it from the current $HOME so the fallback stays live.
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			rules.Precedence = []string{filepath.Join(home, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)}
		}
	}
	return rules
}

// kubeconfigGenuinelyAbsent reports whether cfgErr (from getKubeConfig) came from having no
// kubeconfig at all -- no explicit --kubeconfig flag, no $KUBECONFIG override, and no file at
// the default path -- as opposed to a kubeconfig that was explicitly requested (via
// --kubeconfig or $KUBECONFIG) or exists on disk but failed to load.
func kubeconfigGenuinelyAbsent(cfgErr error) bool {
	if cfgErr == nil {
		return false
	}
	if kubeconfig != "" {
		return false // an explicit --kubeconfig flag failing to load is never "absent"
	}
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		return false // $KUBECONFIG explicitly set but failing to load is a real error, not absence
	}
	path := kubeconfigLoadingRules().GetDefaultFilename()
	if path == "" {
		return true // nothing on disk to point at
	}
	_, statErr := os.Stat(path)
	return os.IsNotExist(statErr)
}

func getKubeConfig() (*rest.Config, error) {
	kubeconfigPath := kubeconfigLoadingRules().GetDefaultFilename()
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	return rest.InClusterConfig()
}

func createK8sClient(config *rest.Config) (client.Client, error) {
	// Add OttoFlow API types to scheme
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := ottoflowv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}

	// Create client
	return client.New(config, client.Options{Scheme: s})
}

func loadWorkflowRunFromFile(filePath string) (*ottoflowv1alpha1.WorkflowRun, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	if wr, ok := parseWorkflowRunDoc(data); ok {
		return wr, nil
	}
	return nil, fmt.Errorf("no WorkflowRun found in file")
}
