# Upgrading: the `agent-executor-ca` volume name is reserved

A runner Job volume named `agent-executor-ca` in a WorkflowRun's `spec.execution.job.volumes` now fails the run with `Failed to build runner Job: a volume named "agent-executor-ca" already exists on the runner Job pod spec ... reserved for the agent-executor CA`. Before, this was refused only when the workflow had an agent step and the agent-executor CA was mounted.

The controller picks the ConfigMap it publishes the agent-executor CA into from the runner Job volume with that name, so a user volume with the same name could point the publish at a ConfigMap of the user's choosing.

To upgrade, rename any `spec.execution.job.volumes` entry called `agent-executor-ca`, and its `volumeMounts` entry, in your WorkflowRuns and in anything that creates them.
