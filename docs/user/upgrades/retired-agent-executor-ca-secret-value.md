# Upgrading: `workflowRunner.agentExecutorCASecret` is retired

The chart value `workflowRunner.agentExecutorCASecret` is removed. With the agent-executor's internal TLS (`agentExecutor.tls.internal.enabled` and no `agentExecutor.tls.secretName`), a non-empty value now fails `helm install` and `helm upgrade` with `workflowRunner.agentExecutorCASecret is no longer supported`.

With only the chart's RBAC, the value could not work: the controller reads the CA Secret in the install namespace under the certificate manager's Role, which grants `get` only on the Secret names the certificate manager writes, so any other name failed every agent-step run with `failureReason: SecretAccessDenied`. The chart now always passes the name the certificate manager uses, `<agent-executor>.<install-namespace>.svc.tls-ca`.

To upgrade, delete the key from your values. With external TLS (`agentExecutor.tls.secretName` set), the value was never rendered and is still ignored.
