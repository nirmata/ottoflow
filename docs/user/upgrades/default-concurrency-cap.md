# Upgrading: default concurrency cap

A Workflow that leaves `spec.run.maxConcurrentRuns` unset is now capped at 10 active (Pending, Running, or not yet picked up) WorkflowRuns. Previously cron, event, webhook and MCP runs had no limit unless the field was set. At 10 active runs, cron and event triggers skip creating a run, MCP calls return an error and webhooks answer `429`. The cap is in the controller, so it applies without re-applying CRDs.

**To keep the old behavior,** set `maxConcurrentRuns: 0` (no limit) on the Workflow.
