# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.3.0] - 2026-09-26

### Added

- The `native` harness constant and the `agentwire.Credentials` type. A consumer registers an in-process driver with `Runtime.SetDriver(agentwire.Native, drv)`; the root module ships no loop.
- `agentwire.Detector` lets a driver answer `Detect` without a binary. The `native` driver uses it.
- `Detection.Ready` folds `Supported && Auth == AuthOK` into one field for pickers.
- The `native` module (`github.com/trebi-ai/agent-wire/native`): an in-process loop that folds one provider stream per turn into message parts, and runs tools, approvals, skills, an MCP session, and a summary compactor. It keeps one message log per session in a `FileStore` that also reads the legacy trebi llm.Message lines.
- The provider modules `native/provider/anthropic` (official SDK, thinking and web search) and `native/provider/openai` (chat completions over any OpenAI-compatible endpoint).
- `StartRequest.Credentials` carries the provider credentials of a native session.

## [v0.2.2] - 2026-09-26

### Fixed

- Claude: the phantom tool start keyed by the control `request_id` no longer produces a tool event with no result. A tool start without a matching result settles as cancelled at turn end.
- Claude: `Permission.ToolUseID` carries the tool-use id, so one UI answer matches one tool call.
- Claude: a result or exit cancels the open permission requests.
- Detect: the auth probe caches, and `Runtime.InvalidateDetect` drops the cache after a login or logout.

## [v0.2.1] - 2026-09-26

### Fixed

- The operator default model resolves for OpenCode 2 sessions.

- Initial extraction of the harness subprocess runtime from trebi `internal/runtime/cli` into a public library.
- One event model over five vendor protocols: Claude stream-json, Codex app-server, OpenCode serve, ACP (Copilot, Cursor, Gemini) and pi RPC. Every harness emits the same event vocabulary and the same terminal `EventExit`.
- Launch builder. The caller describes the session in `StartRequest`; the library builds the vendor command for new and resumed sessions, and maps model, effort, home, permissions, MCP servers, skills and instructions per harness.
- `PermissionPolicy`: one policy (`ask`, `auto_edit`, `auto`, `inherit`) for every harness, mapped onto the vendor's own mechanism. A request the policy does not answer becomes an `EventPermission`.
- `docs/compat.md` records the Phase 0 vendor spike: the exact binary versions, the command behind every verdict, and the surfaces that were confirmed, refuted, or could not be run without a live model turn.
- MCP injection per session (`StartRequest.MCPServers`), rendered per harness. Injection adds to the user's own servers.
- Skill injection per session (`StartRequest.Skills`), with three strategies: native load, overlay config dir, and an index in the instructions.
- Per-session instructions (`StartRequest.Instructions`), rendered per harness.
- Harness detection (`Runtime.Detect`): binary resolution, version floor, and best-effort login state.
- Crash ledger. `Runtime.Reconcile` kills positively matched leftover children from a previous process and never touches a reused pid.
- Shared JSON-RPC client in `internal/jsonrpc`, used by the Codex and ACP drivers, with number and string ids, and with waiter cleanup on timeout, cancel and send error.
- Go-native wire with no runtime SDK dependency. The vendor SDKs are a test-time fixture oracle only.
- `OpenCode2`: the OpenCode 2.x HTTP surface, spoken by the `opencode2` beta binary. Every route lives under `/api` behind the server's basic auth, responses carry a `{data: …}` envelope, events carry their payload under `data`, and a turn ends with `session.execution.succeeded` instead of `session.idle`. Sessions, prompts, per-session model pins, per-session instructions, permissions, attachments and interrupts all work. `OpenCode` (1.x) is unchanged, and both harnesses are selectable side by side.
- OpenCode 2.x classifies a failure from the vendor's structured error type (`provider.auth`, `provider.quota`, `provider.rate-limit`, `provider.internal`, …) before falling back to the shared text and status rules.

### Fixed

- Claude: an auto-answered `can_use_tool` control request emitted a second tool `started` event keyed by the control `request_id` (a UUID). Nothing completed that id, so every auto-allowed tool left a phantom chip in a transcript, a duplicate of the real one, and one leaked id disabled stall detection for the rest of the session. The driver no longer emits a tool event on the auto-answer path: the `tool_use` block already announced the start, and the `tool_result` completes the same id. On `result` and on process exit the driver closes every tool still open as `cancelled`, and the tracker clears its in-flight table on `result` and `exit`.
- `Permission` events carry `ToolUseID` when the vendor supplies it, so a consumer can join an approval card to its tool chip.
- `Detect` caches the vendor login probe per harness and binary behind a TTL (`Options.AuthCacheTTL`, default 5 min, negative disables) with single-flight on a miss. A consumer calls `Runtime.InvalidateDetect(harness)` after a login or a logout. Repeated `Detect` calls no longer spawn a process.
- Wire framing. A frame larger than `MaxFrameBytes` emits `EventError` with code `frame_too_large` and parsing continues instead of stalling the child on a full pipe.
- OpenCode 2: a session created without a model inherited a provider default that could point at an unusable credential. An expired Anthropic OAuth token answered every prompt with "OAuth access token is invalid", while the vendor CLI, which resolves the operator's own default, ran the same prompt. The driver now resolves that default from `~/.config/opencode/opencode.json` (a `provider/model` string, or the 2.x `{providerID, model}` object) when the caller pins no model.
- `EventExit` is delivered after `Close`, instead of being dropped by the emit select.
- A failed handshake is returned as an error from `Runtime.Start`, not just as an `EventError`.
- Driver defects carried over from the trebi copy: request waiter leaks, the reused ACP prompt id, the Codex interrupt id, user text parsed as assistant text on OpenCode, model and variant lost on OpenCode prompts, the ACP turn-result flag not reset per turn, and the tracker entry for a tool with no id.
- Classification. A bare HTTP status number (`401`, `429`, `503`) matches only in error context, so ordinary prose such as "I read 429 files" no longer classifies as a limit error.
- Claude never received `--permission-prompt-tool stdio`, so the CLI answered prompts itself and never sent `can_use_tool`. The flag is now part of the permission policy, and it is kept in every mode, including `inherit`.
- OpenCode: the session's done channel was never created, so every finished session panicked while closing it.
- OpenCode: the `/event` stream was opened once per server with no directory, but OpenCode scopes its event bus by directory. A session whose working directory differed from the server's received no events at all and hung until the caller gave up. The stream is now opened per session directory, before the session is created.
- OpenCode: the event stream is now awaited before the session is created, and OpenCode 2 reports the session itself instead of waiting for the bus. The stream connection was started in a goroutine, and the session was routable only after the create answered, so `session.created` could be published before the stream was live, or before the session was subscribed. Either way the caller saw no init event. Affects both wires; OpenCode 1 keeps its bus-driven init, and OpenCode 2 emits it from the driver.
- The live tier takes `AGENTWIRE_LIVE_MODEL_<HARNESS>` (or `AGENTWIRE_LIVE_MODEL`) to pin a model, so the gate measures the library and not an unavailable account default.
- Codex: an app-server request for a method the library cannot answer is refused with `-32601` instead of being turned into a permission event the peer cannot use.
- `Runtime.Detect` reports the fake harness as always supported. It has no vendor binary and no login, so it can never be "not installed".
- The `.cmd`/`.bat` shim refusal moved into portable code, so the message is the same on every platform and the check is compiled everywhere.
- `go.sum` was missing the `/go.mod` hashes of `golang.org/x/sys` and of the test-only dependencies. A build with `-mod=readonly` on Windows failed to load the module graph, because only the Windows build tag imports `golang.org/x/sys`. The Windows job is out of the CI matrix; the Windows build is checked with `GOOS=windows go vet ./...`.

### Changed

- `Runtime.SetDriver(h, nil)` now restores the built-in driver for that harness instead of removing it. A caller that injects a driver for a test can always get back to the default table.
- `PermissionInherit` adds no permission-mode flag, so the operator's own harness configuration decides, while the host prompt callback stays installed.
- `internal/proc.Proc.Wait` returns `(exitCode int, err error)`.
- The OpenCode server pool is keyed by wire as well as binary, environment and extra arguments, so an `opencode` and an `opencode2` session never share a server.
