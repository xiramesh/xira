---
schema_version: xira.skill.v0
id: local-research
name: Local Research
version: 0.1.0
description: Use local workspace files and command evidence to produce source-backed answers.

activation:
  mode: explicit

requires:
  tools:
    - search_file
    - read_file
  optional_tools:
    - command.run
    - tool_output.read
  secrets: []
  mcp_servers: []

context:
  includes:
    - references/
  forbidden:
    - secrets/

verification:
  default_checks:
    - final_response_non_empty

artifacts:
  output_dir: artifacts/skills/local-research
  retention: local
---
# Instructions

Use this skill when the task requires source-backed local research inside the Xira workspace.

Rules:

- Prefer `search_file` before `read_file` when the target file is unknown.
- Cite workspace-relative paths when making claims from local files.
- If command output is truncated, use `tool_output.read` before relying on missing stdout or stderr.
- Do not claim evidence exists unless it came from a tool result or an explicitly loaded reference.
- Keep the final answer compact and explicit about uncertainty.
