# lyon 未完成总账（BACKLOG）

> 2026-10-16 汇总自 `archive/` 里那五份快照（分析 / 断点 / 路线 / 交接 / 需求打捞）
> 的逐条盘点，去重合并。**活文档**：做完勾掉或删行，新发现的欠账也记这里。
> 与 archive 的关系：那是「当时的快照与论证」，本文只管「还欠什么」——
> 目录说明见 [`README.md`](README.md)。

---

## 0. web v1 —— 已完成（2026-10-16 拍板「先做 web」；2026-10-19 全线交付）

> **状态（2026-10-19）**：Phase 0–5 全部交付，之后继续推进 web v2 Phase A–F
> （审批真接线、面板与斜杠命令、workspace 写操作、视觉收尾）与 GetHistory
> 历史直渲（7aa9b5f，「点旧会话不渲染」已修）。下列原始验收条目留档不再执行；
> 新欠账见 §2。web v1.1 面板（任务市场/记忆/考卷）仍归 §2.6。

规划全文：`archive/web-v1-plan.md`（两参考仓库实读调研 + 字段编号级
proto 草案 + Event Hub 架构 + 19 数据源清单）。三选型已随规划定案：
① 协议栈 Connect RPC + buf 工具链（用户原话本就指定 protobuf）；
② 现有 SSE 端点与 Connect 并存，内部共享新增 Event Hub；
③ 鉴权静态 bearer token（复用 lyond 的 `-token`/`LYON_TOKEN`，多用户只在 proto 预留）。

- [x] **Phase 0 — 契约与工具链**：`buf.yaml`、`buf.gen.yaml`（4 本地插件纪律），
  六份 proto（event/session/file/dashboard/approval/plan），go.mod 引
  `connectrpc.com/connect` + `google.golang.org/protobuf`，生成并提交 `gen/go/lyon/v1/**`
  - 验收：`buf lint && buf generate && go build ./... && go vet ./...`
- [x] **Phase 1 — Event Hub + SessionService + 鉴权冒烟**：`pkg/server/hub.go`
  （唯一消费 `Session.Events()`、seq 自增、4096 replay ring、慢订阅者踢
  ResourceExhausted、UTF-8 卫生）、`connect_session.go`（错误映射：
  ErrPromptInProgress/ErrNoPromptRunning→FailedPrecondition、无会话→NotFound）、
  store 接 hub 生命周期、`streamPrompt` 改订阅 hub（帧字节不变）、
  `Session.Busy()`/`QueueLen()` 只读 accessor
  - 验收：`go test ./pkg/server/... ./pkg/agent/...` +
    `go test ./bench -run TestSSE` **原样通过**（50 并发零丢失 + TTFD p95 门禁）
- [x] **Phase 2 — FileService + Confine 导出**：三端点（目录 JSON 2000 项截断 /
  文本 512KB+truncated 行分页 / 二进制 16MB），`pkg/tools` 导出 `Confine` 包装，
  路径攻击用例（../ 逃逸、绝对路径、NUL、symlink 逃逸）
- [x] **Phase 3 — web 脚手架 + connect-es**：Vite + React + TS，`web/src/api.ts`
  transport/interceptor 范式，buf.gen.yaml 放开 TS 插件（生成器与运行时 exact 同版），
  页面 v1 = 会话列表（活跃+历史）+ 对话视图（WatchEvents 流式）+ 文件树预览
- [x] **Phase 4 — Dashboard 面板**：GetOverview（复用 /metrics 聚合）、
  ListRuns/GetRun（dumps 目录）、ListAudit（audit JSONL 尾巴）、ListSnapshots；
  run 回放依赖 `pkg/agent/dump.go` 的 `tempWriteSkip` 开关移除
- [x] **Phase 5 — 审批与 plan 占位收口**：ApprovalService/PlanService 全部
  `Unimplemented`（消息形状已冻结），前端降级提示不崩
- web v1.1 三块面板 2026-10-28 收齐：记忆（MemoryPanel）、考卷候选/血统
  （2026-10-26 EvolvePanel）、任务市场（2026-10-28 市场区 + `/market` 进
  web 斜杠族）— 详见 §2.6

---

## 1. 等用户拍板（晨报四件）

- [x] **无主 memory 套件处置** —— 2026-10-19 用户拍板「按推荐：修复认领」，
  已逐行评审后提交 `432fea2`（store.go Open loaders 锁 + index.go upsert 族 +
  upsert_perf_test.go，+490/-29）。Delete/Reload 全量重建防孤儿 posting、
  publish 前独占已核；`go test ./pkg/memory -count=3` 连绿。评审 B-#7 尾巴随之清。
- [x] **CI 仓名 slug** — 2026-10-28 核实：remote origin 即
  `github.com/LyonLen/lyon-agent`，README badge 的假设成真（1.2 Release
  的前置已解）。
- [x] **规划类 md 是否入库** —— 2026-10-21 拍板：活账 `docs/planning/BACKLOG.md`
  入库，其余快照归 `docs/planning/archive/`；`.claude/settings.json` 属切机私有
  配置，进 .gitignore。
- [ ] **GOCACHE 是否迁 D 盘**（现 C 盘；全量测试时有 `unlinkat test.exe: Access
  denied` 构建噪声）

---

## 2. 押后队列（去重后按欠账轻重排序）

### 2.1 大规划 —— 先设计文档再动代码

- [ ] **分屏多会话 + queue 碎片合并**（HANDOFF A-7）。欠账最久，用户原话
  「要好好规划哦」。拆解：设计文档 → 分屏布局 → queue 合并优化（#2 间隙插队
  已先行落地 d7f3527，剩碎片合并）。
  - [x] 规划 subagent + 子代理结果合并 —— 2026-11-04 落地 `pkg/agent/planner.go`
    （方案A：并行 planner 换 plan，`LYON_PLANNER=off|auto|on` 默认 auto）+
    `mergeSubresults`（dispatch.go：一次 cheap-tier Ask 收拢各子代理结果）。
- [ ] **对话实时思维导图（mindmap）** —— 2026-11-03 用户口述新功能，
  方案已写：`docs/design/mindmap.md`（13 节，含图→memory 抽象与 7 组测试用例），
  视觉基线 `docs/design/mindmap-sample.png|html|js`（样图支持真圈选）。
  要点：旁路蒸馏挂 `prompt_end` tap（不侵入内核）、图是「待你确认的理解」而非结论
  （提案态虚线 / 确认态实线）、人工改动 `pinned` 权威、结构操作本地零 token /
  语义操作走 chat 并以 `map_ops` 回作用图、只把 `confirmed` 抽成 memory。
  分期：M1 见证 → M2 人工通道（核心）→ M2.5 沉淀 → M3 双向。
  - **M1 已交付（2026-11-04）**：后端 `mindmap_update`/`mindmap_edit` 事件 +
    Get/UpdateMindmap RPC（`pkg/mindmap`、`pkg/server/mindmap.go`，edit 事件
    入 history，rev 守卫幂等）；web 卡片 `web/src/components/MindMapCard.tsx`
    （对话流尾部、提案虚线/确认实线、点节点/t# 跳回原话、新节点 400ms 高亮、
    折叠一行 `N 分支 · 最近 +k`）。
  - 原「待确认」已拍板（2026-11-04）：蒸馏走 subagent 档便宜模型；
    `LYON_MINDMAP_ASK` 默认开（M2 才实现，现零代码）；不写 hot 索引。
  - 仍欠：M2 人工通道（edit→chat 800ms 合并窗 + 自动提问纪律 §4.4）、
    M2.5 圈选沉淀、M3 双向。

### 2.2 产品硬要求收尾

- [x] **1.6 并发规划增强 F1/F2**（memory: lyon-concurrency-disposition ——
  Go 式最大化并发是产品硬要求，串行是缺陷）—— 2026-11-04 落地：
  - F1 习惯回路进 prompt：todo 计划后提示「独立项用 run_subagents 一次派发」；
  - F2 计划感知并发：≥3 独立项首条 todo 即派发，steps 面板显示「N 项并行中」。
- [ ] **1.6 F3 度量闭环**：stats 记 fan-out 计数，evolve 面板加「并发度」列。
  验收：stats 有 fan-out 计数；evolve 面板可见并发度。

- [x] **记忆自动蒸馏（治本）** —— 2026-11-04 落地 `pkg/agent/distill.go`：
  `DistillAt(trigger)` + 触发点接线（teardown 的 park/reap/delete/shutdown、
  TUI exit、`/remember` —— store.go / cmd/lyon；prompt_end 那一针是
  mindmap 蒸馏的 seam，不是 memory 的，每轮一调太贵故不挂），`LYON_DISTILL=off`
  静默，audit 留痕由 `distill_audit_test.go` 钉住。原欠账「写入纯靠模型自觉」
  已治本。另修 reasoning 模型空转：Ask/蒸馏两路请求显式 `thinking: disabled`
  （thinking 计入 max_tokens，曾吃满预算 → 空回复 / 10s teardown 超时）。
  - 参照：web `/remember` 手动兜底归 §2.5 漂移收敛。

- [ ] **演化动作入口随 TUI 移除悬空（2026-11-10 新欠账）**：`RoutingSwap`/
  `AdoptAgent` 已无调用方；跑考核（/exam 手动）、开擂台（/contest）、
  `/flow save` 与 `flow.BumpStreak` 连绿投票、知识覆盖率门禁投票的驱动方
  （原 `cmd/lyon`）已删；`pkg/timer` 无人再实例化 `NewScheduler`，prompt
  任务依赖的 `lyon run` 执行体也不存在（包休眠，见 `pkg/timer/DESIGN.md` 顶注）；
  `pkg/doctor` 同样无调用方（原 `lyon doctor` CLI，包与测试仍在）。
  不受影响：prompt_end 的 `evolve.Hook`（记账/评分/考试钟/错因归纳）仍由
  lyond 驱动。
  - 验收：上述动作在 web 有真实入口，或在面板/文档明确标记「暂不提供」；
    timer 要么接线 lyond/web，要么归档 pkg。

### 2.3 ROADMAP 阶段 1 功能欠账

- [x] **1.1 `lyon doctor` 一键体检** —— 2026-11-04 落地 `cmd/lyon/doctor.go`
  + `pkg/doctor`（网关/MCP/记忆库/leaderboard 摘要，输出可贴 issue）。
- [ ] **1.1 `/help` 分级**（新手三问一屏：我在哪、能干什么、怎么退出）。
- [x] **1.4 repo-map 轻量上下文** —— 2026-11-04 落地 `pkg/agent/repomap.go`
  （workspace 扫描缓存 → system note 注入「本仓库地图」，engine 启用）。
  同项的 search_files 与 read_file offset/limit 此前已完成。
- [ ] **1.2 安装一行命令 + GitHub Release**：brew tap / curl | sh / scoop，
  tag + 三平台产物 + checksum。依赖 1.2 的仓名拍板。
- [x] **1.3 git 即后悔药** —— 2026-11-04 落地：side ref `refs/lyon/auto/<sid>`
  自动 commit（不碰 HEAD/分支/用户 index，`pkg/server/autocommit.go`）+
  `UndoLastRun` RPC 与 web `/undo`（链走到根删 ref，NotFound 收口；
  autocrlf 幻影脏已修——index 与 parent 同 blob 时走 index 恢复刷 stat）。
  （原「TUI /undo 押后」随 2026-11-10 TUI 移除作废——/undo 只剩 web 入口。）
- [x] **1.7 仓库卫生收尾** — 2026-11-10 收口：badge slug 核实与 remote 一致
  （LyonLen/lyon-agent）、`docs/screens` 截图随 TUI 移除整体删除（README 改
  界面导览不再挂图）、README 双语重写为 Web 中心且 22 章同构（LICENSE、
  CI workflow、exe 出 git 三项早已落地）。

### 2.4 工程债

- [x] ~~评审 B-#5：tui.go 拆分~~ → 随 2026-11-10 TUI 整体移除自然消解
  （`cmd/lyon` 已删，无物可拆）。
- [x] **C-1 bench TestLoad/overhead 全量并行 flaky 根治** —— 2026-10-30 按实测
  分布重定门限：旧 25ms 压在本机噪声中位（静机 best-of-3 ≈ 25.5ms）上，全量
  并行 ~50% 误报；抬至 40ms（~1.5× 噪声头寸，真回归 +20ms/turn 仍触线），
  数据来源写进 `bench/load_test.go` 门限注释，CI 头注记同步更新。同轮补
  ubuntu-only race job（`CGO_ENABLED=1 go test -race ./pkg/agent ./pkg/tools`），
  盯 bgshell 注册表 / session emit-close 这批新并发面。
- [ ] **全量测试 3 连绿验收**（HANDOFF D-④，历次从未达成：三次跑三种结果）。
- [x] **C-4 pkg/eval TempDir `001` 空目录 RemoveAll 竞态** —— 2026-10-19 核实
  已被现行覆盖：`scratchHome` 带 40×50ms 重试 RemoveAll + 扫描器 `001` marker
  注释，`go test ./pkg/eval -count=3` 连绿，无需再修。
- [x] **hooks SessionStart 触发点** —— 2026-11-04 落地：`EnsureSessionStart`
  每会话恰好一发（session.go latch），NewSession→Restore→Register 三路归一
  （store.go），`Source=startup|resume|clear`。
- [ ] **热层已超预算 2 倍**（2026-11-03 核实，独立于 mindmap 方案）：
  `memoryHotBytes = 4096`（`pkg/tools/memory.go:21`），而项目热层现有 4 条共
  **8226 字节**，其中 `26-27年汇报-sdas-skill-与软件团队定位口径.md` 单条 4722 字节
  ——自己一条就吃穿预算。溢出行为是**整条丢弃**（`pkg/memory/store.go:424`
  `if used+len(entry) > maxBytes && any { return }`），所以至少 1 条 hot fact
  每次会话都**没进上下文**，而且当事人不自知。
  - 修法二选一：清理热层（把长条目降冷，只留 description + 索引）/ 改双限（条数+字节）
    + 超限时 audit 告警而不是静默丢弃。
- [x] ~~background_end 的 TUI 转录尾巴~~ → web 侧早已通（chat 事件 + 系统注记 +
  间隙注入 + 生命周期）；TUI 转录半边随 2026-11-10 移除作废。空闲自动唤醒
  仍为有意不做（后台完成 = 系统注记，等用户下一轮）。

### 2.5 入口对齐 —— 核心闭环不能只对一个入口生效（2026-10-16 盘点）

内核单份（`pkg/agent` 的 ReAct/工具/记忆/hooks、`pkg/eval`、`pkg/routing`
都是库，lyon/lyond/web 同一个 `agent.NewEngine`），**但装配层双份、触发点错位**：

- [x] ~~评分/考试触发下沉~~ → 2026-10-26 落地为 `pkg/evolve`（共享计算包，
  非 `pkg/agent`：grading/考试钟/竞赛/进化/诊断全在一处，import 无环）：
  TUI 只留面板半边（`cmd/lyon/evolve.go` 类型+函数别名，事件环照旧）；
  lyond 经 `Store.SetPromptEndObserver(evolve.Hook)` 挂在 hub 的
  prompt_end tap（`pkg/server/store.go`，与 interval 标题共用一个 tap，
  Hook 自身 fan-out goroutine、不堵 hub）。`LYON_EVOLVE=off` 总关，
  各腿沿用 `LYON_SCORE`/`LYON_EXAM`/`LYON_DIAGNOSE` 分开关。
  - 验收：`pkg/evolve` 单测（BookRound 钟/FinishExam 淘汰/诊断回写）+
    `pkg/server` tap 单测通过；lyond 跑一条 prompt 后 leaderboard 出卡、
    考试钟照走（重启后实测见 §2.6）。
- [ ] **抽共享 engine builder（`pkg/boot` 或并入 `pkg/config`）**：Config 解析 +
  MCP 三层（mcp.json / lyon.toml / flags）+ `RouteFor` 注入 + memory Flush，
  两个 cmd 都调它。现状对照表（grep 实测）：

  | 能力 | lyon | lyond |
  |---|---|---|
  | MCP 三层配置 | ✅ | ❌ 0 处引用 |
  | lyon.toml | ✅ | ❌ 完全不读 |
  | `RouteFor` 路由换将 | ✅ | ❌ 未注入（路由闭环 web 侧失效） |
  | memory 退出 Flush | ✅ | ❌ Search 的 hits/dirty 不落盘 |
  | timers 调度器 | ✅ | ❌ 不启动 |

  （审批默认值差异是有意的：lyon TTY 默认问人、lyond 默认 auto-approve 对应
  non-TTY run，保留。）
  - 验收：lyond 侧 MCP 工具可用、`-model` 不钉时 subagent 走 routing、
    同一 workspace 两边配置行为一致。
- [x] ~~TUI/web 斜杠命令漂移收敛~~ → 2026-11-10 TUI 移除后漂移不复存在
  （web `SLASH` 成为唯一命令表）；原「缺的 13 条」里演化动作入口的缺账
  改记 §2.5 新条目（驱动悬空）。`dashClassOrder` 是 evoClassOrder 唯一
  存活拷贝，不再构成手工镜像对。

### 2.6 明确暂缓 / 不在本次

- [x] ~~候选 agent 四维代际演化（断点 5 后半）~~ → 2026-10-23 重定义落地：
  `eval.EvolveChild`（代际上限 3、专长空间继承、lesson 即 knowledge）+
  竞选逐代淘汰（子必须胜父）+ Gen/Parent 复活。四维里的 prompt 维以
  lesson 文档形式进入子代空间（非系统提示注入——见 §3 重定义条）。
- [x] ~~web v1.1 面板~~ — 2026-10-28 全部落地：记忆 ✅（MemoryPanel）、
  考卷候选/血统 ✅（2026-10-26 `EvolveService` + EvolvePanel：lineage
  Gen/Parent/seats、spaces、flows、exam banks；跑考/竞赛仍 TUI）、
  任务市场 ✅（2026-10-28 `GetEvolveState` 扩 market_stats/market_recent，
  面板市场区 = TUI /market 的分类价目表 + 最近 20 单台账（失败单带 err
  保留、不入价），/market 进 web 斜杠族开同一面板）。

### 2.7 阶段推进

- [ ] **ROADMAP 阶段 2 启动**：2.1 月度《模型竞技榜》、2.2 狗粮叙事、
  2.3 分发渠道（HN/V2EX/清单收录）、2.4 版本节奏。前置：阶段 1 主体 + Release 通道。

---

## 3. 已完成对照（免重复盘点）

- **进化重定义五件套（2026-10-23，设计见 agent-evolution.md §7）**：
  ①动态项目评测集（`pkg/eval/project_exam.go`，指纹再生成 + 并发 RunExam +
  考核时钟捎带 project 类）；②专长空间与共识共享（`pkg/space`：knowledge +
  skills 各自空间、背书≥2 晋升 commons、`space` 工具、SetAgent 身份、
  commons 技能进 system prompt）；③自己进化（`eval.EvolveChild` 代际≤3、
  空间继承、诊断/考挂双触发、竞选子胜父否则删档、胜者 ShareAll）；
  ④场景路由收口（routingSwap + adoptAgent 身份切换，⇄ 行点名 agent）；
  ⑤流程固化 v2 脚本（`pkg/flow`：freeze 成 .json+.sh、RunScript 零 AI、
  `flow` 组合单元工具、系统提示挂 frozen-workflows 段）。
- **EVOLUTION-GAPS 任务 A–E 全闭环**（= ROADMAP 0.1）；断点 1–4 均有回路。
- **ROADMAP 0.2** lyon.toml 落地（`pkg/config`：手写 TOML 子集解析 +
  home/workspace 双层覆盖 + mcpServers 对齐 + 测试）。
- **ROADMAP 0.3** 主体：`demo/demo-60s.sh` 四会话 14 断言 14/14（0268d8e）；
  GIF 与 demo 整体随 2026-11-10 TUI 移除退役（原「见 2.3」的 GIF 条目已删）。
- **HANDOFF A 12 项中 9 项完成**：#1 plan 常驻 / #2 queue 插队 / #3 run dump /
  #5 文档两轮 / #6 快捷键 legend / #8 双定时器 / #9 日志 / #10 dashboard /
  #11 alt+o 实时输出。剩 #4 web（当前主线）、#7 分屏（2.1）；#12 报错清单归 C 节。
- **HANDOFF B 评审 8 项**：#1 confine symlink、#2 lyond 鉴权、#3 writeLoop Close、
  #4 WaitDelay 已入库；#6 挂在 C-1；#7 尾巴挂 memory 冻结；#5 tui 拆分未动；
  测试缺口（memory/mcp/hooks）已补齐。
- **HANDOFF C**：C-2 TestKickScoreFilesCard 已修；F 两问均已解决
  （protobuf 参考仓库 → web plan 调研；dashboard 数据源 → 94ce8f0/59dcf32 落地）。
- **ROADMAP 1.4/1.5**：search_files、read_file offset/limit、SKILL.md 读取、
  `.claude/settings.json` hooks 解析均已做。
- **记忆冷启动死锁修复**（`61931c6`，2026-10-19）：`MemoryFacts` 热层空时也注入
  写路径引导（此前空仓 → 无引导 → 模型永不调 save_memory → 永远空仓，实证
  86 轮会话零调用）；顺带修回放缺陷——`synthSnapshotEvents` 曾把 memory 注入块
  伪造成 user_message 重放（restore/rollback），现由 `tools.IsInjection` 过滤。
- **memory 无主套件认领**（`432fea2`，2026-10-19 拍板）：Open() per-workspace
  首开锁（磁盘 I/O 不再阻塞全局 storesMu）+ Save 从 O(N) 重建降为 O(单条)
  upsert + 配套 perf/隔离测试；-count=3 连绿。
- **web v2 全量交付（plan `snazzy-orbiting-cosmos` Phase A–F）**：GetHistory 历史
  直渲（7aa9b5f，修「点旧会话不渲染」）、审批接线（lyond `-approve` + 真
  WatchPending/Decide + 前端卡）、转录渲染、面板与斜杠命令、workspace 写操作、
  视觉收尾。
- **2026-10-19 收尾六件**：SidePanel steps 重复 key（execId=0 撞车）、vite proxy
  上游连接回收（浏览器 abort 不再钉住 lyond 半边）、reaped 会话点击 UX
  （refresh 清选 + 流 NotFound→刷新联动）、`Decide.remember` 落
  `$LYON_HOME/approvals.json` 持久化（proto TODO(v2) 兑现；早期误写
  `policy.json`（那是 `hooks.Policy` 的操作员安全门）导致 run_shell 永久免审，
  7a50da4 起改为按 seat 隔离的独立账本、坏账降级空）、
  C-4 核实（见 §2.4）、本文件回写；另修 approval 测试债——TestMain 全包共享
  LYON_HOME 使早期 remember 写共享 ledger 误放行后续测试，fixture 改为无条件
  钉私有 home（`TestApprovalRememberPersists` 钉住契约）。
- **近期主线提交**：81e0561 timer OS 注册双缺陷、576fe3c fork-storm floor、
  94ce8f0 dashboard 展示层退役 + sysinfo 10s 可调、59dcf32 header 四格、
  0268d8e demo-60s、4ff59cc memfix D2。
