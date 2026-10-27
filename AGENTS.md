# AGENTS.md — OttoFlow

---

## House rules

- **Don't silently expand scope for theoretical robustness.** Before you add
  complexity to guard against a speculative or unlikely race, security, or
  failure edge case, tell the user the concrete failure mode, how likely it is,
  and what the guard costs in complexity, and get their agreement first. This
  rule doesn't cover a known vulnerability or a bug you can reproduce: fix those.
- **Don't let perfect be the enemy of good.** Ship working software in small
  increments. Apply the Pareto principle: fix the issues that affect
  correctness, security, or the user's task in this PR, and file a follow-up
  issue for polish, rare edge cases, and nice-to-haves rather than holding the
  PR open. A linked follow-up issue is a valid disposition for a review
  finding.
- **Build for the current requirement.** Don't add helpers, abstractions, config
  options, or fallbacks for callers that don't exist yet. Generalize when a
  second real use appears. Don't add dependencies, renames, or refactors that
  the task doesn't need.
- **Fix the cause, not the symptom.** Reproduce a bug before you fix it, and
  rerun the same reproduction after. If you can't reproduce it, say what you
  need. Never make an error go away by skipping a test, adding a lint
  suppression, or swallowing the error.
- **Stop after two failed attempts.** If the same fix or command fails twice,
  stop, note what failed, and re-plan instead of trying a third variation. This
  doesn't limit review loops such as `copilot-review`.
- **An unrun check is not a pass.** Report every check you skipped or couldn't
  run, and read test output yourself rather than trusting another agent's
  summary.
- **Verify a subagent's key claim before you build on it.** Agent reports can be
  wrong about which files changed, which branch they ran on, or whether tests
  passed; confirm with `git status`, the diff, or the test output.

---

## What is OttoFlow

OttoFlow is a Kubernetes-native workflow engine that orchestrates multi-step automation combining CEL expressions, Kubernetes resource queries, LLM agents, and MCP tools. Workflows are defined as CRDs and executed as DAGs with dependency resolution, retry logic, and conditional execution.

## nirmata/ottoflow is public: no internal Nirmata detail

Never put Nirmata-internal or proprietary detail in anything the public can read (commit messages, PR titles and descriptions, issue bodies and comments, code comments, sample workflows, documentation), including internal repository names, internal PR or issue numbers, internal service hostnames or cluster/environment names, internal deployment manifests, and proprietary tooling and product internals. When a change originates elsewhere, describe the problem and the fix on their own terms and do not cite the internal source.

Before pushing, committing, or posting, grep the content for internal repo names, internal issue references, `*.svc.cluster.local` hostnames, and environment names.

## Build and test

```bash
make build                # Controller binary (runs manifests, generate, fmt, vet, lint)
make build-cli            # CLI binary -> bin/ottoflow
make test                 # Unit tests (runs manifests, generate, fmt, vet)
make lint                 # golangci-lint

# After editing api/v1alpha1/*_types.go
make manifests            # CRDs
make generate             # DeepCopy methods
make codegen-docs         # API and CLI reference docs
make verify-codegen       # Fails if generated files are stale

make test-e2e             # Needs an existing cluster
make test-e2e-kind        # Ensures a kind cluster exists, then runs e2e
```

`make help` lists targets; local controller and Kind runs: `docs/dev/build.md`.

Before you push: `make test`, `make lint`, `make verify-codegen` and `make build-cli` pass.

## Architecture

### CRDs (api/v1alpha1/)

Workflow (template), WorkflowRun (instance), Agent (LLM config), MCPServer, StepTemplate: `docs/user/concepts/concepts-guide.md` ("Concept 1"); fields in `docs/user/reference/api/`.

### Step types

The action fields are the pointer fields of `Step` other than `retry` in `api/v1alpha1/workflow_types.go` (rendered in `docs/user/reference/api/api-docs.md`).

### Execution flow

```
Controller watches WorkflowRun -> creates runner Job -> Job runs workflow-runner binary
  -> WorkflowExecutor:
    1. Initialize context with inputs
    2. Evaluate workflow variables sequentially
    3. Build DAG from explicit DependsOn declarations (NO inference from output refs)
    4. Loop: compute ready steps -> execute batch (respecting MaxConcurrentSteps) -> repeat
    5. Evaluate workflow-level outputs
```

### Key packages

- `internal/workflow/controller/`: `WorkflowReconciler` (validates CEL at reconcile time, registers triggers), `WorkflowRunReconciler` (runner Jobs, pod monitoring, status, run policy), `TriggerManager`, `Scheduler` (leader-elected cron engine)
- `internal/workflow/executor/`: `WorkflowExecutor` (DAG step dispatch), CEL evaluator (Kubernetes + Kyverno CEL libraries, `resource.Get/List`, LRU compilation cache), `DAG` (cycle detection)
- `internal/agent/`: `RoutingAgentExecutor` and `DefaultAgentExecutor` for agent steps, `MCPClient` (wraps the kubectl-ai MCP client), `OutputExtractor`

### Binaries (cmd/)

- **controller** (also serves the validating webhooks) and **workflow-runner**: see Key packages and Execution flow
- **agent-executor**: LLM-execution service, auth via TokenReview + SubjectAccessReview
- **serve-a2a**: A2A protocol server (JSON-RPC 2.0 + SSE); exposes a Workflow as an A2A agent, opt-in via `spec.expose.kagent`

### CLI (cli/)

- `ottoflow run` executes in-process with no controller when given `--workflow-dir`; full command reference: `docs/cli/` (generated by `make codegen-docs`)

## CEL variable context

Variables and their scope: `docs/user/reference/cel-reference.md` ("Variable Access"); read its "Common Pitfalls" before writing CEL. Also the context globals `http`, `globalContext`, `image`.

## Critical patterns

- **Generated files**: never hand-edit `config/crd/bases/`, `charts/ottoflow/crds/` or `docs/user/reference/api/api-docs.md`; regenerate them with the codegen targets in Build and test
- **Restart durability**: a controller restart does not interrupt runs, since each runs in its own runner Job. A runner-pod death mid-run loses progress unless per-step checkpointing (off by default) is on, which resumes from the last completed step

## Docs layout and proposals

| Content | Location |
|--------|----------|
| Concepts / architecture | `docs/user/concepts/` |
| API / CRD reference | `docs/user/reference/` |
| How-to guides | `docs/user/tasks/` |
| RBAC and Secret access | `docs/user/rbac-secret-access.md` |
| Draft proposal | `docs/dev/PROPOSAL_<name>.md` |
| Design docs | `docs/dev/DESIGN.md`, `docs/dev/DESIGN_<name>.md` |
| Sample workflows | `samples/workflows/` |

## Comments and diff size

A small code change should touch few files.

- Change an existing comment only when it is untrue (usually because your change made it so). Do not reword, extend or add caveats to a comment that is still correct.
- Do not edit a sample (`samples/`), doc (`docs/`) or generated file only to add or reword a comment that is still true.
- Keep new comments short. In code, say why, not what the code already shows; API doc comments (`api/v1alpha1/*_types.go`) describe the field and are copied into the generated CRDs and API docs.
- No drive-by reformatting, renames or moving code between files unless that is the change; put that, and any other unrelated change, in its own commit.
