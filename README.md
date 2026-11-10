# lyon-agent

**自己打分、考试、淘汰、越用越强的自进化 AI 编程智能体** —— 本地优先，
一个 Go 守护进程 `lyond` + 浏览器里的 Web 工作台。

[![Go 1.25](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![CI](https://github.com/LyonLen/lyon-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/LyonLen/lyon-agent/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

一个进程内跑完整的 agent 循环（流式输出、工具调用、上下文压缩、子代理并发扇出），
经 connectRPC/SSE 暴露给 React Web 工作台；自带压力验收套件，性能阈值不达标
直接 FAIL。外圈是一整套**演化机制**——生产盲评入榜、每 10 轮闭卷考核（连挂撤职）、
任务分类与证据路由、错因归纳回灌——账本全部落盘、重启原样恢复，Web 面板直接可看。

> 📘 **文档站**：浏览器打开 [`docs/index.html`](docs/index.html)（产品介绍）与
> [`docs/manual.html`](docs/manual.html)（使用手册：上手 → 界面 → 演化机制 →
> 配置参考 → FAQ），纯本地静态页、无网络资源，双击即可读。
> **English**: [`README.en.md`](README.en.md) ·
> [`docs/index.en.html`](docs/index.en.html) · [`docs/manual.en.html`](docs/manual.en.html)。
>
> 终端 TUI（`lyon chat`）已于 2026-11 整体移除——界面以 Web 为准，本文档即新版本。

## 特性

- **Web 工作台**：平铺窗口（tiling）布局——聊天、计划、步骤审计、文件、预览、
  终端、思维导图、记忆、设置、演化面板都是可拆分的 pane，布局随会话保存；
  另有 Dashboard 总览视图（聚合指标 · 路由与榜单 · 运行回放 · 审计尾）。
- **会话侧栏**：按工作区分组、分组点击折叠、git 仓库徽章（一眼区分代码库）、
  两次点击确认删除、running/queued 状态徽章；新会话可选工作区（本地目录 /
  直接 git clone）。
- **流式转录 + 工具审计**：markdown 渲染（代码块一键复制）、thinking 折叠、
  工具输出按类别着色、步骤面板实时审计每步工具的名称/耗时/状态、
  运行中命令的输出实时流入。
- **审批卡片**：shell/写文件弹 y/n/a 卡片（`a` = 本会话该工具全部放行）；
  plan 拦截以 mode_block 卡片呈现，fail closed。
- **产出文件卡片**：每次 run 结束差分出「产出文件 · N」卡片，行点击直达
  预览 pane（可切 unified diff 视图）。
- **思维导图 pane**：对话实时长出的理解图，与聊天平级；**图上编辑会回流成
  事件推进对话并触发 agent 自动追问**——第二条输入通道。
- **并发多会话**：忙时排队、间隙插队（不丢问题）、一键打断、队列徽章；
  会话独立持久化，重启续聊。
- **四种执行模式**：`default` 人工审批 · `plan` 只读探索 · `bypass` 自动放行 ·
  `mindmap` 图交互——左下模式坞点选或 `shift+tab` 循环。
- **21 条斜杠命令**：输入 `/` 唤出分类候选面板——`/rollback` 检查点、`/undo`
  撤销本轮文件变更、`/model` 模型卡、`/stats` 用量、`/memory` 记忆、
  `/evolve` `/exam` `/route` `/flow` 演化面板……（全表见「界面导览」）。
- **三层持久记忆**：team / personal / project 单事实 Markdown，本地 BM25
  （CJK 二元组）检索，热层随首条 prompt 注入，`/remember` 一键蒸馏入库；
  web 记忆面板带检索验证与标签过滤。
- **hooks 审计与安全门**：全生命周期 jsonl 审计 + 聚合统计 + `policy.json`
  正则拦截 + 外部阻塞钩子（`LYON_AUDIT`/`LYON_HOOKS` 可关）。
- **演化机制**：盲评入榜 → 每 10 轮闭卷考核（连挂 2 次撤职）→ 任务分类推荐
  路由——全部由 `lyond` 在 prompt_end 后台自动执行（`LYON_EVOLVE` 总闸），
  Web 演化面板与 Dashboard 只读展示，账本落盘、重启恢复。
- **OpenAI 兼容网关**：HTTP 层协议翻译（`-openai-base` 任意端点），工具系统、
  压缩、子代理零改动复用。
- **并发子代理**：`run_subagent(s)` 只读子代理，一次扇出 2–6 个独立子任务
  **并行**执行，各用该任务类有证据的模型。
- **MCP 接入**：stdio 与 streamable HTTP 双传输，`~/.lyon/mcp.json` 持久配置，
  工具映射为 `mcp_<srv>_<tool>`。
- **性能验收硬门限**：进程内 mock Anthropic API + bench 套件（引擎 p95 开销
  ≤ 25ms、稳态缓存命中 ≥ 95%、SSE 零丢事件），不达标直接 FAIL。

## 安装

需要 Go ≥ 1.25（`go.mod` 声明 1.25.0）与 Node ≥ 18。从源码构建：

```bash
git clone https://github.com/LyonLen/lyon-agent.git
cd lyon-agent

# 1) 守护进程
go build -o bin/lyond ./cmd/lyond        # Windows 产出 bin\lyond.exe

# 2) Web 前端
cd web && npm install && cd ..
```

配置凭证（勿提交）：

```bash
export ANTHROPIC_AUTH_TOKEN=...          # 模型 API key
export ANTHROPIC_MODEL=...               # 可选，覆盖默认模型
export LYON_TOKEN=...                    # 可选，Web/RPC 鉴权 bearer
```

三平台预编译产物（+ checksum）将随 GitHub Release 分发——现阶段请从源码构建。

## 快速开始

```bash
# 终端 1：启动守护进程（默认只绑回环 127.0.0.1:8080）
./bin/lyond -workspace /path/to/your/repo -prewarm

# 终端 2：启动 Web（Vite 开发服，默认 http://localhost:5183）
cd web && npm run dev
```

浏览器打开 `http://localhost:5183`：

1. 左上 **+ new session** → 选择工作区（本地目录，或填 git URL 直接 clone）；
2. 底部输入框开聊——流式回答、工具调用审计、审批卡片即时上屏；
3. 输入 `/` 看命令面板，`shift+tab` 切模式，右下角是上下文油量表。

生产部署用 `npm run build` 产出静态资源（`tsc --noEmit && vite build`），
由任意静态服务器托管，API 指向 `lyond` 即可。

> 常用启动项：`-addr` 监听地址 · `-workspace` 工具/文件禁闭根 ·
> `-token` 鉴权（绑非回环地址时必须，或显式 `-insecure`）· `-approve`
> 严格审批（每个写操作都问；默认 path-aware，只有越出工作区的 shell 才问）·
> `-model` `-effort` · `-max-sessions` `-session-ttl` 准入与驱逐 ·
> `-max-turns` 全局在飞 turn 硬顶 · `-prewarm` 预热 prompt 缓存 ·
> `-openai` `-openai-base` OpenAI 网关。完整表见手册「配置参考」。

## 界面导览

### 会话侧栏（左）

- **按工作区分组**：组头 = 工作区名，点击**折叠/展开**该组全部会话；
  git 仓库组头带绿色 **git** 徽章（一次 `os.Stat`，区分代码库与普通目录）。
- **会话行**：标题 · 模式 · `running`/`queued N` 徽章 · 最后活跃时间；
  行尾 `×` **两次点击确认**删除（点一次变「确认?」，3 秒自愈）。
- **+ new session**：工作区选择器——最近用过的目录历史、本地浏览、
  git URL clone（`https:// / http:// / git@ / ssh:// / file://` 或本地路径）。
- **saved**：历史会话快照（TUI 时代写入的存档），点选即恢复回对话。

### 聊天区（中）

- 流式转录：markdown 渲染、代码块悬停出 **copy** 按钮、thinking 块折叠、
  工具行按类别着色（shell 绿 · 子代理蓝 · 写入黄 · web/MCP 品红 · 读取青）。
- **审批卡片**：`y` 放行 · `n` 拒绝 · `a` 本会话全部放行（方向键/Tab 走卡片）。
- **产出文件卡片**（`files_changed`）：本 run 新改的文件逐行列出，
  点行开预览，变更行切 unified diff。
- 输入框：`/` 斜杠面板（分类：session/model/panel/misc）、`@` 文件补全、
  忙时发送即排队（`queued N`）。
- **底部状态条**：左下模式坞（点开选 default/plan/bypass/mindmap），
  右下 **HP 条上下文油量表**（`ctx% · used/window`，绿→黄→红分色带）+ 会话花费。

### 面板工作区（平铺窗格）

聊天两侧可拆分出的 pane（`/` 命令或界面入口打开）：

| Pane | 打开方式 | 内容 |
|---|---|---|
| 计划 plan | 计划卡片角标 | todo 计划，可回跳对话 |
| 步骤 steps | `/steps` | 工具执行审计：状态 ⟳/✓/✗、耗时、可读参数、实时输出尾 |
| 文件 files | 导航 | 目录树 → 点击进预览 |
| 预览 preview | 文件卡片/链接 | 文本/代码高亮、markdown、xlsx/pptx 等预览、diff 视图 |
| 终端 terminal | 界面入口 | 会话内 xterm 终端 |
| 思维导图 mindmap | `/mindmap` 或 mindmap 模式 | 实时理解图，节点编辑回流推进对话 |
| 记忆 memory | `/memory` | 三层记忆浏览、BM25 检索验证（分数/命中词/段落）、标签过滤 |
| 设置 settings | `/settings` | 引擎模型与子代理模型（可切换）、模型知识表（网关 `/models` × 内置知识库）、环境变量清单 |
| 演化 evolve | `/evolve` `/exam` `/route` `/flow` `/agents` `/market` | 只读面板：候选谱系 · 在岗席位 · 考试时钟与题库 · 路由表与榜单 · 冻结流程 · 工时市场 |

布局随会话保存：切会话即恢复该会话自己的窗格树与打开的标签。

### Dashboard（总览视图）

侧栏导航切换的独立视图，四块并发加载（总览 10s 轮询）：
**总览**（聚合指标 · 路由推荐 · 在岗榜单）· **运行列表 → 回放时间线** ·
**审计尾**（hooks jsonl）· **saved 快照列表**。

### 斜杠命令全表

输入 `/` 唤出分类候选面板，↑/↓ 选择、enter 执行：

| 类别 | 命令 | 作用 |
|---|---|---|
| session | `/new` | 新会话 |
| | `/resume [N]` | 弹出存档选择列表恢复（`N` 指定槽位） |
| | `/rollback [save [text] \| N]` | 列出检查点 / 标记 / 恢复第 N 个 |
| | `/undo` | 撤销上一轮 run 的文件变更（git side-ref） |
| | `/clear` | 清空转录视图（历史保留） |
| model | `/mode` | 循环 default → plan → bypass |
| | `/model` | 当前会话生效模型卡 |
| | `/think` | 展开/折叠全部 thinking 块 |
| panel | `/steps [open\|close\|width N]` | 步骤审计面板 |
| | `/mindmap`（`/mm`） | 思维导图 pane |
| | `/memory` | 记忆 pane |
| | `/settings` | 设置 pane |
| | `/evolve` `/exam` `/contest` `/route` `/agents` `/flow` `/market` | 演化家族 → evolve 面板对应分区 |
| misc | `/remember` | 立即把本会话耐久事实蒸馏入库 |
| | `/stats` | 用量合计：轮次、token、缓存命中 |
| | `/help` | 命令与按键一览 |

## 架构

分层简图（调用方向自上而下；演化/记忆/治理层只经账本与事件流，不侵入引擎循环）：

```text
┌─ 入口   web/（React + Vite，connect-es）── HTTP(S) ──► lyond（connectRPC + SSE）
├─ 服务   pkg/server  SessionService/FileService/ApprovalService/… · Event Hub ·
│           会话存储与 drafts 持久化 · 审批桥 · x-lyon-workspace 头
├─ 引擎   pkg/agent   驱动循环 · 事件流 · 压缩 · 快照/回滚 · 子代理与并发扇出
├─ 工具   pkg/tools   shell / files / search / image / web / memory / todos
│           pkg/mcp    MCP stdio 与 streamable HTTP
├─ 演化   pkg/eval · pkg/evolve · pkg/routing · pkg/models · pkg/flow · pkg/market
│           评分榜单 · 闭卷考核 · 任务路由 · 模型画像 · workflow 状态机 · 工时台账
├─ 记忆   pkg/memory  三层持久记忆 · BM25 检索 · 蒸馏 · 云同步
└─ 治理   pkg/hooks   审计 jsonl · 聚合统计 · policy/外部钩子安全门
（传输侧）pkg/openaix（OpenAI 网关协议翻译）
```

目录明细：

```text
cmd/lyond           守护进程：connectRPC + legacy SSE、/metrics、/healthz、
                      审批桥、prompt_end 观察者接线（mindmap 蒸馏 + 演化循环）
web/                React + TypeScript 前端（Vite · @connectrpc/connect-es）
  src/App.tsx        会话生命周期、斜杠命令路由、平铺窗格、模式坞、油量表
  src/components/    ChatView / SessionList / StepsPane / MindMapCard /
                      FilePanel / ScmView / TerminalPane / PreviewPane /
                      PlanPane / MemoryPanel / SettingsPane / DashboardPanel /
                      EvolvePanel / NewSessionDlg / CodeEditor …
pkg/agent           引擎核心
  engine.go           Engine：HTTP 连接池调优、全局并发 turn 信号量、Warmup
  session.go          Session：每会话一个驱动 goroutine，事件流、压缩触发、
                      历史组装、忙时排队的间隙注入
  compact.go          ★ 客户端压缩：渲染历史 → 独立摘要请求 → 摘要替换历史
  snapshot.go         会话快照：Export/Restore + ~/.lyon/sessions 存档
  subagent.go         run_subagent：低配额模型的只读子代理
  dispatch.go         ★ run_subagents：2–6 个独立子任务并发扇出
  events.go           事件协议（connectRPC / SSE / 转录共用同一事件流）
pkg/tools           工具系统（registry / shell / files / search / image /
                      paste / web / memory / todos / stable / metrics）
pkg/server          HTTP 服务与 RPC：hub 环形重放、会话存储、审批、
                      文件/记忆/仪表盘/演化各 Service、x-lyon-workspace
pkg/hooks           审计（~/.lyon/audit/*.jsonl）、聚合统计、policy/hooks 安全门
pkg/memory          三层持久记忆：BM25 检索、热层注入、蒸馏、云同步
pkg/eval            演化机制：四维评分、在岗榜单、闭卷考核、覆盖率门禁、错因归纳
pkg/evolve          演化循环（lyond prompt_end 接线）：记轮 · 盲评 · 考核 · 事后诊断
pkg/routing         任务分类（中英双语）与推荐路由表
pkg/models          模型参数知识库 + 能力画像（32 个型号）
pkg/flow            workflow 状态机：机械执行、连绿自动固化
pkg/market          任务工时台账
pkg/openaix         OpenAI 兼容网关：HTTP 层协议翻译（http.RoundTripper）
internal/mockapi    验收用假 API：真实 SSE 帧序 + 前缀哈希 prompt-cache 模拟器
bench               性能验收套件（阈值强制）
```

默认配置：模型 `claude-opus-5-5`（`ANTHROPIC_MODEL` 可覆盖）、effort medium、
`MaxIterations=0`（单 prompt 的 ReAct 轮次**默认不设上限**，跑到模型自己收工为止；
要硬顶就 `-max-iter N` 或 `LYON_MAX_ITER=N`）、压缩阈值 0.7×200k、
子代理默认低配额档。

## 使用详解

### Web 交互要点

- **审批**：需要确认的工具在聊天流里出卡片，`y`/`n`/`a` 或鼠标点按；
  超时/关窗一律拒绝（fail closed），拒绝不持久化。`lyond -approve` 切严格模式
  （每个写操作都问）；默认 path-aware，只有越出工作区的 shell 命令才问。
- **排队与打断**：prompt 跑着时发送即入队（行尾 `queued N`），队头在下一轮
  工具执行前的**间隙**注入，与该轮工具结果合并成同一条 user 消息——
  不必等整段跑完，也不双发。会话行/工具栏的中断按钮随时打断（历史自动修复）。
- **图片输入**：prompt 里用 `@路径` 引用工作区图片（png/jpeg/gif/webp，
  文件名含空格写 `@"my shot.png"`），随消息以 image block 发给模型；
  模型侧对应 `view_image` 工具（`read_file` 只读 UTF-8 文本）。
- **检查点回滚**：每次进入 prompt 前（历史非空时）与 compact 替换历史前
  自动捕获检查点，`/rollback save [text]` 可手动标记；`/rollback N` 恢复——
  轮次统计、花费、标题保留，只有对话回退；恢复后转录重锚、状态条如实标注。
  compact 检查点存的是替换前完整原文，恢复后原文回归。

### 会话持久化

- **drafts（主路径）**：每个会话是一份 draft（`~/.lyon/sessions/<hash>/drafts/
  <id>.json`），随状态变化落盘；`lyond` 重启时 `LoadDrafts` 重新索引，
  侧栏原样列出、点开即续聊（`Server.acquire` 读回而不是 404）。
  删除会话是唯一删 draft 文件的路径。
- **saved 快照（历史路径）**：`~/.lyon/sessions/` 下的快照槽位与归档
  （每工作区最新槽 + 20 份归档轮转），由已移除的 TUI 写入；侧栏 saved 区
  仍可列出并 `/resume` 恢复（RestoreSession 只读）。
- `LYON_HOME` 可整体改写存储根——容器/CI/测试不碰真实 home。

### 压缩（客户端实现，任意端点可用）

单轮用量达到阈值×上下文窗口（默认 0.7×200k，`-compact-threshold` 调节、
负数禁用）即触发：把历史渲染成带角色标签的文本，发一次独立摘要请求，
再用单条摘要 user 消息**整体替换**历史——不依赖网关原生 compaction，
任何端点都能缩。`tool_use` 轮触发会延迟一拍等工具结果落地，摘要没把用量
压下去就绝不重试（进度护栏），失败不死循环。转录里对应 `compaction` 事件，
`/stats` 里对应 `stop=compaction` 轮。

### 服务端点

```bash
./bin/lyond -addr 127.0.0.1:8080 -workspace . -prewarm   # 默认即回环
bash examples/sse-client.sh http://127.0.0.1:8080        # 验收客户端
```

| 路由 | 说明 |
|---|---|
| `POST /v1/sessions` | 建会话；带 `message` 字段则直接开跑，SSE 返回 |
| `POST /v1/sessions/{id}/messages` | 后续 prompt，SSE 流式事件 |
| `GET /v1/sessions/{id}/stats` | 逐轮 usage（含 cache_read/cache_creation） |
| `DELETE /v1/sessions/{id}` | 立即销毁 |
| `GET /metrics` | 吞吐、cache_share、工具 p50/p95、last_error |
| `GET /healthz` | 存活探针 |

Web 走 connectRPC（`/lyon.v1.SessionService` 等，connect-es transport），
与上表的 legacy SSE 端点共享同一 Event Hub 与会话存储。

服务端防御：`-max-sessions` 准入上限（超出 429）、`-session-ttl` 空闲驱逐、
`-max-turns` 全局在飞 API turn 硬顶、SSE 写超时 15s + `X-Accel-Buffering: no`。

**鉴权**：默认只绑 `127.0.0.1:8080`。设了 `-token`（或环境变量 `LYON_TOKEN`）后，
除 `/healthz` 外的**全部**路由都要带 `Authorization: Bearer <token>`
（也接受 `X-Lyon-Token`，常量时间比较，401 返回）。绑到非回环地址却没给 token
时**拒绝启动**，必须显式 `-insecure` 才放行——这些路由能跑 `run_shell` 和写文件，
匿名暴露等于远程执行。

**每会话工作区**：Web 对每个 RPC 注入 `x-lyon-workspace` 头（跟随所选会话的
工作区），文件/记忆/工具禁闭面全部随头走；服务端 `wsFor` 校验必须是存在的
绝对目录，非法即回落引擎默认。

### 作为库嵌入

```go
reg := tools.NewRegistry()
tools.RegisterFunc(reg, "get_quote", "描述", true /*并行安全*/, fn)
eng := agent.NewEngine(agent.DefaultConfig(), reg)
sess := eng.NewSession(nil)
defer sess.Close()
sess.Submit("你的 prompt")
for ev := range sess.Events() { /* text_delta / tool_start / prompt_end ... */ }
```

完整示例：`examples/custom-tools`（业务工具）、`examples/mcp-hello`（MCP 客户端）。

## 性能验收（bench）

`internal/mockapi` 是一个进程内假 Anthropic API：发真实 SSE 帧序、可配延迟、
并跑**前缀哈希缓存模拟器**——按语义顺序（tools → system → messages）逐元素拼接
原始字节做哈希，字节级漂移必然 MISS。`bench` 套件的阈值不达标就 FAIL：

```bash
go test ./bench -run 'TestLoad|TestCache|TestSSE' -v
# 可选真实端点报告（不设断言）：LYON_LIVE_TEST=1 go test ./bench -run TestLiveReport -v
```

本机（20 核 Windows）实测：

| 验收项 | 门槛 | 实测 |
|---|---|---|
| 引擎 p95 开销（干净区间 16 会话×8 prompt，引擎 p95 − 裸 SDK p95） | ≤ 25ms | **5.7ms**（20.4ms vs 14.7ms） |
| 稳态 prompt-cache 命中率（第 2 轮起 token 占比） | ≥ 95% | **97.78%**（每轮 read 持续增长 1641→2057） |
| SSE 50 并发客户端事件丢失 | 0 | **0**（序列与基准完全一致） |
| SSE 首事件 TTFD p95 | ≤ 1s | **70ms** |
| 100 / 500 / 1000 会话矩阵 prompt 错误 | 0 | **0**（吞吐 1519 / 483 / 243 prompts/s） |
| 1000 会话关闭后 Session 驱动 goroutine 泄漏 | 0 | **0**（堆 85MB） |

说明：1000 并发下机器整体饱和（裸 SDK 基线同步塌陷到同一水位），差分是调度噪声，
所以**开销门槛只在干净区间执行**；矩阵子测试硬门槛是零错误、全完成、零泄漏，
吞吐/分位延迟只记录不设卡。

## 关键发现：prompt cache 与 anthropic-sdk-go 的字节漂移

**现象**：同一会话逐轮请求 `cache_read` 恒为 0——缓存从未命中。

**根因**：SDK 的 `param.MarshalWithExtras` 对 schema 的 `ExtraFields` 用
`for k, v := range extras` 遍历（Go map 每次迭代顺序随机），而 jsonschema 反射产
生的工具 schema 恰好带 `{$schema, additionalProperties}` 两个 ExtraFields。tools
数组在请求体最前缀，其字节每请求都变 → 缓存前缀链永远对不上。语义完全相同的两
个请求，仅 key 顺序不同即 MISS（mockapi 的 `TestCacheMissOnSchemaKeyOrderDrift`
专门钉住这一行为）。

**修复**（`pkg/tools/stable.go`）：每个工具 schema 只做一次规范化——marshal →
`UseNumber` 解码 → 重排序键 marshal，再以 `param.Override[...](json.RawMessage)`
返回。Override 分支不走 map 遍历，字节从此确定。挂在 `Adapt` 这个唯一收口点
上，会话工具、子代理、Warmup 全部经过且幂等。

修复后实测：跨进程字节一致、8 轮缓存份额 94.2%、子代理轮 93.6%、
`TestAdaptedSchemaMarshalDeterministic` 连续 100 次 marshal 字节完全一致。

**已知网关限制**（非本库问题）：

- 本网关只报 `cache_read` 从不报 `cache_creation`（缓存写入不可观测）；
- 网关不返回原生 compaction block——因此压缩走客户端实现（见上「压缩」），
  任何端点都能缩历史。

## 健壮性设计

**shell 探测**（`pkg/tools/shell.go`）：Windows 上 `exec.LookPath` 会命中 WSL
占位符 `C:\Windows\System32\bash.exe`——能解析却跑不了。因此每个候选 shell 都
真实执行一次 `echo lyon-shell-probe-ok`（5s 超时、校验 stdout 标记与退出码），
坏二进制直接淘汰；PATH 里没有 Git Bash 时还会探测
`%ProgramFiles%\Git\bin\bash.exe` 等常见安装路径。全部失败才报
`no working shell found (tried: ...)`。

**可中断 prompt**（`Session.Interrupt`）：中断取消的是本次 prompt 派生的
context——API 流与运行中的工具一起停，会话本身存活，prompt_end 以
`interrupted` 收尾（不发红色 error 事件）。若历史停在 assistant 的 `tool_use`
上，`repairInterrupted-History` 会给每个悬空调用排入合成 `tool_result`，
与下一条用户消息合并进同一条 user message，保证 role 交替完整、API 不会拒绝。
若干净收尾却仍需补偿，说明是迭代预算到顶
（`prompt_end.stop_reason="max_iterations"`，只在设了 `-max-iter`/`LYON_MAX_ITER`
时会发生——默认无上限）：对应步骤如实标 `over budget`。busy 锁在发出 prompt_end
**之前**释放，避免事件缓冲消费者立刻重投时撞上 `ErrPromptInProgress`。

## 统计、审计与安全控制（hooks）

`pkg/hooks` 在每个会话生命周期上留了钩子点，并实现三个消费方——事件表里
`prompt_start/end`、`turn_end`、`tool_use/exec_start/end`、`approve_request/result`、
`compaction`、`subagent_end`、`error`、`security_block` 等都会经过：

- **审计**：`~/.lyon/audit/<日期>.jsonl`，每行一个 JSON Record（时间、事件、工具、
  截断后的输入、耗时、审批结果、token 用量、拦截原因）。写盘在独立 goroutine，
  热路径零 I/O；保留 30 天自动清理。`LYON_AUDIT=off` 关闭。
  Web 的 Dashboard「审计尾」直接读它展示。
- **统计**：`~/.lyon/stats.json` 持久化聚合——prompt/迭代/compaction/subagent 次数、
  审批 yes/no、安全拦截数、token 累计、每工具调用/失败/耗时。Web `/stats`
  展示会话与全局用量。prompt_end 落盘（≤2s 延迟），`Session.Close` 时
  `Flush` 同步收尾。
- **安全控制**（两层，先于审批执行——被拦的工具不会打扰人类）：
  1. `~/.lyon/policy.json` 本地策略：
     ```json
     {
       "allow_tools": ["read", "grep", "glob"],   // 非空即白名单模式
       "deny_tools": ["bash"],
       "deny_patterns": ["(?i)rm\\s+-rf\\s+/", "curl[^|]*\\|\\s*(ba)?sh"]
     }
     ```
     模式对工具输入的原始 JSON 做正则匹配；拦截以 `blocked by security policy`
     作为工具结果返回给模型，并记 `security_block`（`blocked_by` 指明规则）。
  2. `~/.lyon/hooks.json` 外部钩子：
     ```json
     [
       {"event": "tool_end",   "command": "node C:/hooks/log.js", "timeout_ms": 5000},
       {"event": "tool_start", "argv": ["gate.exe"], "blocking": true}
     ]
     ```
     Record 以 JSON 写到钩子 stdin；`"blocking": true`（仅 tool_start）退出码 2
     或 stdout `{"deny":true,"reason":"..."}` 即否决该次工具执行。非阻塞钩子
     异步执行，失败计入 `hook_failures` 并记 `hook_error`。`LYON_HOOKS=off` 关闭。

外部钩子可用于自建统计上报、审计外送、DLP/沙箱等安全网关；子代理工具同样
经过 `PreTool` 安全门。

## 记忆模块（分层热/冷 + 自进化 + 云同步）

三层持久记忆，one-fact-per-file Markdown，人可直接手改：

| 层 | 位置 | 作用域 |
|---|---|---|
| team | `$LYON_HOME/memory/team/` | 跨项目、跨 agent 共享（组织约定） |
| personal | `$LYON_HOME/memory/personal/` | 此用户的所有项目（个人偏好） |
| project | `<workspace>/.lyon/memory/` | 当前代码库（决策、约定、坑） |

- **检索**：本地 BM25 倒排索引（CJK 二元分词，无需分词器），字段加权 name 3.0 /
  tags 2.5 / description 2.0 / body 1.0；平局时近层优先（project > personal >
  team）。纯内存索引，毫秒级。**记忆面板**的检索框在浏览器侧复现同一排序——
  输入即看分数、命中词与命中段落，验证 `read_memory` 能否找到你要的事实。
- **热层**：`hot: true` 的事实随会话首条 prompt 常驻系统提示（默认 4KB 预算）。
  已存事实被检索/精读命中累计 hits≥2 后，再蒸馏时自动晋级热层。
- **模型工具**：`save_memory`（按标题 upsert，`scope`/`hot` 参数）、`read_memory`
  （`query`=BM25 检索 / `title`=精读 / 空=紧凑列表）、`forget_memory`（按名删除）。
- **自进化（distill）**：`/remember` 手动触发——引导模型立刻跑一遍蒸馏，把
  本会话的耐久事实（决策与理由、偏好、约定、环境坑）写进记忆（最多 6 条，
  幂等、内容没变不重写）；每次写入记 `memory_save` 审计事件。
  `LYON_DISTILL=off` 关闭。
- **云同步**：设置 `LYON_SYNC_URL`（可选 `LYON_SYNC_TOKEN`）后启用，
  `POST /memory/push {"facts":[…]}`、`POST /memory/pull {"scopes":["team","personal"]}`，
  `updated` 新者胜；只同步 team+personal，project 层永不出机。
- **旧格式迁移**：`<workspace>/.lyon/memory.json` 首次打开时自动拆为 project 层
  单条事实，原文件改名 `memory.json.imported`。

事实文件格式（frontmatter 可手改，无 frontmatter 的文件以文件名为名整体作正文）：

```markdown
---
name: toast-notification-preference
description: 通知用右下角 toast，不要老式弹窗
scope: personal
hot: true
created: 2026-10-10T18:00:00Z
updated: 2026-10-10T18:30:00Z
hits: 3
---
正文……
```

## 会话自动标题（意图识别）

- 每次 prompt 结束后，用低价位子模型做一次意图判定：同话题回 `{"changed":false}`
  保持原题（不写不花），换题才重出标题（3-8 个词，跟随用户语言）。
- 判定输入只有「首条 prompt + 当前标题 + 最新 prompt」（几百 token，绝不带整段
  历史），回复上限 64 token；`LYON_TITLE=off` 关闭。
- 展示与持久化：侧栏会话行常驻标题；快照保存标题，恢复后延续。
  换题检测在 prompt_end 异步进行，UI 不等待；在途时新来的 prompt_end 会补跑。

## 步骤审计面板（StepsPane）

- **任务树结构**：面板按**父子任务**组织——每条 prompt 一个分组标题行，
  其下动作缩进一级；`run_subagent` 是任务节点，子代理内部执行（事件 `Sub`
  标记）再缩进一级挂在它下面，`subagent_end` 收尾为 `task · N API calls`。
- **可读参数**：`run_shell` 显示命令、`web_search` 显示 query、文件工具显示
  path、`run_subagent` 显示任务——多行压成一行，不出现裸 JSON。
- **工具配色**：shell 绿 · 子代理蓝 · 写入类黄 · web/MCP 品红 · 读取类青。
- **实时动作审计**：每步状态 `⟳` 进行中（实时耗时）· `✓` 完成 · `✗` 失败/中断；
  数据全部来自引擎事件流，exec_start/exec_end 按执行 ID 配对（并行同名工具
  FIFO），prompt_end 收尾时仍在跑的步骤如实标 interrupted/aborted。
- **实时输出**：展开步骤即见运行中命令的 stdout/stderr 流入（首写即刷、
  其后 40ms / 16KB 合帧）；列表行尾带最新输出尾行。
- **审计可追溯**：步骤号会话内连续递增（与 hooks 审计日志对得上），
  工具报错文本直接显示在行尾。

## 回滚与撤销

- **检查点**：每次进入 prompt 前（历史非空时）自动捕获完整历史副本；
  **compact 替换历史前**也捕获——compact 永远可以撤销。
  手动标记：`/rollback save [text]`（动危险文件前先标一个）。
- **恢复**：`/rollback` 列出检查点（新→旧，带消息数/时间/标签），`/rollback N`
  恢复第 N 个。恢复 = 下一 prompt 从检查点重建历史；**轮次统计、花费、标题保留**。
- **compact 后回滚到 compact 前**：compact 检查点存的是替换前完整原文，
  恢复后下一次请求重新携带原始对话（`TestCompactRollback` 断言摘要彻底消失）；
  compact 检查点在淘汰时最长寿（undo 栈上限 20，先删 prompt 检查点）。
- **安全语义**：恢复只在空闲时执行（busy 自动打断并在 prompt_end 后执行）；
  目标之后的检查点随之作废；每次恢复写一条 `rollback` 审计记录；
  转录保留被丢弃的行 + `rolled back` 标记，审计痕迹不抹。
- **`/undo`**：撤销上一轮 run 的文件变更（git side-ref）——聊天里改了文件不满意，
  一条命令回到 run 之前。

## 内置模型参数知识库

- **离线知识库**（`pkg/models`）：常见模型家族的上下文窗口、输出上限、官方推荐
  参数（温度/top_p/推理模型约束）、知识截止与**参考价**（USD/百万 token）——
  Anthropic Claude、OpenAI GPT/o 系、Google Gemini、DeepSeek、Qwen、Llama、
  Mistral、**Xiaomi MiMo**，无网络查询，编译进二进制。缓存命中/写入价按厂商
  各自记录，未记录的条目沿用 Anthropic 的 0.1×/1.25×。
- **查找规则**：先精确匹配（不分大小写），再最长通配前缀；**未收录返回 nil，
  绝不猜窗口**——调用方回退到配置值。
- **Web 展示**：`/model` 出当前会话生效模型卡；**设置面板**列出网关
  `/models` 实际提供的型号 × 知识库增强（id · 上下文 · 价格 · 家族 · 是否视觉），
  网关新模型知识库没听过也照样有一行；点选可切换引擎主模型与子代理模型。
- **价格是参考价**：厂商随时调价，以官网为准；价格未知显示「未知」不编数字。
  未收录型号按配置执行、不猜参数。
- **为「上下文占比 + 花费」铺路**：知识库的 context 窗口与单价是油量表与
  花费展示的数据源。

## 上下文占比与花费（常驻油量表）

- **右下角 HP 条**：贴在状态栏右端，`ctx% · used/window` + 会话累计花费——
  上下文占窗口百分比 + 当前占用/窗口 + 油钱。颜色随水位走：低负载绿、
  逼近 compact 阈值黄、危急红。
- **占用估算**：取最后一轮的请求侧（input+cache_read+cache_write）加回复——
  回复已进历史，就是下一次请求的增量；compact 轮特判（只有摘要留在上下文）。
  窗口数取知识库，未收录回退配置值。
- **花费**：逐轮 input/output 按面值、cache read ×0.1、cache write ×1.25
  （Anthropic 公布倍率）计价累加；**子代理用量按模型单独记账**（常是更便宜
  的档）。单价来自知识库（参考价）；**未收录的模型绝不编价格**——显示
  「未知」，或用环境变量声明网关税率：`LYON_PRICE_IN` / `LYON_PRICE_OUT`
  （USD/百万 token，两个都设才生效）。
- 打开会话即渲染（gauge 随 Session 快照与 `turn_end` 事件更新），
  切会话不残留旧值。

## 执行模式（default / plan / bypass / mindmap）

- **四种会话级模式**（引擎强制，运行中实时生效）：
  - `default` — **人工审批**：需要确认的工具弹 y/n/a 卡片（path-aware 默认，
    `lyond -approve` 严格化）；
  - `plan` — **只读探索**：写文件、shell、记忆写入、MCP 工具与未注册工具
    一律 `blocked by plan mode`（fail closed：放行名单 = registry 的
    ParallelSafe 只读标注，`mcp_*` 因副作用未知额外排除，`run_subagent`
    因子代理只拿只读工具且过同一道门而放行）。拦截以 mode_block 卡片呈现
    （`security_block(blocked_by=mode:plan)` 落审计），不打扰审批、不触发
    外部 hook；system prompt 同步注入 `[MODE: PLAN]` 合约；
  - `bypass` — **审批自动放行**：需要审批的工具直接 yes，approve_request/result
    两条审计照写（auto-grant 必留痕）。**policy.json 与 blocking hooks 照常
    执行**——bypass 只跳过人工，不削弱安全底线；
  - `mindmap` — **图交互**：进入即打开思维导图 pane，图上编辑回流推进对话
    并触发 agent 自动追问。
- **切换**：左下模式坞点选 · `shift+tab` 循环（default → plan → bypass）·
  `/mode`。每次切换写一条 `mode` 审计记录（`default → plan`）。
- **常驻可见**：状态条模式徽章一直在，切了就看得见；模式不随切会话偷偷改。

## OpenAI 兼容网关（协议翻译层）

- **单点翻译**：`pkg/openaix` 实现一个 `http.RoundTripper`，在 HTTP 层把
  Anthropic Messages 协议 ⇄ OpenAI `chat/completions` 双向翻译。工具系统、
  历史构建、tool runner、compact、子代理、标题生成……所有调用点共用一个
  Engine client，**零业务代码改动**，不开启时零开销。
- **开启方式**（任选其一）：
  - `lyond -openai` — 默认网关 `https://token-plan-cn.xiaomimimo.com/v1`
  - `lyond -openai-base https://host/v1` — 任意 OpenAI 兼容端点（隐含 `-openai`）
  - 环境变量 `LYON_OPENAI_BASE`（flag 优先，尾部 `/` 自动去掉）
- **同一个 key**：请求仍带 `x-api-key`，翻译层自动换成 `Authorization: Bearer …`
  发给网关（网关若本身用 Bearer 则原样透传）；**鉴权头/Anthropic 专有头不会
  泄漏给网关**。
- **网关友好默认值**：`WebToolMode` 强制 `client`（网关跑不了 Anthropic 服务端
  web 工具）；未设 `LYON_SUBAGENT_MODEL` 时子代理自动用主模型（单模型网关会
  拒绝第二个模型 id）。
- **协议要点**：
  - 请求：system→首条 system 消息；`tool_result` 提升为独立 `role:"tool"`
    消息并排在 user 文本之前；相邻同角色合并；thinking/未知块丢弃；
    `tool_choice` none/any/tool/auto→none/required/function/auto；
    流式请求带 `stream_options:{include_usage:true}`。
  - 响应：网关 SSE 逐帧合成为 Anthropic `message_start/content_block_delta/
    message_stop` 事件流；usage 在 message_start 置零、message_delta 带全
    四个字段；JSON 响应同样可喂流式 SDK。
  - 错误：非 2xx 保留原 HTTP 状态码，body 包成 Anthropic `{"type":"error",…}`
    结构（401→authentication_error、429→rate_limit_error、529→overloaded_error、
    ≥500→api_error），`retry-after`/`x-request-id` 透传；200 但 body 是
    HTML/错误对象时映射为 502/500 而不是静默空消息。
- **验收**：翻译单测、RoundTrip 往返（流式/非流式/错误/透传/鉴权）、真实
  anthropic-sdk-go 客户端穿层、引擎级真实 tool runner 两轮循环 e2e
  （`pkg/agent/openai_gateway_test.go`）。

## Agent 演化机制（评分 · 考核 · 路由 · workflow 固化）

设计文档 `docs/design/agent-evolution.md`。四层代码：`pkg/eval`（评分/榜单/考核/
覆盖率/错因归纳）、`pkg/evolve`（演化循环——`lyond` 把它接在 prompt_end 观察者
上）、`pkg/routing`（任务分类与推荐网络）、`pkg/models`（模型能力画像）、
`pkg/flow`（机械执行器）。账本落盘 `$LYON_HOME/`，**Web 演化面板与 Dashboard
只读展示**；总闸 `LYON_EVOLVE=off`，分闸 `LYON_SCORE` / `LYON_EXAM` /
`LYON_DIAGNOSE`。

### 生产评分与在岗榜单

- **四维评分** `eval.ScoreWith`：Correctness / Efficiency / Cost / Compliance
  加权出 Total，`DefaultThresholds` 四维全过才 Passed。
- **榜单** `$LYON_HOME/leaderboard.json` 按任务类（coding/fix/review/research/
  writing/design/data/general）分别记账：每个 Agent 保留最近 50 张记分卡，
  每张生产记录都会重新推选 on-duty（证据最新且达标的当选）。
- **盲评入榜**：无标准答案的 prompt 跑完交 `eval.JudgeRun`（一次低价调用给
  0..1 的 correctness），prompt_end 后台判分入榜；判分失败**不入账**（宁缺毋滥）。

### 闭卷考核（每 10 轮）

- **手写题库**：8 个任务类各 4 题，随二进制冻结分发（绝不由被考核的生产任务
  生成），首次使用落盘 `$LYON_HOME/exams/<class>.json`——可检查、可改。
- **闭卷** `ClosedBookSystem`：无工具、不读文件，只考权重里的知识；
  `DefaultExamPass = 0.75`（4 题过 3）。
- **时钟**：每 10 次成功 prompt 记一轮，到点且席位有人时自动开考；
  连挂 2 次 → 撤职、席位空缺，下一张生产记分卡重新选举。
- 考题、时钟、在岗席位在 Web evolve 面板可核对（题量、候选场、考试时钟）。

### 任务分类与推荐路由

- `routing.Classify`：中英双语关键词计分（同分保留靠前者）；`Openness(text)`
  给任务打 0–5 开放度。
- **推荐表** `$LYON_HOME/routing.json`：每个 (class, agent) 记运行均值分与
  置信度 `mean × samples / (samples + 3)`——样本越多置信越高，新玩家要靠
  真实分数挣位置。
- **能力画像** `pkg/models`：注册表 32 个模型全部标注 Reasoning/LongHaul/
  Openness/Speed/Thrift 五维 0–5 画像。
- 路由表与榜单在 Dashboard / evolve 面板直接可读；考试与评分改写谁在岗，
  进化的输出真正变成行为。

### 知识覆盖率门禁（设计 §2）

- **两段闭卷测验**：`GenSystem` 从任务生成可核对的 Q/A 清单 → `BookSystem`
  盲答 → 对照评分；`CoverageThreshold = 0.6` 以下算缺口。
- **定向注入**：缺口行（`问题 → 期望答案`）拼进任务正文；flow 里经 `{{input}}`
  进入每个节点的 prompt。门禁本身出错只记 `gate skipped`，**永不阻断**。

### workflow 冻结与机械执行（设计 §3）

- **控制流是数据**：`Workflow{slug, title, trigger, nodes}`，节点退出判据
  `always | command | 退出码 | contains 标记 | judge 窄问`；失败策略
  `retry（默认，诊断回灌后有界重试 ≤3）| abort | continue | goto:<name>`——
  **LLM 只做节点内填空（`{{input}}`），不决定控制流**；tick 预算杀死 goto 死循环。
- **内置 4 条**（`go:embed`）：`feature-dev` · `bugfix` · `code-review` ·
  `release`。用户文件 `$LYON_HOME/workflows/*.json` 按 slug 覆盖内置。
- **自动固化（§3.1）**：同一 slug 连续 3 次绿灯自动冻结为
  `<slug>-proven.json`（标题带溯源：streak · 最近得分 · 冻结时的模型），
  红灯清零重计——经得起生产检验的流程自己变成工件。
- **错因归纳回灌**：失败的 prompt 异步跑 `SummarizeFailure`，产出可核对
  条件 + patch——audit 留痕（`LYON_DIAGNOSE=off` 关）；诊断结论还会**写入
  项目记忆热层**（内容去重、热窗上限 3 条），下一次会话的注入带着教训开跑。

### 历史实跑记录（2026-10-12 → 10-13，整夜 11h21m / 224 轮全成功）

- 考试 20 场 **18 PASS / 2 FAIL**：research 在岗模型连挂两场触发撤职，
  席位由生产证据重新选举（被驱逐者不自动回位）。
- 盲评入榜 206 次，得分区间 **0.80–1.00**（含 4 个真实低分，评分确有区分度）。
- 路由换手 8 次，终局 fix 类 100 样本 / 置信 0.96 → 推荐 mimo-v2.6-flash。
- bugfix workflow **18 连绿**自动固化，终局标题
  `缺陷修复标准流程 (frozen after 18 green runs · score 1.00 · mimo-v2.6-flash)`。
- 全部账本（leaderboard/routing/exams/workflows/audit）落盘重启原样恢复。

## 路线图

- ✅ Web 工作台 v1–v2（平铺窗格、审批、记忆/演化/仪表盘面板、每会话工作区）
- 🚧 Release 预编译分发与一键安装 · 进程内与操作系统级定时器
- 🗺 心智图多人协同、演化面板动作执行（开考/竞赛/固化）的 Web 化

欢迎在 issue 里讨论优先级。

## 测试

```bash
go build ./... && go vet ./... && go test ./... -count=1   # 后端全量
cd web && npx tsc -b                                       # 前端类型检查
node web/e2e/smoke.mjs                                     # Playwright 冒烟（需 LYON_E2E=1）
```

覆盖：工具系统（schema 稳定性、审批、白名单、指标、shell 探测、记忆读写）、
引擎（事件协议、客户端压缩终局/延迟重建/阈值谓词、快照 Save↔Load↔Restore、
回滚、记忆注入、子代理、真实缓存命中、中断与历史修复）、模式与审批
（plan 门矩阵、bypass 短路、mode 审计）、模型知识库、OpenAI 翻译层
（协议互转、SSE 合成、错误映射、SDK 穿层、引擎级 e2e）、MCP、服务
（connectRPC、SSE、hub 重放、会话存储、文件/记忆/审批/仪表盘）、演化机制
（评分四维、榜单持久与连挂撤职、考核出题判分、分类与推荐置信度、
覆盖率注入、flow 执行器全分支）、mockapi、bench（性能验收）。
全部无网络、无真实密钥依赖。

持续集成（`.github/workflows/ci.yml`）在 ubuntu / windows / macos 三平台执行
`go build ./... && go vet ./... && go test ./... -count=1`——阈值不放宽，红了就修。

## 许可

[MIT License](LICENSE) © 2026 LyonLen。欢迎 issue 与 PR。
