/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	celapi "github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	lru "github.com/hashicorp/golang-lru/v2"
	kyvernoglobalcontext "github.com/kyverno/sdk/extensions/cel/libs/globalcontext"
	kyvernohttp "github.com/kyverno/sdk/extensions/cel/libs/http"
	kyvernoimagedata "github.com/kyverno/sdk/extensions/cel/libs/imagedata"
	kyvernojson "github.com/kyverno/sdk/extensions/cel/libs/json"
	kyvernoresource "github.com/kyverno/sdk/extensions/cel/libs/resource"
	kyvernoyaml "github.com/kyverno/sdk/extensions/cel/libs/yaml"
	apiservercel "k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/kubernetes"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// macroContextHolder carries the per-evaluation context into CEL macro function
// closures (resourceLogs, resourceEvents, resourceMetrics, prometheusMetrics).
// Because CEL function bindings are registered at environment-creation time and
// cannot receive a context argument at call time, the evaluator sets the holder
// before each prg.Eval() call of an expression that calls one of them so macros always use the current caller context
// rather than context.Background().
//
// Each evaluator owns its holder; compiled programs are never shared across evaluators.
type macroContextHolder struct {
	mu      sync.RWMutex
	ctx     context.Context
	readers []string
}

type cachedProgram struct {
	prg           celapi.Program
	readsMacroCtx bool
}

func (h *macroContextHolder) get() context.Context {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.ctx == nil {
		return context.Background()
	}
	return h.ctx
}

func (h *macroContextHolder) set(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ctx = ctx
}

const (
	// defaultCELCacheSize is the default maximum number of compiled CEL programs to cache
	defaultCELCacheSize = 1000

	// DefaultCELCostLimit is the default CEL evaluation cost budget. Kept conservative
	// for user-authored workflows to limit worst-case CPU per evaluation. For cost-heavy
	// expressions (e.g. Prometheus podMetrics over many pods/samples), set
	// Workflow.Spec.CELCostLimit (spec.celCostLimit) on the workflow.
	DefaultCELCostLimit uint64 = 2 << 20 // 2 * 1024 * 1024
)

// CELEvaluator evaluates CEL expressions with workflow context
type CELEvaluator struct {
	env                 *celapi.Env
	client              client.Client
	metricsClient       metricsclientset.Interface
	customMetricsClient CustomMetricsClient
	prometheusClient    PrometheusClient
	workflowRun         *ottoflowv1alpha1.WorkflowRun
	// programCache caches compiled CEL programs keyed by expression string
	// Using hashicorp/golang-lru for thread-safe bounded LRU cache
	programCache *lru.Cache[string, cachedProgram]
	// programOptions stores ProgramOptions from all libraries that need to be passed when creating programs
	programOptions []celapi.ProgramOption
	// celCostLimit is the cost budget for CEL evaluation (per-workflow when set via SetCELCostLimit)
	celCostLimit uint64
	// imageDataFetcher is optional; when set (e.g. in tests), used when building image context for CEL
	imageDataFetcher ImageDataFetcher
	// kubeClient is the typed Kubernetes clientset used by resource.GetLogs and resourceLogs.
	// CELEvaluator is constructed fresh per WorkflowRun; never pool or reuse across runs,
	// as namespace fallback and log fetches are scoped to the in-flight WorkflowRun.
	kubeClient kubernetes.Interface
	// macroCtx is updated by EvaluateExpression so that CEL macro function closures
	// (resourceLogs, resourceEvents, etc.) use the caller's context rather than context.Background().
	macroCtx *macroContextHolder
	// macroEvalMu serialises the macroCtx.set + prg.Eval window so concurrent forEach
	// goroutines that share this evaluator cannot observe each other's context in macro
	// closures (resourceLogs, resourceEvents, resourceMetrics, prometheusMetrics).
	macroEvalMu sync.Mutex
	// macroOverloads holds the overload IDs of the functions named in macroCtx.readers.
	macroOverloads map[string]struct{}
	// resourceListPageSize overrides the default 500-item page size for resource.List()
	// CEL expressions. 0 means use the default. Set via SetResourceListPageSize.
	resourceListPageSize int64
}

// NewCELEvaluator creates a new CEL evaluator with Resource library support
func NewCELEvaluator(client client.Client, workflowRun *ottoflowv1alpha1.WorkflowRun) (*CELEvaluator, error) {
	return NewCELEvaluatorWithMetrics(client, nil, nil, nil, nil, workflowRun, 0, nil)
}

// NewValidationCELEnv returns a CEL environment for compile-time syntax checking only.
// Nil clients are safe: resource macro bindings are closures invoked only at Eval() time.
func NewValidationCELEnv() (*celapi.Env, error) {
	env, _, err := createCELEnvironment(nil, nil, nil, nil, nil, "", &macroContextHolder{})
	return env, err
}

// createCELEnvironment builds a CEL environment with all OttoFlow libraries.
// Shared by CELEvaluator (per-run) and CELCompilationCache (shared singleton).
// macroCtx is used by resource macro function closures to propagate the caller's
// context; callers should pass their own *macroContextHolder instance.
// kubeClient is optional; when nil, resource.GetLogs and resourceLogs return a CEL error.
func createCELEnvironment(
	k8sClient client.Client,
	kubeClient kubernetes.Interface,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	namespace string,
	macroCtx *macroContextHolder,
) (*celapi.Env, []celapi.ProgramOption, error) {
	baseEnvSet := apiservercel.MustBaseEnvSet(apiservercel.DefaultCompatibilityVersion())

	jsonImpl := &kyvernojson.JsonImpl{}
	yamlImpl := &kyvernoyaml.YamlImpl{}

	kyvernoOpts := GetKyvernoCELOptionsWithImpls(namespace, jsonImpl, yamlImpl)
	kyvernoProgramOpts := GetKyvernoCELProgramOptions(namespace, jsonImpl, yamlImpl)

	resourceMacroOpts, err := GetResourceMacroOptionsWithMetrics(k8sClient, kubeClient, metricsClient, customMetricsClient, prometheusClient, namespace, macroCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get resource macro options: %w", err)
	}

	stringFormatOpts := GetStringFormatOptions()
	listFuncOpts := GetListFunctions()

	floOpts := make([]celapi.EnvOption, 0, 17)
	floOpts = append(floOpts,
		celapi.Variable("inputs", celapi.MapType(celapi.StringType, celapi.DynType)),
		celapi.Variable("expressions", celapi.MapType(celapi.StringType, celapi.DynType)),
		celapi.Variable("variables", celapi.MapType(celapi.StringType, celapi.DynType)),
		celapi.Variable("steps", celapi.MapType(celapi.StringType, celapi.MapType(celapi.StringType, celapi.DynType))),
		celapi.Variable("stepStatus", celapi.MapType(celapi.StringType, celapi.MapType(celapi.StringType, celapi.DynType))),
		celapi.Variable("outputs", celapi.MapType(celapi.StringType, celapi.DynType)),
		celapi.Variable("agentResponse", celapi.StringType),
		celapi.Variable("agentOutputs", celapi.DynType),
		celapi.Variable("http", kyvernohttp.ContextType),
		celapi.Variable("globalContext", kyvernoglobalcontext.ContextType),
		celapi.Variable("image", kyvernoimagedata.ContextType),
		celapi.Variable("object", celapi.DynType),
		celapi.Variable("items", celapi.DynType),
		celapi.Variable("toolResult", celapi.DynType),
		celapi.Variable("a2aResult", celapi.DynType),
		// openReport step result (reportResult.mode, .name, .namespace, .summary, .data)
		celapi.Variable("reportResult", celapi.DynType),
		// forEach: default item variable (users may override via ItemVariable,
		// but `item` must be declared so static compilation succeeds)
		celapi.Variable("item", celapi.DynType),
		// Prometheus query step result (result.type, result.samples, result.value)
		celapi.Variable("result", celapi.DynType),
	)
	floOpts = append(floOpts, resourceMacroOpts...)
	floOpts = append(floOpts, stringFormatOpts...)
	floOpts = append(floOpts, listFuncOpts...)

	extendedEnvSet, err := baseEnvSet.Extend(
		apiservercel.VersionedOptions{
			IntroducedVersion: apiservercel.DefaultCompatibilityVersion(),
			EnvOptions:        floOpts,
		},
		apiservercel.VersionedOptions{
			IntroducedVersion: apiservercel.DefaultCompatibilityVersion(),
			EnvOptions:        kyvernoOpts,
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extend EnvSet: %w", err)
	}

	env, err := extendedEnvSet.Env(apiservercel.StoredExpressions)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get environment from EnvSet: %w", err)
	}

	// Install the unstructured type adapter so typed Kubernetes objects bound as
	// CEL variables convert correctly under cel-go 0.31 (see cel_unstructured_adapter.go).
	env, err = env.Extend(celapi.CustomTypeAdapter(unstructuredAdapter{base: env.CELTypeAdapter()}))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to install unstructured type adapter: %w", err)
	}

	return env, kyvernoProgramOpts, nil
}

// NewCELEvaluatorWithMetrics creates a new CEL evaluator with optional metrics clients and optional ImageDataFetcher.
// celCacheSize is the maximum number of compiled CEL expressions to cache (0 uses default).
// imageDataFetcher is optional; when nil, default (Kyverno loader) is used.
// kubeClient is optional; when nil, resource.GetLogs and resourceLogs return a CEL error.
// CELEvaluator is per-WorkflowRun; never pool or reuse across runs.
func NewCELEvaluatorWithMetrics(
	client client.Client,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	kubeClient kubernetes.Interface,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	celCacheSize int,
	imageDataFetcher ImageDataFetcher,
) (*CELEvaluator, error) {
	namespace := ""
	if workflowRun != nil {
		namespace = workflowRun.Namespace
	}

	macroCtx := &macroContextHolder{}
	env, kyvernoProgramOpts, err := createCELEnvironment(client, kubeClient, metricsClient, customMetricsClient, prometheusClient, namespace, macroCtx)
	if err != nil {
		return nil, err
	}

	cacheSize := celCacheSize
	if cacheSize <= 0 {
		cacheSize = defaultCELCacheSize
	}

	programCache, err := lru.New[string, cachedProgram](cacheSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL program cache: %w", err)
	}

	macroOverloads := map[string]struct{}{}
	for _, name := range macroCtx.readers {
		if fd, ok := env.Functions()[name]; ok {
			for _, o := range fd.OverloadDecls() {
				macroOverloads[o.ID()] = struct{}{}
			}
		}
	}

	return &CELEvaluator{
		macroOverloads:      macroOverloads,
		env:                 env,
		client:              client,
		metricsClient:       metricsClient,
		customMetricsClient: customMetricsClient,
		prometheusClient:    prometheusClient,
		kubeClient:          kubeClient,
		workflowRun:         workflowRun,
		programCache:        programCache,
		programOptions:      kyvernoProgramOpts,
		celCostLimit:        DefaultCELCostLimit,
		imageDataFetcher:    imageDataFetcher,
		macroCtx:            macroCtx,
	}, nil
}

// ResolveCELCostLimit returns the CEL cost limit for a workflow (spec.CELCostLimit or default).
func ResolveCELCostLimit(spec *ottoflowv1alpha1.WorkflowSpec) uint64 {
	if spec == nil || spec.CELCostLimit == nil || *spec.CELCostLimit <= 0 {
		return DefaultCELCostLimit
	}
	return uint64(*spec.CELCostLimit)
}

// SetCELCostLimit sets the cost budget for CEL evaluation (e.g. from Workflow.Spec.CELCostLimit).
// Used when compiling expressions on the fly; preloaded programs already have the workflow's limit.
func (e *CELEvaluator) SetCELCostLimit(limit uint64) {
	e.celCostLimit = limit
}

// SetResourceListPageSize overrides the default 500-item page size used by resource.List() CEL
// expressions. Call this when a workflow specifies a smaller page size for resource-heavy types.
// A value of 0 or negative is ignored (default applies).
func (e *CELEvaluator) SetResourceListPageSize(n int64) {
	if n > 0 {
		e.resourceListPageSize = n
	}
}

// SetImageDataFetcher sets the optional image data fetcher (for tests). When nil, default is used.
func (e *CELEvaluator) SetImageDataFetcher(f ImageDataFetcher) {
	e.imageDataFetcher = f
}

// BuildVariableMap creates a variable map from context for CEL evaluation
// Context structure:
//   - inputs: map[string]interface{} - workflow input values
//   - expressions: map[string]interface{} - expression results from current step
//   - variables: map[string]interface{} - flat variables (no namespacing)
//   - steps: map[string]map[string]interface{} - step results (steps.step-name.field)
func (e *CELEvaluator) BuildVariableMap(context map[string]interface{}) map[string]interface{} {
	vars := make(map[string]interface{})

	// Add inputs - accessible as inputs.name
	if inputs, ok := context["inputs"].(map[string]interface{}); ok {
		vars["inputs"] = inputs
	} else {
		vars["inputs"] = make(map[string]interface{})
	}

	// Add expressions - accessible as expressions.name
	if expressions, ok := context["expressions"].(map[string]interface{}); ok {
		vars["expressions"] = expressions
	} else {
		vars["expressions"] = make(map[string]interface{})
	}

	// Add variables - accessible as variables.name (flat, no namespacing)
	if variables, ok := context["variables"].(map[string]interface{}); ok {
		vars["variables"] = variables
	} else {
		vars["variables"] = make(map[string]interface{})
	}

	// Add steps - accessible as steps.step-name.field
	if steps, ok := context["steps"].(map[string]interface{}); ok {
		vars["steps"] = steps
	} else {
		vars["steps"] = make(map[string]interface{})
	}

	// Add stepStatus - accessible as stepStatus.step-name.{phase,succeeded,failed,error}
	if stepStatus, ok := context["stepStatus"].(map[string]interface{}); ok {
		vars["stepStatus"] = stepStatus
	} else {
		vars["stepStatus"] = make(map[string]interface{})
	}

	// Add outputs - accessible as outputs.name (workflow-level outputs, for metric labels)
	if outputs, ok := context["outputs"].(map[string]interface{}); ok {
		vars["outputs"] = outputs
	} else {
		vars["outputs"] = make(map[string]interface{})
	}

	// Add agent step output context (set by agent executor when evaluating step outputs)
	if v, ok := context["agentResponse"]; ok {
		vars["agentResponse"] = v
	} else {
		vars["agentResponse"] = ""
	}
	if v, ok := context["agentOutputs"]; ok {
		vars["agentOutputs"] = v
	} else {
		vars["agentOutputs"] = map[string]interface{}{}
	}
	if v, ok := context["reportResult"]; ok {
		vars["reportResult"] = v
	} else {
		vars["reportResult"] = map[string]interface{}{}
	}

	// Add the current forEach item - accessible as bare `item`. Set by processForEachItem,
	// which is the only writer, so the key's presence is itself the "inside a forEach" signal.
	// Deliberately left unbound outside a loop: `item` is declared in the env, so an absent
	// binding fails with "no such attribute(s): item" -- which names the actual mistake.
	// Binding it to nil instead turned that into "no such key: name" (pointing at the field,
	// not the missing loop) and let a bare `item` evaluate to null with no error at all.
	if v, ok := context["item"]; ok {
		vars["item"] = v
	}

	return vars
}

// getOrCompileProgram gets a compiled program from cache or compiles it if not cached
// Returns the compiled program and any error encountered during compilation
func (e *CELEvaluator) getOrCompileProgram(expr string) (cachedProgram, error) {
	// Check cache first
	if cached, ok := e.programCache.Get(expr); ok {
		return cached, nil
	}

	// Compile expression
	ast, issues := e.env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return cachedProgram{}, fmt.Errorf("failed to compile expression '%s': %w", expr, issues.Err())
	}

	reads := false
	for _, ri := range ast.NativeRep().ReferenceMap() {
		for _, id := range ri.OverloadIDs {
			if _, ok := e.macroOverloads[id]; ok {
				reads = true
			}
		}
	}

	// ProgramOptions: When using EnvSet.Extend(), ProgramOptions from libraries should be
	// automatically embedded in the environment. However, some libraries (like time) may
	// require explicit ProgramOptions to be passed when creating programs.
	//
	// Try creating program with just eval options first - if libraries provide ProgramOptions
	// through the environment, they should be automatically included.
	limit := e.celCostLimit
	if limit == 0 {
		limit = DefaultCELCostLimit
	}
	programOpts := []celapi.ProgramOption{
		celapi.EvalOptions(celapi.OptOptimize),
		celapi.CostLimit(limit),
	}

	// If we have explicit ProgramOptions from libraries, add them
	// JSON/YAML libraries require ProgramOptions to provide runtime variables
	if len(e.programOptions) > 0 {
		programOpts = append(programOpts, e.programOptions...)
	}

	// Create program with ProgramOptions
	prg, err := e.env.Program(ast, programOpts...)
	if err != nil {
		return cachedProgram{}, fmt.Errorf("failed to create program: %w", err)
	}

	// Store in cache (LRU will handle eviction if at capacity)
	c := cachedProgram{prg: prg, readsMacroCtx: reads}
	e.programCache.Add(expr, c)
	return c, nil
}

// EvaluateExpression evaluates a CEL expression with the given variables
func (e *CELEvaluator) EvaluateExpression(ctx context.Context, expr string, vars map[string]interface{}) (interface{}, error) {
	// Get or compile program (with caching)
	cp, err := e.getOrCompileProgram(expr)
	if err != nil {
		return nil, err
	}
	prg := cp.prg

	// Add context variables to vars map (matching Kyverno's pattern)
	// Kyverno provides http, globalContext, image, resource as variables in the evaluation data map
	if vars == nil {
		vars = make(map[string]interface{})
	}
	vars["http"] = kyvernohttp.Context{ContextInterface: NewCELHTTPContext()}
	vars["globalContext"] = kyvernoglobalcontext.Context{ContextInterface: noopGlobalContext{}}

	// Add imageData context with Kyverno's imageData loader implementation
	namespace := ""
	if e.workflowRun != nil {
		namespace = e.workflowRun.Namespace
	}
	imageDataCtx, err := NewImageDataContext(ctx, e.client, namespace, e.imageDataFetcher)
	if err != nil {
		return nil, fmt.Errorf("failed to create imageData context: %w", err)
	}
	vars["image"] = kyvernoimagedata.Context{ContextInterface: imageDataCtx}

	// Add resource context with controller-runtime client implementation (for CEL expressions that use resource.Get/List).
	// If vars["resource"] or vars["items"] are already set (e.g. by resourceQuery step output evaluation), do not overwrite.
	if _, set := vars["resource"]; !set {
		resCtx := &resourceContext{
			client:    e.client,
			namespace: namespace,
			pageSize:  e.resourceListPageSize,
			ctx:       ctx,
		}
		vars["resource"] = kyvernoresource.Context{ContextInterface: resCtx}
	}

	// Evaluate with variables. When macro functions are registered (resourceLogs, resourceEvents,
	// etc.), their closures read context from the shared macroContextHolder. Serialise the
	// set+eval window so concurrent forEach goroutines can't overwrite each other's context.
	var out ref.Val
	if e.macroCtx != nil && cp.readsMacroCtx {
		e.macroEvalMu.Lock()
		e.macroCtx.set(ctx)
		out, _, err = prg.Eval(vars)
		e.macroEvalMu.Unlock()
	} else {
		out, _, err = prg.Eval(vars)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate expression '%s': %w", expr, err)
	}

	// Convert CEL value to native Go type for proper JSON serialization
	return convertCELValueToNative(out), nil
}

// convertCELValueToNative converts a CEL ref.Val to a native Go type
// This ensures proper JSON serialization of CEL values (maps, lists, etc.)
func convertCELValueToNative(val ref.Val) interface{} {
	if val == nil {
		return nil
	}

	// Handle Mapper (map-like types) by iterating and extracting key-value pairs
	if mapper, ok := val.(traits.Mapper); ok {
		result := make(map[string]interface{})
		it := mapper.Iterator()
		hasNext := it.HasNext()
		for hasNext.Equal(types.True) == types.True {
			keyVal := it.Next()
			// The iterator returns keys for maps
			keyStr := fmt.Sprintf("%v", convertCELValueToNative(keyVal))
			valVal, found := mapper.Find(keyVal)
			if found {
				result[keyStr] = convertCELValueToNative(valVal)
			}
			hasNext = it.HasNext()
		}
		return result
	}

	// Handle Lister (list-like types)
	if lister, ok := val.(traits.Lister); ok {
		size := lister.Size()
		sizeVal := size.Value()
		sizeInt := int64(0)
		if s, ok := sizeVal.(int64); ok {
			sizeInt = s
		} else if s, ok := sizeVal.(int); ok {
			sizeInt = int64(s)
		}
		result := make([]interface{}, sizeInt)
		for i := int64(0); i < sizeInt; i++ {
			elem := lister.Get(types.Int(i))
			result[i] = convertCELValueToNative(elem)
		}
		return result
	}

	// Try ConvertToNative first, which handles CEL types properly
	if nativeVal, err := val.ConvertToNative(reflect.TypeOf((*interface{})(nil)).Elem()); err == nil {
		return convertCELValueToNativeRecursive(nativeVal)
	}

	// Fallback to Value() if ConvertToNative fails
	value := val.Value()

	// Recursively convert nested structures that might contain CEL types
	return convertCELValueToNativeRecursive(value)
}

// convertCELValueToNativeRecursive recursively converts nested structures
// that might contain CEL types (ref.Val) to native Go types
func convertCELValueToNativeRecursive(v interface{}) interface{} {
	if v == nil {
		return nil
	}

	// If it's a CEL ref.Val, convert it recursively
	if refVal, ok := v.(ref.Val); ok {
		// Try ConvertToNative first for proper conversion
		if nativeVal, err := refVal.ConvertToNative(reflect.TypeOf((*interface{})(nil)).Elem()); err == nil {
			return convertCELValueToNativeRecursive(nativeVal)
		}
		// Fallback to Value()
		underlyingValue := refVal.Value()
		return convertCELValueToNativeRecursive(underlyingValue)
	}

	// Handle structs that might be CEL wrapper types (e.g., with Adapter field)
	// Use reflection to extract actual data
	if reflect.TypeOf(v).Kind() == reflect.Struct {
		vValue := reflect.ValueOf(v)
		vType := vValue.Type()

		// Check if it has common CEL wrapper field names
		for _, fieldName := range []string{"Value", "value", "Data", "data", "Map", "map"} {
			if field := vValue.FieldByName(fieldName); field.IsValid() && field.CanInterface() {
				return convertCELValueToNativeRecursive(field.Interface())
			}
		}

		// If it's a struct with only an Adapter field (empty wrapper), try to find the actual data
		// or convert the whole struct to a map
		if vType.NumField() > 0 {
			// Try to convert struct to map[string]interface{}
			result := make(map[string]interface{})
			for i := 0; i < vType.NumField(); i++ {
				field := vType.Field(i)
				if fieldValue := vValue.Field(i); fieldValue.CanInterface() {
					// Skip Adapter field as it's just metadata
					if field.Name != "Adapter" {
						result[field.Name] = convertCELValueToNativeRecursive(fieldValue.Interface())
					}
				}
			}
			// Only return map if we found non-Adapter fields
			if len(result) > 0 {
				return result
			}
		}
	}

	// Handle maps
	if mapVal, ok := v.(map[string]interface{}); ok {
		result := make(map[string]interface{})
		for k, val := range mapVal {
			result[k] = convertCELValueToNativeRecursive(val)
		}
		return result
	}

	// Handle slices/arrays
	if sliceVal, ok := v.([]interface{}); ok {
		result := make([]interface{}, len(sliceVal))
		for i, val := range sliceVal {
			result[i] = convertCELValueToNativeRecursive(val)
		}
		return result
	}

	// For primitive types, return as-is
	return v
}

// EvaluateStepExpressions evaluates all expressions in a step sequentially
func (e *CELEvaluator) EvaluateStepExpressions(ctx context.Context, step ottoflowv1alpha1.Step, vars map[string]interface{}) (map[string]interface{}, error) {
	results := make(map[string]interface{})

	// Evaluate expressions sequentially
	for _, exprDef := range step.Expressions {
		result, err := e.EvaluateExpression(ctx, exprDef.Expression, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate expression '%s': %w", exprDef.Name, err)
		}

		results[exprDef.Name] = result

		// Update vars so subsequent expressions can reference this result
		if vars["expressions"] == nil {
			vars["expressions"] = make(map[string]interface{})
		}
		vars["expressions"].(map[string]interface{})[exprDef.Name] = result
	}

	return results, nil
}

// EvaluateStepOutputs evaluates all outputs in a step
func (e *CELEvaluator) EvaluateStepOutputs(ctx context.Context, step ottoflowv1alpha1.Step, vars map[string]interface{}) (map[string]interface{}, error) {
	outputs := make(map[string]interface{})

	for _, outputDef := range step.Outputs {
		var result interface{}
		var err error

		// Value field takes precedence over Expression field
		if outputDef.Value != nil {
			// Unmarshal JSON value to Go interface{}
			var valueData interface{}
			if err := json.Unmarshal(outputDef.Value.Raw, &valueData); err != nil {
				return nil, fmt.Errorf("failed to unmarshal output '%s' value: %w", outputDef.Name, err)
			}
			result, err = e.EvaluateOutputValue(ctx, valueData, vars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate output '%s' value: %w", outputDef.Name, err)
			}
		} else if outputDef.Expression != "" {
			result, err = e.EvaluateExpression(ctx, outputDef.Expression, vars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate output '%s' expression: %w", outputDef.Name, err)
			}
		} else {
			return nil, fmt.Errorf("output '%s' must specify either 'expression' or 'value'", outputDef.Name)
		}

		outputs[outputDef.Name] = result
	}

	return outputs, nil
}

// EvaluateOutputValue recursively evaluates CEL expressions in a YAML value structure
// String values that look like CEL expressions are evaluated; if evaluation fails, the literal string is used
func (e *CELEvaluator) EvaluateOutputValue(ctx context.Context, value interface{}, vars map[string]interface{}) (interface{}, error) {
	switch v := value.(type) {
	case string:
		// Try to evaluate as CEL expression
		// If it fails or doesn't look like a CEL expression, return as literal string
		result, err := e.tryEvaluateAsCEL(ctx, v, vars)
		if err == nil {
			return result, nil
		}
		// A cancel is not an ordinary evaluation failure: the literal fallback would store the expression text as the value.
		if ctx.Err() != nil {
			return nil, err
		}
		// Evaluation failed or not a CEL expression, return literal string
		return v, nil

	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, val := range v {
			evaluatedVal, err := e.EvaluateOutputValue(ctx, val, vars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate map key '%s': %w", k, err)
			}
			result[k] = evaluatedVal
		}
		return result, nil

	case []interface{}:
		result := make([]interface{}, len(v))
		for i, val := range v {
			evaluatedVal, err := e.EvaluateOutputValue(ctx, val, vars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate array element %d: %w", i, err)
			}
			result[i] = evaluatedVal
		}
		return result, nil

	default:
		// For other types (numbers, booleans, nil), return as-is
		return v, nil
	}
}

// tryEvaluateAsCEL attempts to evaluate a string as a CEL expression
// Returns the evaluated result if successful, or an error if evaluation fails
func (e *CELEvaluator) tryEvaluateAsCEL(ctx context.Context, expr string, vars map[string]interface{}) (interface{}, error) {
	// Quick check: if string doesn't contain dots or parentheses, it's likely not a CEL expression
	// This is a heuristic to avoid evaluating plain strings unnecessarily
	hasCELIndicators := false
	for _, char := range expr {
		if char == '.' || char == '(' || char == '[' {
			hasCELIndicators = true
			break
		}
	}

	// If it doesn't look like a CEL expression, don't try to evaluate it
	if !hasCELIndicators {
		return nil, fmt.Errorf("not a CEL expression")
	}

	// Try to compile and evaluate as CEL expression
	result, err := e.EvaluateExpression(ctx, expr, vars)
	if err != nil {
		return nil, fmt.Errorf("evaluation failed: %w", err)
	}

	return result, nil
}
