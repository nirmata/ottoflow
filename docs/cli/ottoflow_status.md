## ottoflow status

Get status of a workflow run

### Synopsis

Get the current status of a workflow run.

Examples:
  # Get status of a workflow run
  ottoflow status my-workflow-run-1234567890

  # Get status in JSON format
  ottoflow status my-workflow-run-1234567890 --output json

  # Get status in YAML format
  ottoflow status my-workflow-run-1234567890 --output yaml

```
ottoflow status [workflow-run-name] [flags]
```

### Options

```
  -h, --help             help for status
      --include-inputs   Include spec.inputValues in json/yaml output (may contain secrets)
  -o, --output string    Output format: table, json, yaml (default "table")
```

### Options inherited from parent commands

```
      --kubeconfig string   Path to kubeconfig file (defaults to $HOME/.kube/config)
  -n, --namespace string    Kubernetes namespace (defaults to current kubeconfig context namespace, then "ottoflow")
```

### SEE ALSO

* [ottoflow](ottoflow.md)	 - OttoFlow CLI - Execute and manage workflows

