## ottoflow version

Show CLI build version details

### Synopsis

Show the OttoFlow CLI build version details, including the version,
git commit, build time, Go version, and platform.

Examples:
  # Show build version details
  ottoflow version

  # Show build version details in JSON format
  ottoflow version --output json

```
ottoflow version [flags]
```

### Options

```
  -h, --help            help for version
  -o, --output string   Output format: table, json (default "table")
```

### Options inherited from parent commands

```
      --kubeconfig string   Path to kubeconfig file (defaults to $HOME/.kube/config)
  -n, --namespace string    Kubernetes namespace (defaults to current kubeconfig context namespace, then "ottoflow")
```

### SEE ALSO

* [ottoflow](ottoflow.md)	 - OttoFlow CLI - Execute and manage workflows

