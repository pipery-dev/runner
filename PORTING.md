# Porting Notes

Upstream source reviewed from `github.com/actions/runner`:

- `src/Runner.Listener/Program.cs`
- `src/Runner.Listener/Runner.cs`
- `src/Runner.Listener/JobDispatcher.cs`
- `src/Runner.Worker/Program.cs`
- `src/Runner.Worker/Worker.cs`
- `src/Runner.Worker/JobRunner.cs`
- `src/Runner.Worker/StepsRunner.cs`
- `src/Runner.Worker/Handlers/ScriptHandler.cs`
- `src/Runner.Worker/Handlers/ScriptHandlerHelpers.cs`
- `src/Runner.Worker/ActionCommandManager.cs`
- `src/Runner.Common/ProcessInvoker.cs`
- `src/Sdk/DTPipelines/Pipelines/AgentJobRequestMessage.cs`
- `src/Sdk/DTPipelines/Pipelines/ActionStep.cs`

The official runner is split into a listener process and a worker process.
The listener registers/configures the runner, creates a session, long-polls
for messages, dispatches a worker process, renews locks, completes job
requests, and handles self-update. The worker receives an
`AgentJobRequestMessage`, masks secrets, sets culture, runs the job, listens
for cancellation, and returns a task result.

`runner` ports the worker execution path first. The Go shape is:

- `internal/job`: JSON models for runner job messages, variables, mask hints,
  and script steps.
- `internal/shell`: default shell behavior and script fixups from
  `ScriptHandlerHelpers.cs`.
- `internal/command`: parser for runner workflow commands emitted on stdout.
- `internal/masker`: value/regex secret masking.
- `internal/runner`: job and step execution, temp script creation, env
  assembly, output handling, cancellation, and result mapping.

Implemented areas:

- Hosted GitHub runner-admin registration with `--url`, `--token`, runner
  groups, create/replace runner, OAuth config persistence, and `.runner` /
  `.credentials` output.

Deferred areas:

- Message listener, broker/run-service clients, OAuth credentials, RSA keys.
- Worker subprocess IPC. This implementation runs worker logic in-process.
- Expression/template evaluation.
- JavaScript, composite, Docker, artifact, cache, and repository action
  handlers.
- Timeline records and upload/complete APIs.
- Service installation and self-update.

Those pieces should be added incrementally behind interfaces so the local
worker remains testable.
