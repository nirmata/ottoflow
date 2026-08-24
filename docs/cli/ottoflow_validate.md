## ottoflow validate

Validate a Workflow definition without executing it

### Synopsis

Validate a Workflow definition by running static checks without executing any steps.

Checks performed:
  - DAG cycle detection and invalid dependsOn references
  - Step expression-to-dependsOn alignment (MISSING_DEPENDS_ON)
  - Undefined inputs.* references (UNDEFINED_INPUT)
  - CEL expression syntax (compile-time, no evaluation)
  - WorkflowRef and AgentRef existence (when connected to a cluster)

Examples:
  ottoflow validate --workflow-dir samples        # validate all workflows in directory
  ottoflow validate my-workflow --workflow-dir samples
  ottoflow validate -f workflow.yaml
  ottoflow validate my-workflow

  # Generate RBAC manifests after validation
  ottoflow validate -f workflow.yaml --generate-rbac
  ottoflow validate --workflow-dir samples --generate-rbac --output rbac.yaml

```
ottoflow validate [workflow-name] [flags]
```

### Options

```
      --agent-executor-namespace string   Namespace where the agent-executor service runs (used for agentRef RBAC rules) (default "ottoflow")
  -f, --file string                       Load workflow from a YAML file
      --generate-rbac                     After validation passes, generate RBAC manifests for the workflow
  -h, --help                              help for validate
      --output string                     Write generated RBAC to a file (default: stdout); only used with --generate-rbac
      --workflow-dir string               Load workflows from directory (local mode, no cluster required)
```

### Options inherited from parent commands

```
      --kubeconfig string   Path to kubeconfig file (defaults to $HOME/.kube/config)
  -n, --namespace string    Kubernetes namespace (defaults to current kubeconfig context namespace, then "ottoflow")
```

### SEE ALSO

* [ottoflow](ottoflow.md)	 - OttoFlow CLI - Execute and manage workflows

