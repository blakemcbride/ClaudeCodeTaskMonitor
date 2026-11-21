# Claude Code Task Monitor — Plan

A local dashboard showing every Claude Code session running on this machine:
what each is doing, whether it is waiting on you, and how close it is to its
context limit. Modeled on `ClaudeCodeTaskManager.png`. Runs on Linux, macOS
and Windows.

## Status

Milestones 1 to 7 are implemented and run on Linux (see section 6). Still
open: running on macOS and Windows, GitHub Actions, and the items under
"Not yet known".

## 1. Approach

One Go program. It reads Claude Code's own files, serves a single web page on
`127.0.0.1`, and opens it in your browser. Nothing is written to `~/.claude`,
and nothing leaves the machine.

- **Language: Go.** One binary per platform with no runtime to install, and
  all platforms build from Linux with one command.
- **UI: one HTML file** with inline CSS and JS, no framework, embedded in the
  binary with `go:embed`.
- **Server binds to `127.0.0.1` only.** Transcripts contain private text.

### Keeping OS special cases out

The goal is no OS-specific branches in this project's own code. The things
that differ per OS are handed to something that already hides the difference:

| Need | Handled by |
|---|---|
| Process alive, start time, memory, CPU, children, host RAM | `gopsutil` library |
| Desktop notifications, voice, sound | the browser (`Notification`, `speechSynthesis`, audio) |
| Window, text, layout, theme | the browser |
| Paths, home directory | Go standard library (`filepath`, `os.UserHomeDir`) |
| Running `git` and `svn` | Go standard library (`os/exec`) |
| Opening the browser | a small library, or leave the user to click the printed URL |

Checked on this machine with Go 1.26.10 and `gopsutil` v4.26.10: a probe
using those process calls cross-compiled for Linux, macOS (Intel and ARM) and
Windows with cgo off, each about 4 MB, and returned correct values for a live
session here. Only the Linux build has been run.

Dependencies are kept to `gopsutil` and at most one more; JSON, HTTP and
embedding are all standard library.

## 2. Where the data comes from

Checked against Claude Code 2.1.330 on this machine.

| Shown on the page | Source |
|---|---|
| Running sessions, pid, name, cwd, start time | `~/.claude/sessions/<pid>.json` |
| Status and how long | `status`, `waitingFor`, `statusUpdatedAt` in the same file |
| Latest reply text | last `assistant` record in the transcript |
| Last tool and its description | last `tool_use` block in the transcript |
| Model | `message.model` on the last `assistant` record |
| Context used | `usage` on the last `assistant` record (input + cache read + cache creation) |
| Transcript size | size of `~/.claude/projects/<encoded-cwd>/<sessionId>.jsonl` |
| Recent tool errors | `tool_result` blocks with `is_error` in the last N minutes |
| Last typed | timestamp of the last `user` record that was typed by a person |
| Session title | `ai-title` record |
| Branch, ahead count, upstream, uncommitted files | `git` or `svn` run in the session's cwd |
| Tokens and estimated cost, per session and per day | sum of `usage` records, times a price table |
| Permission mode | `permissionMode` on transcript records |
| Memory and CPU per session, host free RAM | `gopsutil`, by pid |

The config directory is `$CLAUDE_CONFIG_DIR` if set, otherwise `.claude` in
the user's home directory.

### Not yet known

- **Context limit per model.** The transcript gives tokens used, not the
  window size. Needs a small model-to-limit table, overridable in config.
- **Remote Control on/off.** Source not found yet.
- **Subagents and background sessions.** How they appear in `sessions/`,
  `jobs/` and `daemon/roster.json` needs a look.
- **Prices.** Cost needs a per-model price table, which goes stale. Ship
  defaults, make it overridable in config, and label the figure an estimate.
  Subscription users pay no per-token price, so cost display is optional.
- **Session files on macOS and Windows.** Assumed to have the same layout as
  on Linux; not yet seen.

### What will not be copied

The screenshot's "triad" (main / worker / research seats), "journal" and
"release" lines come from the friend's own multi-session setup, not from
Claude Code. This tool groups sessions by project directory instead, with the
group header showing path, branch and the worst status in the group. Seat
roles could be added later as an optional naming convention.

## 3. Waiting on you

This is the feature the tool exists for, so it is built first and gets the
most prominent place on the page.

### Detection

Claude Code reports this itself. Confirmed on 2.1.330: while a session had a
question open, its `sessions/<pid>.json` showed `status: waiting` and
`waitingFor: input needed`, and it returned to `busy` when the question was
answered. Other `waitingFor` values seen in the program's strings but not yet
observed: `dialog open` and `sandbox request`. A plain permission prompt has
not been observed yet either.

Three states matter, and the page keeps them apart:

| State | Meaning | Source |
|---|---|---|
| **Needs an answer** | Stopped mid-task on a permission prompt, question or dialog. Work is held up until you reply. | `status: waiting` plus `waitingFor` |
| **Your turn** | Finished its turn and is waiting for your next prompt. | `status: idle` |
| **Blocked** | Cannot continue for a reason outside the session (login, limit). | `status: blocked`, if confirmed |

The pending tool call or question shown beside a waiting session comes from
the transcript: the newest `tool_use` with no `tool_result`.

### Making it hard to miss

- **Fast detection.** The session files are tiny, so the server re-reads them
  every 1 to 2 seconds, separately from the slower transcript and git
  refresh, and pushes changes to the page over a server-sent event stream.
- **Banner and sort order.** Waiting sessions are listed in a banner at the
  top with how long each has waited, and their project cards sort first.
- **What it is asking.** Show the `waitingFor` reason and the pending tool
  call or question from the transcript, so you can judge urgency without
  switching windows.
- **Browser tab.** The count goes in the tab title and favicon, e.g.
  `(2) Claude sessions`, so it shows while you work in another tab.
- **Alerts.** Desktop notification, a chime and optional voice when a session
  starts waiting, with a repeat reminder after a configurable interval if it
  is still waiting. "Needs an answer" alerts by default; "your turn" alerts
  only after the session was busy for more than a minute or so, so quick
  back-and-forth does not nag.
- **Quiet hours** and per-project mute.

One limit of doing alerts in the browser: the page must be open in a tab, and
the browser asks once for notification permission. Sound and voice need one
click on the page first.

## 4. Design

```
main.go              flags, settings file, text output, start server
internal/
  discover/          read sessions/*.json, find transcripts
  procinfo/          liveness, memory, CPU (wraps gopsutil)
  transcript/        incremental JSONL reader
  vcsinfo/           git and Subversion state, cached
  model/             status rules, stuck detection, prices, snapshot
  monitor/           gathers everything on a timer
  server/            HTTP handlers, event stream
web/index.html       the page, embedded in the binary
```

- **Liveness.** A session file can outlive its process, and a pid can be
  reused. A session counts as live when its pid exists and that process began
  no later than the file's `startedAt`; a reused pid belongs to a process
  created afterwards. On this machine the gap was about
  half a second. This avoids interpreting `procStart`, whose format is
  OS-specific.
- **Large transcripts.** They reach hundreds of MB. Never read one whole: on
  first sight read only the tail (a few MB), then remember the byte offset and
  read only what was appended. Keep a small per-session summary in memory.
- **Format drift.** These files are undocumented and change between Claude
  Code versions. Every field is optional in the parser; a missing one shows as
  blank, never as a crash. Record the versions tested in the README.
- **Version control.** Git first, then Subversion. Cached per directory for
  30 seconds with a short timeout, refreshed in the background. Git runs with
  `GIT_OPTIONAL_LOCKS=0` so it does not rewrite the index. Subversion reads
  the working copy only and never contacts the server; unversioned files are
  not counted.
- **Transcript lookup.** Found by session id (`projects/*/<id>.jsonl`)
  rather than by reproducing how Claude Code names project folders.
- **Stuck detection.** Flag a session as "stuck" when it has been busy for
  longer than a threshold with nothing appended to its transcript, or when its
  last several tool calls are the same call failing the same way. Thresholds
  are configurable. A long-running tool (a build, a test run) also looks
  quiet, so the flag is a hint, not a verdict.
- **Process stats.** Resident memory and CPU by pid, plus host free RAM.
  Include child processes, since the memory is usually in what the session
  launched.
- **Usage totals.** Summing `usage` needs every assistant record, not just
  the tail. Do one full pass per transcript in a background goroutine, cache
  the running total with the byte offset, and add to it incrementally.
- **Local access only.** Besides binding to `127.0.0.1`, the server checks
  the `Host` header and rejects cross-origin requests, so a web page open in
  the same browser cannot read session text from it.
- **Privacy.** A `--private` flag (and per-project setting) hides reply text
  behind a "Show" link, as in the screenshot. Useful when screen sharing.
- **Text output.** `--once` prints the same summary to the terminal and
  exits, with `--json` for scripts. It shares all the code above and is how
  the early milestones are tested.

## 5. The page

Top to bottom, as in the screenshot:

1. Header: counts line, last-updated time, controls (voice alerts with a quiet
   hours range, desktop alerts, auto-refresh interval, Refresh).
2. "Waiting on you" banner listing sessions that need attention, with how long.
3. Count tiles: sessions running, blocked, waiting on you, working, idle.
4. One card per project: name, status badge, path, last typed, branch.
   One row per session: name, badge, time in state, latest reply (two lines,
   truncated), then a detail line with last tool, model, context used and
   percent, transcript size, and recent tool errors.

The detail line also carries permission mode, memory use, and tokens and
estimated cost. The header shows host free RAM and today's total cost.

Context over 85%, tool errors, stuck sessions, and sessions running with
permission prompts bypassed are highlighted. Crossing the context threshold
or becoming stuck also raises an alert, once per occurrence. Dark theme by
default, light theme following the system setting. Alert settings are kept in
the browser.

## 6. Milestones

1. **Discovery and waiting status.** `--once`: list live sessions from the
   command line, with status. Confirm the `waiting` status by experiment
   (section 3).
2. **Transcript summary.** Incremental reader; reply, tool, model, context,
   errors. Test against a 500 MB transcript for time and memory.
3. **Server and page.** Cards, tiles, event stream.
4. **Waiting on you.** Banner, sort order, what is being asked, tab title
   count, fast poll.
5. **Alerts.** Notification, chime and voice, repeat reminders, quiet hours,
   per-project mute. Includes context-threshold alerts.
6. **Git and Subversion info, grouping.**
7. **Health and usage.** Stuck detection, process stats and host RAM,
   permission mode, token and cost totals.
8. **macOS and Windows.** Run the cross-built binaries on real machines or
   VMs; check session file layout and the path encoding of project folders.
9. **Release.** README with screenshots, LICENSE, tests with made-up
   transcript fixtures, GitHub Actions testing on all three systems and
   publishing a binary for each.

### After the first release

In rough priority order:

1. **Jump to the session.** Click a row to focus its terminal window or tab.
   This is per-OS and per-terminal by nature, the one feature that cannot
   avoid special cases, so it stays optional and starts with tmux only.
2. **Recently ended sessions.** A collapsed list with the last reply and a
   copyable `claude --resume <id>` command.
3. **Subagents and background jobs.** Nested under the parent session with
   their own status.
4. **Search and filter.** Filter by project or status; search latest replies.
5. **Terminal mode.** A full `--tui` for use over SSH.
6. **Multiple machines.** One page aggregating several hosts over SSH
   tunnels, still read-only.

### Deliberately left out

- **Replying to or approving sessions from the page.** It turns a read-only
  tool into one that can act, which is a much larger security surface.
- **Phone push notifications.** Needs an outside service.

## 7. Before going public

- License: MIT (done).
- Do not commit `ClaudeCodeTaskManager.png`: it is someone else's screen and
  shows their paths and project names. It is in `.gitignore`, and the README
  uses a screenshot of this tool with made-up sessions.
- Add GitHub Actions: test on all three systems, publish binaries.
- Test fixtures must be synthetic. No real transcripts in the repo.
- State plainly in the README that it reads undocumented Claude Code files,
  is read-only, and is not an Anthropic product.
