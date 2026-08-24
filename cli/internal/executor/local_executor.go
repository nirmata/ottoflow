/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	workflowexecutor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

// localDefaultNamespace is used for any loaded object or WorkflowRun that declares no
// namespace, so local execution behaves like the CRD's cluster-side default.
const localDefaultNamespace = "default"

// ProgressCallback is called when step status changes during local execution.
type ProgressCallback func(workflowRun *ottoflowv1alpha1.WorkflowRun, workflow *ottoflowv1alpha1.Workflow)

// LocalWorkflowExecutor loads Workflow/Agent/MCPServer/StepTemplate from a directory and runs workflows in-process.
type LocalWorkflowExecutor struct {
	targetClient     client.Client
	controlClient    client.Client
	prometheusURL    string
	maxWorkers       int
	progressCallback ProgressCallback
	// workflowRuns stores WorkflowRun documents from YAML, keyed by "namespace/workflowRef.name".
	workflowRuns     map[string]*ottoflowv1alpha1.WorkflowRun
	providerOverride string
	modelOverride    string
	// metricsClient backs the resourceMetrics() CEL function. Optional: nil disables it,
	// which is what happens when the caller has no reachable metrics-server.
	metricsClient metricsclientset.Interface
}

// SetMetricsClient supplies the metrics.k8s.io client used by resourceMetrics(). Without
// it that function fails with "metrics client not available" even when metrics-server is
// running, because local mode has no other way to reach it.
func (e *LocalWorkflowExecutor) SetMetricsClient(c metricsclientset.Interface) {
	e.metricsClient = c
}

// NewLocalWorkflowExecutor creates a local executor. Call LoadFromDirectory before ExecuteWorkflow.
func NewLocalWorkflowExecutor(k8sClient client.Client, prometheusURL string, maxWorkers int, providerOverride, modelOverride string) *LocalWorkflowExecutor {
	return &LocalWorkflowExecutor{
		targetClient:     k8sClient,
		prometheusURL:    prometheusURL,
		maxWorkers:       maxWorkers,
		providerOverride: providerOverride,
		modelOverride:    modelOverride,
	}
}

// SetProgressCallback sets an optional callback for step progress during execution.
func (e *LocalWorkflowExecutor) SetProgressCallback(cb ProgressCallback) {
	e.progressCallback = cb
}

// LoadFromDirectory walks dir and loads all Workflow, Agent, MCPServer, StepTemplate YAML manifests into a fake control-plane client.
func (e *LocalWorkflowExecutor) LoadFromDirectory(dir string) error {
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("workflow dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workflow dir is not a directory: %s", dir)
	}

	// Deduplicate by GVK/namespace/name so the same resource in multiple files doesn't panic the fake client (last wins).
	seen := make(map[string]client.Object)
	var rawRuns []*ottoflowv1alpha1.WorkflowRun
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		return loadDocuments(data, path, seen, &rawRuns)
	})
	if err != nil {
		return err
	}
	workflowRuns, err := indexWorkflowRuns(rawRuns, seen)
	if err != nil {
		return err
	}
	e.workflowRuns = workflowRuns

	return e.buildControlClient(seen)
}

// LoadFromReader reads a single YAML (or multi-document YAML) stream -- e.g. stdin or a
// downloaded manifest -- and loads Workflow/Agent/MCPServer/StepTemplate/Secret documents
// into a fake control-plane client, mirroring LoadFromDirectory for a single in-memory source.
func (e *LocalWorkflowExecutor) LoadFromReader(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	seen := make(map[string]client.Object)
	var rawRuns []*ottoflowv1alpha1.WorkflowRun
	if err := loadDocuments(data, "<input>", seen, &rawRuns); err != nil {
		return err
	}
	workflowRuns, err := indexWorkflowRuns(rawRuns, seen)
	if err != nil {
		return err
	}
	e.workflowRuns = workflowRuns

	return e.buildControlClient(seen)
}

// loadDocuments splits data into YAML documents and loads each Workflow/Agent/MCPServer/
// StepTemplate/Secret document into seen, and appends each WorkflowRun document to *rawRuns
// (unindexed -- namespace defaulting and rebinding happens later, in indexWorkflowRuns, once
// every document from every source has been seen). srcName identifies the source (a file path
// or a placeholder like "<input>") in error text.
func loadDocuments(data []byte, srcName string, seen map[string]client.Object, rawRuns *[]*ottoflowv1alpha1.WorkflowRun) error {
	// YAML document separators only count at the start of a line. Splitting on a bare
	// "---" also splits on markdown table separators like |---|---| inside expression
	// strings, silently truncating the document mid-expression -- which surfaced as a
	// CEL "mismatched input <EOF>" on a workflow that `ottoflow validate` accepted,
	// because validate already splits correctly. Prepend a newline so the first
	// document is matched by the same "\n---" rule regardless of a leading marker.
	docs := strings.Split("\n"+string(data), "\n---")
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		// WorkflowRun is stored separately, not in the fake client.
		kind, decodeErr := documentKind([]byte(doc))
		if decodeErr != nil {
			return fmt.Errorf("%s: %w", srcName, decodeErr)
		}
		if kind == "WorkflowRun" {
			wr := &ottoflowv1alpha1.WorkflowRun{}
			if yamlErr := yaml.Unmarshal([]byte(doc), wr); yamlErr != nil {
				return fmt.Errorf("%s: parse WorkflowRun: %w", srcName, yamlErr)
			}
			*rawRuns = append(*rawRuns, wr)
			continue
		}
		obj, err := decodeObject(kind, []byte(doc))
		if err != nil {
			return fmt.Errorf("%s: %w", srcName, err)
		}
		if obj != nil {
			if obj.GetNamespace() == "" {
				obj.SetNamespace(localDefaultNamespace)
			}
			key := objectKey(obj)
			seen[key] = obj
		}
	}
	return nil
}

// indexWorkflowRuns defaults each WorkflowRun's namespace and indexes it by
// "namespace/workflowRef.name" for mergeInputValues lookup, mirroring the cross-namespace
// matching ResolveWorkflow uses to pick a Workflow by name. A WorkflowRun that declares no
// namespace anywhere (neither metadata.namespace nor spec.workflowRef.namespace) is rebound to
// the namespace of the single loaded Workflow matching its workflowRef.name -- otherwise it
// would key as "default/<name>" while the Workflow resolves into its own declared namespace
// (e.g. "ottoflow"), and spec.inputValues would silently never be applied. If more than one
// loaded Workflow shares that name, the ambiguity is a hard error rather than a silent drop.
func indexWorkflowRuns(
	runs []*ottoflowv1alpha1.WorkflowRun, seen map[string]client.Object,
) (map[string]*ottoflowv1alpha1.WorkflowRun, error) {
	indexed := make(map[string]*ottoflowv1alpha1.WorkflowRun, len(runs))
	for _, wr := range runs {
		ns := wr.Spec.WorkflowRef.Namespace
		if ns == "" {
			ns = wr.Namespace
		}
		if ns == "" {
			matches := findWorkflowsByName(seen, wr.Spec.WorkflowRef.Name)
			switch len(matches) {
			case 0:
				ns = localDefaultNamespace
			case 1:
				ns = matches[0].Namespace
			default:
				namespaces := make([]string, 0, len(matches))
				for _, wf := range matches {
					namespaces = append(namespaces, wf.Namespace)
				}
				return nil, fmt.Errorf(
					"WorkflowRun for workflow %q declares no namespace, and it is loaded in "+
						"multiple namespaces (%s); set spec.workflowRef.namespace to disambiguate",
					wr.Spec.WorkflowRef.Name, strings.Join(namespaces, ", "),
				)
			}
		}
		if wr.Namespace == "" {
			wr.Namespace = localDefaultNamespace
		}
		indexed[ns+"/"+wr.Spec.WorkflowRef.Name] = wr
	}
	return indexed, nil
}

// findWorkflowsByName returns every loaded Workflow named name.
func findWorkflowsByName(seen map[string]client.Object, name string) []*ottoflowv1alpha1.Workflow {
	var matches []*ottoflowv1alpha1.Workflow
	for _, obj := range seen {
		if wf, ok := obj.(*ottoflowv1alpha1.Workflow); ok && wf.Name == name {
			matches = append(matches, wf)
		}
	}
	return matches
}

// buildControlClient builds the fake control-plane client from the deduplicated set of
// loaded objects. Shared by LoadFromDirectory and LoadFromReader.
func (e *LocalWorkflowExecutor) buildControlClient(seen map[string]client.Object) error {
	objects := make([]client.Object, 0, len(seen))
	for _, obj := range seen {
		objects = append(objects, obj)
	}

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		return err
	}
	if err := ottoflowv1alpha1.AddToScheme(s); err != nil {
		return err
	}
	builder := fake.NewClientBuilder().WithScheme(s)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	e.controlClient = builder.Build()
	return nil
}

// documentKind reports the kind a YAML document declares, without requiring its body to
// be well-formed. An empty kind means the document declares none and is not ours.
func documentKind(data []byte) (string, error) {
	var meta metav1.TypeMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse YAML document: %w", err)
	}
	if meta.APIVersion == "" {
		return "", nil
	}
	return meta.Kind, nil
}

// decodeObject decodes a document of the given kind. A document that declares a kind we
// own but whose body does not parse is a broken manifest, not an unknown kind: returning
// an error keeps it from being silently dropped from the loaded set, which made
// `validate --workflow-dir` omit it from the results entirely rather than fail.
func decodeObject(kind string, data []byte) (client.Object, error) {
	var obj client.Object
	switch kind {
	case "Workflow":
		obj = &ottoflowv1alpha1.Workflow{}
	case "Agent":
		obj = &ottoflowv1alpha1.Agent{}
	case "MCPServer":
		obj = &ottoflowv1alpha1.MCPServer{}
	case "StepTemplate":
		obj = &ottoflowv1alpha1.StepTemplate{}
	case "Secret":
		obj = &corev1.Secret{}
	default:
		// Not a kind local mode loads (a ConfigMap sample, a Namespace, ...). Ignore it.
		return nil, nil
	}
	if err := yaml.Unmarshal(data, obj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", kind, err)
	}
	return obj, nil
}

func objectKey(obj client.Object) string {
	// Use type + namespace + name so we dedupe correctly even when GVK is not set on decoded YAML.
	ns := obj.GetNamespace()
	if ns == "" {
		ns = localDefaultNamespace
	}
	return fmt.Sprintf("%s/%s/%s", reflect.TypeOf(obj).String(), ns, obj.GetName())
}

// ControlClient returns the fake control-plane client the loader populated from the loaded
// manifests, so callers outside this package (e.g. `ottoflow validate`) can run the same
// reference-existence checks the local executor itself relies on.
func (e *LocalWorkflowExecutor) ControlClient() client.Client {
	return e.controlClient
}

// LoadedWorkflowRun pairs a loaded WorkflowRun with the namespace the loader
// resolved it into (recovered from the index key), which is the namespace the
// runtime uses to find its Workflow. This can differ from Run.Namespace, which
// indexWorkflowRuns force-defaults to "default".
// NOTE: the index map keys by resolvedNs + "/" + workflowRef.Name, so two runs
// referencing the same Workflow in one namespace collide last-wins -- this list
// therefore under-reports such duplicates (a benign under-check, never a false positive).
type LoadedWorkflowRun struct {
	ResolvedNamespace string
	Run               *ottoflowv1alpha1.WorkflowRun
}

// ListWorkflowRuns returns every loaded WorkflowRun paired with the namespace the loader
// resolved it into, sorted by (ResolvedNamespace, Run.Name) so callers that print one line
// per run (e.g. `validate --workflow-dir`'s FAIL workflowRun output) get stable, reproducible
// ordering instead of Go's randomized map iteration order.
func (e *LocalWorkflowExecutor) ListWorkflowRuns() []LoadedWorkflowRun {
	out := make([]LoadedWorkflowRun, 0, len(e.workflowRuns))
	for key, wr := range e.workflowRuns {
		// ResolvedNamespace is recovered from the index key ("namespace/workflowRef.name")
		// rather than carried as its own field on LoadedWorkflowRun/indexWorkflowRuns: the
		// index map is also read directly by mergeInputValues and workflowRunNameMismatch,
		// and threading an explicit namespace field through those call sites for this one
		// caller wasn't worth the churn.
		resolvedNS := strings.TrimSuffix(key, "/"+wr.Spec.WorkflowRef.Name)
		out = append(out, LoadedWorkflowRun{ResolvedNamespace: resolvedNS, Run: wr})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResolvedNamespace != out[j].ResolvedNamespace {
			return out[i].ResolvedNamespace < out[j].ResolvedNamespace
		}
		return out[i].Run.Name < out[j].Run.Name
	})
	return out
}

// GetWorkflow returns a Workflow by name and namespace from the loaded manifests.
func (e *LocalWorkflowExecutor) GetWorkflow(ctx context.Context, name, namespace string) (*ottoflowv1alpha1.Workflow, error) {
	if e.controlClient == nil {
		return nil, fmt.Errorf("no manifests loaded: call LoadFromDirectory first")
	}
	wf := &ottoflowv1alpha1.Workflow{}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := e.controlClient.Get(ctx, key, wf); err != nil {
		return nil, fmt.Errorf("workflow %s/%s: %w", namespace, name, err)
	}
	return wf, nil
}

// ListWorkflows returns all Workflow objects loaded from the directory.
func (e *LocalWorkflowExecutor) ListWorkflows(ctx context.Context) ([]*ottoflowv1alpha1.Workflow, error) {
	if e.controlClient == nil {
		return nil, fmt.Errorf("no manifests loaded: call LoadFromDirectory first")
	}
	list := &ottoflowv1alpha1.WorkflowList{}
	if err := e.controlClient.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	out := make([]*ottoflowv1alpha1.Workflow, len(list.Items))
	for i := range list.Items {
		out[i] = &list.Items[i]
	}
	return out, nil
}

// ResolveWorkflow picks the Workflow to run from the loaded manifests. If name is empty and
// exactly one Workflow was loaded, that one is used. If name is given, it must match exactly
// one loaded Workflow. namespace is used only to disambiguate an otherwise-ambiguous match
// (the same name, or no name, loaded in more than one namespace) -- pass "" when the caller has
// no namespace preference. Every other case returns an error that lists what was actually
// loaded, so the caller can point the user at the fix (name a workflow, add one, or
// disambiguate).
func (e *LocalWorkflowExecutor) ResolveWorkflow(ctx context.Context, name, namespace string) (workflowName, resolvedNamespace string, err error) {
	wfs, err := e.ListWorkflows(ctx)
	if err != nil {
		return "", "", err
	}
	if len(wfs) == 0 {
		if len(e.workflowRuns) > 0 {
			return "", "", fmt.Errorf("input contains only a WorkflowRun; include its Workflow in the same input to run locally")
		}
		return "", "", fmt.Errorf("no Workflow found in input")
	}

	if name == "" {
		if len(wfs) == 1 {
			return wfs[0].Name, wfs[0].Namespace, nil
		}
		if wf := singleWorkflowInNamespace(wfs, namespace); wf != nil {
			return wf.Name, wf.Namespace, nil
		}
		return "", "", fmt.Errorf("input contains multiple workflows, specify one with --workflow: %s", describeWorkflows(wfs))
	}

	var matches []*ottoflowv1alpha1.Workflow
	for _, wf := range wfs {
		if wf.Name == name {
			matches = append(matches, wf)
		}
	}
	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("workflow %q not found in input; loaded workflows: %s", name, describeWorkflows(wfs))
	case 1:
		return matches[0].Name, matches[0].Namespace, nil
	default:
		if wf := singleWorkflowInNamespace(matches, namespace); wf != nil {
			return wf.Name, wf.Namespace, nil
		}
		namespaces := make([]string, 0, len(matches))
		for _, wf := range matches {
			namespaces = append(namespaces, wf.Namespace)
		}
		return "", "", fmt.Errorf("workflow %q found in multiple namespaces, specify one with --namespace: %s", name, strings.Join(namespaces, ", "))
	}
}

// singleWorkflowInNamespace returns the one workflow in wfs whose namespace matches namespace,
// or nil if namespace is empty or zero/more-than-one workflow matches. Used to let an explicit
// namespace disambiguate an otherwise-ambiguous ResolveWorkflow match.
func singleWorkflowInNamespace(wfs []*ottoflowv1alpha1.Workflow, namespace string) *ottoflowv1alpha1.Workflow {
	if namespace == "" {
		return nil
	}
	var match *ottoflowv1alpha1.Workflow
	for _, wf := range wfs {
		if wf.Namespace == namespace {
			if match != nil {
				return nil // more than one in this namespace; not disambiguated
			}
			match = wf
		}
	}
	return match
}

// describeWorkflows renders "name (namespace)" for each workflow, for use in error messages.
func describeWorkflows(wfs []*ottoflowv1alpha1.Workflow) string {
	names := make([]string, 0, len(wfs))
	for _, wf := range wfs {
		names = append(names, fmt.Sprintf("%s (%s)", wf.Name, wf.Namespace))
	}
	return strings.Join(names, ", ")
}

// ExecuteWorkflow runs the given workflow in-process using manifests loaded from the workflow directory.
func (e *LocalWorkflowExecutor) ExecuteWorkflow(ctx context.Context, workflowName, namespace string, inputValues map[string]string) (*ottoflowv1alpha1.WorkflowRun, error) {
	wf, err := e.GetWorkflow(ctx, workflowName, namespace)
	if err != nil {
		return nil, err
	}

	// Merge inputs: CLI flags > WorkflowRun YAML > workflow defaults (handled later by context manager)
	mergedInputs := e.mergeInputValues(workflowName, namespace, inputValues)

	runName := fmt.Sprintf("local-%s-%d", workflowName, time.Now().UnixNano())
	workflowRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: namespace,
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: workflowName, Namespace: namespace},
			InputValues: mergedInputs,
		},
	}

	// Fail fast: validate agent providers and prometheus requirements before execution.
	for _, step := range wf.Spec.Steps {
		if step.AgentRef != nil {
			agentNs := step.AgentRef.Namespace
			if agentNs == "" {
				agentNs = namespace
			}
			ag := &ottoflowv1alpha1.Agent{}
			agKey := client.ObjectKey{Name: step.AgentRef.Name, Namespace: agentNs}
			if err := e.controlClient.Get(ctx, agKey, ag); err == nil {
				if e.providerOverride == "" && !agent.IsValidProvider(ag.Spec.ModelProvider) {
					if ag.Spec.ModelProvider == "" {
						return nil, fmt.Errorf(
							"agent %q: spec.modelProvider is required (step %q); valid values: "+
								"nirmata, openai, anthropic, azure-openai, google, gemini, local",
							ag.Name, step.Name,
						)
					}
					return nil, fmt.Errorf(
						"agent %q has unknown provider %q (step %q); valid providers: "+
							"nirmata, openai, anthropic, azure-openai, google, gemini, local",
						ag.Name, ag.Spec.ModelProvider, step.Name,
					)
				}
			}
		}
		if step.PrometheusQuery != nil && e.prometheusURL == "" {
			return nil, fmt.Errorf(
				"workflow %q requires Prometheus (step %q uses prometheusQuery)"+
					" but --prometheus-url is not set",
				workflowName, step.Name,
			)
		}
		// UX only: the executor's own nil-client guards (resource_query_executor.go,
		// mutate_executor.go) would catch this too, but only after the workflow has already
		// started, and forEach/subworkflow steps can hide it several layers deep. Catching it
		// here, before any step runs, gives a clean top-level error instead of a mid-run failure.
		if e.targetClient == nil && (step.ResourceQuery != nil || step.Mutate != nil) {
			return nil, fmt.Errorf(
				"workflow %q step %q: kubernetes client not available (no kubeconfig); this workflow requires a cluster",
				workflowName, step.Name,
			)
		}
	}

	var prometheusClient workflowexecutor.PrometheusClient = &workflowexecutor.NoOpPrometheusClient{}
	if e.prometheusURL != "" {
		pc, err := workflowexecutor.NewHTTPPrometheusClient(e.prometheusURL)
		if err != nil {
			return nil, fmt.Errorf("prometheus client: %w", err)
		}
		prometheusClient = pc
	}

	maxWorkers := e.maxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 5
	}

	exec, err := workflowexecutor.NewWorkflowExecutorWithClientsAndAgentExecutor(
		e.controlClient,
		e.targetClient,
		e.metricsClient,
		&workflowexecutor.NoOpCustomMetricsClient{},
		prometheusClient,
		workflowRun,
		nil,
		nil,
		true, // localExecutionMode: run agent steps in-process
		0,
		maxWorkers,
		nil,
		nil, // kubeClient: CLI local mode does not use resource.GetLogs
	)
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}
	if e.providerOverride != "" || e.modelOverride != "" {
		exec.SetAgentOverrides(e.providerOverride, e.modelOverride)
	}

	defer exec.Close() //nolint:errcheck

	if e.progressCallback != nil {
		exec.SetProgressCallback(workflowexecutor.ProgressCallback(e.progressCallback))
	}

	if err := exec.ExecuteWorkflow(ctx, wf, workflowRun); err != nil {
		return workflowRun, fmt.Errorf("execute workflow: %w", err)
	}
	return workflowRun, nil
}

// mergeInputValues builds the final input map: CLI flags override WorkflowRun YAML values.
func (e *LocalWorkflowExecutor) mergeInputValues(workflowName, namespace string, cliInputs map[string]string) map[string]string {
	if len(e.workflowRuns) == 0 {
		return cliInputs
	}
	key := namespace + "/" + workflowName
	wr, ok := e.workflowRuns[key]
	if !ok {
		if e.workflowRunNameMismatch(workflowName, key) {
			// A loaded WorkflowRun does reference this workflow by name, just under a
			// different namespace key -- likely a real mismatch worth flagging, not just an
			// unrelated WorkflowRun for some other workflow in the same input.
			klog.Warningf("mergeInputValues: a loaded WorkflowRun references workflow %q but not in namespace %q; "+
				"its spec.inputValues will not be applied", workflowName, namespace)
		} else {
			klog.V(2).InfoS("mergeInputValues: no loaded WorkflowRun references this workflow",
				"workflow", workflowName, "namespace", namespace)
		}
		return cliInputs
	}
	if len(wr.Spec.InputValues) == 0 {
		return cliInputs
	}
	merged := make(map[string]string, len(wr.Spec.InputValues)+len(cliInputs))
	for k, v := range wr.Spec.InputValues {
		merged[k] = v
	}
	for k, v := range cliInputs {
		merged[k] = v
	}
	return merged
}

// workflowRunNameMismatch reports whether any loaded WorkflowRun references workflowName by
// name (spec.workflowRef.name) but is indexed under a namespace key other than wantKey. Used
// by mergeInputValues to tell a real namespace mismatch (worth a warning) apart from a
// WorkflowRun that simply targets some other workflow in the same input (expected in a
// multi-workflow tree, not worth warning about).
func (e *LocalWorkflowExecutor) workflowRunNameMismatch(workflowName, wantKey string) bool {
	for key, wr := range e.workflowRuns {
		if wr.Spec.WorkflowRef.Name == workflowName && key != wantKey {
			return true
		}
	}
	return false
}
