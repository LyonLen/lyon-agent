# lyon-agent

High-performance general-purpose AI agent, written in Go, **shipped as both a server and a CLI**.
One process runs the full agent loop (streaming output, tool calls, context compaction,
subagents) and exposes an HTTP/SSE service plus an interactive CLI; a built-in stress
acceptance suite fails the build when performance thresholds are not met.

> 📘 **Docs site**: open [`docs/index.en.html`](docs/index.en.html) (product tour + screenshots)
> and [`docs/manual.en.html`](docs/manual.en.html) (user manual: getting started → UI →
> evolution mechanics → configuration reference → FAQ) in a browser — pure local static
> pages, no network resources, double-click to read.
> **中文**: [`README.md`](README.md) ·
> [`docs/index.html`](docs/index.html) · [`docs/manual.html`](docs/manual.html)。
> The interface itself is bilingual too: `/lang zh|en` or `LYON_LANG=zh` at startup
> (see the manual, "Interface language").

```
go build ./...          # build everything
go test ./... -count=1  # full test suite (includes bench acceptance)
go test ./bench -run 'TestLoad|TestCache|TestSSE' -v   # performance acceptance (prints result tables)
```

## Architecture

```
cmd/lyon            CLI: run (one-shot) / chat (fullscreen TUI) / tools (tool listing)
  tui.go             bubbletea TUI for chat: streaming transcript, scrollable viewport,
                     y/n approval overlay, thinking fold, slash suggestion popup, alt+v clipboard paste
cmd/lyond           HTTP service: SSE streaming sessions, /metrics, admission control
pkg/agent           engine core
  engine.go           Engine: tuned in-process HTTP connection pool, global concurrent-turn
                      semaphore, Warmup
  session.go          Session: one driving goroutine per session, serial tool lock,
                      event stream (text_delta → turn_end → prompt_end), compaction trigger,
                      history assembly
  compact.go          ★ client-side compaction: render history → separate summarize request →
                      summary replaces history (works on any endpoint)
  snapshot.go         session snapshots: Export/Restore + ~/.lyon/sessions multi-session archive
                      (latest slot + 20 archive rotation; /sessions /resume N /new)
  subagent.go         run_subagent: read-only subagent on a low-quota model
  events.go           event protocol (server SSE and CLI share one event stream)
pkg/tools           tool system
  registry.go         registration / adaptation / parallel-safety annotation
  shell.go files.go   run_shell / read_file / write_file / edit_file (workspace confinement)
  image.go            view_image / @path image attachments (magic-byte checks, size & pixel caps)
  paste.go            ★ PasteImage/PasteFile: alt+v writes .lyon/paste + over-limit re-encode + rotation
  web.go              web_search / web_fetch (client-side implementation, shared pool)
  memory.go           ★ save/read/forget_memory: cross-session memory (<workspace>/.lyon/memory.json)
  stable.go           ★ prompt-cache byte stabilization (see below)
  metrics.go          per-tool p50/p95 + last_error
pkg/mcp             MCP client: stdio and streamable HTTP, tools mapped to mcp_<srv>_<tool>
pkg/server          HTTP/SSE service: /v1/sessions, /metrics, /healthz, session store & TTL
internal/mockapi    fake API for acceptance tests: real SSE frame order + prefix-hash
                    prompt-cache simulator
bench               performance acceptance suite (thresholds enforced)
```

Defaults: model `claude-opus-5-5` (override with `ANTHROPIC_MODEL`), effort medium,
`MaxIterations=0` (ReAct turns per prompt are **unlimited by default** — the loop runs until
the model finishes on its own; to impose a hard cap use `-max-iter N` or `LYON_MAX_ITER=N`,
which ends honestly with
`prompt_end.stop_reason="max_iterations"`), compaction threshold 0.7×200k,
subagents default to the low-quota tier (their turn count is likewise uncapped).

## Getting started

### CLI

```bash
export ANTHROPIC_AUTH_TOKEN=...        # your key, never commit it
go run ./cmd/lyon run "run echo hello in the shell then summarize"   # one-shot
go run ./cmd/lyon chat                                      # fullscreen TUI (auto-saves on exit)
go run ./cmd/lyon chat -c                                   # resume this workspace's last session
go run ./cmd/lyon tools                                     # list all tools
```

Common flags: `-model` `-effort` `-workspace` (shell/file confinement root) `-approve`
(manual confirmation before shell/write; **on by default in interactive chat**, off with
`-approve=false`, never shown on piped stdin so scripts can't hang; `lyon run` only enables
it with an explicit `-approve`) `-shell-allow echo,ls,git` (allowlist) `-mcp name=cmd args`
(connect an MCP server) `-mcp-http name=url` `-prewarm` (warm the cache before the first
prompt) `-max-turns N` (global concurrent-turn cap) `-web-tools server|client|off`.

**Image input**: reference png/jpeg/gif/webp files inside the workspace with `@path` in your
prompt; the image is sent to the model as an image block (`look at @screenshot.png`; quote
filenames with spaces as `@"my shot.png"`; emails like `a@b.com` are unaffected). On the
model side the `view_image` tool can pull workspace images into context at any time —
`read_file` only reads UTF-8 text and will tell you to use `view_image` for binaries.
Size/pixel caps follow the API limits (~3.5 MB, 8000 px per side).

**`alt+v` image paste** (or `/paste`): read an image from the system clipboard — a
screenshot, a browser "copy image", or an image file copied in the file explorer — store it
under `<workspace>/.lyon/paste/` and insert the matching `@path` into the input; press enter
and it goes out with your message. The read runs on a background thread (the clipboard may
be briefly held by another process), over-limit images are **re-encoded smaller first**
(PNG preferred for fidelity, JPEG only as a last resort), the directory rotates at 32 files
deleting only the oldest so history replay never dangles; if the clipboard has no image the
status line says so instead of leaving half a token. Windows only for now (direct Win32
read, no subprocess); other platforms get a clear message suggesting plain `@path`.

### chat TUI

`lyon chat` enters a fullscreen TUI in a real terminal: streaming transcript (markdown-lite
highlighting, code-block coloring, automatic CJK wrapping), a single-line input at the
bottom, follow-scroll. On a non-TTY (pipe/redirect) or with `-plain` it degrades to a
line-based REPL.

**Layout rework**: on wide terminals (≥46 cols × ≥26 rows) a large Lyon logo is pinned at
the top (a 6-line × 36-col block-glyph `LYON` wordmark + identity line + full-width rule),
and below the logo a full-width thin line **separates the session area from the input area**
(a `──── session │ input ────`-style rule row); narrow terminals automatically degrade to
"identity line + thin rule", and the frame geometry invariant (exactly height rows, each ≤
width cells) still holds.

**Thinking fold**: consecutive thinking blocks are grouped, shown live while streaming
(with a `▾ thinking · N lines` header); the moment the next non-thinking content arrives the
whole block folds into one line
`▸ thinking · N lines  <first-line preview>`; the fold is **strict** — the last thinking
block at the end of a conversation also folds when the prompt ends instead of staying open
just because nothing follows; blocks replayed by `/resume` fold too. `ctrl+t` (or
`/think`) expands/collapses everything.

| Key | Action |
|---|---|
| `enter` | send (`/`-prefixed input goes to local commands; a unique matching prefix executes directly, `/st` → `/stats`) |
| `tab` | complete the highlighted slash command in the suggestion popup |
| `esc` | while busy, interrupt the current prompt (history is repaired, you can keep chatting); while idle with a popup open, close it first |
| `ctrl+c` | while busy: interrupt first, press again to exit; while idle: exit |
| `ctrl+t` | expand/collapse all thinking blocks |
| `↑` / `↓` | move through suggestions when the popup is open, otherwise recall input history |
| `alt+↑` / `alt+↓` | step-panel step selection (first press picks the latest; walking past the end returns to live follow) |
| `alt+enter` | expand/collapse the selected step's full action |
| `alt+←` / `alt+→` | widen/narrow the panel (28–72, transcript columns re-flow in sync) |
| `alt+e` | open/close the **evolution panel** (leaderboard + exam clock, same as `/evolve`) |
| `alt+v` | paste clipboard image: lands in `<workspace>/.lyon/paste/` and inserts `@path` into the input (same as `/paste`) |
| `pgup` / `pgdn` / wheel | scroll the transcript (scrolling away from the bottom stops following, returning to the bottom resumes) |

**Shift variants**: the `alt` combos are often swallowed by terminals like Trae, so all four
step-panel keys also accept `alt+shift+…` variants (`alt+shift+↑/↓/←/→`, plus
`alt+shift+enter`) — the detection logic (`KeyMsg.String()`) renders shift as labels like
`alt+shift+up`, case-insensitively, so both presses are equivalent. **Resident key legend**:
while idle the status line permanently shows
`enter send · esc interrupt · pgup/pgdn scroll · ctrl+t think · alt+↑↓ steps ·
alt+e evolve · alt+v paste · /help` (the full key table lives in `/help`); narrow terminals
trim the tail without affecting sending.

**Queue while busy**: typing your next question while a prompt is running is **no longer
lost** — press enter and it is queued, the status line shows
`queued (N) — sends when the current prompt ends`, and each message is sent FIFO once the
current prompt's `prompt_end` lands (interrupts drain the queue too).

**Interface language**: `/lang zh` / `/lang en` switches the whole interface chrome at
runtime (status line, command help, popup descriptions, panel labels, approval row, console
prompts); `LYON_LANG=zh` starts in Chinese. Transcript content (prompts, answers, tool
output) is data and is never translated.

**Slash suggestion popup**: typing `/` pops the command list above the input, filtered live
as you type, with a trailing hint
`↑/↓ choose, enter runs · tab complete · esc close`; the viewport yields rows in sync so
the layout never gets pushed off. `enter` **runs the currently highlighted command** (one
match is it; multiple matches are picked with ↑/↓), tab only completes the name into the
input; no match falls through to normal local-command handling; closing with `esc` and
typing `/` again reopens it.

**Chat-area hygiene**: the chat transcript holds only **questions, model answers and call
activity** (tool lines, turns, compaction, approval and rollback audit); command output and
query results (`/stats` `/help` `/model` `/memory` `/sessions` `/rollback` lists, mode-switch
feedback, ★ checkpoints, memory save/sync) render in the **temporary output area below the
transcript** (separated by `── output ──`, capped at 10 lines + `…(+N more)` truncation).
A new prompt, `esc`, or the next command clears it — only the latest answer is ever shown.
**Slash commands leave a trace**: an executed `/xxx` is written into the transcript as a
prompt line (followed by a blank separator), so scrolling up shows "what did I run";
`/clear` wipes its own trace along with the view (that's the point). Per-turn usage rows
like `[turn N] in=… out=…` are **no longer printed into the transcript** — the token ledger
lives in `/stats` and the resident bottom-right gauge.

Commands: `/stats` usage & cache-hit table · `/clear` clear the view (session history kept) ·
`/resume` restore an archive (**pops a picker** when several exist, `/resume N` picks the
Nth) · `/sessions` list all archives · `/new` start a fresh session (current one archived) ·
`/think` same as `ctrl+t` · `/steps` `/evolve` side panels (or `alt+↑↓` / `alt+e`) ·
`/lang zh|en` interface language · `/help` · `/quit` (`/memory` `/remember` `/rollback`
`/route` `/exam` `/flow` `/model` `/mode` `/paste` are covered in their own sections below).
Interactive chat **pops** a y/n overlay in the status line for shell/write tools by default
(off with `-approve=false`, auto-off on piped stdin): `y` approves once ·
**`a` approves that tool for the whole session** (so a long task isn't interrupted ten times
by the same question) · `n` denies (timeout/closed window always denies, fail closed; denials
are not persisted). `lyon run` does not pop by default; `-approve` enables it (denies when
stdin closes, fail closed).

### Session restore & memory

**Session restore (multi-session archive)**: on exit `lyon chat` saves the conversation to
`~/.lyon/sessions/` (`LYON_HOME` rewrites the storage root entirely — containers/CI/tests
never touch the real home), **one slot plus one archive directory per workspace**
(`<sha256(workspace absolute path)[:16]>.json` is the latest slot, the same name without
`.json` is the archive; each keeps the latest 20, tmp+rename atomic writes, empty
conversations are not written). Saving **rotates instead of overwriting**: conversation
changed → the old slot moves into the archive before the new one is written; unchanged
(peeked at an old session and quit) → nothing is written, no duplicates. So **every distinct
session stays restorable forever**:

- `/resume` — restore an archive: a single one restores directly, several **pop a picker**
  (↑/↓ select, enter restores, esc closes); `lyon chat -c` restores the newest on startup;
- `/sessions` — list all archives (index, time, message count, newest first);
- `/resume N` — restore the Nth archive; browse old sessions freely;
- `/new` — start a fresh session in place: **the current conversation is archived first**
  (skipped if already in the store), then history/turn stats/transcript are cleared; while
  busy it interrupts first, like `/resume`, and runs after prompt_end.

Restore behavior: the TUI replays the transcript by prompt/answer/tool lines with a
`restored N messages · <time>` line added; the line REPL prints the same notice; subsequent
prompts continue on the old history; starting from a blank session with archives present
prompts a count and `/resume` usage. `/resume` during a running prompt interrupts first and
restores after prompt_end lands — a bare `/resume` pops the picker in the meantime (it never
races the driving goroutine for the history lock). The save order is fixed
`Interrupt → Export → Save`: Export takes the history lock and snapshots only after the
in-flight prompt winds down, so what you get is always a complete history.

**Memory**: built-in `save_memory` / `read_memory` / `forget_memory` write facts into
`<workspace>/.lyon/memory.json` (case-insensitive upsert by title, capped at 500 facts /
8 KB each, oldest evicted, atomic writes). The first prompt of each session injects a
`[persistent-memory]` snapshot once; sessions restored with `chat -c` skip injection when
the history already contains the current memory block. Injection goes through a user
message block rather than `role:"system"` — the latter is unproven across gateways, the
former is 100% compatible.

**Compaction (client-side, works on any endpoint)**: when one turn's usage reaches
threshold × context window (default 0.7×200k, tuned by `-compact-threshold`, negative
disables) it triggers: history is rendered as role-labeled text (thinking dropped, each
block truncated to 4000 chars), one separate summarize request is sent, and a single
summary user message **replaces** the history wholesale. No dependence on the SDK's
`CompactBeforeNextTurn` — when a gateway doesn't return a native compaction block that path
just burns one request and keeps the history intact. Details: a `tool_use` turn arms
compaction one beat late (`compactArmed`) so tool results land before the summary is
generated and nothing is lost; when history ends on a dangling `tool_use` the runner is
rebuilt (the old runner would dispatch abandoned calls from the summary), otherwise only
`Params.Messages` is swapped so the turn finishes naturally; a progress guard
(`lastCompactInput`) guarantees no retry unless the summary actually reduced usage, so
failures can't loop forever. In the TUI it shows as `📦 compaction ... (in=... out=...)`,
corresponding to `stop=compaction` turns in `/stats` and the SSE event stream.

### Server

```bash
go run ./cmd/lyond -addr :8080 -workspace . -prewarm
bash examples/sse-client.sh http://127.0.0.1:8080   # acceptance client
```

| Route | Description |
|---|---|
| `POST /v1/sessions` | create a session; with a `message` field it starts running immediately, SSE back |
| `POST /v1/sessions/{id}/messages` | follow-up prompt, SSE streaming events |
| `GET /v1/sessions/{id}/stats` | per-turn usage (incl. cache_read/cache_creation) |
| `DELETE /v1/sessions/{id}` | destroy immediately |
| `GET /metrics` | throughput, cache_share, tool p50/p95, last_error |
| `GET /healthz` | liveness probe |

Server-side defenses: `-max-sessions` admission cap (429 beyond it), `-session-ttl` idle
eviction, `-max-turns` global in-flight API turn cap, SSE write timeout 15s +
`X-Accel-Buffering: no`.

### Embedding as a library

```go
reg := tools.NewRegistry()
tools.RegisterFunc(reg, "get_quote", "description", true /* parallel-safe */, fn)
eng := agent.NewEngine(agent.DefaultConfig(), reg)
sess := eng.NewSession(nil)
defer sess.Close()
sess.Submit("your prompt")
for ev := range sess.Events() { /* text_delta / tool_start / prompt_end ... */ }
```

Full examples: `examples/custom-tools` (business tools), `examples/mcp-hello` (MCP client).

## Performance acceptance (bench)

`internal/mockapi` is an in-process fake Anthropic API: it emits real SSE frame order,
supports configurable latency, and runs a **prefix-hash cache simulator** — concatenating
raw bytes element-wise in semantic order (tools → system → messages) and hashing, so any
byte-level drift is a guaranteed MISS. The `bench` suite FAILs when thresholds are missed:

```bash
go test ./bench -run 'TestLoad|TestCache|TestSSE' -v
# optional real-endpoint report (no assertions): LYON_LIVE_TEST=1 go test ./bench -run TestLiveReport -v
```

Measured on this machine (20-core Windows):

| Acceptance item | Threshold | Measured |
|---|---|---|
| Engine p95 overhead (clean window, 16 sessions × 8 prompts, engine p95 − bare SDK p95) | ≤ 25ms | **5.7ms** (20.4ms vs 14.7ms) |
| Steady-state prompt-cache hit rate (token share from round 2 on) | ≥ 95% | **97.78%** (per-round read grows 1641→2057) |
| SSE 50 concurrent clients event loss | 0 | **0** (sequence identical to baseline) |
| SSE first-event TTFD p95 | ≤ 1s | **70ms** |
| 100 / 500 / 1000 session matrix prompt errors | 0 | **0** (throughput 1519 / 483 / 243 prompts/s) |
| Session driver goroutine leaks after 1000-session close | 0 | **0** (85MB heap) |

Notes: at 1000 concurrent the machine saturates overall (the bare-SDK baseline collapses
synchronously to the same level), so the differential is scheduling noise — hence the
**overhead threshold is only enforced in the clean window**; the matrix subtests hard-fail
on zero errors, full completion, and zero leaks, while throughput/percentile latency are
recorded but not gated. Real-endpoint report (`LYON_LIVE_TEST=1`): 100% steady-state hit
rate, ttft p50 3.5s (gateway model, not native Anthropic endpoint).

## Key finding: prompt cache vs byte drift in anthropic-sdk-go

**Symptom**: in the same session, `cache_read` stays 0 every turn — the cache never hits.

**Root cause**: the SDK's `param.MarshalWithExtras` iterates the schema's `ExtraFields`
with `for k, v := range extras` (Go map iteration order is random), and the tool schemas
produced by jsonschema reflection carry exactly two ExtraFields: `{$schema,
additionalProperties}`. The tools array is the request-body prefix, so its bytes change
every request → the cache prefix chain never lines up. Two requests that are semantically
identical MISS purely on key order (`mockapi`'s
`TestCacheMissOnSchemaKeyOrderDrift` pins this behavior).

**Fix** (`pkg/tools/stable.go`): each tool schema is canonicalized exactly once — marshal →
`UseNumber` decode → re-ordered-key marshal — then returned via
`param.Override[...](json.RawMessage)`. The Override branch skips the map walk, making the
bytes deterministic forever. It hangs on `Adapt`, the single funnel every path (session
tools, subagents, Warmup) goes through, and it is idempotent.

After the fix: byte-identical across processes, 94.2% cache share over 8 turns, 93.6% on
subagent turns, and `TestAdaptedSchemaMarshalDeterministic` marshals byte-identically 100
times in a row.

**Known gateway limitations** (not this library's problem):

- this gateway only reports `cache_read`, never `cache_creation` (cache writes are
  unobservable);
- the gateway does not return native compaction blocks — hence compaction uses the
  client-side implementation (see "Session restore & memory" above) and doesn't depend on
  that capability; any endpoint can shrink history.

## Robustness design

**Shell probing** (`pkg/tools/shell.go`): on Windows `exec.LookPath` can resolve the WSL
placeholder `C:\Windows\System32\bash.exe` — resolvable but unrunnable. So every candidate
shell actually executes `echo lyon-shell-probe-ok` once (5s timeout, verifying the stdout
marker and exit code) and broken binaries are eliminated; when Git Bash isn't on PATH the
probe also tries common install paths like `%ProgramFiles%\Git\bin\bash.exe`. Only when all
fail does it report `no working shell found (tried: ...)`.

**Interruptible prompts** (`Session.Interrupt`): `esc`/`ctrl+c` cancels the context derived
from that prompt — the API stream and running tools stop together, the session itself
survives, and prompt_end ends with `interrupted` (no red error event). If history ends on an
assistant `tool_use` (interrupted before the tool ran, or `MaxIterations` cut before
dispatch), `repairInterruptedHistory` queues a synthetic `tool_result` for every dangling
call, merged into the same user message as the next user turn, keeping role alternation
valid so the API won't reject it. If the turn ended cleanly yet compensation is still
needed, the iteration budget hit its cap (`prompt_end.stop_reason="max_iterations"`, only
possible when `-max-iter`/`LYON_MAX_ITER` is set — default is unlimited): the TUI shows
`⚡ budget reached` and prompts enter-to-continue, and skipped steps are honestly marked
`over budget`. The busy lock releases **before** prompt_end is emitted so a buffered-event
consumer that immediately resubmits doesn't hit `ErrPromptInProgress`.

## Stats, audit and security control (hooks)

`pkg/hooks` plants hook points across each session lifecycle and implements three consumers
— `prompt_start/end`, `turn_end`, `tool_use/exec_start/end`, `approve_request/result`,
`compaction`, `subagent_end`, `error`, `security_block` and more all pass through:

- **Audit**: `~/.lyon/audit/<date>.jsonl`, one JSON Record per line (time, event, tool,
  truncated input, duration, approval result, token usage, block reason). Disk writes run on
  a separate goroutine — zero I/O on the hot path; 30-day retention with automatic cleanup.
  Off with `LYON_AUDIT=off`.
- **Stats**: `~/.lyon/stats.json` persists aggregates — prompt/iteration/compaction/subagent
  counts, approval yes/no, security blocks, cumulative tokens, per-tool calls/failures/
  duration. In the TUI, `/stats` appends this global section after the per-session table.
  Flushed on prompt_end (≤2s latency), with a synchronous `Flush` on `Session.Close`.
- **Security control** (two layers, running before approval — a blocked tool never bothers a
  human):
  1. `~/.lyon/policy.json` local policy:
     ```json
     {
       "allow_tools": ["read", "grep", "glob"],   // non-empty ⇒ allowlist mode
       "deny_tools": ["bash"],
       "deny_patterns": ["(?i)rm\\s+-rf\\s+/", "curl[^|]*\\|\\s*(ba)?sh"]
     }
     ```
     Patterns are regex-matched against the tool input's raw JSON; a block returns
     `blocked by security policy` to the model as the tool result and logs
     `security_block` (`blocked_by` names the rule).
  2. `~/.lyon/hooks.json` external hooks:
     ```json
     [
       {"event": "tool_end",   "command": "node C:/hooks/log.js", "timeout_ms": 5000},
       {"event": "tool_start", "argv": ["gate.exe"], "blocking": true}
     ]
     ```
     The Record is written as JSON to the hook's stdin; `"blocking": true` (tool_start only)
     vetoes the execution on exit code 2 or stdout `{"deny":true,"reason":"..."}`.
     Non-blocking hooks run async, failures count into `hook_failures` and log `hook_error`.
     Off with `LYON_HOOKS=off`.

External hooks can back your own metrics shipping, audit export, DLP/sandbox gateways and
more; subagent tools pass through the same `PreTool` security gate.

## Memory module (layered hot/cold + self-evolving + cloud sync)

Three layers of persistent memory, one-fact-per-file Markdown, hand-editable:

| Layer | Location | Scope |
|---|---|---|
| team | `$LYON_HOME/memory/team/` | shared across projects and agents (org conventions) |
| personal | `$LYON_HOME/memory/personal/` | this user's all projects (personal preferences) |
| project | `<workspace>/.lyon/memory/` | current codebase (decisions, conventions, gotchas) |

- **Retrieval**: local BM25 inverted index (CJK bigram tokenization, no tokenizer needed),
  field weights name 3.0 / tags 2.5 / description 2.0 / body 1.0; ties prefer the nearer
  layer (project > personal > team). Pure in-memory index, millisecond-fast, with better
  recall and ranking than grep.
- **Hot layer**: facts with `hot: true` ride permanently in the system prompt of the first
  prompt of a session (default 4KB budget, rendered whole without truncation). `/memory N`
  toggles hot/cold; a stored fact hit ≥2 times by retrieval/close-reads is auto-promoted to
  the hot layer the next time it is distilled.
- **Model tools**: `save_memory` (upsert by title, `scope`/`hot` params), `read_memory`
  (`query`=BM25 search / `title`=read closely / empty=compact list), `forget_memory` (delete
  by name).
- **Self-evolution (distill)**: triggered manually with `/remember` or automatically once on
  session exit — a cheap submodel (SubagentModel) extracts durable facts from the recent
  conversation into the store. Idempotent (unchanged content is not rewritten), 20s time
  limit so it never blocks exit, off with `LYON_DISTILL=off`; every write logs a
  `memory_save` audit event.
- **Cloud sync**: enable by setting `LYON_SYNC_URL` (optionally `LYON_SYNC_TOKEN`),
  `POST /memory/push {"facts":[…]}`, `POST /memory/pull {"scopes":["team","personal"]}`,
  newer `updated` wins; only team+personal sync — the project layer never leaves the machine.
  Manual trigger: `/memory sync`.
- **Legacy migration**: `<workspace>/.lyon/memory.json` is auto-split into project-layer
  facts on first open, the original renamed `memory.json.imported`.
- **TUI/REPL**: `/memory` renders a numbered table (layer, hits, updated, summary),
  `/memory N` makes hot, `/memory forget N` deletes, `/memory reload` re-reads disk (other
  agents' edits), `/memory sync` cloud sync; the plain REPL supports the same.

Fact file format (frontmatter is hand-editable; files without frontmatter use the filename
as the name and the whole file as body):

```markdown
---
name: toast-notification-preference
description: notify via bottom-right toast, not old-school dialogs
scope: personal
hot: true
created: 2026-10-10T18:00:00Z
updated: 2026-10-10T18:30:00Z
hits: 3
---
body…
```

## Automatic session titles (intent detection)

- After each prompt, a cheap submodel judges intent: same topic returns
  `{"changed":false}` and keeps the title (no write, no cost); only a topic change regenerates
  the title (3–8 words, following the user's language).
- The judge sees only "first prompt + current title + latest prompt" (a few hundred tokens,
  never the full history), reply capped at 64 tokens; off with `LYON_TITLE=off`.
- Display & persistence: the TUI header permanently shows `lyon chat · <title>`;
  `/sessions` and the resume picker list each session's title; snapshots save the title and
  `chat -c` continues it. The plain REPL prints `🏷 title …` when the title changes.
- Topic-change detection runs async at prompt_end, the UI never waits; a newer prompt_end
  arriving mid-check reruns it so the latest topic is always recognized.

## Right-side session task panel (step audit)

- **Task tree**: the panel is organized by **parent/child tasks** — at a glance you see
  which steps solve which problem and what any newly added part is for. Every prompt
  produces a **group heading line** (`▸ <prompt compressed to one line>`, bold, zero
  indent) with its actions indented one level; `run_subagent` is a **task node** and the
  subagent's internal execution (events tagged `Sub`) indents another level beneath it;
  `subagent_end` closes the node as `task · N API calls` (merging never duplicates rows).
- **Readable parameters**: parameter lines in the panel and chat show what the model
  actually asked for — `run_shell` shows the command, `web_search` the query, `web_fetch`
  the URL, file tools the path, `run_subagent` the task — multi-line compressed to one line
  (escapes like `&` restored to plain text), no more bare `{"command":"…"}` JSON.
- **Tool color coding**: shell green · subagent blue · write-class yellow · web/MCP magenta
  · read-class cyan · prompt headings bold cyan — categories read at a glance.
- **Live action audit**: every step carries a status mark: `⟳` running (live duration) ·
  `✓` done (with duration) · `✗` failed/interrupted. All data comes from the engine event
  stream (tool_use_start / tool_exec_* / server_tool_result / subagent_end / compaction /
  prompt_end); use_start records the decision, exec_start pairs the execution ID, exec_end
  closes by ID (parallel same-name tools pair FIFO); steps still running at prompt_end are
  honestly recorded as interrupted/aborted.
- **Interactive**: `alt+↑/↓` walks history (selected row highlighted, header shows `@#n`,
  window follows the selection, passing the newest returns to live follow); `alt+enter`
  expands the selected step — full command wrapped (up to 6 lines, then truncated), full
  error text, start time; `alt+←/→` resizes the panel 28–72 with transcript re-flow in
  sync. `/steps` toggles, `/steps open|close|width N` mirror the keys.
- **Auditable**: step numbers increase monotonically within a session (line up with the
  hooks audit log); compaction failures and tool error text show inline at the row end;
  /new /resume reset the panel (historical action audit lives in the hooks log and
  transcript).
- **Geometry**: narrow terminals (<110 cols) hide it automatically and `/steps` cannot go
  below 110; layout is `[transcript tw] │ [panel 40]` with the transcript never squeezed
  below 40 columns, and the frame geometry invariant (exactly height rows, each ≤ width)
  holds throughout.

## Rollback (/rollback, including pre-compaction rollback)

- **Checkpoints**: every prompt entry (when history is non-empty) automatically captures a
  full copy of history; **before compaction replaces history** another is captured — so
  compaction is always undoable. Manual markers: `/rollback save [text]` (e.g. mark one
  before touching dangerous files).
- **Restore**: `/rollback` lists checkpoints (new→old, with message count/time/label,
  compact ones marked 📦), `/rollback N` restores the Nth (same numbering convention as
  /sessions). Restore = the next prompt rebuilds history from the checkpoint; **turn stats,
  spend and title are kept** (money spent is spent — only the conversation rolls back).
- **Roll back a compaction**: the compact checkpoint stores the complete pre-replacement
  original, so after restore the next request carries the original conversation again
  (`TestCompactRollback` asserts the summary vanishes and the original returns); compact
  checkpoints are the longest-lived on eviction (undo stack capped at 20, prompt checkpoints
  deleted first).
- **Safety semantics**: restore only runs while idle (busy auto-interrupts and runs after
  prompt_end, the same deferred mechanism as /resume); checkpoints after the target are
  invalidated; unknown ids error instead of panicking; each restore writes a `rollback`
  audit record (hooks log). The transcript keeps the discarded lines plus a
  `↺ rolled back` mark — the audit trail is never erased.

## Built-in model parameter knowledge base (/model)

- **Offline knowledge base** (`pkg/models`): context windows, output caps, officially
  recommended parameters (temperature/top_p/reasoning-model constraints), knowledge cutoffs
  and **reference prices** (USD per million tokens) for common model families — Anthropic
  Claude, OpenAI GPT/o series, Google Gemini, DeepSeek, Qwen, Llama, Mistral,
  **Xiaomi MiMo (gateway default `mimo-v2.6-flash`)** — no network lookups, compiled into
  the binary. Cache hit/write prices are recorded per vendor (MiMo hit at 2%, writes free
  during the promo); entries without a record fall back to Anthropic's 0.1×/1.25×.
- **Lookup rules**: exact match first (case-insensitive), then longest wildcard prefix
  (`gpt-4o-mini-2024-07-18` → the `gpt-4o-mini*` card, not `gpt-4o*`);
  **unknown models return nil — never guess a window**; callers fall back to the configured
  value.
- **`/model` command** (same in TUI and plain REPL):
  - `/model` — current model's knowledge card + an `effective:` line (actually-effective
    model/effort/max_tokens/ctx config/compact threshold);
  - `/model list` — full table (id · context · price · family);
  - `/model <id>` — look up any model, **without switching**;
  - unknown models show "not in the knowledge base — running on config, no guessed params".
- **Prices are reference prices**: vendors reprice anytime, the official site is
  authoritative (stated on the card); unknown prices show "unknown (check the vendor site)"
  instead of inventing numbers.
- **Paves the way for "context percentage + spend"**: the knowledge base's context windows
  and unit prices are the data source for the resident percentage and cost display
  (Lookup(nil) falls back to the configured window).

## Context usage & spend (resident display)

- **Bottom-right resident gauge**: pinned at the far right of the input line, always
  `ctx 68.2% · 136,400/200,000 · $0.412` — percentage of window used + current usage/window +
  session cumulative spend (moved from the header to the bottom-right corner: a glance while
  typing reads like a fuel gauge for the session). The gauge's seat is **carved out of the
  input's own width** (room for the gauge + 3-cell gap; when the input area is under 40
  cells the whole segment yields — a cramped input hurts more than a missing number); color
  follows the level: ≤60% cyan, ≥60% yellow (approaching the compact threshold), ≥85% red;
  the frame geometry invariant still holds.
- **plain REPL**: prints a `── context 68.2% · … tok · cost …` line after every prompt, and
  also on startup when restoring an archive (picking up the session's gauge); `/stats`
  appends the same summary line at the end.
- **Usage estimate**: takes the last turn's request side (input+cache_read+cache_write) plus
  the reply — the reply is already in history and is the next request's increment;
  **compact turns are special-cased** (that turn's request side was the discarded
  pre-compaction history; only its reply — the summary — remains in context). Window size
  comes from the knowledge base (the `/model` machinery), falling back to the configured
  value when unknown.
- **Spend**: per-turn input/output at face value, cache read ×0.1, cache write ×1.25 (the
  published Anthropic multipliers), accumulated; **subagent usage is accounted separately
  per model** (`Session.SubUsage()`, often a cheaper tier) — the submodel's price is used
  when in the knowledge base, otherwise it follows the main model. Unit prices come from the
  knowledge base (reference prices); **unknown models never get invented prices** — display
  `cost unknown`, or declare the gateway's tariff via environment:
  `LYON_PRICE_IN` / `LYON_PRICE_OUT` (USD per million tokens; both must be set).
- Subagent rows live in the session archive but not on disk (resumed sessions backfill on
  the next run); `/new` zeroes them together with turn stats.

## Plan mode / bypass mode (/mode)

- **Three execution modes** (session-scoped, engine-enforced, identical in TUI/REPL):
  - `default` — **human approval**: interactive chat pops y/n by default (off with
    `-approve=false`, never on pipes); `lyon run` auto-approves by default (enable pops with
    `-approve`) — this is exactly the layer bypass exempts;
  - `plan` — **read-only exploration**: file writes, shell, memory writes, MCP tools and
    unregistered tools are all `blocked by plan mode` (fail closed: the allowed set =
    registry's ParallelSafe read-only annotation, `mcp_*` additionally excluded as
    side-effect-unknown, `run_subagent` allowed because the subagent only gets read-only
    tools through the same gate). Blocks land in audit as
    `security_block(blocked_by=mode:plan)`, **without disturbing approval or firing
    external hooks**; the system prompt simultaneously injects the `[MODE: PLAN]` contract
    (the model knows the rules first; each switch costs one cache MISS, switching back to
    default restores the bytes and hits again);
  - `bypass` — **auto-approve**: tools that would require approval no longer pop to a human,
    they go straight to yes, with approve_request/result audit rows still written
    (auto-grants are always recorded). **policy.json and blocking hooks still run** — bypass
    only skips the human, it does not weaken the security floor.
- **Switching**: `/mode` (bare = show status) · `/mode plan|bypass|default` ·
  **shift+tab cycles** default→plan→bypass→default; every switch writes a `mode` audit
  record (`default → plan`).
- **Always visible**: TUI header badge `· [plan]` (yellow) / `· [bypass]` (red), and a
  `mode → …` confirmation line in the transcript; mode is never silently changed by `/new`
  or `/resume` — the badge is always there, so a switch is always visible.
- Takes effect live on a running prompt: the next tool call follows the new mode (atomic
  switch, safe across UI and driving threads).

## OpenAI-compatible gateway (protocol translation layer)

- **Single-point translation**: `pkg/openaix` implements an `http.RoundTripper` that
  translates the Anthropic Messages protocol ⇄ OpenAI `chat/completions` at the HTTP layer.
  Tool system, history assembly, tool runner, compaction, subagents, title generation…
  every call site shares one Engine client — **zero business-code changes**, zero overhead
  when disabled.
- **Enabling** (any one):
  - `lyon chat -openai` — default gateway `https://token-plan-cn.xiaomimimo.com/v1`
  - `lyon chat -openai-base https://host/v1` — any OpenAI-compatible endpoint (implies
    `-openai`)
  - environment variable `LYON_OPENAI_BASE` (flag wins, trailing `/` stripped)
  - `lyond -openai` / `lyond -openai-base …` also supported
- **Same key**: requests still carry `x-api-key`; the translation layer swaps it to
  `Authorization: Bearer …` for the gateway (passed through verbatim if the gateway itself
  uses Bearer); **auth headers / Anthropic-specific headers never leak to the gateway**.
- **Gateway-friendly defaults**: `WebToolMode` forced to `client` (the gateway can't run
  Anthropic server-side web tools); when `LYON_SUBAGENT_MODEL` is unset, subagents use the
  main model (single-model gateways reject a second model id).
- **Protocol notes**:
  - Requests: system → first system message; `tool_result` hoisted into a standalone
    `role:"tool"` message placed before user text (OpenAI requires tool messages to follow
    the assistant's tool_calls); adjacent same-role messages merged; thinking/unknown blocks
    dropped; `tool_choice` none/any/tool/auto → none/required/function/auto; streaming
    requests carry `stream_options:{include_usage:true}`.
  - Responses: gateway SSE frames are synthesized into the Anthropic
    `message_start/content_block_delta/message_stop` stream (the SDK's Accumulate dispatches
    strictly by event name and drops frames missing the `event:` line — all supplied);
    usage zeroed at message_start with all four fields on message_delta; JSON responses
    also feed the streaming SDK.
  - Errors: non-2xx keeps the original HTTP status (retry policy unchanged), the body is
    wrapped in the Anthropic `{"type":"error",…}` shape (401→authentication_error,
    429→rate_limit_error, 529→overloaded_error, ≥500→api_error), `retry-after`/
    `x-request-id` passed through; a 200 whose body is HTML/an error object maps to
    502/500 instead of a silent empty message.
- **Acceptance**: translation unit tests, RoundTrip round-trips (streaming/non-streaming/
  errors/passthrough/auth), a real anthropic-sdk-go client through the layer, and an
  engine-level real tool-runner two-cycle e2e
  (`pkg/agent/openai_gateway_test.go`: tool_call → local execution → role:"tool" reply →
  text finish).

## Agent evolution (scoring · exams · routing · workflow freezing)

Design document: `docs/design/agent-evolution.md`. Four code layers: `pkg/eval`
(scoring/leaderboard/exams/coverage/failure diagnosis), `pkg/routing` (task classification
and recommendation network), `pkg/models` (model capability profiles), `pkg/flow` (the
mechanical executor); wiring lives in TUI commands and `lyon run` flags.

### Production scoring & on-duty leaderboard

- **Four-dimension scoring** `eval.ScoreWith`: Correctness / Efficiency / Cost / Compliance
  weighted into Total; `DefaultThresholds` requires all four to pass — everyone on duty is
  qualified.
- **Leaderboard** `$LYON_HOME/leaderboard.json` keeps books per task class
  (coding/fix/review/research/writing/design/data/general): each Agent keeps its latest 50
  scorecards, and every production record re-elects on-duty (freshest evidence that passes
  wins).
- Production score sources: **ordinary prompts auto-enter the leaderboard when finished** —
  with no ground truth, design §1.2 routes to a **blind judge** (`eval.JudgeRun`, one cheap
  call returning 0..1 correctness): the TUI judges in the background after each successful
  prompt_end (a `score` line in the steps panel), `lyon run` prints
  `✎ score … → leaderboard [class]` at the end; `LYON_SCORE=off` disables it (exam rounds
  still count), a failed judgment **is not recorded** (better no card than a wrong one).
  `lyon run -flow` results enter as `source=flow` (correctness = node pass rate, efficiency
  from steps vs node count).

### Closed-book exams (every 10 rounds)

- **Handwritten question bank**: 4 questions for each of 8 task classes, frozen into the
  binary (never generated from the production tasks being examined), written to
  `$LYON_HOME/exams/<class>.json` on first use — inspectable and editable.
- **Closed book** `ClosedBookSystem`: no tools, no file reads, only the knowledge in the
  weights; a question scores when the expected string matches or over half the keywords hit,
  `DefaultExamPass = 0.75` (3 of 4).
- **Clock**: every 10 successful prompts book a round (`ExamInterval=10`); when due and the
  seat is occupied an exam auto-runs (`LYON_EXAM=off` disables triggering, rounds still
  count); two consecutive fails → dismissed, seat vacant, the next production scorecard
  re-elects.
- **Entry points**: automatic in the TUI / `/exam [class]` (bare command classifies the
  previous prompt); `lyon run -exam auto|<class>` examines before running (a failed exam
  only warns, the scorecard is still recorded).

### Task classification & recommended routing

- `routing.Classify`: bilingual keyword scoring (ties keep the earlier class, order fix →
  review → research → design → coding → writing → data → general); `Openness(text)` scores
  the task 0–5.
- **Recommendation table** `$LYON_HOME/routing.json`: each (class, agent) records the mean
  run score with confidence `mean × samples / (samples + 3)` — more samples, more
  confidence; newcomers earn their seat with real scores.
- **Capability profiles** `pkg/models`: all 32 registry models annotated with
  Reasoning/LongHaul/Openness/Speed/Thrift 0–5 profiles.
- Bare `/route` = evidence table + exam clock + current model profile; `/route <text>` =
  classification + openness + that class's recommendation + profile comparison (suggests a
  different model when the task's openness exceeds the profile) + workflow matching.
- **Consumption loop**: when `lyon run` doesn't pin `-model`, it consults the recommendation
  table first — if the class's on-duty seat has ≥3 samples and confidence ≥0.35, this run
  works with it right away (prints `⇄ routing [class] model …`); an explicit `-model` always
  wins. Exams and scored rewrites decide who holds the seat, and the next task is handed to
  them — the output of evolution becomes behavior.

### Knowledge coverage gate (design §2)

- **Two-stage closed-book quiz**: `GenSystem` generates a checkable Q/A list from the task →
  `BookSystem` blind-answers → scored against it; below `CoverageThreshold = 0.6` counts as
  a gap.
- **Targeted injection**: gap rows (`question → expected answer`) are spliced into the task
  body; in flows they enter **every node's** prompt via `{{input}}`. A gate error only logs
  `gate skipped` — **never blocks**.
- **Entry points**: `lyon run -coverage` (front gate for ordinary prompts, prints the score);
  `lyon run -flow` gates by default (`LYON_FLOW_GATE=off` to disable); `/flow save` forces
  the gate and **brands the gaps into the nodes**, so the frozen file carries that knowledge.

### Workflow freezing & mechanical execution (design §3)

- **Control flow is data**: `Workflow{slug, title, trigger, nodes}` with node exit criteria
  `always | command | command exit code | contains marker | judge narrow question`; failure
  policies `retry (default, bounded retries ≤3 with diagnostic feedback) | abort | continue
  | goto:<name>`. The state machine picks nodes, judges exits, bounds retries — **the LLM
  only fills in the node's `{{input}}`, it does not decide control flow**; a tick budget
  kills goto loops.
- **Four built-ins** (`go:embed`): `feature-dev` (clarify→design→implement→self-test→review→
  deliver), `bugfix` (reproduce→locate→fix→regress→summarize), `code-review` (inventory→
  file-by-file→verdict→auto-fix), `release` (state check→version→changelog→build
  verification→tag). User files `$LYON_HOME/workflows/*.json` override built-ins by slug;
  broken files are skipped, never fatal.
- **Freeze entry** `/flow save <slug>`: serializes this session's prompt groups into nodes —
  behind two gates: **passing the closed-book exam for that task class** + the coverage gate
  (gaps branded in) — before writing to disk.
- **Execution entry**:
  - `lyon run -flow <slug> "task"` — CLI mechanical execution: gate → node loop (drainText
    collects model output, `Shell.Run` runs command criteria, narrow judge, diagnostic
    feedback) → results scored into the leaderboard + recommendation table → exit code 1
    unless all pass.
  - TUI: `/flow list|show <slug>|match <text>|export <slug>` are local and instant;
    `/flow run` in-TUI execution is not wired yet (the CLI has validated the same executor).
- **Auto-freeze (§3.1)**: after the same slug survives **3 consecutive green runs**,
  `lyon run -flow` freezes the workflow into `$LYON_HOME/workflows/<slug>-proven.json` with
  provenance in the title (streak · latest score · the model frozen under); a red run resets
  the streak to zero, `LYON_AUTOFREEZE=off` opts out. A procedure that survives production
  turns itself into an artifact.
- **Failure diagnosis feedback**: failed prompts run `SummarizeFailure` async, producing
  checkable conditions + a patch — shown in the panel, recorded in audit (off with
  `LYON_DIAGNOSE=off`); flow retries use the same mechanism to append the constraint block
  into the next node prompt.

### Evolution panel (alt+e / /evolve)

- **steps-style side popup**: `alt+e` (or `/evolve`) opens/closes the **evolution panel** in
  the right column — sharing the steps panel's column geometry (width 40,
  `alt+←/→` and `/steps width` adjust 28–72 in sync; auto-hidden below 110 columns with an
  honest notice).
- **One row per class**: the 8 task classes in fixed row order (coding/fix/review/research/
  writing/design/data/general, new ledger classes appended), each row = class name · seat
  holder (model short name, `(vacant)` when empty) · exam clock `rounds/10` · mean of the
  seat's latest 10 runs, with alert flags on the right:
  `EXAM` (due, yellow) · `fN` (N consecutive fails, red) · `open` (seat vacant, dim).
- **Race-safe**: everything is read from `eval.Leaderboard.Views'` **locked snapshot** —
  scoring records into the leaderboard on a background goroutine and the panel never reads
  the map directly (data race).
- **Empty state**: with no scorecards yet it shows a guidance block ("every completed prompt
  files a scorecard and books a round"); the footer permanently shows
  `/exam · /route · alt+e close`.

### Self-evolution demo (`demo/`, overnight live run)

- **Reproduce it**: `bash demo/evolve-loop.sh` — the driver materializes the template
  project `demo/template` (a 3-function Go arithmetic library) under `.demo/`, and every
  round `git clone`s a clean workspace, injects a bug or assigns a new task (fix /
  implement / review / research / writing rotation), and runs `lyon run` serially across
  3 candidate models (mimo-v2.6-flash / mimo-v2.6-pro / mimo-v2.5-pro): the first 9 rounds
  pin models to gather evidence, then `-model` is dropped and the **routing table picks the
  worker**; every 6th fix task rides the mechanical `-flow bugfix` workflow; when a class's
  exam clock strikes the round automatically carries `-exam <class>`.
- **Artifacts** (all persisted in `.demo/evolve-home/`, the demo's `$LYON_HOME`):
  `leaderboard.json` (on-duty seats + candidate scorecards per class), `routing.json` (the
  classification & recommendation network), `exams/<class>.json` (question banks),
  `workflows/*-proven.json` (auto-frozen procedures), `flow-streak.json` (the freeze
  clock), `audit/` (scoring audit).
- **Why serial**: leaderboard/routing JSON writes are mutex-guarded in-process and atomic
  (tmp + rename) but have **no cross-process file lock** — parallel writers on one
  `$LYON_HOME` overwrite each other; a long-running driver must be single-instance.
- Live-run results in the next section.

### Self-evolution demo results (2026-10-13 morning)

**Overnight run: 2026-10-12 19:12 → 10-13 06:34 (11 h 21 m), 224 rounds, all
exited 0 (zero failures), ~3 min/round.**

- **Exams (§1.4 closed-book)**: 20 exams, **18 PASS / 2 FAIL**. Key event: the
  research seat's sitting mimo-v2.6-flash lost two in a row (23:06 scored 1/4=25%,
  then 03:35 scored 2/4=50%), triggering demotion (log: `demoted after two
  failures — the seat is open`). With the seat cleared, the next production
  evidence re-elected: flash's trailing-10 average had been dragged to ~0.94 by the
  0.5 exam loss, so the earlier challenger mimo-v2.6-pro won the seat at 0.95 and
  held it to the end — **the expelled agent does not get the seat back for free;
  seats are re-contested on evidence** (§1.4's design semantics, demonstrated live).
- **Production scoring (§1.2 blind judge)**: 206 scores filed, range **0.80–1.00**
  — including 4 genuine low scores (0.80 / 0.82 / 0.85 / 0.86), so grades actually
  discriminate instead of uniformly hitting the ceiling; the two exam-failure cards
  (0.25, 0.50) are archived the same way. 208 cards total on the board (50 per
  agent, rolling).
- **Consumption loop (§5 routing swaps)**: 8 recorded `⇄ routing` swaps, sample
  `⇄ routing [fix] mimo-v2.5-pro (12 samples, confidence 0.78)`. Final routing table:

  | Class | Samples | Confidence | Recommended model |
  | --- | --- | --- | --- |
  | fix | 100 | 0.96 | mimo-v2.6-flash |
  | coding | 61 | 0.94 | mimo-v2.6-flash |
  | review | 30 | 0.89 | mimo-v2.6-flash |
  | writing | 30 | 0.87 | mimo-v2.6-flash |
  | research | 30 | 0.85 | mimo-v2.6-flash |

- **Workflow auto-freeze (§3.1)**: the bugfix flow ran **18 consecutive greens,
  0 failures**; frozen automatically from the third green and refreshed on every
  green after (16 freezes total); final provenance title
  `缺陷修复标准流程 (frozen after 18 green runs · score 1.00 · mimo-v2.6-flash)`.
- **A persisted set of agents** (`$LYON_HOME` = `.demo/evolve-home/`, restored
  as-is after a process restart): `leaderboard.json` (7 class seats + 208 cards),
  `routing.json` (class→model recommendation network), `exams/*.json` (5 question
  banks), `workflows/bugfix-proven.json` (the frozen procedure), `flow-streak.json`
  (the freeze clock), `audit/` (two days of scoring audit jsonl ≈2.4 MB).
- **Overnight fixes**: judge timeout 60s→240s, thinking eating the budget
  1024→4096 tokens, flow's narrow judge 64→1024, classifier gained `解释/分析`
  keys — **zero judge failures** in the final instance after the fixes.

### Command & flag cheat sheet

| Entry | Purpose |
| --- | --- |
| `/route [text]` | routing evidence table / task classification + recommendation + profile comparison |
| `/exam [class]` | manual closed-book exam (auto every 10 rounds, `LYON_EXAM=off`) |
| `/flow list\|show\|match\|export\|save <slug>` | workflow browsing and freezing |
| `lyon run -exam auto\|<class>` | examine before executing |
| `lyon run -coverage` | coverage gate + gap injection |
| `lyon run -flow <slug>` | mechanically execute a workflow, results scored |
| `LYON_SCORE=off` | disable blind-judge entry for ordinary prompts (exam rounds still count) |
| `LYON_AUTOFREEZE=off` | disable workflow auto-freeze after consecutive green runs (§3.1) |

## Testing

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

Coverage: tools (schema stability, approval, allowlist, metrics, shell probing, memory
read/write/limits/registration), engine (event protocol, client-compaction finale/deferred
rebuild/threshold predicate, snapshot Save↔Load↔Restore round-trip with turn continuation,
rollback (multi-undo/compact-undo/eviction policy), memory injection, subagents, real cache
hits, interruption & history repair),
TUI (transcript rendering, state machine, commands, approval overlay, input history,
thinking fold, slash suggestion popup, ctrl+t end-to-end keys, restore replay, steps panel
lifecycle/render sizes/column geometry, /model card/list/unknown fallback, context/spend
gauge & header resident geometry, /mode and shift+tab cycling),
modes & approval (plan gate allow/block matrix, bypass short-circuit, system injection, mode
audit), model knowledge base (exact/wildcard/unknown lookup, registry sanity, price
formatting, CostUSD cache multipliers), subagent usage accounting, OpenAI translation layer
(protocol conversion, SSE synthesis, error mapping, SDK through the layer, engine-level tool
loop e2e), MCP client, server (SSE, metrics, session store), evolution (four-dimension
scoring/leaderboard persistence & two-fail dismissal/exam question generation & grading/seed
bank write-out, classification & recommendation confidence, coverage-gate injection, flow
executor branches — retry with diagnostics, exhaustion abort, continue/goto, tick budget,
built-in workflow structure & user override, TUI /route /exam /flow and the 10-round exam
clock, CLI -exam/-coverage/-flow e2e mock scoring into the leaderboard), mockapi (cache
simulator fidelity), bench (performance acceptance).
All offline — no network, no real keys required.
