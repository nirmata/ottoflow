<div align="center">
  <img src="../../images/brand/ottoflow-horizontal-light.png" alt="OttoFlow Logo" width="300">
</div>

# Workflow Package

This package contains the core workflow management functionality, organized into two subpackages:


## Structure

```
internal/workflow/
├── controller/     # Kubernetes controllers for Workflow and WorkflowRun CRDs
└── executor/       # Workflow execution engine
```

## Subpackages

### `controller/` - Kubernetes Controllers

Manages Kubernetes reconciliation for Workflow and WorkflowRun resources:
- **WorkflowReconciler**: Registers/unregisters workflow triggers (cron and event)
- **WorkflowRunReconciler**: Orchestrates workflow execution by creating WorkflowExecutor instances
- **TriggerManager**: Manages cron triggers (CronJobs) and event triggers (resource watchers)

### `executor/` - Workflow Execution Engine

Core execution engine for workflows:
- **WorkflowExecutor**: Main orchestrator that executes workflow steps
- **ContextManager**: Manages workflow context (inputs, variables, outputs)
- **CELEvaluator**: Evaluates CEL expressions with Kyverno CEL libraries (via [kyverno/sdk/cel](https://github.com/kyverno/sdk/tree/main/cel))
- **DAG**: Dependency graph for step ordering
- Step executors: agent, MCP, resource query, step template

## Dependencies

- `controller/` depends on `executor/`
- `executor/` depends on `internal/agent/`
- Both depend on `api/v1alpha1` for CRD definitions

## Usage

The controllers are registered in `cmd/controller/main.go` and automatically reconcile Workflow and WorkflowRun resources. The executor is used internally by the WorkflowRunReconciler to execute workflows.
