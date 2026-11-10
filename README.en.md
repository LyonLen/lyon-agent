# lyon-agent

**the self-evolving AI coding agent that grades itself, sits exams, replaces
underperformers — and gets better the more you use it** — local-first, one Go
daemon `lyond` plus a Web workbench in your browser.

[![Go 1.25](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![CI](https://github.com/LyonLen/lyon-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/LyonLen/lyon-agent/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

One process runs the complete agent loop (streaming output, tool calls, context
compaction, concurrent subagent fan-out), exposed to a React Web workbench over
connectRPC/SSE; a stress-acceptance suite ships with it — thresholds that miss
FAIL the build. Around it sits an **evolution mechanism** — blind-graded
production runs onto a leaderboard, a closed-book exam every 10 rounds
(two failures and the seat is vacated), task classification with evidence-based
routing, failure post-mortems fed back into memory — every ledger persisted to
disk and restored verbatim after a restart, directly readable in the Web panels.

> 📘 **Docs site**: open [`docs/index.html`](docs/index.html) (product tour) and
> [`docs/manual.html`](docs/manual.html) (user manual: start → interface →
> evolution → configuration → FAQ) in a browser — pure local static pages, no
> network resources, double-click to read.
> **English**: [`README.en.md`](README.en.md) ·
> [`docs/index.en.html`](docs/index.en.html) · [`docs/manual.en.html`](docs/manual.en.html).
>
> The terminal TUI (`lyon chat`) was removed wholesale in 2026-11 — the
> interface is the Web, and this document is the new version.

## Features

- **Web workbench**: tiling layout — chat, plan, step audit, files, preview,
  terminal, mindmap, memory, settings, and evolution panels are all splittable
  panes; the layout is saved per session. Plus a Dashboard overview route
  (aggregates · routing & leaderboard · run replay · audit tail).
- **Session sidebar**: groups per workspace, click a header to **fold/unfold**
  that group, green **git** badge on repository workspaces, two-click confirmed
  delete, `running`/`queued` badges; new sessions pick a workspace (local
  directory or a direct git clone).
- **Streaming transcript + tool audit**: markdown rendering (one-click **copy**
  on code blocks), collapsible thinking blocks, tool output color-coded by
  category, a step panel auditing every tool call's name/duration/status live,
  running commands' output flowing in as it lands.
- **Approval cards**: shell/file writes pop a y/n/a card (`a` = always allow
  this tool for this session); plan-mode blocks arrive as mode_block cards,
  fail closed.
- **Files-changed card**: each run diffs the workspace into a "produced files ·
  N" card — click a row to jump straight into the preview pane (unified-diff
  view available).
- **Mindmap pane**: a live understanding map grown from the conversation, a
  peer of chat; **edits on the map flow back as events that advance the chat
  and trigger an automatic follow-up question** — a second input channel.
- **Concurrent multi-session**: queue while busy, inject in the gap (no lost
  questions), one-click interrupt, queue badges; sessions persist independently
  and resume after a restart.
- **Four execution modes**: `default` human approval · `plan` read-only
  exploration · `bypass` auto-approve · `mindmap` map interaction — pick from
  the bottom-left mode dock or cycle with `shift+tab`.
- **21 slash commands**: type `/` for a categorized candidate panel —
  `/rollback` checkpoints, `/undo` revert this run's file changes, `/model`
  model card, `/stats` usage, `/memory` memory, `/evolve` `/exam` `/route`
  `/flow` evolution panels … (full table in "Tour of the interface").
- **Three-tier persistent memory**: team / personal / project one-fact-per-file
  Markdown, local BM25 (CJK bigram) retrieval, hot tier injected with the first
  prompt, `/remember` distills this session in one keystroke; the web memory
  panel adds retrieval verification and tag filtering.
- **Hooks: audit & security gates**: full-lifecycle jsonl audit + aggregate
  stats + `policy.json` regex blocks + external blocking hooks
  (`LYON_AUDIT`/`LYON_HOOKS` to disable).
- **Evolution mechanism**: blind grading onto the leaderboard → closed-book
  exam every 10 rounds (two fails and the seat opens) → task-class routing —
  all run by `lyond` in the background on prompt_end (`LYON_EVOLVE` master
  gate), read-only in the Web evolution panel and Dashboard, every ledger
  persisted and restored on restart.
- **OpenAI-compatible gateway**: HTTP-layer protocol translation
  (`-openai-base`, any endpoint) — tool system, compaction, and subagents
  reused with zero changes.
- **Concurrent subagents**: `run_subagent(s)` read-only subagents, fanning out
  2–6 independent subtasks **in parallel**, each on the model with evidence for
  that task class.
- **MCP**: stdio and streamable HTTP transports, persistent config in
  `~/.lyon/mcp.json`, tools mapped to `mcp_<srv>_<tool>`.
- **Hard performance gates**: in-process mock Anthropic API + bench suite
  (engine p95 overhead ≤ 25 ms, steady-state cache hit ≥ 95%, zero SSE event
  loss) — miss the threshold, FAIL.

## Installation

Requires Go ≥ 1.25 (`go.mod` declares 1.25.0) and Node ≥ 18. Build from source:

```bash
git clone https://github.com/LyonLen/lyon-agent.git
cd lyon-agent

# 1) the daemon
go build -o bin/lyond ./cmd/lyond        # Windows produces bin\lyond.exe

# 2) the web frontend
cd web && npm install && cd ..
```

Configure credentials (never commit them):

```bash
export ANTHROPIC_AUTH_TOKEN=...          # model API key
export ANTHROPIC_MODEL=...               # optional, overrides the default model
export LYON_TOKEN=...                    # optional, bearer auth for Web/RPC
```

Prebuilt binaries for all three platforms (+ checksums) will ship with GitHub
Releases — for now, build from source.

## Quick start

```bash
# terminal 1: start the daemon (loopback 127.0.0.1:8080 by default)
./bin/lyond -workspace /path/to/your/repo -prewarm

# terminal 2: start the web app (Vite dev server, http://localhost:5183)
cd web && npm run dev
```

Open `http://localhost:5183` in a browser:

1. **+ new session** (top-left) → pick a workspace (local directory, or paste a
   git URL to clone directly);
2. type in the bottom input to chat — streaming answers, tool-call audit, and
   approval cards appear immediately;
3. type `/` for the command panel, `shift+tab` to cycle modes, and watch the
   context fuel gauge at the bottom right.

For production, `npm run build` emits the static assets
(`tsc --noEmit && vite build`); serve them from any static server pointed at
`lyond`.

> Common startup flags: `-addr` listen address · `-workspace` tool/file
> confinement root · `-token` auth (required on non-loopback binds, or explicit
> `-insecure`) · `-approve` strict approval (every write asks; default is
> path-aware — only shell commands that leave the workspace ask) · `-model`
> `-effort` · `-max-sessions` `-session-ttl` admission & eviction · `-max-turns`
> global in-flight turn cap · `-prewarm` warm the prompt cache · `-openai`
> `-openai-base` OpenAI gateway. Full table in the manual's "Configuration".

## Tour of the interface

### Session sidebar (left)

- **Grouped by workspace**: the header shows the workspace name — click it to
  **fold/unfold** every session in that group; repository groups carry a green
  **git** badge (a single `os.Stat` — distinguishes code repos from plain
  directories).
- **Session rows**: title · mode · `running`/`queued N` badges · last-active
  time; the row's `×` deletes **on two clicks** (first click arms "confirm?",
  it self-heals after 3s).
- **+ new session**: the workspace chooser — recent-directory history, local
  browse, git URL clone (`https:// / http:// / git@ / ssh:// / file://` or a
  local path).
- **saved**: historical session snapshots (archives written by the retired TUI
  era); click one to restore it back into a conversation.

### Chat (center)

- Streaming transcript: markdown rendering, a **copy** button on hover for code
  blocks, collapsible thinking blocks, tool rows color-coded by category
  (shell green · subagent blue · writes yellow · web/MCP magenta · reads cyan).
- **Approval cards**: `y` allow · `n` deny · `a` always allow this tool for
  this session (arrows/Tab walk the cards).
- **Files-changed card** (`files_changed`): every file this run touched,
  row by row — click to open the preview, change rows switch to a unified diff.
- Input box: `/` slash panel (categories: session/model/panel/misc), `@` file
  completion, sending while busy queues the prompt (`queued N`).
- **Status bar**: mode dock at the bottom-left (open to pick
  default/plan/bypass/mindmap), **HP-bar context gauge** at the bottom-right
  (`ctx% · used/window`, green → yellow → red bands) plus session spend.

### Pane workspace (tiling)

Panes splittable around the chat (opened via `/` commands or UI affordances):

| Pane | Opens via | Contents |
|---|---|---|
| plan | plan-card corner chip | todo plan, click through to the chat |
| steps | `/steps` | tool-execution audit: status ⟳/✓/✗, durations, readable args, live output tail |
| files | navigation | directory tree → click into preview |
| preview | file card / links | text/code highlighting, markdown, xlsx/pptx previews, diff view |
| terminal | UI entry | in-session xterm terminal |
| mindmap | `/mindmap` or mindmap mode | live understanding map; node edits flow back and advance the chat |
| memory | `/memory` | three-tier memory browsing, BM25 retrieval verification (score / matched terms / passage), tag filters |
| settings | `/settings` | engine model & subagent model (switchable), model knowledge table (gateway `/models` × built-in knowledge base), environment variable inventory |
| evolve | `/evolve` `/exam` `/route` `/flow` `/agents` `/market` | read-only panels: candidate lineage · seated models · exam clock & banks · routing table & leaderboard · frozen procedures · task-hour market |

Layouts are saved per session: switching sessions restores that session's own
pane tree and open tabs.

### Dashboard (overview route)

A separate view behind the sidebar nav, four loads firing concurrently
(overview polls every 10s): **overview** (aggregates · routing recommendations ·
on-duty leaderboard) · **run list → replay timeline** · **audit tail** (hooks
jsonl) · **saved snapshots**.

### Full slash-command table

Type `/` for the categorized candidate panel, ↑/↓ to pick, enter to run:

| Category | Command | Effect |
|---|---|---|
| session | `/new` | new session |
| | `/resume [N]` | pop the archive picker and restore (`N` selects a slot) |
| | `/rollback [save [text] \| N]` | list checkpoints / mark one / restore the Nth |
| | `/undo` | revert the last run's file changes (git side-ref) |
| | `/clear` | clear the transcript view (history kept) |
| model | `/mode` | cycle default → plan → bypass |
| | `/model` | effective-model card for this session |
| | `/think` | expand/collapse all thinking blocks |
| panel | `/steps [open\|close\|width N]` | step-audit pane |
| | `/mindmap` (`/mm`) | mindmap pane |
| | `/memory` | memory pane |
| | `/settings` | settings pane |
| | `/evolve` `/exam` `/contest` `/route` `/agents` `/flow` `/market` | evolution family → the matching evolve-panel section |
| misc | `/remember` | distill this session's durable facts into memory now |
| | `/stats` | usage totals: turns, tokens, cache hits |
| | `/help` | commands and keys at a glance |

## Architecture

Layered sketch (calls flow top-down; evolution/memory/governance touch only the
ledgers and the event stream, never the engine loop):

```text
┌─ entry   web/ (React + Vite, connect-es) ── HTTP(S) ──► lyond (connectRPC + SSE)
├─ service pkg/server  SessionService/FileService/ApprovalService/… · Event Hub ·
│             session store & draft persistence · approval bridge · x-lyon-workspace header
├─ engine  pkg/agent   driver loop · event stream · compaction · snapshot/rollback ·
│             subagents & concurrent fan-out
├─ tools   pkg/tools   shell / files / search / image / web / memory / todos
│             pkg/mcp    MCP stdio & streamable HTTP
├─ evolve  pkg/eval · pkg/evolve · pkg/routing · pkg/models · pkg/flow · pkg/market
│             scoring & leaderboard · closed-book exams · task routing · model profiles ·
│             workflow state machine · task-hour ledger
├─ memory  pkg/memory  three-tier persistent memory · BM25 retrieval · distill · cloud sync
└─ govern  pkg/hooks   audit jsonl · aggregate stats · policy/external-hook security gates
(transport) pkg/openaix (OpenAI-compatible gateway protocol translation)
```

Directory map:

```text
cmd/lyond           daemon: connectRPC + legacy SSE, /metrics, /healthz,
                      approval bridge, prompt_end observer wiring (mindmap
                      distill + evolution loop)
web/                React + TypeScript frontend (Vite · @connectrpc/connect-es)
  src/App.tsx        session lifecycle, slash-command routing, tiling panes,
                      mode dock, context gauge
  src/components/    ChatView / SessionList / StepsPane / MindMapCard /
                      FilePanel / ScmView / TerminalPane / PreviewPane /
                      PlanPane / MemoryPanel / SettingsPane / DashboardPanel /
                      EvolvePanel / NewSessionDlg / CodeEditor …
pkg/agent           engine core
  engine.go           Engine: tuned HTTP connection pool, global concurrent
                      turn semaphore, Warmup
  session.go          Session: one driver goroutine per session, event stream,
                      compaction triggers, history assembly, gap-injection queueing
  compact.go          ★ client-side compaction: render history → standalone
                      summary request → summary replaces history
  snapshot.go         session snapshots: Export/Restore + ~/.lyon/sessions archives
  subagent.go         run_subagent: read-only subagent on a cheaper model
  dispatch.go         ★ run_subagents: 2–6 independent subtasks fanned out in parallel
  events.go           event protocol (connectRPC / SSE / transcript share one stream)
pkg/tools           tool system (registry / shell / files / search / image /
                      paste / web / memory / todos / stable / metrics)
pkg/server          HTTP services & RPC: hub ring replay, session store,
                      approvals, file/memory/dashboard/evolution services,
                      x-lyon-workspace
pkg/hooks           audit (~/.lyon/audit/*.jsonl), aggregate stats, policy/hooks gates
pkg/memory          three-tier memory: BM25 retrieval, hot-tier injection, distill, sync
pkg/eval            evolution machinery: four-dimension scoring, leaderboard,
                      closed-book exams, coverage gate, failure post-mortems
pkg/evolve          evolution loop (wired to lyond's prompt_end): book rounds ·
                      blind grade · sit exams · post-mortem
pkg/routing         task classification (zh/en) and recommended-routing table
pkg/models          model parameter knowledge base + capability profiles (32 models)
pkg/flow            workflow state machine: mechanical execution, auto-freeze on streaks
pkg/market          task-hour ledger
pkg/openaix         OpenAI-compatible gateway: HTTP-layer protocol translation
internal/mockapi    fake API for acceptance: real SSE frame order + prefix-hash
                      prompt-cache simulator
bench               performance acceptance suite (enforced thresholds)
```

Defaults: model `claude-opus-5-5` (override with `ANTHROPIC_MODEL`), effort
medium, `MaxIterations=0` (ReAct iterations per prompt are **unbounded by
default** — the model runs until it stops; cap with `-max-iter N` or
`LYON_MAX_ITER=N`), compaction threshold 0.7×200k, subagents default to the
cheap tier.

## Usage details

### Web interactions

- **Approvals**: tools needing confirmation pop a card in the stream —
  `y`/`n`/`a` or click. Timeout/close is always deny (fail closed); denials are
  not persisted. `lyond -approve` switches to strict mode (every write asks);
  the default is path-aware — only shell commands that leave the workspace ask.
- **Queue & interrupt**: sending while a prompt runs queues it (row shows
  `queued N`); the queue head is injected in the **gap** before the next tool
  execution, merged with that round's tool results into a single user message —
  no waiting for the whole run, no double delivery. The interrupt control stops
  any time (history self-repairs).
- **Image input**: reference workspace images in a prompt with `@path`
  (png/jpeg/gif/webp; spaces: `@"my shot.png"`) — they ride along as an image
  block. The model side has `view_image` (`read_file` reads UTF-8 text only).
- **Checkpoint rollback**: a full history copy is captured before every prompt
  (when history is non-empty) and again before compaction replaces it —
  compaction is always undoable. `/rollback save [text]` marks one manually;
  `/rollback N` restores — turn stats, spend, and title are kept, only the
  conversation rewinds; after a restore the transcript re-anchors and the status
  bar says so. Compaction checkpoints store the complete pre-compact original,
  so restoring brings the original back.

### Session persistence

- **drafts (primary path)**: each session is a draft
  (`~/.lyon/sessions/<hash>/drafts/<id>.json`) written on every state change;
  on restart `lyond` re-indexes them with `LoadDrafts` — the sidebar lists them
  as-is and clicking continues the conversation (`Server.acquire` reads it back
  instead of 404ing). Deleting a session is the only path that removes draft
  files.
- **saved snapshots (legacy path)**: snapshot slots and archives under
  `~/.lyon/sessions/` (latest slot + 20 archives per workspace), written by the
  now-removed TUI; the sidebar's saved section still lists and restores them
  (`RestoreSession` is read-only).
- `LYON_HOME` rewrites the whole storage root — containers/CI/tests never touch
  the real home.

### Compaction (client-side, works on any endpoint)

When a turn's usage crosses threshold × context window (default 0.7×200k, tune
with `-compact-threshold`, negative disables) the history is rendered into
role-labeled text, sent through one standalone summary request, and replaced
wholesale by a single summary user message — no dependency on gateway-native
compaction; any endpoint can shrink history. A `tool_use` turn delays one beat
so tool results land first, and a progress guard never retries if usage didn't
drop (no failure loops). In the transcript this is the `compaction` event; in
`/stats`, the `stop=compaction` turns.

### Server endpoints

```bash
./bin/lyond -addr 127.0.0.1:8080 -workspace . -prewarm   # loopback by default
bash examples/sse-client.sh http://127.0.0.1:8080        # acceptance client
```

| Route | Purpose |
|---|---|
| `POST /v1/sessions` | create session; with `message`, starts a run and returns SSE |
| `POST /v1/sessions/{id}/messages` | follow-up prompt, SSE event stream |
| `GET /v1/sessions/{id}/stats` | per-turn usage (incl. cache_read/cache_creation) |
| `DELETE /v1/sessions/{id}` | destroy immediately |
| `GET /metrics` | throughput, cache_share, tool p50/p95, last_error |
| `GET /healthz` | liveness probe |

The web client speaks connectRPC (`/lyon.v1.SessionService` et al., connect-es
transport), sharing the same Event Hub and session store as the legacy SSE
routes above.

Server-side defenses: `-max-sessions` admission cap (429 beyond),
`-session-ttl` idle eviction, `-max-turns` global in-flight API turn cap, SSE
write timeout 15s + `X-Accel-Buffering: no`.

**Auth**: binds loopback `127.0.0.1:8080` by default. With `-token` (or env
`LYON_TOKEN`) every route except `/healthz` requires
`Authorization: Bearer <token>` (also accepts `X-Lyon-Token`, constant-time
compare, 401). Binding a non-loopback address **without** a token refuses to
start unless `-insecure` is explicit — these routes run `run_shell` and write
files; anonymous exposure is remote execution.

**Per-session workspace**: the web injects `x-lyon-workspace` on every RPC
(following the selected session's workspace), so files/memory/tool confinement
all follow the header; the server's `wsFor` validates it is an existing
absolute directory and falls back to the engine default otherwise.

### Embedding as a library

```go
reg := tools.NewRegistry()
tools.RegisterFunc(reg, "get_quote", "description", true /*parallel-safe*/, fn)
eng := agent.NewEngine(agent.DefaultConfig(), reg)
sess := eng.NewSession(nil)
defer sess.Close()
sess.Submit("your prompt")
for ev := range sess.Events() { /* text_delta / tool_start / prompt_end ... */ }
```

Full examples: `examples/custom-tools` (business tools), `examples/mcp-hello`
(MCP client).

## Performance acceptance (bench)

`internal/mockapi` is an in-process fake Anthropic API: it emits real SSE frame
order, supports configurable latency, and runs a **prefix-hash cache
simulator** — elements concatenated in semantic order (tools → system →
messages) and hashed — any byte drift necessarily MISSes. The `bench` suite fails on
missed thresholds:

```bash
go test ./bench -run 'TestLoad|TestCache|TestSSE' -v
# optional real-endpoint report (no assertions): LYON_LIVE_TEST=1 go test ./bench -run TestLiveReport -v
```

Measured on this machine (20-core Windows):

| Acceptance item | Threshold | Measured |
|---|---|---|
| Engine p95 overhead (clean window, 16 sessions × 8 prompts, engine p95 − bare SDK p95) | ≤ 25ms | **5.7ms** (20.4ms vs 14.7ms) |
| Steady-state prompt-cache hit rate (token share from round 2) | ≥ 95% | **97.78%** (per-round read grows 1641→2057) |
| SSE event loss at 50 concurrent clients | 0 | **0** (sequence identical to baseline) |
| SSE first-event TTFD p95 | ≤ 1s | **70ms** |
| Prompt errors across 100 / 500 / 1000 session matrices | 0 | **0** (throughput 1519 / 483 / 243 prompts/s) |
| Leaked Session driver goroutines after closing 1000 sessions | 0 | **0** (85MB heap) |

Notes: at 1000 concurrent the machine saturates (the bare-SDK baseline sags to
the same level, so the delta is scheduling noise) — hence the overhead gate runs
only in the clean window; matrix subtests hard-fail on zero errors / all
complete / zero leaks, while throughput and percentiles are recorded without
gating.

## Key finding: prompt cache vs byte drift in anthropic-sdk-go

**Symptom**: `cache_read` stays 0 round after round — the cache never hits.

**Root cause**: the SDK's `param.MarshalWithExtras` iterates schema
`ExtraFields` with `for k, v := range extras` (Go map iteration order is
random), and the jsonschema-reflected tool schemas happen to carry exactly two
ExtraFields (`{$schema, additionalProperties}`). The tools array sits at the
front of the request body, so its bytes change every request → the cache prefix
chain never matches. Two semantically identical requests MISS on key order
alone (mockapi's `TestCacheMissOnSchemaKeyOrderDrift` pins this behavior).

**Fix** (`pkg/tools/stable.go`): each tool schema is normalized once — marshal →
`UseNumber` decode → re-ordered marshal — returned via
`param.Override[...](json.RawMessage)`. The Override branch never walks a map,
so bytes are deterministic from then on. Hooked into `Adapt`, the single
chokepoint every session tool, subagent, and Warmup passes through, idempotently.

After the fix: byte-identical across processes, 94.2% cache share over 8 rounds,
93.6% on subagent turns, `TestAdaptedSchemaMarshalDeterministic` byte-identical
over 100 consecutive marshals.

**Known gateway limits** (not this library's problem):

- This gateway reports `cache_read` but never `cache_creation` (cache writes are
  unobservable);
- the gateway returns no native compaction block — hence client-side compaction
  (see "Compaction" above), which works on any endpoint.

## Robustness design

**Shell probing** (`pkg/tools/shell.go`): on Windows `exec.LookPath` hits the
WSL placeholder `C:\Windows\System32\bash.exe` — resolvable but unrunnable.
So every candidate shell actually executes `echo lyon-shell-probe-ok` (5s
timeout, stdout marker + exit code checked); broken binaries are eliminated,
and when Git Bash is not on PATH, common install paths like
`%ProgramFiles%\Git\bin\bash.exe` are probed too. Only when all fail:
`no working shell found (tried: ...)`.

**Interruptible prompts** (`Session.Interrupt`): interrupting cancels the
context derived from that prompt — the API stream and running tools stop
together, the session survives, and prompt_end closes with `interrupted` (no
red error event). If history stops on an assistant `tool_use`,
`repairInterrupted-History` queues a synthetic `tool_result` for every dangling
call, merged with the next user message into one user message — role alternation
stays valid and the API never rejects. If a clean close still needs
compensation, the iteration budget was hit
(`prompt_end.stop_reason="max_iterations"`, only when `-max-iter`/
`LYON_MAX_ITER` is set — unlimited by default): affected steps are honestly
marked `over budget`. The busy lock releases **before** prompt_end is emitted,
so an event-buffer consumer replaying immediately never hits
`ErrPromptInProgress`.

## Stats, audit & security control (hooks)

`pkg/hooks` hangs hooks off every session lifecycle and implements three
consumers — `prompt_start/end`, `turn_end`, `tool_use/exec_start/end`,
`approve_request/result`, `compaction`, `subagent_end`, `error`,
`security_block` and more all pass through:

- **Audit**: `~/.lyon/audit/<date>.jsonl`, one JSON Record per line (time,
  event, tool, truncated input, duration, approval result, token usage, block
  reason). Written on a dedicated goroutine — zero I/O on the hot path;
  30-day retention. `LYON_AUDIT=off` disables. The Dashboard's "audit tail"
  reads it directly.
- **Stats**: `~/.lyon/stats.json` aggregates — prompt/iteration/compaction/
  subagent counts, approval yes/no, security blocks, cumulative tokens, per-tool
  calls/failures/latency. Web `/stats` shows session and global usage. Flushed
  on prompt_end (≤2s latency), `Flush` on `Session.Close`.
- **Security control** (two layers, before approval — a blocked tool never
  bothers a human):
  1. `~/.lyon/policy.json` local policy:
     ```json
     {
       "allow_tools": ["read", "grep", "glob"],   // non-empty ⇒ allowlist mode
       "deny_tools": ["bash"],
       "deny_patterns": ["(?i)rm\\s+-rf\\s+/", "curl[^|]*\\|\\s*(ba)?sh"]
     }
     ```
     Patterns match the raw JSON of the tool input; blocks return
     `blocked by security policy` to the model as the tool result and log
     `security_block` (`blocked_by` names the rule).
  2. `~/.lyon/hooks.json` external hooks:
     ```json
     [
       {"event": "tool_end",   "command": "node C:/hooks/log.js", "timeout_ms": 5000},
       {"event": "tool_start", "argv": ["gate.exe"], "blocking": true}
     ]
     ```
     The Record is JSON on the hook's stdin; `"blocking": true` (tool_start
     only) vetoes the execution on exit code 2 or stdout
     `{"deny":true,"reason":"..."}`. Non-blocking hooks run async; failures
     count into `hook_failures` and log `hook_error`. `LYON_HOOKS=off` disables.

External hooks suit custom telemetry, audit export, DLP/sandbox gateways;
subagent tools pass the same `PreTool` gate.

## Memory module (tiered hot/cold + self-evolving + cloud sync)

Three tiers of persistent memory, one-fact-per-file Markdown you can hand-edit:

| Tier | Location | Scope |
|---|---|---|
| team | `$LYON_HOME/memory/team/` | across projects & agents (org conventions) |
| personal | `$LYON_HOME/memory/personal/` | this user's projects (personal prefs) |
| project | `<workspace>/.lyon/memory/` | the current codebase (decisions, conventions, pitfalls) |

- **Retrieval**: local BM25 inverted index (CJK bigram tokenization, no
  tokenizer needed), field weights name 3.0 / tags 2.5 / description 2.0 /
  body 1.0; ties favor the nearer tier (project > personal > team). Pure
  in-memory index, millisecond-scale. The **memory panel's** query box replays
  the same ranking in the browser — see the score, matched terms, and matching
  passage, i.e. whether `read_memory` can find the fact you expect.
- **Hot tier**: `hot: true` facts ride the system prompt from the session's
  first prompt (default 4KB budget). Facts whose retrieval/read hits reach
  ≥2 are auto-promoted on the next distill.
- **Model tools**: `save_memory` (upsert by title, `scope`/`hot` args),
  `read_memory` (`query`=BM25 search / `title`=read verbatim / empty=tight
  list), `forget_memory` (delete by name).
- **Self-evolution (distill)**: `/remember` triggers it now — the model is
  steered to run the distillation pass immediately, saving durable facts from
  this conversation (decisions and reasons, preferences, conventions,
  environment gotchas; at most 6, idempotent — unchanged content is not
  rewritten); every write logs a `memory_save` audit event.
  `LYON_DISTILL=off` disables.
- **Cloud sync**: with `LYON_SYNC_URL` (optional `LYON_SYNC_TOKEN`),
  `POST /memory/push {"facts":[…]}` and `POST /memory/pull
  {"scopes":["team","personal"]}` — newer `updated` wins; only team+personal
  sync, the project tier never leaves the machine.
- **Legacy migration**: `<workspace>/.lyon/memory.json` is split into project
  facts on first open; the original is renamed `memory.json.imported`.

Fact file format (frontmatter is hand-editable; files without frontmatter use
the filename as the name and the whole body as content):

```markdown
---
name: toast-notification-preference
description: notify with bottom-right toasts, not old-school dialogs
scope: personal
hot: true
created: 2026-10-10T18:00:00Z
updated: 2026-10-10T18:30:00Z
hits: 3
---
body...
```

## Automatic session titles (intent detection)

- After every prompt, a cheap submodel judges intent: same topic answers
  `{"changed":false}` and keeps the title (costs nothing), a topic change
  regenerates it (3–8 words, following the user's language).
- The input is only the first prompt + current title + latest prompt (a few
  hundred tokens, never the whole history), reply capped at 64 tokens;
  `LYON_TITLE=off` disables.
- Display & persistence: sidebar session rows show the title; snapshots save
  it and restores continue it. Detection runs async after prompt_end — the UI
  never waits — and a newer prompt_end arriving mid-check re-runs it.

## Step audit panel (StepsPane)

- **Task tree**: the panel is organized by **parent/child tasks** — one bold
  group header per prompt, actions indented one level; `run_subagent` is a task
  node with its internals (event `Sub`) indented under it, closed by
  `subagent_end` into `task · N API calls`.
- **Readable args**: `run_shell` shows the command, `web_search` the query,
  file tools the path, `run_subagent` the task — multi-line collapsed to one
  line, never raw JSON.
- **Tool colors**: shell green · subagent blue · writes yellow · web/MCP
  magenta · reads cyan.
- **Live action audit**: every step carries `⟳` running (live elapsed) ·
  `✓` done · `✗` failed/interrupted; data comes straight from the engine event
  stream, exec_start/exec_end paired by execution id (parallel same-name tools
  FIFO), and steps still running at prompt_end are honestly marked
  interrupted/aborted.
- **Live output**: expanding a step shows the running command's stdout/stderr
  flowing in (first write flushes, then 40ms / 16KB coalescing); list rows carry
  the newest output tail.
- **Traceable**: step numbers increment within the session (lining up with the
  hooks audit log); tool error text shows right on the row.

## Rollback & undo

- **Checkpoints**: a full history copy is captured before every prompt (when
  history is non-empty) and again before compaction replaces it — compaction is
  always undoable. `/rollback save [text]` marks one manually (e.g. before
  touching dangerous files).
- **Restore**: `/rollback` lists checkpoints (newest first, with message
  count/time/labels), `/rollback N` restores the Nth. Restore = the next prompt
  rebuilds history from the checkpoint; **turn stats, spend, and title are
  kept**.
- **Post-compaction rollback**: a compaction checkpoint stores the complete
  pre-compact original — restoring makes the next request carry the original
  conversation again (`TestCompactRollback` asserts the summary vanishes);
  compaction checkpoints outlive prompt checkpoints in the undo stack (capped
  at 20, prompt checkpoints evicted first).
- **Safe semantics**: restore only runs when idle (a busy session auto-
  interrupts and restores after prompt_end); checkpoints after the target are
  invalidated; every restore logs a `rollback` audit record; the transcript
  keeps the discarded lines plus a `rolled back` marker — audit trail intact.
- **`/undo`**: revert the last run's file changes (git side-ref) — one command
  back to before the run.

## Built-in model parameter knowledge base

- **Offline knowledge base** (`pkg/models`): context windows, output caps,
  officially recommended parameters (temperature/top_p/reasoning-model
  constraints), knowledge cutoffs, and **reference prices** (USD per million
  tokens) for the common model families — Anthropic Claude, OpenAI GPT/o series,
  Google Gemini, DeepSeek, Qwen, Llama, Mistral, **Xiaomi MiMo** — no network
  lookups, compiled into the binary. Cache read/write prices are recorded per
  vendor; entries without them fall back to Anthropic's 0.1×/1.25×.
- **Lookup rules**: exact match first (case-insensitive), then longest wildcard
  prefix; **unknown ids return nil — never guess a window** — callers fall back
  to the configured value.
- **Web surfaces**: `/model` shows the session's effective-model card; the
  **settings pane** lists what the gateway's `/models` actually serves, enriched
  by the knowledge base (id · context · price · family · vision) — a gateway
  model the table never heard of still gets a row; picking a row switches the
  engine's main and subagent models.
- **Prices are reference prices**: vendors reprice anytime, official sites win;
  unknown prices show "unknown" instead of invented numbers. Uncatalogued models
  run on config — no guessed parameters.
- **Feeds the context gauge**: knowledge-base context windows and unit prices
  are the data source for the gauge and spend display.

## Context share & spend (resident fuel gauge)

- **HP bar, bottom right**: pinned at the right end of the status bar as
  `ctx% · used/window` plus session spend — share of the window, current
  occupancy, and money burned. Color tracks the level: green low, yellow near
  the compaction threshold, red critical.
- **Occupancy estimate**: last turn's request side (input+cache_read+
  cache_write) plus the reply — the reply is already in history and is the next
  request's increment; compaction turns are special-cased (only the summary
  remains). Window size comes from the knowledge base, falling back to config.
- **Spend**: per-turn input/output at face value, cache read ×0.1, cache write
  ×1.25 (Anthropic's published multipliers); **subagent usage is accounted per
  model** (usually the cheaper tier). Unit prices come from the knowledge base
  (reference prices); **uncatalogued models never get invented prices** — show
  "unknown", or declare the gateway tariff via `LYON_PRICE_IN` / `LYON_PRICE_OUT`
  (USD per million tokens; both must be set).
- The gauge renders the moment a session opens (fed by the Session snapshot and
  `turn_end` events) and never keeps a stale value across session switches.

## Execution modes (default / plan / bypass / mindmap)

- **Four session-level modes** (enforced by the engine, effective mid-run):
  - `default` — **human approval**: tools needing confirmation pop y/n/a cards
    (path-aware by default; `lyond -approve` makes it strict);
  - `plan` — **read-only exploration**: file writes, shell, memory writes, MCP
    tools, and unregistered tools are all `blocked by plan mode` (fail closed:
    the allowlist = registry ParallelSafe read-only annotations, `mcp_*`
    excluded for unknown side effects, `run_subagent` allowed because
    subagents get read-only tools and pass the same gate). Blocks arrive as
    mode_block cards (`security_block(blocked_by=mode:plan)` audited), never
    bothering approval or external hooks; the system prompt also injects the
    `[MODE: PLAN]` contract;
  - `bypass` — **auto-approve**: tools needing approval are granted yes
    directly, with approve_request/result audit rows still written (auto-grant
    must leave a trail). **policy.json and blocking hooks still run** — bypass
    only skips the human, it never weakens the security floor;
  - `mindmap` — **map interaction**: entering opens the mindmap pane; map
    edits flow back to advance the conversation and trigger an automatic
    follow-up.
- **Switching**: the bottom-left mode dock · `shift+tab` cycle
  (default → plan → bypass) · `/mode`. Every switch logs a `mode` audit record
  (`default → plan`).
- **Always visible**: the status-bar mode badge is always there — if it
  changed, you see it; modes never change silently when you switch sessions.

## OpenAI-compatible gateway (protocol translation layer)

- **Single-point translation**: `pkg/openaix` is an `http.RoundTripper` that
  translates Anthropic Messages ⇄ OpenAI `chat/completions` at the HTTP layer.
  Tool system, history building, tool runner, compaction, subagents, title
  generation … every call site shares one Engine client — **zero business-code
  changes**, zero cost when disabled.
- **Enable** (any one):
  - `lyond -openai` — default gateway `https://token-plan-cn.xiaomimimo.com/v1`
  - `lyond -openai-base https://host/v1` — any OpenAI-compatible endpoint
    (implies `-openai`)
  - env `LYON_OPENAI_BASE` (flag wins; trailing `/` stripped)
- **Same key**: requests still carry `x-api-key`; the translation layer swaps
  it to `Authorization: Bearer …` for the gateway (passed through verbatim if
  the gateway itself uses Bearer); **auth headers never leak to the gateway**.
- **Gateway-friendly defaults**: `WebToolMode` forced to `client` (gateways
  cannot run Anthropic server-side web tools); without `LYON_SUBAGENT_MODEL`
  subagents use the main model (single-model gateways reject a second id).
- **Protocol notes**:
  - Request: system → first system message; `tool_result` lifted into a
    standalone `role:"tool"` message placed before user text (OpenAI requires
    tool messages to follow assistant tool_calls); adjacent same-role messages
    merged; thinking/unknown blocks dropped; `tool_choice`
    none/any/tool/auto→none/required/function/auto; streaming requests carry
    `stream_options:{include_usage:true}`.
  - Response: gateway SSE frames reassembled into Anthropic
    `message_start/content_block_delta/message_stop` events; usage zeroed at
    message_start with all four fields on message_delta; JSON responses feed
    the streaming SDK too.
  - Errors: non-2xx keeps the original HTTP status (retry policy unchanged),
    body wrapped as Anthropic `{"type":"error",…}` (401→authentication_error,
    429→rate_limit_error, 529→overloaded_error, ≥500→api_error),
    `retry-after`/`x-request-id` passed through; a 200 with an HTML/error-object
    body maps to 502/500 instead of a silent empty message.
- **Acceptance**: translation unit tests, RoundTrips (streaming/non-streaming/
  errors/passthrough/auth), a real anthropic-sdk-go client through the layer,
  engine-level real tool-runner two-turn e2e
  (`pkg/agent/openai_gateway_test.go`).

## Agent evolution mechanism (scoring · exams · routing · workflow freezing)

Design doc `docs/design/agent-evolution.md`. Four code layers: `pkg/eval`
(scoring/leaderboard/exams/coverage/post-mortems), `pkg/evolve` (the evolution
loop — `lyond` wires it onto its prompt_end observer), `pkg/routing` (task
classification & recommendation network), `pkg/models` (capability profiles),
`pkg/flow` (mechanical executor). Ledgers live under `$LYON_HOME/`, **read-only
in the Web evolution panel and Dashboard**; master gate `LYON_EVOLVE=off`,
leg gates `LYON_SCORE` / `LYON_EXAM` / `LYON_DIAGNOSE`.

### Production scoring & on-duty leaderboard

- **Four-dimension scoring** `eval.ScoreWith`: Correctness / Efficiency / Cost /
  Compliance weighted into Total; `DefaultThresholds` require all four to pass.
- **Leaderboard** `$LYON_HOME/leaderboard.json` keeps a per-task-class account
  (coding/fix/review/research/writing/design/data/general): each agent retains
  its latest 50 scorecards, and every production record re-elects the on-duty
  seat (freshest passing evidence wins).
- **Blind grading**: prompts without a gold answer go to `eval.JudgeRun` (one
  cheap call, 0..1 correctness), graded in the background after prompt_end;
  failed grades are **not booked** (better no entry than a fake one).

### Closed-book exam (every 10 rounds)

- **Hand-written banks**: 4 questions per task class × 8 classes, frozen into
  the binary (never generated from the production tasks being examined),
  materialized on first use at `$LYON_HOME/exams/<class>.json` — inspectable
  and editable.
- **Closed-book** `ClosedBookSystem`: no tools, no files, only the weighted
  knowledge; `DefaultExamPass = 0.75` (3 of 4).
- **Clock**: every 10 successful prompts books a round; when due and a seat is
  filled the exam auto-runs; two consecutive failures → demotion, seat opens,
  the next production scorecard re-elects.
- Questions, clock, and seats are verifiable in the Web evolve panel (bank
  sizes, candidate field, exam clock).

### Task classification & recommended routing

- `routing.Classify`: zh/en keyword scoring (ties keep the earlier class);
  `Openness(text)` scores a task 0–5.
- **Recommendation table** `$LYON_HOME/routing.json`: each (class, agent) pair
  stores the running mean score and confidence
  `mean × samples / (samples + 3)` — more samples, more confidence; newcomers
  earn their seat with real scores.
- **Capability profiles** `pkg/models`: all 32 registered models carry
  Reasoning/LongHaul/Openness/Speed/Thrift five-dimension 0–5 profiles.
- The routing table and leaderboard read directly in Dashboard / evolve panels;
  exams and scores rewrite who sits, so evolution output becomes behavior.

### Knowledge coverage gate (design §2)

- **Two-stage closed-book test**: `GenSystem` generates a checkable Q/A list
  from the task → `BookSystem` answers blind → graded against it;
  below `CoverageThreshold = 0.6` counts as a gap.
- **Targeted injection**: gap lines (`question → expected answer`) are spliced
  into the task body; in a flow they enter **every node's** prompt through
  `{{input}}`. A gate error only logs `gate skipped` — it **never blocks**.

### Workflow freezing & mechanical execution (design §3)

- **Control flow is data**: `Workflow{slug, title, trigger, nodes}` with node
  exit criteria `always | command | exit code | contains marker | narrow judge`;
  failure policies `retry (default, bounded ≤3 with diagnosis fed back) |
  abort | continue | goto:<name>` — **the LLM only fills in node content
  (`{{input}}`), it never decides control flow**; a tick budget kills goto
  loops.
- **Four built-ins** (`go:embed`): `feature-dev` · `bugfix` · `code-review` ·
  `release`. User files `$LYON_HOME/workflows/*.json` override by slug;
  broken files are skipped, never fatal.
- **Auto-freeze (§3.1)**: the same slug green **3 runs in a row** freezes to
  `<slug>-proven.json` (title carries provenance: streak · last score · model
  at freeze); a red resets the streak — procedures that survive production
  become artifacts by themselves.
- **Failure post-mortem**: failed prompts asynchronously run
  `SummarizeFailure`, producing a checkable condition + patch — audited
  (`LYON_DIAGNOSE=off` disables); the diagnosis also **writes into the project
  memory hot tier** (deduplicated, 3-entry hot window), so the next session's
  injection starts with the lesson; flow retries append the same constraint
  block into the next node prompt.

### Historical overnight run (2026-10-12 → 10-13, 11h21m / 224 rounds, all green)

- 20 exams: **18 PASS / 2 FAIL** — the research seat's model failed twice in a
  row and was demoted; the seat was re-elected by production evidence (the
  expelled model does not return automatically).
- 206 blind-graded bookings, scores **0.80–1.00** (including four genuinely low
  ones — the grader discriminates rather than rubber-stamping).
- 8 routing handovers; final fix class: 100 samples / confidence 0.96 →
  recommends mimo-v2.6-flash.
- bugfix workflow **18 consecutive greens** auto-frozen, final title
  `缺陷修复标准流程 (frozen after 18 green runs · score 1.00 · mimo-v2.6-flash)`.
- Every ledger (leaderboard/routing/exams/workflows/audit) persisted and
  restored verbatim across restarts.

## Roadmap

- ✅ Web workbench v1–v2 (tiling panes, approvals, memory/evolution/dashboard
  panels, per-session workspaces)
- 🚧 Release prebuilt distribution & one-click install · in-process and OS-level
  timers
- 🗺 multi-user mindmap collaboration; Web-driven evolution-panel actions
  (start exams / contests / freeze procedures)

Priority discussion welcome in issues.

## Testing

```bash
go build ./... && go vet ./... && go test ./... -count=1   # backend, full
cd web && npx tsc -b                                       # frontend type check
node web/e2e/smoke.mjs                                     # Playwright smoke (needs LYON_E2E=1)
```

Covers: tool system (schema stability, approvals, allowlists, metrics, shell
probing, memory read/write), engine (event protocol, client compaction
terminal/delayed-rebuild/threshold predicates, snapshot Save↔Load↔Restore,
rollback, memory injection, subagents, real cache hits, interrupt & history
repair), modes & approvals (plan gate matrix, bypass short-circuit, mode
audit), model knowledge base, OpenAI translation layer (protocol
conversion, SSE synthesis, error mapping, SDK through-layer, engine-level
e2e), MCP, services (connectRPC, SSE, hub replay, session store,
file/memory/approval/dashboard), evolution (four-dimension scoring,
leaderboard persistence & demotions, exam authoring & grading, classification
& routing confidence, coverage injection, every flow-executor branch),
mockapi, bench (performance acceptance). All offline — no network, no real keys.

Continuous integration (`.github/workflows/ci.yml`) runs
`go build ./... && go vet ./... && go test ./... -count=1` on ubuntu / windows /
macos — thresholds are not relaxed; red means fix.

## License

[MIT License](LICENSE) © 2026 LyonLen. Issues and PRs welcome.
