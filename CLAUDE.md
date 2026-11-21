# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`ccmon` is a single Go binary that shows every Claude Code session running on
the machine in a local web page (and as text with `-once`). It reads Claude
Code's own files and never writes to them. README.md is the user
documentation; PLAN.md holds the design notes, milestones and what is
deliberately left out.

## Commands

```sh
make build                 # builds ./ccmon (go build with version ldflags)
make test                  # go vet ./... && go test ./...
make dist                  # cross-builds all five platform binaries into dist/
go test ./internal/transcript -run TestName   # one test
./ccmon -once              # print the current state and exit; quickest check of a change
./ccmon -once -json        # the same snapshot the page receives
./ccmon -no-open -port N   # serve without opening a browser
```

`web/index.html` is embedded with `go:embed`, so a page change needs a rebuild.

## Architecture

Data flows one way, once a second:

```
discover + procinfo + transcript + vcsinfo + usage
        -> monitor.Collector.Collect()
        -> model.Build()  -> model.Snapshot (JSON)
        -> server (SSE /api/events)  -> web/index.html
        -> main.printText (-once)
```

- `internal/discover` reads `<claude-dir>/sessions/<pid>.json` and finds a
  transcript by globbing `projects/*/<sessionId>.jsonl` (the project folder
  encoding is deliberately not reproduced).
- `internal/procinfo` wraps gopsutil: liveness (pid exists and the process
  started no later than the session file's `startedAt`, which catches pid
  reuse), RSS/CPU summed over the process tree, host memory.
- `internal/transcript` is the incremental JSONL reader. A transcript is
  never read whole: the first read takes the last 4 MB, later reads take only
  appended bytes, and the older part is summed once in a background
  goroutine (`TotalsComplete` is false until then). Usage is counted once per
  message id, because Claude Code writes one record per content block and
  repeats the usage on each.
- `internal/vcsinfo` runs git, then Subversion, cached per directory for 30
  seconds and refreshed in the background. `Query` is the blocking form used
  by `-once`.
- `internal/usage` fetches plan usage from Anthropic every five minutes
  (server mode only, off with `-no-fetch`) using the token in
  `.credentials.json`, and falls back to the figures cached in
  `.claude.json`. This is the only outbound request. The token is never
  renewed or written; an expired one is skipped.
- `internal/monitor` is the only package that touches the disk on a timer.
  It owns the per-session trackers, the CPU sampler and the caches.
- `internal/model` is pure: it turns the gathered `Input`s into the
  `Snapshot`, applying the state, stuck, context and cost rules.
  `prices.go` is the model price and context-window table, matched by longest
  id prefix and overridable from the settings file.
- `internal/server` re-collects every second and pushes a snapshot over SSE
  only when it differs from the last one (compared with `Updated` zeroed).
- `web/index.html` is the whole front end: inline CSS and JS, no framework,
  no build step. All alerts (chime, voice, notifications, quiet hours, mute)
  live there, with settings in `localStorage`.

A new field shown on the page normally touches four places: the reader
package, `model.Session` or `model.Snapshot`, `web/index.html`, and
`printText` in `main.go`.

## Constraints

- **Read-only.** Nothing is written under the Claude config directory, and
  git and svn are run in ways that do not modify the working copy (git with
  `GIT_OPTIONAL_LOCKS=0`; svn never contacts the server). The page cannot act
  on sessions; replying or approving from it is deliberately out of scope.
- **Loopback only** (apart from the usage fetch above). The server binds `127.0.0.1`, accepts only GET/HEAD, and
  `guard` in `server.go` rejects any request whose `Host` or `Origin` is not
  its own. The CSP allows only inline script and style and same-origin
  connections, so the page cannot load anything external.
- **No OS-specific code.** Per-OS differences go to gopsutil, the Go standard
  library or the browser. No build tags, no cgo (`make dist` builds with
  `CGO_ENABLED=0`). Keep dependencies to gopsutil and `pkg/browser`.
- **Undocumented inputs.** Claude Code's session and transcript formats can
  change in any release. Every parsed field is optional; a missing one shows
  blank and must never cause a failure.
- **Synthetic fixtures only.** Never add a real transcript or session file to
  the repository; they contain private text. `ClaudeCodeTaskManager.png` is
  someone else's screen and stays out of git.
