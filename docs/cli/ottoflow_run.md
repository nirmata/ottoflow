## ottoflow run

Create and watch a WorkflowRun

### Synopsis

Run a workflow: locally (with --workflow-dir, --file, or a plain file path) or in-cluster (default).

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
  ottoflow run my-workflow --workflow-dir samples/workflows --provider openai --model gpt-4

```
ottoflow run [workflow-name|workflow-file.yaml] [flags]
```

### Options

```
      --allow-insecure-url      Permit http:// (non-TLS) URLs with -f or a bare http(s) URL argument
  -f, --file string             Run a manifest locally, in-process, from a file, an http(s) URL, or '-' for stdin (no cluster/controller required)
  -h, --help                    help for run
      --include-inputs          Include spec.inputValues in json/yaml output (may contain secrets; use only when needed)
  -i, --input stringToString    Input values as key=value pairs (can be specified multiple times) (default [])
      --max-workers int         Max concurrent workers for forEach steps (local mode only) (default 5)
      --model string            Override LLM model for all agent steps (local mode only); e.g. gpt-4, gemini-flash-latest
  -o, --output string           Output format: table, json, yaml (default "table")
      --output-dir string       Save run output (JSON + Markdown) to directory (created if needed)
      --prometheus-url string   Prometheus server URL for CEL/prometheus steps (local mode only)
      --provider string         Override LLM provider for all agent steps (local mode only); e.g. openai, anthropic, google
      --timeout string          Maximum time to wait for workflow completion (cluster watch) (default "10m")
  -W, --watch                   Watch workflow execution progress (cluster mode only) (default true)
  -w, --workflow string         Name of the workflow to execute
      --workflow-dir string     Load workflows from directory and run locally (in-process); if set, cluster path is not used
```

### Options inherited from parent commands

```
      --kubeconfig string   Path to kubeconfig file (defaults to $HOME/.kube/config)
  -n, --namespace string    Kubernetes namespace (defaults to current kubeconfig context namespace, then "ottoflow")
```

### SEE ALSO

* [ottoflow](ottoflow.md)	 - OttoFlow CLI - Execute and manage workflows

