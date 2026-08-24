# OttoFlow CLI

A command-line tool for running OttoFlow workflows — either locally (in-process, no cluster
required) or against a Kubernetes cluster.

## Installation

See [Installing the CLI](../docs/user/tasks/installation.md#installing-the-cli) for Homebrew,
prebuilt binary, and build-from-source options.

To build from this checkout directly:

```bash
go build -o bin/ottoflow ./cli
# or, from the project root:
make build-cli
```

## Usage

Run a workflow locally, no cluster needed — by path:

```bash
ottoflow run samples/workflows/production/cluster-overview.yaml
```

...or from a directory, by name:

```bash
ottoflow run cluster-overview --workflow-dir samples/workflows --input name="World"
```

Without `--workflow-dir`/`--file`/a manifest path, `ottoflow run` creates a `WorkflowRun` in the
cluster (the `Workflow` must already exist there) and, by default, watches it to completion:

```bash
ottoflow run my-workflow --input name="World"
ottoflow run my-workflow --watch=false          # create and exit
ottoflow status <workflowrun-name> --output json
ottoflow validate --workflow-dir samples/workflows
```

## Flags and environment variables

For the full, current flag reference for `run`, `validate`, and `status`, and every environment
variable the CLI reads, see the
[Configuration Reference](../docs/user/reference/configuration.md#ottoflow-cli) — or just run:

```bash
ottoflow --help
ottoflow run --help
```
