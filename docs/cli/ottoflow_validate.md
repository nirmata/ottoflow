## ottoflow validate

Validate a Workflow definition without executing it

### Synopsis

Validate a Workflow definition by running static checks without executing any steps.

Checks performed:
  - DAG cycle detection and invalid dependsOn references
  - Step expression-to-dependsOn alignment (MISSING_DEPENDS_ON)
  - Undefined inputs.* references (UNDEFINED_INPUT)
  - CEL expression syntax (compile-time, no evaluation)
  - workflowRef, agentRef, stepTemplateRef (direct and forEach, plus one level into a
    resolved template's own step), mcpToolCall.server, and the workflowRef/agentRef/
    mcpToolCall.server declared inline on a forEach.step -- checked against the cluster in
    cluster mode, or against the manifests loaded from --workflow-dir in local mode; also
    checked for a loaded WorkflowRun's workflowRef.
    In -f/--file mode, these are checked against every OttoFlow object found in the same
    file (e.g. a Workflow plus its own StepTemplate/Agent/MCPServer), but an unresolved ref
    is reported as a WARNING rather than a failure: a single file may legitimately reference
    objects that are applied to the cluster separately and are not visible to this command.

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

