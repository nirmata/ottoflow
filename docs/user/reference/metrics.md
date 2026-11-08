# Workflow Metrics

OttoFlow exposes Prometheus metrics for workflow execution observability. Metrics are served on the controller's metrics endpoint (default `:8080/metrics`) and can be scraped by Prometheus or compatible systems.

## Built-in Metrics

The controller emits the following metrics for workflow executions and event triggers:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `ottoflow_workflow_runs_total` | Counter | `workflow`, `namespace`, `phase` | Total WorkflowRuns by phase (`succeeded`, `failed`) |
| `ottoflow_workflow_trigger_events_dropped_total` | Counter | `workflow`, `namespace`, `reason` | Event-trigger events dropped on a WorkflowRun create-path error (`reason`: `api_error`) |
| `ottoflow_workflow_run_duration_seconds` | Histogram | `workflow`, `namespace` | Duration from workflow start to completion |
| `ottoflow_workflow_steps_total` | Counter | `workflow`, `namespace`, `step`, `phase` | Total step executions by phase (`succeeded`, `failed`, `skipped`) |
| `ottoflow_workflow_step_duration_seconds` | Histogram | `workflow`, `namespace`, `step` | Step execution duration |
| `ottoflow_workflow_runs_active` | Gauge | `workflow`, `namespace` | Number of WorkflowRuns currently in Running phase |

### Label Conventions

- **workflow**: Workflow template name (from `workflowRef.name`)
- **namespace**: WorkflowRun namespace
- **step**: Step name (for step-level metrics)
- **phase**: `succeeded`, `failed`, or `skipped`
- **reason**: why an event-trigger event was dropped (`api_error`: the WorkflowRun create path returned an error)

### Example Queries

```promql
# Success rate by workflow
rate(ottoflow_workflow_runs_total{phase="succeeded"}[5m]) 
  / rate(ottoflow_workflow_runs_total[5m])

# P95 workflow duration
histogram_quantile(0.95, 
  rate(ottoflow_workflow_run_duration_seconds_bucket[5m]))

# Currently running workflows
ottoflow_workflow_runs_active
```

## Custom Workflow Metrics

Workflows can publish custom Prometheus metrics by adding a `metric` field to workflow-level outputs. See [Defining Custom Metrics](../tasks/custom-metrics.md) for details.

Custom metrics are prefixed with `ottoflow_workflow_` and support counter, gauge, and histogram types.

## Scraping Metrics

### Prometheus Operator

When using the Prometheus Operator, enable the ServiceMonitor in the Helm chart:

```yaml
controller:
  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      interval: 30s
      scrapeTimeout: 10s
```

### Manual Prometheus Config

Add a scrape config for the OttoFlow controller:

```yaml
scrape_configs:
  - job_name: 'ottoflow'
    static_configs:
      - targets: ['ottoflow-controller.ottoflow.svc:8080']
    metrics_path: /metrics
```

## ServiceMonitor

The Helm chart includes an optional ServiceMonitor resource for Prometheus Operator. When enabled, it configures Prometheus to scrape the controller's metrics endpoint automatically.
