# Vendor compatibility

This file records the vendor releases and protocol surfaces that agentwire depends on. It is the record required by the Phase 0 spike of the extraction plan (`docs/plans/2026-09-22-agentwire-harness-library.md`, Part I).

Four labels are used:

- **verified in code**: the surface is in the driver source and covered by fixture tests against the recorded frames.
- **confirmed `<version>` (`<date>`)**: the Phase 0 spike exercised the surface on that vendor release. The command and the output are in the procedure section at the end of this file.
- **refuted `<version>` (`<date>`)**: the spike ran and the result contradicts the plan.
- **not run**: the spike did not exercise the surface. The blocker and the exact command are written out.

The Phase 0 spike ran on 2026-09-22 against the vendor binaries installed on this machine: `claude` 2.1.280, `codex` codex-cli 0.155.1, `opencode` 1.18.29, `pi` 0.84.1, `cursor-agent` 2026.09.18-9a7762b. `copilot` and `gemini` are not installed. No check that spends a live model turn ran; each one is labelled **not run** with its command. A second spike on 2026-10-24 covered `omp` 18.12.6 and `grok` 1.0.70. It ran live turns for both; their sections record the results.

## Claude Code

| Item | Value |
|---|---|
| Binary | `claude` |
| Protocol | stream-json over stdin and stdout (NDJSON) |
| Verified version | 2.1.280 (2026-09-22) |
| Version floor | 2.0.0 (verified in code: `versionFloors` in `detect.go`) |

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `--input-format stream-json` and `--output-format stream-json` frames, including `system/init`, `assistant`, `user`, `result` | verified in code; both flags present in `--help` (2.1.280, 2026-09-22); the frames still need a live turn |
| `--session-id <id>` for a new session and `--resume <id>` for a resume | verified in code; both flags present in `--help` (2.1.280, 2026-09-22) |
| `--permission-prompt-tool stdio`, `--permission-mode default\|acceptEdits\|bypassPermissions` | confirmed 2.1.280 (2026-09-22): all three modes and the hidden `--permission-prompt-tool` are accepted by the CLI; `--help` lists `acceptEdits, auto, bypassPermissions, manual, dontAsk, plan` and omits `default` |
| `can_use_tool` control request and the `control_response` reply | verified in code; not run: needs a live turn |
| `--append-system-prompt` for per-session instructions | verified in code; the flag is present in `--help` (2.1.280, 2026-09-22); the flag together with `--input-format stream-json` needs a live turn |
| `--mcp-config <file>` adds to the user's servers rather than replacing them | not run: needs a live turn. `--help` (2.1.280) says `--mcp-config` loads servers and that a separate `--strict-mcp-config` "Only use MCP servers from `--mcp-config`, ignoring all other MCP configurations", which implies the additive default |
| `--plugin-dir <dir>` loads plugin skills in `-p` stream-json mode | not run: needs a live turn. The flag is present in `--help` (2.1.280, repeatable, "for this session only") |

## Codex

| Item | Value |
|---|---|
| Binary | `codex` |
| Protocol | app-server JSON-RPC over stdin and stdout |
| Verified version | codex-cli 0.155.1 (2026-09-22) |
| Version floor | none (any version the binary reports) |

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `initialize` then the `initialized` notification | verified in code; `initialize` answered in 1.3 s on 0.155.1 and the notification is not required before `thread/start` |
| `thread/start`, `thread/resume` with `cwd`, `approvalPolicy`, `sandbox` | verified in code; `thread/start` with `cwd` returned a thread on 0.155.1; the generated schema lists `cwd`, `approvalPolicy`, `sandbox` on `ThreadStartParams` |
| `turn/start`, `turn/steer` (with `expectedTurnId`), `turn/interrupt` (with `turnId`) | verified in code; not run: a turn needs a live model turn. The generated schema lists `threadId` and `input` as required on `TurnStartParams` |
| Notifications `thread/started`, `turn/started`, `item/*`, and the turn end event | verified in code; `thread/started` and `mcpServer/startupStatus/updated` observed on 0.155.1; `turn/*` needs a live turn |
| `developerInstructions` field name on `thread/start` | confirmed codex-cli 0.155.1 (2026-09-22): `thread/start` with `developerInstructions` returns a result, and the vendor's own generated schema defines `ThreadStartParams.developerInstructions` as `string \| null` |
| `-c mcp_servers.<name>.*` works together with the `app-server` subcommand | confirmed codex-cli 0.155.1 (2026-09-22): the server starts with both `-c` overrides, and `mcpServer/startupStatus/updated` reports the injected `spike` server next to the user's own servers |
| Skills folder location under `CODEX_HOME` | confirmed codex-cli 0.155.1 (2026-09-22): `$CODEX_HOME/skills` is `~/.codex/skills`; `skills/list` reports skills from `~/.codex/skills/.system` (scope `system`) and from `~/.agents/skills` (scope `user`) |
| Effort accepted on `turn/start`, not only through `-c model_reasoning_effort` | confirmed codex-cli 0.155.1 (2026-09-22): the generated schema defines `TurnStartParams.effort` as `ReasoningEffort \| null` ("Override the reasoning effort for this turn and subsequent turns"), where `ReasoningEffort` is any non-empty string |

Extra finding, not in the plan: the app-server exposes `skills/extraRoots/set` and `skills/config/write` on 0.155.1. An extra root passed to `skills/extraRoots/set` makes its skills appear in `skills/list` at run time, with no `CODEX_HOME`.

## OpenCode

| Item | Value |
|---|---|
| Binary | `opencode` |
| Protocol | `serve` REST plus the `/event` SSE stream |
| Verified version | 1.18.29 (2026-09-22) |
| Version floor | none (any version the binary reports) |

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `GET /global/health` | verified in code; not run (the spike used `/config`) |
| `POST /session`, `GET /session/<id>`, `GET /session/<id>/message` | verified in code; not run (a session needs a live turn to be interesting) |
| `POST /session/<id>/prompt_async` | verified in code; the served OpenAPI document lists the path and its body on 1.18.29 |
| `POST /session/<id>/permissions/<id>` | verified in code; not run |
| `POST /session/<id>/abort` | verified in code; not run |
| `GET /event` SSE stream, routed by `sessionID` | verified in code; not run |
| `OPENCODE_CONFIG_CONTENT` for per-server config | verified in code; confirmed 1.18.29 (2026-09-22): the served config takes the keys from the content and still carries the user's own MCP servers |
| HTTP basic auth on the server | verified in code; not run |
| `OPENCODE_CONFIG_DIR` adds to the user config instead of replacing it | confirmed 1.18.29 (2026-09-22): the served config keeps the user's `username`, `model` and MCP servers (`agent-browser`, `pencil`, `sling`, `trebi`) while the spike directory is present |
| `POST /mcp` at run time | confirmed 1.18.29 (2026-09-22): the endpoint adds a server at run time and returns the status map; the body must be `{"name":…,"config":{…}}`. The body in the spike procedure returns 400 `Missing key at ["config"]` |
| Skills folder name (`skill`) under the config dir | confirmed 1.18.29 (2026-09-22): `GET /skill` lists a skill from `<dir>/skill/<name>/SKILL.md` and from `<dir>/skills/<name>/SKILL.md`. `GET /config` never reports skills |
| `system`, `model` and `variant` on `prompt_async` | confirmed 1.18.29 (2026-09-22): the served OpenAPI document gives `prompt_async` a body of `parts` plus `system` (string), `model` (`{providerID, modelID}`) and `variant` (string) |

## OpenCode 2 (beta)

| Item | Value |
|---|---|
| Binary | `opencode2` |
| Protocol | `/api` REST plus the `/api/event` SSE stream |
| Verified version | `opencode2 v0.0.0-beta-19242` (2026-09-22) |
| Version floor | none (any version the binary reports) |

The V2 beta is a separate binary from the V1 line, so both are driven side by side. V2 has no compatibility layer: every unprefixed path answers with the web UI, so a V1 client that talks to it reads HTML instead of JSON.

Wire differences from V1 that the driver handles:

| Surface | V1 | V2 |
|---|---|---|
| API root | `/` | `/api` |
| Readiness | `GET /global/health` | `GET /api/health` |
| Response shape | the payload itself | `{"data": …}` |
| Event envelope | `{type, properties}` | `{id, type, data}` |
| Event scope | one stream per session directory | one stream for every location |
| Session create | `POST /session` → `{id}` | `POST /api/session` with `location` → `{data:{id}}` |
| Prompt | `POST …/prompt_async` with `parts` | `POST …/prompt` with `text` and `files` |
| Model pin | `model` on the prompt body | `model` on the create body, or `POST …/model` |
| Permission reply | `POST …/permissions/<id>` `{response}` | `POST …/permission/<id>/reply` `{reply}` |
| Interrupt | `POST …/abort` | `POST …/interrupt`, no body |
| Turn end | `session.idle` | `session.execution.succeeded` / `session.execution.failed` |

Protocol surfaces the library depends on, all confirmed on `0.0.0-beta-19242` (2026-09-22) against a running server, with a real model turn:

| Surface | Status |
|---|---|
| `GET /api/health` | confirmed: `{"healthy":true,"version":"0.0.0-beta-19242","pid":…}` |
| `POST /api/session` with `location.directory` | confirmed: the session is created in that directory and reports it back |
| `GET /api/session/<id>`, `GET /api/session/<id>/message` | confirmed: both answer by session id; the `location` query parameter is accepted and changes nothing, so per-session calls need no directory |
| `POST /api/session/<id>/prompt` | confirmed: a body of `{"text": …}` starts a turn. A V1 body with `parts` returns 400 `Missing key at ["text"]` |
| `POST /api/session/<id>/model` | confirmed: `{"model":{"providerID":…,"id":…}}` selects the model, and the session reports it in `session.model.selected` |
| `PUT /api/experimental/session/<id>/instructions/entries/<key>` | confirmed: an entry reaches the model. OpenCode 2.0.26 (2026-10-18) serves it under `/api/experimental` only; `/api/session/<id>/instructions/...` returns 404. The key must match `^[a-z0-9][a-z0-9._-]*$`; `core/instructions` returns 404 because of the slash, while `agentwire.instructions` is accepted |
| `POST /api/session/<id>/interrupt` | confirmed by the live tier |
| `POST /api/session/<id>/permission/<id>/reply` | confirmed by the unit tier; the served OpenAPI document gives the body as `{"reply":"once"\|"always"\|"reject"}` |
| `GET /api/event` SSE stream | confirmed: no query parameters, one connection for every location, events routed by `data.sessionID` |
| Event vocabulary | confirmed from real turns: `session.created`, `session.execution.started/succeeded/failed`, `session.step.started/streamed/ended/failed`, `session.text.started/delta/ended`, `session.reasoning.started/delta/ended`, `session.tool.input.started/delta/ended`, `session.tool.called/success/failed/progress`, `session.usage.updated`, `session.inbox.enqueued/delivered`, `session.instructions.updated`, `session.model.selected`, `permission.asked`, `shell.created/exited` |
| Structured errors | confirmed: `session.step.failed` carries `error:{type:"provider.auth",message:…,status:401}`. The driver maps the type before it falls back to text |
| `OPENCODE_SERVER_PASSWORD` / `OPENCODE_SERVER_USERNAME` | confirmed: the server takes the password from the environment and does not print one; a wrong password answers 401 |
| `OPENCODE_CONFIG_CONTENT` and `OPENCODE_CONFIG_DIR` | confirmed present in the binary, so per-server MCP injection and an isolated home work as they do on V1 |
| `opencode2 auth list` | confirmed: prints the stored credentials, so the login probe is the V1 probe |

Findings that are not wire facts:

- `opencode2` does not take the `model` key of `~/.config/opencode/opencode.json` as its session default. A fresh session started on `anthropic/claude-opus-5-5` while the config named `opencode-go/deepseek-v4.1-flash`, and the turn then failed with `provider.auth`. Pin the model with `StartRequest.Model` (or `AGENTWIRE_LIVE_MODEL_OPENCODE2` for the live tier) instead of relying on the config default.
- The V2 event stream does not replay history: a stream opened after a session exists receives only later events. The library opens the stream before it creates the session, and waits for the connection before it sends the create.
- `session.step.ended` ends a step, not a turn: a turn with tool calls has several steps and ends at `session.execution.succeeded`.
- `session.created` is published while the create is still in flight, so it can reach the stream before the session is routable and be dropped. V2 does not repeat it, so the driver reports the session itself once the create answers.
- `session.text.ended` can carry text that never streamed as a delta, for example a trailing exit code. The driver reconciles the block against what it already sent and forwards only the difference.

## pi

| Item | Value |
|---|---|
| Binary | `pi` |
| Protocol | pi RPC (NDJSON) over stdin and stdout |
| Verified version | 0.84.1 (2026-09-22) |
| Version floor | none (any version the binary reports) |

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `ready`, `agent_start`, `turn_start`, `message_start`, `message_update`, `message_end`, `turn_end` frames | verified in code; not run: needs a live turn |
| `permission_request` frame and the `permission_response` reply | verified in code; not run: needs a live turn |
| `--session-id <id>` for a new session and `--session <id>` for a resume | verified in code; both flags present in `--help` (0.84.1, 2026-09-22) |
| `--skill <dir>` for per-session skills | confirmed 0.84.1 (2026-09-22): `--skill <path>` is in `--help` ("Load a skill file or directory (can be used multiple times)"); skill discovery is native, because `--no-skills` disables it |
| `--append-system-prompt` for per-session instructions | confirmed 0.84.1 (2026-09-22): `--append-system-prompt <text>` is in `--help` (repeatable) |
| MCP through `<PI_CODING_AGENT_DIR>/mcp.json`, read by `pi-mcp-adapter` | not run: the path and the shape hold (`~/.pi/agent/mcp.json` holds an `mcpServers` object, and `PI_CODING_AGENT_DIR` defaults to `~/.pi/agent`), but `pi list` prints `No packages installed`, so `pi-mcp-adapter` is absent and `pi --help` shows no MCP flag. Confirming that `pi` loads the file needs a live turn |

## ACP agents (Copilot, Cursor, Gemini)

| Item | Value |
|---|---|
| Binaries | `cursor-agent` 2026.09.18-9a7762b (2026-09-22). `copilot` and `gemini` are not installed |
| Protocol | ACP (JSON-RPC over stdin and stdout) |
| Verified version | cursor-agent 2026.09.18-9a7762b (2026-09-22); copilot and gemini not installed |
| Version floor | none (any version the binary reports) |

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `initialize` with `protocolVersion: 1` and `clientCapabilities` | confirmed cursor-agent 2026.09.18-9a7762b (2026-09-22): the result echoes `protocolVersion: 1`, `agentCapabilities.loadSession: true`, `mcpCapabilities: {http: true, sse: true}`, `promptCapabilities.image: true` and `authMethods: [{id: "cursor_login", …}]` |
| `session/new`, `session/load`, `session/resume` | refuted for cursor-agent 2026.09.18-9a7762b (2026-09-22): `session/new` returns a result, but `session/resume` returns `-32601 "Method not found": session/resume`, and `session/load` returns `-32602 "Session … not found"` for both a fresh `session/new` id and a fresh `create-chat` id, although the agent advertises `loadSession: true` |
| `session/prompt`, `session/cancel` | verified in code; not run: a prompt needs a live turn |
| `session/request_permission` and the permission reply | verified in code; not run: needs a live turn |
| `session/update` notifications, including agent message chunks and tool calls | verified in code; `session/update` observed on cursor-agent (the `available_commands_update` variant); tool calls need a live turn |
| `authenticate` with a chosen `methodId` from `authMethods` | verified in code; not run: needs a live turn. cursor-agent advertises one method, `cursor_login` |
| `mcpServers` in `session/new`, `session/load` and `session/resume` | cursor-agent: confirmed accepted by `session/new` (2026.09.18-9a7762b, 2026-09-22 — the call returns a result with the `mcpServers` entry, and the result does not echo it). `session/load` and `session/resume` fail for the reasons above. copilot and gemini: not run, the binaries are not installed |
| `session/set_model` when the agent lists models | confirmed cursor-agent 2026.09.18-9a7762b (2026-09-22): `session/new` returns `models.availableModels`, and `session/set_model` answers with an empty result |
| `fs.readTextFile` and `fs.writeTextFile` through the advertised `fs` capability | not run: the client must advertise the capability and then a live turn must trigger a file read or write |
| Gemini ACP entry flag (`--experimental-acp`, or `--acp` in newer releases) | not run: `gemini` is not installed. For contrast, the cursor-agent ACP entry point is the undocumented `acp` subcommand: `cursor-agent --help` lists `create-chat` but not `acp`, while `cursor-agent acp --help` prints "Start the Cursor Agent as an ACP (Agent Client Protocol) server" |

Cursor is started with `cursor-agent create-chat` before the ACP handshake. Copilot resume uses `--resume <id>` plus ACP resume. Both are verified in code. The spike confirms that `create-chat` returns a chat id, but that id does not load through `session/load` on cursor-agent 2026.09.18-9a7762b, so the fresh-id `session/load` path of the plan needs a live turn before a tag.

## omp (Oh My Pi)

| Item | Value |
|---|---|
| Binary | `omp` (`omp/18.12.6`, 2026-10-24) |
| Protocol | ACP (JSON-RPC over stdin and stdout), entry point `omp acp` |
| Verified version | 18.12.6 (2026-10-24) |
| Version floor | 18.0.0 |

omp is the pi fork from can1357. `omp acp` parses the root flags (`--append-system-prompt`, `--approval-mode`, `--thinking`, `--model`), so the library uses the ACP driver, not the pi RPC wire. The pi RPC driver stays for `pi`.

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `initialize` with `protocolVersion: 1` and `clientCapabilities` | confirmed omp/18.12.6 (2026-10-24): the result carries `agentCapabilities.loadSession: true` and `sessionCapabilities` (`fork`, `resume`, `close`), `authMethods: [{id: "agent"}]`, and `_meta.steering.supported: true` |
| `session/new` with `mcpServers` | confirmed: the result returns `configOptions` for `mode`, `model` and `thinking`, and `_meta.steering`. A bad MCP server is reported as an `agent_message_chunk` warning; the session stays open |
| `session/prompt`, `session/cancel` | confirmed: a turn produced `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `usage_update`, `session_info_update` and `available_commands_update`; `session/cancel` returned `stopReason: "cancelled"` and the result ended the turn |
| `session/request_permission` and the permission reply | confirmed with `--approval-mode always-ask`: one request with four options (`allow_once`, `allow_always`, `reject_once`, `reject_always`); `allow_once` continued the turn |
| `auto_edit` approval mode | not run: omp refuses to switch the mode in a live session, and the spike did not start a separate session for it. The driver stays on `always-ask` plus the policy |
| `yolo` approval mode (`--approval-mode yolo`) | confirmed: no permission request, and the tool turn completed |
| `_session/steering` | confirmed: `initialize` advertises it, a steer during a shell tool joined the turn, and a steer before the turn returned a message that becomes `ErrUnsupported` (recorded in `testdata/acp/omp/steering.ndjson`) |
| `session/set_config_option` for `model` and `thinking` | confirmed: `configId` is `model` or `thinking`, the value is a string (`provider/model`, or a thinking level), and the reply returns the full updated option list |
| `session/resume` | confirmed: the resumed session kept the prior turn in its history |
| `--append-system-prompt` instructions | confirmed: a marker instruction reached the model on the first turn |
| `--plugin-dir` skills | confirmed: `/skill:agentwire-live-marker` listed the injected skill, and a turn read its marker |
| `--allow-home` | confirmed flag in `omp acp --help`; not exercised live |
| `--cwd` and the automatic temp-dir switch when started in `$HOME` | recorded from the 18.12.6 source; not observed live |
| `PI_CODING_AGENT_DIR` home isolation | confirmed: a fresh home lists 0 models and has no login. `--profile` isolates auth the same way |
| Login probe (`omp models --json`) | confirmed: a logged-in machine returns the provider/model list (80 models at the spike); a fresh home returns an empty list |
| `fs.readTextFile` and `fs.writeTextFile` | not exercised: no `fs/*` request arrived during the tool turn. The shared ACP handler serves them, so `Features.FS` is true like the other ACP kinds |

Model ids are `provider/model` (`opencode-go/deepseek-v4.1-flash`). The model config option lists 80 entries on this machine, and `--model` travels straight into the session config.

## Grok (xAI)

| Item | Value |
|---|---|
| Binary | `grok` (`grok 1.0.70 (f50904eaee6c) [stable]`, 2026-10-24) |
| Protocol | ACP (JSON-RPC over stdin and stdout), entry point `grok agent --no-leader stdio` |
| Verified version | 1.0.70 (2026-10-24) |
| Version floor | 1.0.0 |

Grok Build is documented to integrate through ACP (Zed, agent-shell, Endo, Verun). The one-shot `grok -p --output-format streaming-json` is not used.

Protocol surfaces the library depends on:

| Surface | Status |
|---|---|
| `initialize` | confirmed logged out (2026-10-24): `authMethods: [{id: "grok.com"}]` and `_meta.modelState` with two models. Logged in: `authMethods` adds `cached_token`, and `_meta.modelState` lists four models |
| `session/new` with `mcpServers` and `_meta` | confirmed: `_meta.rules` reached the model (marker text), `_meta.yoloMode` is accepted with `--always-approve`, and the result returns `configOptions` (`model`, `reasoning_effort`) plus the `models` header. A bad MCP server logs `Failed to spawn MCP server '<name>'` and does not close the session |
| `session/new` without a login | confirmed: `-32000 "Authentication required"` with `data: "no auth method id provided"` |
| `authenticate` | not run: the spike used `grok login --device-auth`. The library never calls it, because it can start a browser or device login |
| `session/prompt`, `session/cancel` | confirmed: all the session update variants arrived, plus `x.ai/mcp/server_status` and `x.ai/session/update`; `session/cancel` returned `stopReason: "cancelled"` |
| `session/request_permission` and the permission reply | refuted on 1.0.70: no request arrived in `default` or ask mode, including an isolated `GROK_HOME` with `[ui] permission_mode = "default"` and `_meta.yoloMode: false`; the tool ran without approval. `Features.Permissions` is false |
| `--always-approve` | confirmed: no approval, and a shell tool turn completed |
| `session/set_config_option` for `model` and `reasoning_effort` | confirmed: the value is a string, and the reply returns the updated option list. An object value (`{"value": …}`) is rejected with `-32602`. A bad model id answers `data: "unknown model id"` |
| Steering | refuted: `initialize` advertises no steering, and a second prompt during a turn got no reply within 30 seconds (the vendor queues it). `Features.Steer` is false |
| `session/resume` and `session/load` | confirmed: both kept the prior turn in the history |
| `_meta.rules` instructions | confirmed: a marker instruction reached the model |
| `--plugin-dir` skills | confirmed: the injected skill listed and its marker was read |
| `GROK_HOME` home isolation | confirmed for auth: a fresh home is not authenticated, and copying `auth.json` restores the login. Config isolation is partial: a user MCP server still appeared under the fresh home |
| `--no-leader` | accepted; without it a shared leader can outlive the session. The spike did not compare the two |
| Login probe (`grok models`) | confirmed: it prints "You are not authenticated." when logged out and "You are logged in with grok.com." when logged in |
| Error `data` text | confirmed for a bad model id. The HTTP 402 plan case was not forced; the library appends `error.data` text and a unit test covers it |

## Models and mid-turn input

Added in v0.4.0. "Live" means the live tier (`live/models_test.go`) passed on this machine on 2026-09-27. `TestLiveModels` lists the models, runs a turn, calls `SetModel`, and runs a second turn. `TestLiveSteer` sends `Steer` while a shell tool runs, and checks that one turn took both inputs.

| Harness | List | Set | Steer | Tested version | Result |
|---|---|---|---|---|---|
| Claude | `initialize` control response, `response.models[]` (`value`, `displayName`, `description`). Falls back to the aliases `default`, `opus`, `sonnet`, `haiku` | `set_model` control request. A bad id answers `subtype: error` | a second `user` frame during the turn | 2.1.280 | live: list, set, steer. After `SetModel("haiku")`, the assistant frames of the next turn name the Haiku model. The steer joined the turn, and one `result` followed |
| Codex | `model/list` over `nextCursor` pages. Hidden models are left out | `model` on the next `turn/start` | `turn/steer` with `expectedTurnId`. A refused steer returns `ErrUnsupported` and never interrupts | codex-cli 0.157.1 | live: list, set, steer. The vendor frames do not echo the model, so the live tier checks that the next turn succeeds |
| OpenCode | `GET /config/providers`, default from `GET /config`. Without a server: `opencode models` | `model` on the next `prompt_async` | none. `Prompt` during a turn returns `ErrTurnActive` | 1.18.29 | live: list, set |
| OpenCode 2 | `GET /api/model` (polled while the catalog loads), default from `GET /api/model/default`. Disabled models are left out | `POST /api/session/<id>/model` | none. `Prompt` during a turn returns `ErrTurnActive` | `v0.0.0-beta-19242` | live: list, set. A set to a model with an expired login failed the next turn with that login error, so the change took effect |
| pi | `get_available_models` on `pi --mode rpc --no-session`, default from `get_state` | `set_model {provider, modelId}`. The id must be `provider/model` | `steer {message}`. Pi delivers it after the running tool calls | 0.84.1 | live: list, set. Steer is in code and in the unit tier, but `Features.Steer` is false: the only provider on this machine (xAI) has an expired login, so no live turn ran |
| Cursor | `session/new` result, `models.availableModels` and `currentModelId` | `session/set_model` | none | 2026.09.26-dd393fe | live: list, set. The set also changes the default model of the Cursor account |
| Copilot, Gemini | as Cursor, when the agent offers a model list | as Cursor. Without a list, `SetModel` returns `ErrUnsupported` | none | not installed | not run |
| omp | `configOptions` model option in the `session/new` result (`provider/model` ids), default from `currentValue` | `session/set_config_option` (`configId: "model"`, string value) | `_session/steering` while a turn runs. A refusal returns `ErrUnsupported`, so `Send` holds the input | 18.12.6 | live: list, set, steer. After `SetModel` the config echoed the new value and the next turn succeeded. A steer during a shell tool joined the turn |
| Grok | `initialize` `_meta.modelState.availableModels`, and the `session/new` `configOptions` plus `models` | `session/set_config_option` (`configId: "model"`, string value) | none. A second prompt during a turn is queued by the vendor, so `Features.Steer` is false and `Send` holds the input | 1.0.70 | live: list, set, steer hold. The model probe needs no session and works logged out. No permission request arrives on stdio |

Notes:

- The model lister never calls ACP `authenticate`, so a list call cannot start a login flow.
- OpenCode 2 has a `POST /api/session/<id>/inbox/<inboxID>/steer` route. The library does not use it yet.
- Cursor model ids carry parameters, for example `grok-4.7[context=256k,reasoning_effort=high,fast=true]`. Pass the id as the list gives it.
- omp model ids are `provider/model`. The `thinking` config option takes a level string (minimal, low, medium, high, max on 18.12.6), and `--thinking` at launch reaches the same option.
- Grok lists four models on this account (`grok-4.7`, `grok-4.7-build-fast`, `grok-4.6`, `grok-4.5`); the default is `grok-4.7`. `reasoning_effort` accepts `low`, `medium`, `high`, `xhigh`; `Effort: "max"` maps to `xhigh`.
- A `SetModel` probe must not call `authenticate`: omp has one auth method (`agent`), and grok's `grok.com` method can start a browser or device login.

## fake

| Item | Value |
|---|---|
| Binary | a POSIX shell, driven by `AGENTWIRE_FAKE_SCRIPT` |
| Protocol | agentwire NDJSON (`assistant`, `result`, `tool`, `permission`) |
| Verified version | not applicable |

The fake driver exists for consumer tests. It is always supported, and `Runtime.Detect` agrees: it reports the fake as `supported=true auth=ok` on 2026-09-22 with no binary to find on `PATH`.

## Phase 0 spike procedure

The omp and grok spike of 2026-10-24 followed this procedure. Their sections above record the results, and `testdata/acp/omp/` and `testdata/acp/grok/` hold the frames.

Run these commands on a current vendor release before the first tag. Each command confirms one item that this file marked unverified before the spike. Record the output in this file, with the exact version and date. The results below were recorded on 2026-09-22.

Confirm the binary, the version and the login state with `Runtime.Detect`:

```go
det, err := rt.Detect(ctx, agentwire.Claude)
fmt.Println(det.Path, det.Version, det.Supported, det.Auth)
```

Result, 2026-09-22, against the library on `main` (module `github.com/trebi-ai/agent-wire`) through a throwaway program in a temp dir.

```sh
$ go run .        # agentwire.New(Options{ClientName: "spike"}), then rt.Detect(ctx, h) per harness
claude   path="/Users/fritz/.nvm/versions/node/v22.19.0/bin/claude" version="2.1.280 (Claude Code)" supported=true auth=ok authDetail="<account email>"
codex    path="/Users/fritz/.local/bin/codex"                       version="codex-cli 0.155.1"      supported=true auth=ok
opencode path="/Users/fritz/.opencode/bin/opencode"                 version="1.18.29"                supported=true auth=ok
pi       path="/Users/fritz/.nvm/versions/node/v22.19.0/bin/pi"     version="0.84.1"                 supported=true auth=unknown
copilot  path="" supported=false detail="copilot is not installed"
cursor   path="/Users/fritz/.local/bin/cursor-agent"                version="2026.09.18-9a7762b"     supported=true auth=unknown
gemini   path="" supported=false detail="gemini is not installed"
fake     path="" supported=true auth=ok
```

The versions agree with `--version` on every binary. The only open gap is the login state: `Detect` reports `auth=unknown` for pi and cursor, so their auth probes do not prove a login. The fake harness reports `supported=true auth=ok` without a binary, which matches "the fake is always supported".

1. **Claude `--plugin-dir` and `--mcp-config`** (items a, b):

```sh
# A plugin with one skill, plus a marker MCP server, in a temp dir.
mkdir -p /tmp/aw-spike/plugin/.claude-plugin /tmp/aw-spike/plugin/skills/marker
printf '{"name":"spike"}\n' > /tmp/aw-spike/plugin/.claude-plugin/plugin.json
printf -- '---\nname: marker\ndescription: Spike marker\n---\nSay the word SPIKE-MARKER.\n' > /tmp/aw-spike/plugin/skills/marker/SKILL.md

claude --version
printf '{"mcpServers":{"spike":{"command":"/bin/echo","args":["hi"]}}}\n' > /tmp/aw-spike/mcp.json

claude -p --output-format stream-json --verbose \
  --plugin-dir /tmp/aw-spike/plugin \
  --mcp-config /tmp/aw-spike/mcp.json \
  "List your skills and your MCP servers."
```

Pass when the skill list contains `marker` and the MCP list contains the user's own servers plus `spike`. Do not pass `--strict-mcp-config`: injection must stay additive.

Result, 2026-09-22, claude 2.1.280. The deterministic part ran; the `claude -p` turn did not.

```sh
$ claude --version
2.1.280 (Claude Code)

$ claude --help | grep -E -- '--plugin-dir|--mcp-config|--append-system-prompt|--permission-prompt-tool|--permission-mode|--input-format|--output-format|--session-id|--resume|--strict-mcp-config'
  --append-system-prompt <prompt>       Append a system prompt to the default
  --input-format <format>               Input format (only works with --print):
  --mcp-config <configs...>             Load MCP servers from JSON files or
  --output-format <format>              Output format (only works with --print):
  --permission-mode <mode>              Permission mode to use for the session
  --plugin-dir <path>                   Load a plugin from a directory or .zip
  -r, --resume [value]                  Resume a conversation by session ID, or
  --session-id <uuid>                   Use a specific session ID for the
  --strict-mcp-config                   Only use MCP servers from --mcp-config,

$ claude --permission-prompt-tool stdio --version
2.1.280 (Claude Code)

$ claude --permission-mode nonsense --version
error: option '--permission-mode <mode>' argument 'nonsense' is invalid. Allowed choices are acceptEdits, auto, bypassPermissions, manual, dontAsk, plan.

$ claude --permission-mode default --version
2.1.280 (Claude Code)
```

**Verdict: partial — flags confirmed, behaviour not run.** Every flag the plan names is present except `--permission-prompt-tool`, which is hidden from `--help` but accepted. `--permission-mode default` is accepted although the help omits it, so the plan's mapping holds. The additive `--mcp-config` claim and the plugin skill load still need the `claude -p` command above with a live turn.

2. **Codex `developerInstructions`, `-c mcp_servers.*` and skills** (items c, d, e):

```sh
codex --version
codex app-server -c mcp_servers.spike.command="/bin/echo" -c mcp_servers.spike.args='["hi"]' <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"spike","version":"0"}}}
{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"cwd":"/tmp","developerInstructions":"Say SPIKE-MARKER."}}
EOF
ls "$CODEX_HOME/skills" 2>/dev/null || ls ~/.codex/skills
```

Pass when `thread/start` accepts `developerInstructions` without an error, the server starts with the `-c` overrides, and the skills path is the one the plan names.

Result, 2026-09-22, codex-cli 0.155.1. All three checks ran. Stdin was held open for 5 s after the second line, because the server exits on EOF and drops a reply that is still pending.

```sh
$ codex --version
codex-cli 0.155.1

$ codex app-server -c mcp_servers.spike.command="/bin/echo" -c mcp_servers.spike.args='["hi"]' <<'EOF'   # stdin held open 5 s
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"spike","version":"0"}}}
{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"cwd":"/tmp","developerInstructions":"Say SPIKE-MARKER."}}
EOF
{"id":1,"result":{"userAgent":"spike/0.155.1 (Mac OS 15.7.3; arm64) …","codexHome":"/Users/fritz/.codex","platformFamily":"unix","platformOs":"macos"}}
{"id":2,"result":{"thread":{"id":"01a0cb0f-fdde-79c1-bab6-a7b3452b2370","environments":[{"environmentId":"local","cwd":"/tmp","runtimeWorkspaceRoots":["/tmp"]}], …

# The -c overrides applied, and the user's own servers still loaded:
{"method":"mcpServer/startupStatus/updated","params":{"threadId":"01a0cb08-…","name":"spike","status":"starting","error":null,…}
{"method":"mcpServer/startupStatus/updated","params":{"threadId":"01a0cb08-…","name":"spike","status":"failed","error":"MCP client for `spike` failed to start: …"}
{"method":"mcpServer/startupStatus/updated","params":{"threadId":"01a0cb08-…","name":"sling","status":"ready",…}
# also seen: agent-browser, pencil, codex_apps, sling-platform-dev
# `spike` fails only because /bin/echo is not an MCP server.

$ ls ~/.codex/skills        # empty, and the directory exists

$ codex app-server generate-json-schema --out <dir>
ThreadStartParams.properties: approvalPolicy, approvalsReviewer, baseInstructions, config, cwd, developerInstructions, ephemeral, model, modelProvider, personality, sandbox, …
  .properties.developerInstructions = {"type":["string","null"]}
TurnStartParams.properties: approvalPolicy, approvalsReviewer, clientUserMessageId, cwd, effort, input, model, personality, sandboxPolicy, …, summary, threadId, toolOutput, turnTrigger
  .properties.effort = {"anyOf":[{"$ref":"#/definitions/ReasoningEffort"},{"type":"null"}]}   # "Override the reasoning effort for this turn and subsequent turns."
  required = ["input","threadId"]

$ codex app-server  →  {"id":2,"method":"skills/list","params":{"cwds":["/tmp"],"forceReload":true}}
  cwd /tmp, errors [], 32 skills
  roots: /Users/fritz/.codex/skills/.system (6, scope system), /Users/fritz/.agents/skills (26, scope user)
```

**Verdict: confirmed.** The `developerInstructions` field name, the `-c mcp_servers.*` overrides next to the `app-server` subcommand, `effort` on `turn/start`, and the skills folder `$CODEX_HOME/skills` all hold on codex-cli 0.155.1. One trap: the heredoc in this procedure closes stdin at once, so `thread/start` gets no reply. Hold stdin open until both replies arrive.

3. **OpenCode config dir, `POST /mcp` and skills** (items f, g, h):

```sh
opencode --version
mkdir -p /tmp/aw-oc/skill/marker
printf -- '---\nname: marker\ndescription: Spike marker\n---\nSay SPIKE-MARKER.\n' > /tmp/aw-oc/skill/marker/SKILL.md
OPENCODE_CONFIG_DIR=/tmp/aw-oc opencode serve --port 4096 &
curl -s localhost:4096/config | head -c 2000
curl -s -X POST localhost:4096/mcp -H 'content-type: application/json' \
  -d '{"name":"spike","type":"local","command":["/bin/echo","hi"]}'
```

Pass when the served config shows the user's own settings plus the spike skill, and `POST /mcp` answers without an error. If `POST /mcp` works, the driver can keep one shared server per fingerprint. If it does not, the driver must key the fingerprint on the MCP list.

Result, 2026-09-22, opencode 1.18.29. All three ran, on a free port, with the server killed afterwards.

```sh
$ opencode --version
1.18.29

$ opencode --help | grep -E -- 'serve|--port'
  opencode serve               starts a headless opencode server
      --port          port to listen on                                        [number] [default: 0]

$ OPENCODE_CONFIG_DIR=<tmp> opencode serve --port <free>
$ curl -s localhost:<port>/config
{"$schema":…,"model":"xai/grok-4.7","username":…,"mcp":{"agent-browser":…,"pencil":…,"sling":…,"trebi":…}, …}
# The user's own settings survive. The spike skill does not appear here at all.

$ curl -s localhost:<port>/skill
… {"name":"singular-marker","location":"<tmp>/skill/singular-marker/SKILL.md","content":"…"}
… {"name":"plural-marker","location":"<tmp>/skills/plural-marker/SKILL.md","content":"…"}
# Plus 43 skills of the user, loaded from ~/.claude/skills and ~/.agents/skills.

$ curl -s -X POST localhost:<port>/mcp -d '{"name":"spike","type":"local","command":["/bin/echo","hi"]}'
400 {"data":{"message":"Missing key\n  at [\"config\"]","kind":"Payload"},"name":"UnknownError"}

$ curl -s -X POST localhost:<port>/mcp -d '{"name":"spike","config":{"type":"local","command":["/bin/echo","hi"]}}'
200 {"agent-browser":…,"pencil":…,"sling":…,"spike":{"status":"failed","error":"MCP error -32000: Connection closed"},"trebi":…}
# `spike` fails only because /bin/echo is not an MCP server. It was added at run time.
# After the POST, GET /config still lists only the four user servers, so POST /mcp is runtime-only state.

$ curl -s localhost:<port>/doc        # the served OpenAPI document
POST /session/{sessionID}/prompt_async body properties: messageID, model {providerID, modelID}, agent, noReply, tools, format, system, variant, parts; required: ["parts"]; additionalProperties: false
POST /mcp body: {"name":string, "config": McpLocalConfig | McpRemoteConfig}, both required, additionalProperties: false

$ OPENCODE_CONFIG_CONTENT='{"username":"spike-content-probe"}' opencode serve --port <free>
$ curl -s localhost:<port>/config
{"username":"spike-content-probe", …,"mcp":{"agent-browser":…,"pencil":…,"sling":…,"trebi":…}}
```

**Verdict: f confirmed, h confirmed, g confirmed with a corrected body.** `OPENCODE_CONFIG_DIR` adds to the user's config. Both `<dir>/skill/<name>/SKILL.md` and `<dir>/skills/<name>/SKILL.md` load; `GET /skill` is the endpoint that shows them, never `GET /config`. `POST /mcp` does add a server at run time, but the body in this procedure is wrong: the endpoint wants `{"name":…,"config":{…}}` and answers 400 otherwise. The driver keeps one shared server per fingerprint and sends `system`, `model` and `variant` on `prompt_async`.

4. **pi `--skill` and `--append-system-prompt`** (item i):

```sh
pi --version
pi --help | grep -E -- '--skill|--append-system-prompt'
```

Pass when both flags appear. If a flag is missing, the driver falls back to the overlay home and the instructions index.

Result, 2026-09-22, pi 0.84.1.

```sh
$ pi --version
0.84.1

$ pi --help | grep -E -- '--skill|--append-system-prompt'
  --append-system-prompt <text>  Append text or file contents to the system prompt (can be used multiple times)
  --skill <path>                 Load a skill file or directory (can be used multiple times)

$ pi --help | grep -E -- 'mcp'      # no output: pi has no MCP flag
$ pi list
No packages installed.
$ jq -r 'keys[]' ~/.pi/agent/mcp.json
mcpServers                          # values: agent-browser, sling
$ pi --help | grep -E -- 'PI_CODING_AGENT_DIR'
  PI_CODING_AGENT_DIR              - Config directory (default: ~/.pi/agent)
```

**Verdict: partial — both flags confirmed; the MCP path is not run.** `--skill` and `--append-system-prompt` exist on 0.84.1, so the native skill strategy needs no overlay. The MCP claim stays open: the file and its `mcpServers` shape are at the path the plan names, but `pi-mcp-adapter` is not installed and `pi --help` never mentions MCP, so whether `pi` reads the file needs a live turn.

5. **ACP `mcpServers` and `session/set_model`** (Copilot, Cursor, Gemini):

```sh
gemini --help | grep -E -- '--acp|--experimental-acp'
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[{"name":"spike","command":"/bin/echo","args":["hi"],"env":[]}]}}' \
  | gemini --experimental-acp
```

Pass when `session/new` accepts the `mcpServers` entry and the initialize result advertises models for `session/set_model` (or does not, in which case the driver keeps the launch-time model flag).

Result, 2026-09-22. `gemini` and `copilot` are not installed, so this command did not run. The same two lines were sent to cursor-agent 2026.09.18-9a7762b, through its `acp` subcommand.

```sh
$ command -v gemini copilot
gemini: NOT INSTALLED
copilot: NOT INSTALLED

$ cursor-agent --version
2026.09.18-9a7762b

$ cursor-agent --help | grep -iE -- 'acp|create-chat'
  create-chat                  Create a new empty chat and return its ID
# `acp` is not listed. It exists as a hidden subcommand:
$ cursor-agent acp --help
Usage: agent acp [options]
Start the Cursor Agent as an ACP (Agent Client Protocol) server

$ printf '%s\n' <initialize> <session/new> | cursor-agent acp
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":true},"promptCapabilities":{"audio":false,"embeddedContext":false,"image":true},"sessionCapabilities":{"list":{}}},"authMethods":[{"id":"cursor_login","name":"Cursor Login",…}]}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"96e7a8e2-…","modes":{"currentModeId":"agent","availableModes":[…]},"models":{"currentModelId":"grok-4.6[effort=high,fast=true]","availableModels":[…22 models…]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"96e7a8e2-…","update":{"sessionUpdate":"available_commands_update",…}}}

$ session/set_model {"sessionId":"96e7a8e2-…","modelId":"grok-4.7[context=256k,reasoning_effort=high,fast=true]"}
{"jsonrpc":"2.0","id":3,"result":{}}

$ session/resume {"sessionId":"96e7a8e2-…","cwd":"/tmp","mcpServers":[…]}
{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"\"Method not found\": session/resume",…}}

$ session/load {"sessionId":"96e7a8e2-…" | "<fresh create-chat id>","cwd":"/tmp","mcpServers":[…]}
{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"Invalid params","data":{"message":"Session \"96e7a8e2-…\" not found"}}}

$ cursor-agent create-chat
504dfbd2-9265-484b-8e77-c123bfd47cdf
```

**Verdict: partial — cursor-agent confirmed for `session/new` with `mcpServers` and for `session/set_model`; refuted for `session/resume`; not run for copilot, gemini, and the `fs` capability.** `session/new` takes the `mcpServers` entry and returns a result. `session/set_model` is live and returns an empty result. `session/resume` does not exist on cursor-agent 2026.09.18-9a7762b. `session/load` refuses both a fresh `session/new` id and a fresh `create-chat` id with `-32602`, even though the agent advertises `loadSession: true`. The `fs` capability and the copilot and gemini rows need the binaries, or a live turn.

## What the drivers must change in the plan

- Codex can take per-session skills without a home. `skills/extraRoots/set` adds a skill root at run time on 0.155.1, and `skills/list` reports the new skills. The Part E table says "overlay only with `Home`" and an instructions fallback without it. Prefer the native call. It also retires the reason to set `CODEX_HOME` for skills.
- The `POST /mcp` body in Part D and in the spike procedure is wrong. OpenCode wants `{"name":…,"config":{"type":"local","command":[…]}}`. With the corrected body the endpoint works, so the driver can keep one shared server per fingerprint, as Part G.5 hopes.
- `GET /config` reports no skills. A skill check must read `GET /skill` (or `GET /api/skill`).
- Cursor has no `session/resume`, and `session/load` refuses a fresh id. Part C.2 gives cursor the resume path `session/resume` first and `session/load` second. On 2026.09.18-9a7762b only `session/load` exists and it needs an id the agent already knows, so the driver needs a live turn to find the id that loads.
- The cursor ACP entry point is a hidden subcommand. `cursor-agent --help` does not list `acp`. A `--help` probe finds `create-chat` and misses the entry point, so `detect.go` must not gate cursor on the help text.
- The codex spike snippet must hold stdin open. The heredoc closes stdin at once and the pending `thread/start` reply is lost. The `initialized` notification is not required for `thread/start`.
- OpenCode skills load from `<config dir>/skill/` and from `<config dir>/skills/`. Both names work, so the driver does not have to guess.
- `claude --permission-prompt-tool` is hidden from `--help`, and `--permission-mode default` is accepted although the help lists other choices. A flag-presence probe under-reports both. Do not treat the `--help` output as the full flag list.
- `Runtime.Detect` reports `auth=unknown` for pi and cursor, so the doctor cannot tell a logged-in pi from a logged-out one.
- grok 1.0.70 never sends `session/request_permission` on stdio, in `default` or ask mode. `Features.Permissions` is false, so the client cannot enforce ask or auto_edit for it; check a newer release before treating grok as approvable.
- grok `session/set_config_option` takes a plain string value. The object value from the plan is rejected with `-32602`.
- grok turn usage is in the prompt result `_meta` (top-level and `_meta.usage`), not in `result.usage`.
- omp and grok model lists come from the standard `configOptions`, not only `models`. grok also fills `initialize` `_meta.modelState`, so its model probe opens no session and needs no login.
- omp asks with `--approval-mode always-ask`; the spike did not prove `auto_edit`, so the driver keeps the policy on top of always-ask. `yolo` is the auto mode.
- `PI_CODING_AGENT_DIR` and `GROK_HOME` isolate the login as well as the sessions. A caller that wants "use existing login" must leave `StartRequest.Home` empty, and a detection probe must run the vendor's own authenticated command (`omp models --json`, `grok models`).
- omp is ACP only; the pi RPC driver stays for `pi`.

## Rule for a failed spike

A result that differs from the plan changes the injection table in the library, not the public API. Specifically:

- If a per-session skill strategy does not work, the harness moves to the next strategy in `skills.go`, and `Features.SkillsInject` reports the one in use.
- If MCP injection does not work for a harness, `MCPCapability` reports `MCPNone` or `MCPSession` for it, and `Detection.Features.MCPInject` tells the caller.
- If an instruction rendering does not work, the driver prepends the text to the first prompt instead.

The exported types, the `StartRequest` fields and the event vocabulary stay as they are.
