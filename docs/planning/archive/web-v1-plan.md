# lyon web v1 设计方案（protobuf 协议 + 文件预览）

> 产出：任务 #4（web 版本规划调研）。本文所有 lyon 侧结论均来自实际读过的源码（路径随文标注）；
> 两个参考仓库（A：事件追踪服务，B：同族产品）的结论来自实读。
> 不确定处统一标注 **待验证**。实现期约束沿用本仓库工作区惯例：Windows+MinGW bash 无 `cp`
> （用 `cat src > dst`）、临时文件放 `tmp/`、不 git commit/push/add、注释用英文跟随仓库风格。

---

## 0. 决策摘要（TL;DR）

1. **协议栈：Connect RPC（connect-go）+ buf 工具链**，直接挂到 `pkg/server` 现有的
   `http.ServeMux` 上（`pkg/server/server.go:31-38`），复用现有 bearer token 鉴权包裹
   （`server.go:53-71`）。参考仓库 A 全套走的就是这条路，且其鉴权血泪
   （streaming 必须单独包拦截器）可直接抄答案。
2. **SSE 与 Connect 并存，不迁移**。`/v1/sessions/*` + SSE 是 README 公开契约且有
   `bench/load_test.go` TestSSE（50 并发、零丢失、TTFD p95 门禁）背书，v1 不动；
   web 走 Connect 流。两者共享一个新的 **per-session Event Hub**（`Events()` 是单消费者
   chan，不能两边直接各读各的）。
3. **Event Hub 是整个 web v1 的核心新增件**：唯一消费 `Session.Events()`，打 seq、维护
   有界 replay ring、扇出给 N 个订阅者（SSE 迁到 hub 上 + web 流订阅）；慢订阅者被踢
   （`ResourceExhausted`），永不阻塞 driver——符合 lyon 并发硬要求（Go 式最大化并发，
   单个浏览器标签页不允许卡住一个会话）。
4. **文件预览抄参考仓库 B 的三端点形态**（列目录 JSON / 文本 JSON / 二进制 bytes），
   但安全边界用 lyon 自己的 `confine()`（`pkg/tools/files.go:49-84`，symlink 解析 +
   fail-closed，比参考仓库的字符串前缀检查强），需要把它导出成公开 API。
5. **lyond 现状有四个 web 必须补的缺口**：`Store.Sessions()` 不返回 id（只有
   `server.go:122` 一个调用方）；`Session` 无公开 `Busy()`；queue 深度不可查；工具审批
   未接线（`cmd/lyond/main.go` 全程没设 `cfg.Approve` → 一切自动批准）。
6. **鉴权 v1 = 静态 bearer token**（lyond 的 `-token`/`LYON_TOKEN`），connect 拦截器同时包
   unary 和 streaming；多用户登录协议在 proto 里预留但不实现。
7. **#15（plan 树 + 节点审批）在 proto 里把消息形状全部预留**，服务返回 `Unimplemented`，
   等 #15 落地时字段编号不变、直接开写。

---

## 1. 参考仓库 A（事件追踪服务）协议要点

### 1.1 buf 工具链（可整套照搬）

- `buf.yaml`（根目录，v2 格式）：单一 module `path: proto`，lint 用 `STANDARD`，breaking
  用 `FILE`——参考仓库 A 的 `buf.yaml:1-9`。
- `buf.gen.yaml`（v2）：**4 个 LOCAL 插件**，Go 侧 `protoc-gen-go` + `protoc-gen-connect-go`
  输出 `gen/go`（`paths=source_relative`）；TS 侧用 `node web/node_modules/@bufbuild/protoc-gen-es`
  和 `@connectrpc/protoc-gen-connect-es` 输出 `web/src/gen`（`target=ts`）——
  `buf.gen.yaml:10-21`。文件头注释记录了 2026-09-08 的教训：**远程插件每次运行都要连
  buf.build 解析 latest，网络波动即断供**，所以全部本地化，版本与 go.mod / 前端运行时锁定。
- 参考仓库 A 的 `CLAUDE.md:15` 两条硬规矩：
  1. **proto 是 API 契约唯一来源**：改动从 .proto 开始 → `buf generate` → **两处生成产物
     必须同时提交**（Go 在 `gen/go/`，TS 在 `web/src/gen/`），漏一处那侧编译失败。
  2. 新增 Service 还要在 nginx 加同名 `location /tracing.v1.XxxService/` 前缀——nginx 按服务
     逐个枚举，漏了 = 浏览器 POST 落 SPA 静态块报 405（后端直连测不出来，同类事故
     又犯过一次）。流式 location 要加 `proxy_buffering off`（`web/nginx.conf:71`）。
- 本机 buf 路径（该仓库 CLAUDE.md:26 记录）：`$(go env GOPATH)/bin/buf.exe`。
  **待验证**：本次调研中 `command -v buf` 在 PATH 里找不到 buf，该路径存在性未验证。

### 1.2 服务划分与挂载方式

- proto 按域分文件：`proto/tracing/v1/{event,query,ai,auth,admin,bill,recharge,notes}.proto`，
  统一 `package tracing.v1`，`go_package "<org>/event-tracing/gen/go/tracing/v1;tracingv1"`。
- 服务面：`AuthService`（登录/登出/whoami/账号管理，`auth.proto:66-79`）、`EventQueryService`
  （`ListEvents` 游标分页 + `GetModelSuccessRates` + `GetModelThroughput`，`query.proto:73-78`）、
  `EventAdminService`、`BillService`、`RechargeService`、`StickyNotesService` 等。
- 挂载：标准库 `http.NewServeMux` + `mux.Handle(tracingv1connect.NewXxxHandler(svc,
  connect.WithInterceptors(auth.NewInterceptor(...))))`，`/healthz` 走裸 HTTP 不经拦截器
  ——`internal/server/routes.go:38-58`。**不需要 gRPC/HTTP2**，Connect 协议直接吃 HTTP/1.1。

### 1.3 事件建模与流式推送

- 事件消息是**扁平 struct + map 扩展位 + reserved 位号**：`Event{int64 id/log_id/created_at…,
  map<string,string> attrs = 20, reserved 18,19}`——`event.proto:5-25`。`attrs` map 是前向兼容
  汇入位，lyon 的 proto 应抄这个模式。
- 历史查询走 unary 游标分页（`Filter + Page{page_size, cursor}`，`query.proto:7-33`）；
  实时推送走 **server-streaming**：`rpc AnalyzeData(...) returns (stream AnalyzeChunk)`，
  chunk 就是 `{string delta = 1}`——`ai.proto:5-22`。
- 浏览器消费（connect-es）：`web/src/api.ts:45-55` 用
  `createConnectTransport({ baseUrl, interceptors: [authInterceptor] })` 建 transport，每个
  service 一个 `createClient(...)`；流式消费是 `for await (const chunk of aiClient.analyzeData(...,
  { signal: ac.signal }))`，配 `AbortController` 取消（`web/src/AiInterpret.tsx:61-95`）。

### 1.4 鉴权（最重要的一条血泪）

- 拦截器 `internal/auth/interceptor.go`：按 RPC 过程路径表分级
  （public / authenticated / admin / dept 白名单），`guard` 公共路径先做 authn+authz 再把身份
  注入 ctx。
- **必须同时覆盖 unary 与 streaming**：`interceptor.go:120` 注释原话——“此前只有 unary 版时，
  server-streaming RPC 会完全绕过鉴权”。两个方法 `WrapUnary`（:146）与
  `WrapStreamingHandler`（:161）缺一不可。lyon 的 web 拦截器必须一体实现。
- 登录态是不透明 session token（32B 随机 + PBKDF2-SHA256 600k），浏览器放 localStorage，
  靠 transport 级 interceptor 注入 Authorization 头。

### 1.5 该仓库最值得抄的 3 点

1. buf v2 + 4 本地插件的完整配置与“双产物同提交”纪律（离线可用、版本三方锁定）。
2. connect-go 挂标准库 mux 的极简装配（`routes.go:43-56`），以及 connect-es 的
   transport/interceptor/AbortController 流式消费范式。
3. 鉴权拦截器 unary+streaming 双覆盖的强制模式 + nginx 按服务枚举 location 的踩坑记录。

---

## 2. 参考仓库 B（同族产品）文件预览要点

### 2.1 三个读端点（形态直接照抄）

后端在参考仓库 B 的 `internal/api/files/files.go`，chi 路由
（`files.go:80-89`）：

| 端点 | 形态 | 要点 |
|---|---|---|
| `GET /files` | JSON 目录列表 | 返回 `{name, is_dir, size, rel}` 数组 + `truncated` 标记（前端 `SharedFilePage.tsx:505-524` 消费 `dirEntries/dirTruncated`） |
| `GET /file-content` | JSON 文本 | `maxPreviewBytes = 512 * 1024` 硬顶（`files.go:310`）；OSS 优先、pod 内 `head -c 512KB` 兜底（`files.go:331-346`） |
| `GET /file-download` | 原始字节 | `Content-Type` 按文件名推断（`files.go:972,1004`） |

写路径（`PUT /file-content`）和上传流（`file-upload/init|stream|finalize`）是业务需要，web v1
预览**只读不写**，写留 TODO。

### 2.2 类型识别与渲染栈（前端按扩展名白名单）

参考仓库 B 前端的 `DocPreview` 的 `pickRenderer`：
`.pdf`→pdfjs（`previews/PdfCanvas`）；`.xlsx/.xls/.csv`→sheetjs（`previews/XlsxSheet`）；
`.docx/.doc`→mammoth（`previews/DocxPreview`）；`.md/.markdown/.txt`→react-markdown；
约 60 个代码/配置扩展名进 `TEXT_EXTS` 集合 → **`<pre>` 原样渲染**（作者注释：不走
markdown，否则 `#` 注释和缩进会被 ReactMarkdown 误解析破相）；其余 `unsupported`。
`.pptx` 走 `previews/PptxDeck/PptxPreview`（pptx-preview 库）。
**识别是纯前端行为**，后端只管给字节。

### 2.3 安全边界（比对 lyon 现状）

`files.go:1628-1670` 的三件套：

1. `validatePath`：拒空、`filepath.Clean` 后含 `..` 拒绝、必须绝对路径。注释明说**故意不
   钉死单一 root**（sidecar 同时暴露两个挂载点）。
2. `shellEscape`：单引号包裹 + `'\''` 转义（它有 shell 兜底路径，所以要防注入）。
3. `validateInsidePVC`：写/删类操作要求路径在允许挂载点之下**且严格深于挂载点本身**
   （防 `rm -rf /mnt/scc-data` 把整个 PVC 端掉）。

**比对**：lyon 的 `confine()`（`pkg/tools/files.go:32-84`）走真实路径判定（symlink 逐段展开，
`linkBudget=32` 耗尽 fail-closed，`files.go:26-30,92-121`），强度高于参考仓库的字符串检查，
且 lyon 只有**一个** root（engine workspace），不存在“多挂载点”问题。web 预览直接复用
`confine()`，不抄它的 validatePath。参考仓库唯一值得借的是 `truncated` 标记和 512KB 上限。

### 2.4 该仓库最值得抄的 3 点

1. 三端点形态：目录 JSON / 文本 JSON（512KB + truncated）/ 二进制 bytes，前后端契约最小且够用。
2. 前端扩展名白名单 + `TEXT_EXTS` 走 `<pre>` 的渲染决策（markdown 只给 .md）。
3. `validateInsidePVC` 的“严格深于挂载点”思想——lyon 对应物是 `confine()` 的
   `filepath.Rel` 判定（`files.go:79-82`），语义等价但实现更强。

---

## 3. lyon 现状盘点（web 视角）

### 3.1 事件契约（web 协议的映射源）

`pkg/agent/events.go:15-32` 定义 16 个 `EventType` 字符串常量：
`message_start, text_delta, thinking_delta, tool_use_start, tool_exec_start, tool_exec_output,
tool_exec_end, server_tool_result, compaction, subagent_start, subagent_end, memory_injected,
queue_injected, turn_end, prompt_end, error`。
`Event` struct（`events.go:44-87`）：`Type, Text, Thinking, ToolID, ExecID(uint64), ToolName,
Input(json.RawMessage), Server, Sub, SubID, DurationMS, IsError, Err, Turn, StopReason,
Usage(*Usage{Input,Output,CacheRead,CacheCreation int64}), Iterations, TS, Detail`。
注释明说这是“引擎与渲染器（CLI 终端、SSE server）之间的稳定契约”（`events.go:11-12`）——
web 加入后它升级为三方契约。

### 3.2 会话与事件流

- `Session.Events() <-chan Event`（`pkg/agent/session.go:236`）：**有界单消费者 chan**，
  buffer = `cfg.EventBuffer = 256`（`pkg/agent/config.go`）。背压是天然的：chan 满则 emit
  阻塞 driver。
- `Submit(prompt)`（`session.go:240`）busy 时返回 `ErrPromptInProgress`；
  `Enqueue(prompt)`（`session.go:281`）busy 时进 `inbox []string`（`session.go:76-81`）排队，
  turn gap 时 `injectQueuedGap` 注入并 emit `EventQueueInjected{Text: line}`；
  **idle 时 Enqueue 返回 `ErrNoPromptRunning`**（客户端要回退 Submit）。
- `Interrupt() bool`（`session.go:399`）、`Stats() []TurnStat`（`session.go:409`）、
  `Title() string`（`pkg/agent/title.go:42`）、`Mode()/SetMode()`（`pkg/agent/mode.go:37,47`）。
- **缺口**：没有公开 `Busy()`；inbox 深度不可查；这些是 web 会话面板需要补的小 accessor。

### 3.3 lyond 与 HTTP 层

- 路由面（`pkg/server/server.go:32-37`）：`GET /healthz`、`GET /metrics`、`POST /v1/sessions`、
  `POST /v1/sessions/{id}/messages`、`DELETE /v1/sessions/{id}`、`GET /v1/sessions/{id}/stats`。
- 鉴权（`server.go:53-86`）：`Handler()` 包整个 mux，`Authorization: Bearer` 或
  `X-Lyon-Token`，`subtle.ConstantTimeCompare`，`/healthz` 豁免。**Connect handler 挂同一个
  mux 就自动被这套鉴权覆盖**。
- SSE（`pkg/server/sse.go`）：`streamPrompt` 独占读 `sess.Events()` 直到 `prompt_end` 或客户端
  消失；每事件 15s 写超时；`X-Accel-Buffering: no`。
- **断流即删会话**：一次性建会话和后续消息两条路径上，SSE 出错都 `store.Delete(id)`
  （`server.go:224-228, 264-267`）——因为 driver 可能卡在没人读的 buffer 上。
- `Store`（`pkg/server/store.go`）：`Create/Get/Delete/Len/Sessions/Reap/CloseAll/RunReaper`；
  **`Sessions()` 返回 `[]*agent.Session` 不带 id**（唯一调用方 `server.go:122` 的 /metrics
  聚合）——web 列会话必须先补 id 枚举。
- `cmd/lyond/main.go`：`-addr/-workspace/-token(LYON_TOKEN)/-insecure/-model/-effort/-max-iter/
  -max-sessions/-session-ttl/-shell-allow/-web-tools/-max-turns/-prewarm/-openai[-base]`；
  非 loopback 无 token 直接 exit(2)（`main.go:58-61`）；**全程未设 `cfg.Approve` →
  lyond 下所有需审批工具自动放行**（审批在 `pkg/tools/tool.go` adapter：`cfg.Approve != nil &&
  NeedsApproval()` 才问；`pkg/agent/session.go` 的 `wrapApprove` 服务 TUI 路径）。

### 3.4 落盘数据（面板数据源）

`$LYON_HOME`（=`LYON_HOME` 或 `~/.lyon`）下实际存在（本机已确认）：
`audit/<date>.jsonl`（`pkg/hooks/hooks.go`，Record 结构含 Time/Event/Session/Tool/Prompt/
Detail/IsError/Err/DurationMS/Approved/Usage…，保留 30 天）、`stats.json`
（`pkg/hooks/stats.go` 的 Stats 聚合）、`sessions/<hash16>.json` + `<hash>/` 归档
（`pkg/agent/snapshot.go:319-328`，`ListSnapshots → []SnapshotRef{Path,SavedAt,Messages,Title}`）、
`leaderboard.json`（`pkg/eval/leaderboard.go`，`Views(order) → []View{Class,OnDuty,Rounds,
ExamFails,Cards,Mean,Last*}`）、`routing.json`（`pkg/routing/routing.go:23-31` 的
`Entry{Class,Agent,Model,Confidence,Samples,MeanScore,Updated}`，`All()` 排序输出）、
`market.json`（`pkg/market/market.go`，historyCap 500，进程级 `Open()` 缓存）、
`memory/{team,personal}` + `<workspace>/.lyon/memory`（`pkg/memory`，BM25 索引）、
`exams/<class>.json`、`agents/<id>.json`（`pkg/eval`）。
另有 run dump（#3 已完成）：`$LYON_HOME/dumps/<YYYY-MM-DD>/<HHMMSS>-<rand12>.json`，
schema v1，`events[]` 带 `seq` 的完整时间线（`pkg/agent/dump.go:41-74,357-419`）——
**注意**：`dump.go:132-133` 有 `TEMP-WRITE-SKIP: tempWriteSkip = true` 的 bench 隔离开关，
当前分支磁盘上不会真的写 dump 文件（本机无 dumps/ 目录与之一致）。web 的 run 回放面板
依赖该开关移除。**待验证**：该开关的移除时机。

---

## 4. lyon web v1 proto 草案

目录布局（照参考仓库）：`proto/lyon/v1/*.proto`，`package lyon.v1`，
`go_package "lyon-agent/gen/lyon/v1;lyonv1"`。

### 4.1 `proto/lyon/v1/event.proto`

```proto
syntax = "proto3";

package lyon.v1;

option go_package = "lyon-agent/gen/lyon/v1;lyonv1";

import "google/protobuf/timestamp.proto";

// Usage mirrors agent.Usage (pkg/agent/events.go:35).
message Usage {
  int64 input = 1;
  int64 output = 2;
  int64 cache_read = 3;
  int64 cache_creation = 4;
}

// EventType mirrors the agent.EventType string constants 1:1 (pkg/agent/events.go:15-32).
enum EventType {
  EVENT_TYPE_UNSPECIFIED = 0;  // unknown/new type: read type_name instead
  EVENT_MESSAGE_START = 1;
  EVENT_TEXT_DELTA = 2;
  EVENT_THINKING_DELTA = 3;
  EVENT_TOOL_USE_START = 4;
  EVENT_TOOL_EXEC_START = 5;
  EVENT_TOOL_EXEC_OUTPUT = 6;
  EVENT_TOOL_EXEC_END = 7;
  EVENT_SERVER_TOOL_RESULT = 8;
  EVENT_COMPACTION = 9;
  EVENT_SUBAGENT_START = 10;
  EVENT_SUBAGENT_END = 11;
  EVENT_MEMORY_INJECTED = 12;
  EVENT_QUEUE_INJECTED = 13;
  EVENT_TURN_END = 14;
  EVENT_PROMPT_END = 15;
  EVENT_ERROR = 16;
  // TODO(#15): plan-tree events — EVENT_PLAN_UPDATED, EVENT_PLAN_NODE_STARTED,
  // EVENT_PLAN_NODE_FINISHED, EVENT_APPROVAL_REQUEST. Values 17-20 reserved.
}

// SessionEvent is agent.Event lifted onto the wire plus hub metadata
// (seq). Mirrors pkg/agent/events.go:44-87 field-for-field.
message SessionEvent {
  int64 seq = 1;         // hub-assigned, monotonic per session, starts at 1
  EventType type = 2;
  string type_name = 3;  // raw EventType string; authoritative when type == UNSPECIFIED

  // Stream deltas.
  string text = 10;
  string thinking = 11;

  // Tool identification (ToolID = API tool_use id; exec_id = harness-local sequence).
  string tool_id = 20;
  int64 exec_id = 21;      // Go uint64; non-negative in practice
  string tool_name = 22;
  string input_json = 23;  // Event.Input as a JSON string (protojson requires valid UTF-8;
                           // the hub sanitizes with strings.ToValidUTF8, same as SSE's json.Marshal)
  bool server = 24;        // server-side tool (web_search/fetch): no harness exec events follow
  bool sub = 25;           // happened inside a run_subagent call
  string sub_id = 26;      // fan-out task node id (#53); "" = legacy single call

  // Execution result.
  int64 duration_ms = 30;
  bool is_error = 31;
  string error = 32;

  // Turn boundaries.
  int32 turn = 40;
  string stop_reason = 41;
  Usage usage = 42;
  int32 iterations = 43;
  google.protobuf.Timestamp ts = 44;
  string detail = 45;      // compaction/subagent human-readable note

  map<string, string> attrs = 90;  // forward-compat sink (reference repo pattern)

  // TODO(#15): node_id / approval_state for plan-tree attribution.
  reserved 91 to 99;
  reserved "node_id", "approval_state";
}
```

### 4.2 `proto/lyon/v1/session.proto`

```proto
syntax = "proto3";

package lyon.v1;

option go_package = "lyon-agent/gen/lyon/v1;lyonv1";

import "google/protobuf/timestamp.proto";
import "lyon/v1/event.proto";

// SessionService: lifecycle + prompt injection + event streaming.
service SessionService {
  rpc CreateSession(CreateSessionRequest) returns (Session);
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);
  rpc GetSession(GetSessionRequest) returns (Session);
  rpc DeleteSession(DeleteSessionRequest) returns (DeleteSessionResponse);

  // SubmitPrompt runs one prompt now (maps to Session.Submit). Busy session
  // -> codes.FailedPrecondition (client should fall back to EnqueuePrompt).
  rpc SubmitPrompt(SubmitPromptRequest) returns (SubmitPromptResponse);

  // EnqueuePrompt parks a prompt into the session inbox (#2; maps to
  // Session.Enqueue). Idle session -> codes.FailedPrecondition
  // ("not busy; use SubmitPrompt").
  rpc EnqueuePrompt(EnqueuePromptRequest) returns (EnqueuePromptResponse);

  rpc Interrupt(InterruptRequest) returns (InterruptResponse);
  rpc GetStats(GetStatsRequest) returns (GetStatsResponse);
  rpc SetMode(SetModeRequest) returns (SetModeResponse);

  // WatchEvents is the server-streaming event feed: replay from the hub
  // ring then go live. Multiple concurrent watchers per session are fine.
  rpc WatchEvents(WatchEventsRequest) returns (stream SessionEvent);
}

message Session {
  string id = 1;
  string workspace = 2;                     // engine cfg.Workspace
  string model = 3;
  string title = 4;                         // Session.Title(); may be empty early
  string mode = 5;                          // "default" | "plan" | "bypass" (pkg/agent/mode.go)
  bool busy = 6;                            // needs new Session.Busy() accessor (gap)
  int32 queue_len = 7;                      // parked prompts; needs new accessor (gap)
  int32 turns = 8;
  google.protobuf.Timestamp created_at = 9;
  google.protobuf.Timestamp last_active = 10;
}

message CreateSessionRequest {
  string model = 1;   // "" = engine default
  string mode = 2;    // "" = default
}
message ListSessionsRequest {}              // live sessions in the store
message ListSessionsResponse { repeated Session sessions = 1; }
message GetSessionRequest { string session_id = 1; }
message DeleteSessionRequest { string session_id = 1; }
message DeleteSessionResponse {}

message SubmitPromptRequest {
  string session_id = 1;
  string prompt = 2;
  string model = 3;   // per-prompt override, "" = session default
}
message SubmitPromptResponse { bool started = 1; }

message EnqueuePromptRequest {
  string session_id = 1;
  string prompt = 2;
}
message EnqueuePromptResponse { int32 queue_len = 1; }

message InterruptRequest { string session_id = 1; }
message InterruptResponse { bool interrupted = 1; }

message GetStatsRequest { string session_id = 1; }
message TurnStat {
  int32 turn = 1;
  string stop_reason = 2;
  Usage usage = 3;
  int64 duration_ms = 4;
  google.protobuf.Timestamp at = 5;
}
message GetStatsResponse { repeated TurnStat turns = 1; }

message SetModeRequest {
  string session_id = 1;
  string mode = 2;    // "default" | "plan" | "bypass"
}
message SetModeResponse { string mode = 1; }

message WatchEventsRequest {
  string session_id = 1;
  // 0 = live only; -1 = replay everything still in the ring, then live;
  // n > 0 = replay ring entries with seq > n, then live. If the ring no
  // longer holds n+1 the server fails with codes.FailedPrecondition and the
  // client should re-anchor (snapshot list / run dump) and retry with -1.
  int64 from_seq = 2;
}
```

### 4.3 `proto/lyon/v1/file.proto`

```proto
syntax = "proto3";

package lyon.v1;

option go_package = "lyon-agent/gen/lyon/v1;lyonv1";

// FileService: read-only workspace preview. Every path is workspace-relative
// and passed through the same containment as the agent's read_file
// (confine(), pkg/tools/files.go:49) — absolute paths, "..", and symlink
// escapes are rejected fail-closed. v1 is read-only; write/upload RPCs are
// TODO(v2) and will ride the existing tool-approval gate.
service FileService {
  rpc ListDirectory(ListDirectoryRequest) returns (ListDirectoryResponse);
  rpc ReadTextFile(ReadTextFileRequest) returns (ReadTextFileResponse);
  rpc ReadBinaryFile(ReadBinaryFileRequest) returns (ReadBinaryFileResponse);
  // TODO(v2): stream download for large files.
}

enum FileKind {
  FILE_KIND_UNSPECIFIED = 0;
  FILE_KIND_DIR = 1;
  FILE_KIND_TEXT = 2;    // utf-8 text
  FILE_KIND_CODE = 3;    // code/config — rendered <pre> client-side
  FILE_KIND_MARKDOWN = 4;
  FILE_KIND_IMAGE = 5;   // png/jpg/jpeg/gif/webp
  FILE_KIND_PDF = 6;
  FILE_KIND_OTHER = 7;   // unsupported for preview; download only
}

message DirEntry {
  string name = 1;
  bool is_dir = 2;
  int64 size_bytes = 3;
  int64 mtime_unix_ms = 4;
  FileKind kind = 5;     // extension-allowlist detection server-side (mirror of DocPreview.pickRenderer)
}

message ListDirectoryRequest {
  string path = 1;       // workspace-relative; "" = workspace root
}
message ListDirectoryResponse {
  string path = 1;
  repeated DirEntry entries = 2;
  bool truncated = 3;    // entry-count cap hit
}

message ReadTextFileRequest {
  string path = 1;
  int32 offset_lines = 2;  // 1-based; 0 = from start (same semantics as tools.ReadFile)
  int32 limit_lines = 3;   // 0 = no limit
}
message ReadTextFileResponse {
  string path = 1;
  string content = 2;
  bool truncated = 3;      // hit the 512KB preview cap
  int32 total_lines = 4;
  int64 size_bytes = 5;    // full file size, even when truncated
  FileKind kind = 6;
}

message ReadBinaryFileRequest {
  string path = 1;
}
message ReadBinaryFileResponse {
  string path = 1;
  string content_type = 2;  // image/png, application/pdf, ...
  bytes data = 3;           // capped; truncated=true when the cap trimmed it
  bool truncated = 4;
  int64 size_bytes = 5;     // full file size
}
```

### 4.4 `proto/lyon/v1/dashboard.proto`

```proto
syntax = "proto3";

package lyon.v1;

option go_package = "lyon-agent/gen/lyon/v1;lyonv1";

import "google/protobuf/timestamp.proto";
import "lyon/v1/event.proto";

// DashboardService: read-only panels over $LYON_HOME on-disk data plus the
// process aggregates /metrics already computes.
service DashboardService {
  rpc GetOverview(GetOverviewRequest) returns (Overview);
  rpc ListRuns(ListRunsRequest) returns (ListRunsResponse);     // run dumps (#3)
  rpc GetRun(GetRunRequest) returns (RunDump);
  rpc ListSnapshots(ListSnapshotsRequest) returns (ListSnapshotsResponse); // saved sessions
  rpc ListAudit(ListAuditRequest) returns (ListAuditResponse);
}

message RoutingEntry {                       // pkg/routing Entry (routing.go:23)
  string class = 1;
  string agent = 2;
  string model = 3;
  double confidence = 4;
  int32 samples = 5;
  double mean_score = 6;
  google.protobuf.Timestamp updated = 7;
}

message LeaderboardView {                    // pkg/eval View (leaderboard.go:274)
  string class = 1;
  string on_duty = 2;                        // "" = open seat
  int32 rounds = 3;
  int32 exam_fails = 4;
  int32 cards = 5;
  double mean = 6;
  string last_agent = 7;
  double last_total = 8;
  bool last_passed = 9;
  string last_source = 10;                   // "exam" or "" (production)
}

message Overview {
  int32 sessions = 1;
  int64 uptime_seconds = 2;
  string model = 3;
  double cache_share = 4;
  repeated RoutingEntry routing = 5;
  repeated LeaderboardView leaderboard = 6;
}

message GetOverviewRequest {}

message RunRef {                             // run dump header (pkg/agent/dump.go:139)
  string run_id = 1;                         // "YYYYMMDD-HHMMSS-rand12"; also the file locator
  string workspace = 2;
  string model = 3;
  string prompt = 4;
  google.protobuf.Timestamp started_at = 5;
  int64 duration_ms = 6;
  bool failed = 7;                           // prompt_err != ""
}

message ListRunsRequest {
  string day = 1;    // "YYYY-MM-DD"; "" = today
  int32 limit = 2;   // 0 = default 50, newest first
}
message ListRunsResponse { repeated RunRef runs = 1; }

message DumpEvent {                          // dump.go:172 — Event embedded + seq
  int32 seq = 1;
  string type = 2;
  string text = 3;
  string thinking = 4;
  string tool_id = 5;
  int64 exec_id = 6;
  string tool_name = 7;
  string input_json = 8;
  bool server = 9;
  bool sub = 10;
  string sub_id = 11;
  int64 duration_ms = 12;
  bool is_error = 13;
  string error = 14;
  int32 turn = 15;
  string stop_reason = 16;
  Usage usage = 17;
  int32 iterations = 18;
  google.protobuf.Timestamp ts = 19;
  string detail = 20;
}

message RunDump {
  string run_id = 1;
  string workspace = 2;
  string model = 3;
  string prompt = 4;
  string first_prompt = 5;
  google.protobuf.Timestamp started_at = 6;
  google.protobuf.Timestamp ended_at = 7;
  int64 duration_ms = 8;
  int32 iterations = 9;
  string stop_reason = 10;
  string prompt_err = 11;
  Usage usage = 12;
  map<string, Usage> sub_usage = 13;
  repeated DumpEvent events = 14;            // the replayable timeline
  string history = 15;
}

message GetRunRequest { string run_id = 1; }

message SnapshotRef {
  string title = 1;
  int32 messages = 2;
  google.protobuf.Timestamp saved_at = 3;
  string path = 4;
}
message ListSnapshotsRequest {}
message ListSnapshotsResponse { repeated SnapshotRef snapshots = 1; }

message AuditRecord {                        // pkg/hooks Record subset (JSONL line)
  string time = 1;
  string event = 2;                          // prompt_start / approve_request / security_block / ...
  string session = 3;
  string tool = 4;
  string prompt = 5;
  string detail = 6;
  bool is_error = 7;
  string err = 8;
  int64 duration_ms = 9;
}
message ListAuditRequest {
  string day = 1;    // "YYYY-MM-DD"; "" = today
  int32 tail = 2;    // 0 = default 200, newest last
}
message ListAuditResponse { repeated AuditRecord records = 1; }
```

### 4.5 `proto/lyon/v1/approval.proto`（TODO：审批接线前不实现）

```proto
syntax = "proto3";

package lyon.v1;

option go_package = "lyon-agent/gen/lyon/v1;lyonv1";

// ApprovalService — TODO(wire): lyond currently never sets cfg.Approve, so
// every NeedsApproval tool auto-approves (cmd/lyond/main.go; the adapter gate
// is pkg/tools/tool.go). When lyond grows an -approve flag, wire
// tools.ApproveFunc to this service: the approvee parks until Decide or
// timeout (returning false, matching TUI behavior).
service ApprovalService {
  rpc WatchPending(WatchPendingRequest) returns (stream ApprovalRequest);
  rpc Decide(DecideRequest) returns (ApprovalDecision);
}

message WatchPendingRequest {}
message ApprovalRequest {
  string session_id = 1;
  int64 exec_id = 2;
  string tool_name = 3;
  string input_json = 4;
  int64 requested_at_ms = 5;
}
message DecideRequest {
  string session_id = 1;
  int64 exec_id = 2;
  bool approve = 3;
  bool remember = 4;   // TODO(v2): feed policy.json instead of one-shot
}
message ApprovalDecision {
  string session_id = 1;
  int64 exec_id = 2;
  bool approve = 3;
}
```

### 4.6 `proto/lyon/v1/plan.proto`（#15 占位，全部 TODO(#15)）

```proto
syntax = "proto3";

package lyon.v1;

option go_package = "lyon-agent/gen/lyon/v1;lyonv1";

// PlanService — TODO(#15): plan horizontal tree, node-level approval/edit,
// node detail, concurrent/serial step growth, live prompt+output per node,
// in-progress guidance injection. All methods return
// codes.Unimplemented until #15 lands; message shapes are frozen now so the
// web client can be built against them and field numbers never churn.
service PlanService {
  rpc GetPlan(GetPlanRequest) returns (PlanTree);
  rpc WatchPlan(WatchPlanRequest) returns (stream PlanUpdate);
  rpc UpdateNode(UpdateNodeRequest) returns (PlanNode);    // edit content pre-run
  rpc ApproveNode(ApproveNodeRequest) returns (PlanNode);  // node-level approval
  rpc ExecutePlan(ExecutePlanRequest) returns (PlanTree);
  rpc GuideNode(GuideNodeRequest) returns (PlanNode);      // guidance injection into a running node
}

enum NodeStatus {
  NODE_STATUS_UNSPECIFIED = 0;
  NODE_STATUS_PENDING = 1;
  NODE_STATUS_READY = 2;
  NODE_STATUS_RUNNING = 3;
  NODE_STATUS_DONE = 4;
  NODE_STATUS_FAILED = 5;
}

enum ApprovalState {
  APPROVAL_UNSPECIFIED = 0;
  APPROVAL_NOT_REQUIRED = 1;
  APPROVAL_REQUIRED = 2;
  APPROVAL_APPROVED = 3;
  APPROVAL_REJECTED = 4;
}

message PlanNode {
  string node_id = 1;
  string content = 2;
  NodeStatus status = 3;
  ApprovalState approval = 4;
  repeated string child_ids = 5;   // children reference nodes by id (flat map on the client)
  int32 order = 6;
  string prompt = 7;               // the node's run prompt (editable pre-run)
  repeated ExecStep steps = 8;     // grows while running — concurrent fan-out (#53/#15)
  string live_output_tail = 9;     // latest tool_exec_output for this node, coalesced
}

message ExecStep {
  string step_id = 1;
  string summary = 2;
  NodeStatus status = 3;
  int64 exec_id = 4;
  bool is_error = 5;
  string error = 6;
}

message PlanTree {
  string session_id = 1;
  repeated PlanNode nodes = 2;
  int64 revision = 3;
}

message PlanUpdate {
  string session_id = 1;
  int64 revision = 2;
  PlanNode node = 3;
}

message GetPlanRequest { string session_id = 1; }
message WatchPlanRequest { string session_id = 1; }
message UpdateNodeRequest {
  string session_id = 1;
  string node_id = 2;
  string content = 3;
}
message ApproveNodeRequest {
  string session_id = 1;
  string node_id = 2;
  bool approve = 3;
}
message ExecutePlanRequest { string session_id = 1; }
message GuideNodeRequest {
  string session_id = 1;
  string node_id = 2;
  string guidance = 3;
}
```

### 4.7 鉴权（v1 静态 token；proto 预留）

- v1 不建 `auth.proto` 服务：web 前端把 lyond 的 bearer token 存 localStorage，
  connect-es transport 的 interceptor 统一注 `Authorization: Bearer <token>`
  （抄参考仓库 A 的 `web/src/api.ts:45-55` 的 transport 装配）。
- Go 侧**不需要新的鉴权代码路径**：Connect handler 挂在 `server.mux` 上，
  `Handler()` 的 token 包裹（`server.go:53-71`）对所有路径（含 streaming POST）统一生效。
  但若未来把 token 校验下沉成 connect 拦截器（比如要按 RPC 分级），**必须同时实现
  `WrapUnary` 与 `WrapStreamingHandler`**（参考仓库 `interceptor.go:120` 的血泪注释）。
- TODO(multi-user)：登录/会话 token/角色分级等消息留到需要时再加 `auth.proto`，
  沿用参考仓库的 AuthService 形态。

### 4.8 buf 配置与生成命令（lyon 版）

`buf.yaml`（仓库根）：

```yaml
version: v2
modules:
  - path: proto
lint:
  use:
    - STANDARD
breaking:
  use:
    - FILE
```

`buf.gen.yaml`（仓库根；TS 侧插件在 Phase 3 `web/` 脚手架就绪后再放开注释）：

```yaml
version: v2
inputs:
  - directory: proto
plugins:
  - local: protoc-gen-go
    out: gen/go
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen/go
    opt: paths=source_relative
  # Phase 3 enable (requires web/node_modules with exact-pinned versions):
  # - local: ["node", "web/node_modules/@bufbuild/protoc-gen-es/bin/protoc-gen-es"]
  #   out: web/src/gen
  #   opt: target=ts
  # - local: ["node", "web/node_modules/@connectrpc/protoc-gen-connect-es/bin/protoc-gen-connect-es"]
  #   out: web/src/gen
  #   opt: target=ts
```

一次性安装与生成（Windows+MinGW bash 可直接跑）：

```bash
go get connectrpc.com/connect google.golang.org/protobuf
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
go install github.com/bufbuild/buf/cmd/buf@latest   # 若参考仓库记录的 go1.13.15 路径可用亦可
buf lint && buf generate && go build ./...
```

`go.mod` 当前无任何 protobuf/connect 依赖（实读确认），Phase 0 引入。

---

## 5. 服务端架构：Event Hub 与 SSE 并存

### 5.1 为什么必须有 Hub

`Session.Events()` 是**单消费者**有界 chan（`session.go:236`）。今天消费者是 SSE
`streamPrompt`（`sse.go`）。web 要求同一会话同时有：SSE 客户端（兼容面）+ N 个浏览器
标签页的 Connect 流 + （未来）回放缓冲。多消费者直接各读 chan 会互相偷事件——所以新增
**per-session Event Hub**，成为唯一消费者。

### 5.2 Hub 设计（`pkg/server/hub.go`，新增）

```go
// sketch — implementation lives in pkg/server/hub.go
type hub struct {
    sess   *agent.Session
    seq    int64                    // atomic; stamped onto every event
    ring   []SessionEvent           // bounded replay buffer (recommend 4096)
    mu     sync.Mutex               // ring + subs
    subs   map[int64]*subscriber    // per-watcher
    nextID int64
}

type subscriber struct {
    ch     chan agent.Event  // bounded (recommend 1024); never blocks the drain loop
    closed bool
}

func (h *hub) run() {
    for ev := range h.sess.Events() {        // sole consumer
        h.seq++
        // sanitize for protojson: strings.ToValidUTF8 on text/thinking/detail/error/input
        h.appendRing(ev)                      // bounded; evict oldest
        h.fanout(ev)                          // non-blocking send; full sub -> kick(ResourceExhausted)
    }
}
```

关键语义：

- **seq**：每会话从 1 起严格递增，写进 `SessionEvent.seq`。客户端断线重连带
  `from_seq`，hub 先回放 ring 再转 live；ring 已覆盖不到该点则
  `codes.FailedPrecondition`，客户端改用 `ListSnapshots`/`GetRun` 重新锚定。
- **背压语义变化（有意为之）**：今天 SSE 慢客户端会阻塞 driver（天然背压，`sse.go` 注释），
  Hub 落地后 driver 永不因观察者阻塞——慢订阅者被踢。这符合并发硬要求：一个卡死的浏览器
  标签页不允许拖住会话。SSE 侧仍保留每事件 15s 写超时兜底。
- **慢订阅者处理**：subscriber chan 满 → 从 subs 移除并关闭其 chan，Connect 流以
  `codes.ResourceExhausted`（"event subscriber fell behind; reconnect with from_seq"）结束；
  SSE 侧等价于现行为的断流。
- **UTF-8 卫生**：tool 输出可能不是合法 UTF-8（SSE 的 `json.Marshal` 今天会把非法字节替成
  U+FFFD）。protojson 对非法 UTF-8 直接报错（参考仓库踩过 rune-slicing 教训），所以 hub
  在入 ring 前对 `text/thinking/detail/error/input_json` 统一 `strings.ToValidUTF8(x, "\uFFFD")`。

### 5.3 生命周期接线

- `Store.Create`（`store.go:48`）：建 session 后启动 `go hub.run()`，hub 挂在 store entry 上。
- `Store.Delete` / `Reap` / `CloseAll`（`store.go:76,111,132`）：先关 hub（关 subs、停 run），
  再走现有 `sess.Close()` 路径。`Close` 会等 pending dump 写完（`dump.go:267` 的
  `dumpPending`），保持不变。
- `streamPrompt`（`sse.go`）从直读 `sess.Events()` 改为订阅 hub：帧内容一字不改
  （`sseFrame` 的 JSON 就是 agent.Event 的字段），TestSSE 契约不动。

### 5.4 SSE 关系：并存

- `/v1/sessions/*` + SSE 原样保留（README 公开契约 + `bench/load_test.go` TestSSE 背书）。
  迁移仅发生在“事件从哪读”这一层（hub），协议面零变化。
- Connect 流是 web 专属新增。两者在 hub 层共享同一条事件源，不会互偷。
- “断流即删会话”（`server.go:224-228,264-267`）v1 保持原样（web 用独立的
  `DeleteSession` RPC 管理生命周期；web 断流不触发删除，因为 web 流不是 driver 的唯一
  读者——hub 永远在读，这个删除理由在 hub 时代对 web 路径已不成立，但对 SSE 兼容路径
  保留）。

### 5.5 lyond 接入点

- `pkg/server/server.go`：`New()` 里追加
  `s.mux.Handle(lyonv1connect.NewSessionServiceHandler(...))`、`NewFileServiceHandler`、
  `NewDashboardServiceHandler`、`NewApprovalServiceHandler`、`NewPlanServiceHandler`
  （照参考仓库 `routes.go:43-56` 的挂法）。`Handler()` 的 token 包裹自动覆盖。
- `pkg/server/store.go`：新增 `SessionsWithIDs() map[string]*agent.Session`（或改
  `Sessions()` 返回带 id 的结构——当前唯一调用方 `server.go:122` 一并改）。
- `pkg/agent/session.go`：新增 `Busy() bool`、`QueueLen() int` 两个只读 accessor。
- `cmd/lyond/main.go`：flag 面不动；可选加 `-web-dist`（SPA 静态目录）与
  `-approve`（接线 ApprovalService 时）。SPA 建议 `go:embed` 进二进制——ROADMAP 的
  单二进制零依赖哲学；若担心二进制体积则用 `-web-dist` 指目录。**待验证**：embed 体积
  与是否需要 build tag 隔离 TUI-only 构建。
- 若反代 nginx：抄参考仓库教训——每个 service 一个 `location /lyon.v1.XxxService/`，
  流式 location 加 `proxy_buffering off`。开发期用 vite proxy 同源转发可完全绕开 CORS
  与 nginx 问题（参考仓库同款做法）。

---

## 6. web 文件预览方案

1. **端点形态**（§4.3）：`ListDirectory`（JSON，含 `kind` + `truncated`）、
   `ReadTextFile`（JSON，offset/limit 行分页对齐 `tools.ReadFile` 的 1-based 语义，
   `files.go:178-192`；512KB cap + `truncated` 标记，抄参考仓库
   `maxPreviewBytes`）、`ReadBinaryFile`（bytes，图片/PDF，cap 建议 16MB，
   超限 `truncated=true`——**待验证**上限值）。
2. **路径安全**：所有 path 先过 `confine()`（`pkg/tools/files.go:49`）。该函数当前是
   unexported——Phase 3 在 `pkg/tools` 加一个导出包装（如 `func Confine(root, p string)
   (string, error)`），不改内部实现。绝对路径、`..`、symlink 逃逸（budget 32、fail-closed）
   全部拒绝；判定基于真实路径（`files.go:79-82` 的 `filepath.Rel`），比参考仓库的字符串
   前缀检查强一个量级。**v1 只读**；写/上传若做 v2，走既有 `NeedsApproval` 审批门
   （`pkg/tools/tool.go` adapter）。
3. **类型识别**：服务端按扩展名白名单给 `FileKind`（image/pdf/markdown/code/text/other，
   镜像 `DocPreview.pickRenderer` 的决策表 + `TEXT_EXTS` 集合）；前端可二次兜底。
   `.svg` **待验证**——建议 v1 按 download-only 处理（防内联 XSS），不进 image 白名单。
4. **前端渲染**（Phase 3）：code/text → `<pre>`（原样，不走 markdown）；`.md` →
   react-markdown（GFM）；image → `fetch` 拿 bytes 转 blob URL（**不要把 token 放 URL**，
   参考仓库 SharedFilePage 的 token-in-link 模式在 lyon 不适用——lyon 全路由都要 token）；
   pdf → pdfjs（体积大，**待验证**是否 v1 就带）。
5. **边界**：目录列表设 entry 上限（建议 2000）并给 `truncated`；二进制只对
   image/pdf 白名单开放，其余 `unsupported` 提示 download（v1 甚至可以不提供 download，
   因为 workspace 就在本地机器上）。

---

## 7. 数据源清单（每个面板 → lyon 来源）

| # | 面板 | 数据来源 | 现状 |
|---|---|---|---|
| 1 | 会话列表（活跃） | `Store`（`pkg/server/store.go`） | 需补 id 枚举 + `Busy()`/`QueueLen()` accessor |
| 2 | 会话列表（历史/恢复） | `agent.ListSnapshots(workspace)` → `$LYON_HOME/sessions/`（`snapshot.go:319-328`） | 可用（`DashboardService.ListSnapshots`） |
| 3 | 实时事件流（文本/思考/工具） | `Session.Events()`（`session.go:236`）→ hub → `WatchEvents`；16 种事件见 `events.go:15-32` | 需 Hub（§5） |
| 4 | 工具实时输出（#11） | `EventToolExecOutput`（`events.go:21`，源侧 40ms 合流） | 随 #3 |
| 5 | 排队注入（#2） | `EnqueuePrompt` → `inbox` → `EventQueueInjected`（`session.go:281,294`） | 需 `QueueLen()` |
| 6 | 逐轮用量 | `GET /v1/sessions/{id}/stats`（`server.go:278`）/ `Session.Stats()` | 已有 HTTP；proto 化为 `GetStats` |
| 7 | 进程指标 | `GET /metrics`（`server.go:113-173`：turn 聚合 + cache share + 工具延迟分位） | 已有；proto 化进 `Overview` |
| 8 | 审计日志 | `$LYON_HOME/audit/<date>.jsonl`（`pkg/hooks`，30 天滚动） | 需 server 读文件 → `ListAudit` |
| 9 | hooks 统计 | `$LYON_HOME/stats.json`（`pkg/hooks/stats.go`） | 随 #8 或 Overview 附带 |
| 10 | 路由表 | `$LYON_HOME/routing.json`，`routing.Open().All()`（`routing.go:42,114`） | `Overview.routing` |
| 11 | 进化榜 | `$LYON_HOME/leaderboard.json`，`Views(order)`（`leaderboard.go:292`） | `Overview.leaderboard` |
| 12 | 任务市场 | `$LYON_HOME/market.json`（`market.go`，进程级缓存） | TODO(v1.1) |
| 13 | run dump 回放（#3） | `$LYON_HOME/dumps/<day>/*.json`（`dump.go`，events[] 带 seq） | 功能在，但 `tempWriteSkip=true` 短路了写盘——面板依赖开关移除（**待验证**） |
| 14 | 文件浏览/预览 | workspace 根，`confine()` 约束 | 需导出 `Confine` + FileService |
| 15 | 模式切换 | `Session.SetMode`（`mode.go:47`） | proto 化 |
| 16 | 记忆面板 | `$LYON_HOME/memory/{team,personal}` + `<workspace>/.lyon/memory`（`pkg/memory`） | TODO(v1.1)，实现前需补读 memory 搜索 API（**待验证**接口面） |
| 17 | 考卷/候选 agent | `$LYON_HOME/exams/`、`agents/`（`pkg/eval`） | TODO(v1.1) |
| 18 | 工具审批 | `tools.ApproveFunc` + `wrapApprove`（`session.go`）；lyond 未接线 | TODO（`approval.proto` 已预留） |
| 19 | plan 树（#15） | #15 落地时的会话内状态 | `plan.proto` 预留，服务返回 Unimplemented |

---

## 8. 分阶段实施步骤

每步验收命令均为 MinGW bash 可直接执行；实现期注释用英文、临时文件进 `tmp/`、不碰 git。

### Phase 0 — 契约与工具链（半天）

- 新增：`buf.yaml`、`buf.gen.yaml`、`proto/lyon/v1/{event,session,file,dashboard,approval,plan}.proto`
  （§4 全文）；`go.mod`/`go.sum` 增 `connectrpc.com/connect`、`google.golang.org/protobuf`；
  生成并提交 `gen/go/lyon/v1/**`。
- 验收：

```bash
buf lint && buf generate && go build ./... && go vet ./...
```

### Phase 1 — Event Hub + SessionService + 鉴权冒烟（1–2 天）

- 新增：`pkg/server/hub.go`（§5.2）、`pkg/server/connect_session.go`（handler，错误映射：
  `ErrPromptInProgress`→`FailedPrecondition`、`ErrNoPromptRunning`→`FailedPrecondition`、
  会话不存在→`NotFound`、429 准入→`ResourceExhausted`）。
- 修改：`pkg/server/store.go`（Create/Delete/Reap/CloseAll 接 hub 生命周期；
  `SessionsWithIDs`）、`pkg/server/sse.go`（`streamPrompt` 改订阅 hub，帧字节不变）、
  `pkg/agent/session.go`（`Busy()`/`QueueLen()` 只读 accessor）、`pkg/server/server.go`
  （`New()` 挂 Connect handler）。
- 验收：

```bash
go test ./pkg/server/... ./pkg/agent/...
go test ./bench -run 'TestSSE' -v      # 50 并发零丢失 + TTFD p95 门禁必须原样通过（hub 迁移回归）
# 手工冒烟（token 用 lyond -token 设的值）：
curl -s -H "Authorization: Bearer $T" -d '{"prompt":"ping"}' \
  http://127.0.0.1:8080/lyon.v1.SessionService/SubmitPrompt
# 无 token 应 401：
curl -s -o /dev/null -w '%{http_code}\n' -d '{}' http://127.0.0.1:8080/lyon.v1.SessionService/ListSessions
```

### Phase 2 — FileService + Confine 导出（1 天）

- 新增：`pkg/server/connect_file.go`（三端点；cap 常量：文本 512KB、二进制 16MB、目录 2000 项；
  `kind` 扩展名表）。
- 修改：`pkg/tools/files.go`（导出 `Confine` 包装，内部实现不动）；必要时复用
  `splitFileLines` 做行分页（`files.go:201`，导出或在 server 侧重写同语义）。
- 验收：

```bash
go test ./pkg/tools/... ./pkg/server/...
# 路径攻击用例必须覆盖：../ 逃逸、绝对路径、NUL、symlink 指向 workspace 外（stub FS 测试模式，
# files.go:15-24 的可注入变量就是为此设计的）
```

### Phase 3 — web 脚手架 + connect-es（2–3 天）

- 新增：`web/`（Vite + React + TS；`web/src/api.ts` 照参考仓库 transport/interceptor 范式；
  `web/src/gen/` 由 buf 生成——放开 `buf.gen.yaml` 里 TS 插件注释，**exact 版本 pin**，
  connect-es 生成器与运行时严格同版，参考仓库锁 1.7.0 的教训）。
- 页面 v1：会话列表（活跃+历史）、对话视图（`WatchEvents` 流式渲染：文本/思考/工具卡片/
  队列注入提示）、文件树+预览（§6）。
- 验收：

```bash
npm -C web ci && npm -C web run build
npm -C web run dev   # vite proxy 指 lyond，同源免 CORS；浏览器手工过一遍提交→流式→排队→预览
```

### Phase 4 — Dashboard 面板（1 天）

- 新增：`pkg/server/connect_dashboard.go`（`GetOverview` 复用 `handleMetrics` 的聚合逻辑；
  `ListRuns/GetRun` 读 dumps 目录；`ListAudit` 读 audit JSONL 尾部；`ListSnapshots` 调
  `agent.ListSnapshots`）。
- 前端：Overview（routing + leaderboard + 指标）、audit 尾巴、run 回放（events[] 时间线）。
- 依赖：run 面板需要 `dump.go` 的 `tempWriteSkip` 开关移除（**待验证**时机；未移除前面板
  显式提示“dumps 未启用”）。
- 验收：

```bash
go test ./pkg/server/...
curl -s -H "Authorization: Bearer $T" -d '{}' \
  http://127.0.0.1:8080/lyon.v1.DashboardService/GetOverview | head -c 400
```

### Phase 5 — 审批与 plan 占位收口（半天）

- `ApprovalService`：handler 返回 `Unimplemented`（或接 `-approve` flag 提前打通，
  与 lyond 的 `cfg.Approve` 接线一起做）；`PlanService`：全部 `Unimplemented`。
- 验收：

```bash
buf lint && go build ./... && go test ./pkg/server/...
# 前端对 Unimplemented 的降级提示不崩
```

---

## 9. 待验证清单

1. **buf 本机可用性**：`command -v buf` 在 PATH 找不到；参考仓库 CLAUDE.md 记录
   `$(go env GOPATH)/bin/buf.exe`，存在性未验证。没有则
   `go install github.com/bufbuild/buf/cmd/buf@latest`。
2. **run dump 写盘开关**：`pkg/agent/dump.go:132-133` `TEMP-WRITE-SKIP: tempWriteSkip = true`
   当前短路写盘（本机无 `dumps/` 目录）；`DashboardService` run 回放面板依赖其移除，时机未定。
3. **hub 迁移对 TestSSE 的影响**：结构性预期零丢失/TTFD 不退化（hub 只多一次 chan 转发），
   但 p95 门禁必须实测（`go test ./bench -run TestSSE`）。
4. **connect-es 版本配对**：生成器与运行时必须 exact 同版（参考仓库锁 1.7.0）；lyon web
   选型时再定具体版本。
5. **SPA 托管**：`go:embed` 进 lyond（单二进制哲学）vs `-web-dist` 目录 flag；embed 体积、
   TUI-only 构建是否要 build tag 隔离。
6. **二进制预览 cap**：图片/PDF 上限建议 16MB，未实测大文件行为。
7. **SVG**：建议 v1 按 download-only（防内联 XSS），不进 image 白名单；是否提供直接预览未定。
8. **memory 面板**（v1.1）：`pkg/memory` 对外搜索/读取 API 面需补读后再定 RPC 形状。
9. **Enqueue 客户端契约**：idle 时 `ErrNoPromptRunning` → `FailedPrecondition`，前端自动
   回退 `SubmitPrompt` 的 UX 细节。
10. **`title.go` 何时产出标题**：`Title()` 早期可能为空（`pkg/agent/title.go:42`），
    列表面板需要空标题兜底（显示 prompt 首行）。
