# Changelog

All notable changes to Xira are documented in this file. This changelog starts
with version 0.8.0; earlier release history remains available in Git.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and Xira uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.8.1] - 2026-08-03

### Added

- Added opt-in, bounded capture of Feishu `im.message.receive_v1` WebSocket
  payloads for short-lived diagnostics. Captures preserve SDK-unknown fields,
  use isolated `0700`/`0600` storage, enforce capacity and retention across
  restarts, fail loudly on unsupported channels, and include a controlled
  production verification and cleanup runbook
  ([#211](https://github.com/xiramesh/xira/pull/211)).

### Fixed

- Preserved DeepSeek `reasoning_content` across streaming and non-streaming
  tool turns so Thinking models can complete follow-up requests without a
  missing-reasoning-content API failure, while keeping reasoning out of
  user-visible output ([#203](https://github.com/xiramesh/xira/pull/203)).

## [0.8.0] - 2026-07-18

### Added

- Added a standalone, zero-build WebSocket demo for exercising the default
  Agent route, runtime events, final responses, and human-in-the-loop flows
  from a browser ([#179](https://github.com/xiramesh/xira/pull/179)).
- Isolated demo conversations with a persistent per-browser sender identity
  and a per-tab chat identity, including safe fallbacks when browser storage or
  Web Crypto is unavailable ([#180](https://github.com/xiramesh/xira/pull/180)).
- Added `model_policy.format` to Agent `PROFILE.md` files. `format: json` is
  carried through ADK to DeepSeek JSON mode and requires the public final to be
  exactly one JSON object; omitted or explicit `text` format preserves the
  existing plain-text behavior ([#183](https://github.com/xiramesh/xira/pull/183)).

### Fixed

- Made `task clean` recover an incomplete or corrupt Task-managed Go module
  cache, including Go's read-only module directories, without compiling Xira
  before cleanup ([#184](https://github.com/xiramesh/xira/pull/184)).
- Ensured the default `task build` injects version, commit, and build-date
  metadata while retaining debug symbols; production builds remain stripped.
