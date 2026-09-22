# agentwire

Drive vendor coding-agent CLIs as child processes over their structured protocols, behind one Go event model.

agentwire spawns the vendor binary that is already on the machine and speaks the vendor's own wire protocol to it. It normalizes the conversation into one event stream: text, tool calls, permission requests, usage, results and exit. Five protocols are covered:

- Claude `stream-json` over stdin and stdout.
- Codex `app-server` JSON-RPC over stdin and stdout.
- OpenCode `serve` REST plus an SSE event stream, for 1.x and for the 2.x beta.
- ACP over stdin and stdout, for Copilot, Cursor and Gemini.
- pi RPC (NDJSON) over stdin and stdout.

The wire is Go-native. The official vendor SDKs are the reference implementation and the fixture oracle at build and test time only. They are never a runtime dependency. There is no Node, no sidecar and no vendored SDK.

The library knows vendors. It does not know consumers. Vendor flags, environment variables, config formats, protocol quirks and version floors live here. Homes such as `~/.trebi/harness`, lifecycle hooks, memory, secret environment, job semantics and UI text belong to the caller.

## No global state

Every cache, server pool, version record and crash ledger hangs off one `Runtime` value that the caller creates. Two runtimes in one process share nothing. No package-level mutable state exists.

## Harnesses

| Harness | Constant | Binary | Protocol |
|---|---|---|---|
| Claude Code | `agentwire.Claude` | `claude` | stream-json (NDJSON) over stdin and stdout |
| Codex | `agentwire.Codex` | `codex` | app-server JSON-RPC over stdin and stdout |
| OpenCode | `agentwire.OpenCode` | `opencode` | `serve` REST plus SSE |
| OpenCode 2 (beta) | `agentwire.OpenCode2` | `opencode2` | `/api` REST plus SSE |
| pi | `agentwire.Pi` | `pi` | pi RPC (NDJSON) over stdin and stdout |
| Copilot | `agentwire.Copilot` | `copilot` | ACP over stdin and stdout |
| Cursor | `agentwire.Cursor` | `cursor-agent` | ACP over stdin and stdout |
| Gemini | `agentwire.Gemini` | `gemini` | ACP over stdin and stdout |
| fake | `agentwire.Fake` | shell script | agentwire NDJSON, for tests |

`Runtime.Detect` reports whether a binary is installed, its version, whether the version floor is met, and a best-effort login state.

## Getting started

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/trebi-ai/agent-wire"
)

func main() {
	ctx := context.Background()

	rt := agentwire.New(agentwire.Options{
		ClientName:    "myapp",
		ClientVersion: "1.0.0",
	})
	defer rt.Close(ctx)

	sess, err := rt.Start(ctx, agentwire.StartRequest{
		Harness:      agentwire.Claude,
		WorkingDir:   "/path/to/project",
		Instructions: "Read AGENTS.md before you edit anything.",
		Permissions:  agentwire.PermissionPolicy{Mode: agentwire.PermissionAsk},
		MCPServers: []agentwire.MCPServer{{
			Name:    "myapp",
			Command: "myapp",
			Args:    []string{"mcp"},
		}},
	})
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	defer sess.Close(ctx)

	if err := sess.Prompt(ctx, agentwire.Prompt{Text: "Summarize this project."}); err != nil {
		log.Fatalf("prompt: %v", err)
	}

	for ev := range sess.Events() {
		switch ev.Type {
		case agentwire.EventAssistant:
			fmt.Print(ev.Text)
		case agentwire.EventPermission:
			// The policy answered everything it could. A human answers the rest.
			if err := sess.AnswerPermission(ctx, ev.Permission.ID, agentwire.Decision{Allow: true}); err != nil {
				log.Printf("answer: %v", err)
			}
		case agentwire.EventExit:
			fmt.Printf("\nsession ended: %s\n", ev.Error)
		}
	}
}
```

`Runtime.Resume` reopens a known session. `Runtime.Reconcile` kills children that a previous process left behind. `Runtime.Close` stops every session and shared server.

## Session rule

A consumer MUST drain `Session.Events()` until the channel closes, or call `Session.Close`. `EventExit` is always the last event, and it is delivered even after `Close`. A consumer that stops reading without closing leaks the framing goroutine.

## Extension points

`StartRequest` carries everything the caller wants for one session. The library builds the command.

| Field | Effect |
|---|---|
| `MCPServers` | Injects MCP servers for this session only. stdio (`Command`, `Args`, `Env`) or HTTP (`URL`, `Headers`). Injection adds to the user's own servers. |
| `Skills` | Makes extra skill directories visible: natively, through an overlay config dir, or as an index in the instructions. |
| `Instructions` | Adds per-session system instructions. The rendering is per harness. |
| `Permissions` | One policy (`ask`, `auto_edit`, `auto`) mapped onto each vendor's own mechanism. Unanswered requests arrive as `EventPermission`. |
| `Home` | Isolates the harness config home. Empty uses the user's own configuration and login. |
| `Raw` | Per-harness overrides the typed fields do not cover, for example `strict_mcp` (Claude) or `approvalPolicy` and `sandbox` (Codex). |

Other escape hatches: `ExtraArgs` appends consumer flags verbatim, `BaseCommand` skips the launch builder, `LogPath` writes every frame in and out as NDJSON, and `FS` routes ACP file reads and writes to the caller so it can see every edit.

## Vendor binaries and logins

The library spawns the vendor binary that is on the machine and uses the machine's own login. It never reads, copies or stores vendor tokens. It does not install or update vendor binaries, and it does not require vendor SDKs at run time.

## Development

```sh
go test ./...
scripts/test.live.sh claude          # real binaries and real logins
```

The live tier needs the build tag `live`, a named harness in `AGENTWIRE_LIVE`, and a shared login. See `docs/compat.md` for the vendor versions this library was verified against.
