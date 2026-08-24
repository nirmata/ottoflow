## ottoflow

OttoFlow CLI - Execute and manage workflows

### Synopsis

OttoFlow is a CLI tool for executing and managing workflows in Kubernetes clusters.

OttoFlow allows you to:
- Execute workflows against a Kubernetes cluster
- Monitor workflow execution progress
- View workflow results and outputs
- Manage workflow runs

### Options

```
  -h, --help                help for ottoflow
      --kubeconfig string   Path to kubeconfig file (defaults to $HOME/.kube/config)
  -n, --namespace string    Kubernetes namespace (defaults to current kubeconfig context namespace, then "ottoflow")
```

### SEE ALSO

* [ottoflow run](ottoflow_run.md)	 - Create and watch a WorkflowRun
* [ottoflow status](ottoflow_status.md)	 - Get status of a workflow run
* [ottoflow validate](ottoflow_validate.md)	 - Validate a Workflow definition without executing it
* [ottoflow version](ottoflow_version.md)	 - Show CLI build version details

