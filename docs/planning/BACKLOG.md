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
- web v1.1 才做（数据源清单里标 TODO）：任务市场 / 记忆面板 / 考卷候选面板（现归 §2.6）

---

## 1. 等用户拍板（晨报四件）

- [x] **无主 memory 套件处置** —— 2026-10-19 用户拍板「按推荐：修复认领」，
  已逐行评审后提交 `432fea2`（store.go Open loaders 锁 + index.go upsert 族 +
  upsert_perf_test.go，+490/-29）。Delete/Reload 全量重建防孤儿 posting、
  publish 前独占已核；`go test ./pkg/memory -count=3` 连绿。评审 B-#7 尾巴随之清。
- [ ] **CI 仓名 slug**：README badge 用的是假设 `LyonLen/lyon-agent`，待确认
  （影响 1.7 收口与 1.2 Release）
- [x] **规划类 md 是否入库** —— 2026-10-21 拍板：活账 `docs/planning/BACKLOG.md`
  入库，其余快照归 `docs/planning/archive/`；`.claude/settings.json` 属切机私有
  配置，进 .gitignore。
- [ ] **GOCACHE 是否迁 D 盘**（现 C 盘；全量测试时有 `unlinkat test.exe: Access
  denied` 构建噪声）

---

## 2. 押后队列（去重后按欠账轻重排序）

### 2.1 大规划 —— 先设计文档再动代码

- [ ] **分屏多会话 + 规划 subagent + queue 碎片合并**（HANDOFF A-7）。欠账最久，
  连设计文档都还没有；用户原话「要好好规划哦」。拆解：设计文档 → 分屏布局 →
  规划 subagent → queue 合并优化（#2 间隙插队已先行落地 d7f3527，剩碎片合并）。

### 2.2 产品硬要求收尾

- [ ] **1.6 并发规划增强 F1/F2/F3**（memory: lyon-concurrency-disposition ——
  Go 式最大化并发是产品硬要求，串行是缺陷）：
  - F1 习惯回路进 prompt：todo 计划后提示「独立项用 run_subagents 一次派发」；
  - F2 计划感知并发：≥3 独立项首条 todo 即派发，steps 面板显示「N 项并行中」；
  - F3 度量闭环：stats 记 fan-out 计数，evolve 面板加「并发度」列。
  - 验收：≥3 独立子项的任务模型首尝即扇出；stats 有 fan-out 计数；steps 可见并行指示。
  - 现状：prompt 至今只有一句老话，三项全未动。

- [ ] **记忆自动蒸馏（治本）**：`61931c6` 只破了空仓死锁（引导注入），写入仍纯靠
  模型自觉；turn 收尾自动检查本场结论并入库（或推提醒）需要机制级设计。
  - 验收：长会话结束后不需用户点名，持久结论出现在记忆库；audit 可见蒸馏痕迹。
  - 参照：web `/remember` 手动兜底归 §2.5 漂移收敛。

### 2.3 ROADMAP 阶段 1 功能欠账

- [ ] **0.3 GIF 进 README 第一屏**：`make demo-60s` 已 14/14 可跑（0268d8e），
  shot list 与 ffmpeg 两步 GIF 配方在 `demo/README-60s.md`，README 还没挂。
- [ ] **1.1 首跑体验**：`lyon doctor` 一键体检（网关/MCP/记忆库/leaderboard 摘要，
  输出可贴 issue）+ `/help` 分级（新手三问一屏：我在哪、能干什么、怎么退出）。
- [ ] **1.4 repo-map 轻量上下文**（cttags/简单解析 → system note 注入「本仓库
  地图」）。同项的 search_files 与 read_file offset/limit 已完成，只剩 repo-map 本体。
- [ ] **1.2 安装一行命令 + GitHub Release**：brew tap / curl | sh / scoop，
  tag + 三平台产物 + checksum。依赖 1.2 的仓名拍板。
- [ ] **1.3 git 即后悔药**：workspace 改动随 session 自动 commit
  （message = session id + prompt 摘要）+ `/undo` 一键回滚。
- [ ] **1.7 仓库卫生收尾**：README badge slug 确认、docs/screens 截图换新、
  README 双语结构统一（LICENSE、CI workflow、exe 出 git 三项已落地）。

### 2.4 工程债

- [ ] **评审 B-#5：tui.go 拆分**（现约 3202 行，按 popup/render/slash dispatch 分域）。
  「tui.go 只减不增」红线使它必须单独排；拆完全量 3 连绿才算验收。
- [ ] **C-1 bench TestLoad/overhead 全量并行 flaky 根治**：3 轮取最小的缓解不够，
  方案待选（`go test -p 1` / 绝对阈值+环境倍率）；CI 现以 known-Flaky 注记妥协。
- [ ] **全量测试 3 连绿验收**（HANDOFF D-④，历次从未达成：三次跑三种结果）。
- [x] **C-4 pkg/eval TempDir `001` 空目录 RemoveAll 竞态** —— 2026-10-19 核实
  已被现行覆盖：`scratchHome` 带 40×50ms 重试 RemoveAll + 扫描器 `001` marker
  注释，`go test ./pkg/eval -count=3` 连绿，无需再修。
- [ ] **hooks SessionStart 触发点**（EVOLUTION-GAPS 遗留的接入点问题）。

### 2.5 入口对齐 —— 核心闭环不能只对一个入口生效（2026-10-16 盘点）

内核单份（`pkg/agent` 的 ReAct/工具/记忆/hooks、`pkg/eval`、`pkg/routing`
都是库，lyon/lyond/web 同一个 `agent.NewEngine`），**但装配层双份、触发点错位**：

- [ ] **评分/考试触发下沉 `pkg/agent`**：`kickScore`/`kickContest` 现在是
  `tuiModel` 的方法（`cmd/lyon/evolve.go:575`，触发点在 TUI 事件环的 prompt_end）
  ——走 lyond/web 的会话跑完不入榜不考试，「越用越强」只对一个入口生效。
  应挪到 session 收尾路径（TUI 只负责显示结果）。
  - 验收：lyond 跑一条 prompt 后 leaderboard 出现该会话的卡；考试时钟照走。
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
- [ ] **TUI/web 斜杠命令漂移收敛**：TUI 24 条 vs web 11 条（web 独有 0），
  缺的 13 条里演化全家桶（/evolve /exam /contest /route /agents /flow /market）、
  /memory /remember /timer 属 web v1.1+（`ChatView.tsx:17` 已声明 evolve 系
  TUI-only）；`SLASH` 表与 `dashClassOrder`（`connect_dashboard.go:34` 注明
  「package main 无法 import，复制」）是两处手工镜像，需生成器或单一来源。
  - 验收：命令表有单一来源，web 缺项在 BACKLOG §0 有归属而非静默。

### 2.6 明确暂缓 / 不在本次

- [ ] 候选 agent 四维代际演化（断点 5 后半；EVOLUTION-GAPS 明标「不在本次」）。
- [ ] web v1.1 面板（任务市场 / 记忆 / 考卷候选，见 §0 末行）。

### 2.7 阶段推进

- [ ] **ROADMAP 阶段 2 启动**：2.1 月度《模型竞技榜》、2.2 狗粮叙事、
  2.3 分发渠道（HN/V2EX/清单收录）、2.4 版本节奏。前置：阶段 1 主体 + Release 通道。

---

## 3. 已完成对照（免重复盘点）

- **EVOLUTION-GAPS 任务 A–E 全闭环**（= ROADMAP 0.1）；断点 1–4 均有回路。
- **ROADMAP 0.2** lyon.toml 落地（`pkg/config`：手写 TOML 子集解析 +
  home/workspace 双层覆盖 + mcpServers 对齐 + 测试）。
- **ROADMAP 0.3** 主体：`demo/demo-60s.sh` 四会话 14 断言 14/14（0268d8e），
  剩 README GIF（见 2.3）。
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
