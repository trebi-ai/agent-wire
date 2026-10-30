# Screen fixtures

Each file is one `tmux capture-pane -p` frame of a vendor TUI. `screen_test.go` replays them through `Classify` and `Starter`.

## Layout

```
screens/
  <harness>/              # claude, codex, copilot, cursor, gemini, grok, opencode, opencode2, pi
    <scenario>/           # startup, idle, resume-picker, mid-session, ...
      01-boot.txt         # frames in pane order: <NN>-<state>.txt
      02-ready.txt
      info.txt            # CLI version, date, geometry, command
    login/<flow>/         # login screens for ParseLogin
```

## States (the file name is the expectation)

| State | Asserts |
|-------|---------|
| `boot` | not ready; no actions |
| `trust-folder` | not ready; a trust action (`1`, Enter, or an arrow key then Enter) |
| `bypass-permissions` | not ready; `{Text: "2"}` |
| `hooks-trust` | not ready |
| `setup` | not ready; the harness expectation in `setupActions` |
| `picker` | not ready; no actions |
| `ready` | ready; no actions |
| `busy` | state working (mid-session) |
| `idle` | state idle (mid-session) |

An unknown state name fails the test.

## Rules

- A frame keeps the bytes of the capture, with one exception: user names, paths, and product names are replaced with neutral strings of the same length. The same length keeps the wraps and the widths.
- When a CLI update breaks a test, capture the scenarios of that harness again. Do not loosen a rule without new frames.
- Synthetic cases stay in the test files. The frames are for realism.
