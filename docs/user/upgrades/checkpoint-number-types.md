# Upgrading: number types after a checkpoint restore

**This affects only WorkflowRuns with `spec.execution.checkpointing.enabled: true`. A restore
used to bring every number in the workflow context back as a double. It now brings an `int` back
as an `int` and a `double` back as a `double`, the types the run had before the restore (a `uint`
comes back as an `int`, or as a `double` if it is too large for an `int`). A restore happens when
a run resumes after a `waitForCallback` step or after the runner restarts.**

## Expressions that relied on a restored integer being a double

- **Symptom:** in a step after a restore, arithmetic that mixes a restored integer with a double
  now fails with `no such overload`, for example `variables.count + 1.0`. Integer division now
  truncates, for example `variables.passed / variables.total` with 3 and 4 gives `0` instead of
  `0.75`. A step before the restore already behaved this way.
- **Cause:** CEL has no arithmetic operator that mixes an `int` and a `double`, and `/` on two
  `int` values divides as integers.
- **Fix:** convert explicitly where a double is meant, for example
  `double(variables.passed) / double(variables.total)`.

## A run checkpointed before the upgrade and resumed after it

- **Symptom:** a value saved as a whole double, such as `2.0`, comes back as the integer `2`,
  and stays an integer for the rest of that run.
- **Cause:** the earlier release wrote whole doubles without a decimal point, so the restore
  cannot tell them from integers.
- **Fix:** let such runs finish before upgrading, or convert explicitly with `double(...)`
  where a double is meant.
