# StepTemplate

**StepTemplate** defines a reusable step that can be instantiated in workflows with parameters. Placeholders in the step use `{{.parameterName}}` and are replaced by template arguments when the template is referenced via `stepTemplateRef`.

- **API Group:** `ottoflow.nirmata.io`
- **Version:** `v1alpha1`
- **Kind:** `StepTemplate`
- **Scope:** Namespaced
- **Short names:** `steptemplate`, `steptemplates`

---

## Spec (StepTemplateSpec)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `description` | string | No | Human-readable description of what the template does. |
| `parameters` | [][StepTemplateParameter](#steptemplateparameter) | No | Parameters that can be provided when instantiating the template. |
| `step` | [Step](#step) | Yes | The step definition to instantiate. Step name is replaced at instantiation; expressions/outputs can use `{{.parameterName}}`. |

### StepTemplateParameter

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Parameter name (used as `{{.name}}` in the step). |
| `description` | string | No | Human-readable description. |
| `default` | string | No | Default value (CEL expression evaluated in workflow context if not provided). |
| `required` | boolean | No | Whether the parameter must be provided. |

### Step

The `step` field has the same structure as a workflow [Step](workflow.md#step) (expressions, outputs, dependsOn, resourceQuery, prometheusQuery, agentRef, mcpToolCall, workflowRef, externalAgentRef). Within the template:

- **Parameter placeholders:** Use `{{.parameterName}}` in expression strings, output expressions, and other string fields.
- **Step name:** The template's step name is replaced with the actual step name when instantiated.
- **Ignored fields:** On `spec.step`, `dependsOn`, `matchConditions`, `failurePolicy`, `retry` and `timeout` have no effect. Set `dependsOn`, `matchConditions` and `failurePolicy` on the workflow step that references the template.

---

## Status (StepTemplateStatus)

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Latest observations of the template's state (standard Kubernetes condition format). |
