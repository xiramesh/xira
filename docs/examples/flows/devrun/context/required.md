# Required Context

- Repository path must be provided as `input.repo`.
- The runtime must know how to run `command.run` and persist raw command output.
- CLI agent executors such as `codex` and `claude` must be available on PATH when their steps are selected.
- GitHub-related steps require `gh` authentication in the local environment.
