# runner

`runner` is a Go port of the executable core of
[`actions/runner`](https://github.com/actions/runner). The first target is a
small, auditable worker that can execute runner job messages locally.

This is not yet a protocol-complete replacement for the official GitHub
Actions runner. The current implementation focuses on the worker side:

- Decode a job request JSON document.
- Run script steps in order.
- Resolve the same default shell behavior used by the upstream runner.
- Inject variables and step environment.
- Mask configured secrets in logs.
- Parse common workflow command output such as `::error::`,
  `::warning::`, `::notice::`, `::debug::`, `::add-mask::`, and
  `::stop-commands::`.
- Stop on the first failed step and return a non-zero exit code.

## Usage

```bash
go run ./cmd/runner --job ./examples/job.json
```

Container image releases are published to `ghcr.io/pipery-dev/runner`.
The image starts the listener by default, so you can pass registration flags
directly when needed.

Useful flags:

- `--job`: path to the job request JSON file.
- `--url` and `--token`: register the runner using the hosted GitHub
  runner-admin flow and save `.runner` / `.credentials`.
- `--token-env`: read the registration token from an environment variable
  instead of passing it on the command line.
- `--work`: workspace directory. Defaults to the current directory.
- `--temp`: temp directory for generated scripts. Defaults to
  `<work>/_temp`.
- `--dry-run`: print the steps that would run without executing them.
- `--version`: print the runner version.

## Job Format

The JSON format intentionally mirrors the fields used by
`AgentJobRequestMessage` from `actions/runner`, while accepting a compact
form for local use. See [examples/job.json](examples/job.json).

## Porting Notes

See [PORTING.md](PORTING.md) for the upstream files reviewed and the mapping
from the C# runner architecture to this Go implementation.
