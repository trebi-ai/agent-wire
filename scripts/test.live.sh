#!/usr/bin/env bash
#
# Run the live tier of the agentwire test suite.
#
# The live tier spawns the real vendor binaries with the machine's own login.
# It never mocks a vendor, and it never runs in CI.
#
# Usage:
#   scripts/test.live.sh                     # use AGENTWIRE_LIVE as it is set
#   scripts/test.live.sh claude codex        # only these harnesses
#   scripts/test.live.sh all                 # every harness
#   AGENTWIRE_LIVE=opencode scripts/test.live.sh
#
# Environment:
#   AGENTWIRE_LIVE              comma-separated harness list, or "all".
#                               Unset or empty fails: name the harnesses.
#   AGENTWIRE_LIVE_SKIP_NOAUTH  set to 1 to turn an auth failure into a skip.
#   AGENTWIRE_LIVE_TIMEOUT      go test timeout. Default 30m.
#
# Harness names: claude, codex, opencode, pi, copilot, cursor, gemini, fake.
#
# The tier fails when ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN or
# OPENAI_API_KEY is set, because a shared login is the supported path.
#
# Exit codes: 0 pass, 1 test failure, 2 no harness named.
set -euo pipefail

readonly here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly root="$(dirname "$here")"

# Positional names win over the environment. Join them with commas, because
# the live tier reads one variable.
if [[ "$#" -gt 0 ]]; then
	live=""
	for name in "$@"; do
		live="${live:+${live},}${name}"
	done
	AGENTWIRE_LIVE="${live}"
fi

# Pass the variables through to the test binary even when the caller only set
# them in the shell.
export AGENTWIRE_LIVE="${AGENTWIRE_LIVE:-}"
export AGENTWIRE_LIVE_SKIP_NOAUTH="${AGENTWIRE_LIVE_SKIP_NOAUTH:-}"
readonly timeout="${AGENTWIRE_LIVE_TIMEOUT:-30m}"

if [[ -z "${AGENTWIRE_LIVE}" ]]; then
	echo "scripts/test.live.sh: no harness named." >&2
	echo "usage: scripts/test.live.sh <harness> [harness ...]" >&2
	echo "   or: AGENTWIRE_LIVE=all scripts/test.live.sh" >&2
	echo "harnesses: claude codex opencode pi copilot cursor gemini fake" >&2
	exit 2
fi

cd "${root}"
exec go test -tags live -timeout "${timeout}" ./live/...
