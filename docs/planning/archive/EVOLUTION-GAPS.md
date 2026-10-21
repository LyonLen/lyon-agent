# 进化机制断点分析与改造任务书

> 2026-10-15 由 lyon 探索产出，投给 Claude Code 执行。
> 背景：用户反馈「agent 并没有在进化」。经代码 + `~/.lyon/` 实际数据核查，结论是
> **进化机制的账本在记，但记完不回流到行为**——评分/考试/审计都在转，却改变不了
> 下一次执行。本文列出四个断点（带代码位置与实据）和五个改造项（按性价比排序）。

## 一、现状：agent 的机制是什么

### 核心循环（`pkg/agent`）

Session 用 Anthropic SDK 的 `NewToolRunnerStreaming` 跑工具迭代：模型吐 tool_use →
过审批（default/bypass）→ 执行工具（`pkg/tools`）→ 结果回填 → 直到 end_turn。
周边：hooks 审计（每次 prompt/tool 记 jsonl）、三层记忆注入（system prompt 前塞热
记忆）、compaction、distill（退出蒸馏写记忆）、snapshot、消息守卫（preflight.go）。

### 外圈演化机制（`docs/design/agent-evolution.md`，五子系统）

| 机制 | 代码位置 | 触发方式 |
| --- | --- | --- |
| ① 生产评分 | `cmd/lyon/tui.go` `kickScore` → `eval.JudgeRun` 盲评 → leaderboard | 每条提问结束自动（`LYON_SCORE=off` 可关）|
| ① 十轮考核 | `eval.SubmitExam`，连挂 2 次撤职 | 自动（`LYON_EXAM=off`）/ `/exam <class>` |
| ① 候选竞争 | `cmd/lyon/evolve.go` `kickContest` → `lb.Elect` | 手动 `/contest <class>` |
| ⑤ 推荐路由 | `cmd/lyon/evolve.go` `routingSwap`（tui.go:2492 提交时调用）| 自动，但需样本 ≥3 且置信 ≥0.35 且推荐 ≠ 当前模型 |
| ① 错因归纳 | `cmd/lyon/tui.go` `kickDiagnose` → `eval.SummarizeFailure` → `Diagnosis{FailedConditions, Patch}` | 每次失败自动 |
| ③ workflow 固化 | `/flow save`（TUI）；`evolve_cli.go` `BumpStreak ≥ autoFreezeStreak`（仅 `lyon run --flow` CLI 路径）| 手动为主 |
| ② 覆盖率 | `pkg/eval/coverage.go`（已实现，接线不全）| — |
| ④ 三层记忆 | `pkg/memory` + `pkg/agent/distill.go` | `/remember` / 退出时 |

demo（`demo/evolve-loop.sh`，`.demo/` 产物）里这套真转：224 轮、撤职、席位重选
——但那是 3 个模型互相竞争 + 专用 `$LYON_HOME` 的整夜实跑，不代表日常交互。

## 二、五个断点（为什么感觉不到进化）

> **共同根因：进化回路不闭环。** 五个断点不是五个独立 bug，而是同一条链断在
> 不同位置——采集端（评分/审计/蒸馏）都在正常写账本，但**没有任何一条数据从
> 账本流回下一次执行的上下文**（记忆注入、系统提示、候选配置、路由选择）。
> 改造的统一原则：每修一条断点，都要接上「写 → 读回」两端，只补写端等于
> 继续记账。

### 断点 1：单模型——没有竞争就没有进化

- 实据：`~/.lyon/routing.json` 五个任务类（coding/fix/general/research/review）的
  推荐**全部是 mimo-v2.6-flash**——网关只配了这一个模型。
- `routingSwap`（evolve.go:535）的生效条件是「推荐 ≠ 当前模型」→ 永不命中。
- 选举、淘汰、路由换手全是「多个候选里选谁」的机制，单模型下全部空转。
- 佐证：`~/.lyon/exams/` 只有 coding/general 两科、`memory/personal` 只有 1 条事实、
  `flow-streak.json` 只有 `{"code-review": 1}`。

### 断点 2：错因归纳的 patch 只显示、不回流（最重要）

- 设计（agent-evolution.md §1.3）：`failed_conditions` 应重试继承、进记忆项目层
  「同类错误全场可见」；`patch` 应落到候选 prompt/workflow 上。
- 实际：`d.Patch` 在 tui.go 里唯一的去处是**答案区显示一行**（tui.go:1998）+
  写审计日志（`hooks.EvDiagnosis`）。全仓 grep 确认：`pkg/memory`、`pkg/agent`
  **没有任何地方消费 `EvDiagnosis` 或 `Diagnosis.Patch`**。
- 后果：昨天犯的错，明天照样犯。失败教训不进任何下一次执行的上下文。

### 断点 3：记忆几乎不长、长了也看不见

- distill（`pkg/agent/distill.go`）退出时静默跑，`distillMaxFacts=6`，产出很少；
- 热记忆注入（session.go:892 `memoryText`）只在会话创建时读一次（`tools.MemoryFacts`），
  会话中途写入的记忆不影响当前会话；
- 用户侧没有「今天新学到了什么」的可见通道。

### 断点 4：workflow 从不在 TUI 自动固化

- 自动固化（`BumpStreak ≥ N`）只存在于 `evolve_cli.go:268`（`lyon run --flow` 路径）；
- TUI 里只有手动 `/flow save`，没有「同类套路连续成功 N 次 → 提示固化」的机制。

### 断点 5：候选 agent 间几乎没有差异，代际演化是空壳

- 设计（agent-evolution.md §1.1）：候选只允许在 {模型, prompt_ref, 工具白名单,
  params} 四维上不同，且带 `parent` + `gen`——失败归纳的 patch 追加重试，
  每代必须比上代分高，否则淘汰。
- 实际（`pkg/eval/candidates.go`）：`Candidate` 只有 `{ID, Model, Effort, Note,
  Gen, Parent}`，**真实生效的差异 = 模型 + effort 两个维度**；
  `prompt_ref`（每人不同提示词）、`tools` 白名单、参数差异化**字段都不存在**。
- `runContest`（evolve.go:322）给所有候选发**同一份考题**（`LoadOrSeedExam`），
  只换 `c.Model`——`/contest` 实际是「同一提示词、同一工具下的模型选型竞赛」。
- **`Gen`/`Parent` 是死字段**：全仓没有任何地方递增或写入——错因 patch 不回流
  （断点 2），代际演化整条链是断的。`self`（运行中主模型）永远在场且不可删，
  单模型时整个候选场就是 `self` 一人。

### 附：MCP / skill 的缺席（用户已做 MCP、常做 skill，但从未见到）

- **MCP 引擎在（`pkg/mcp`，514 行，stdio + streamable HTTP），但没有配置文件入口**：
  只认启动 flag `-mcp name=cmd` / `-mcp-http name=url`（main.go:133-134）；
  `buildEngine`（main.go:236）无 flag 直接短路返回空 registry。
  `~/.lyon/` 下没有 mcp.json，不读 `.mcp.json`，不读 Claude Code 配置——
  每次启动不手动传参，MCP 工具永远不注册。
- **skill 机制完全没做**：全仓 grep `skill` 零命中。用户 `~/.claude/skills/` 的
  13 个 SKILL.md 是 Claude Code 的，lyon 既不加载也不执行。
  概念最近的是 workflow（`pkg/flow`）但不等价：skill 是按需读的操作说明书
  （模型读了照做），workflow 是状态机（模型只能在节点内填空）。

## 三、改造任务（按性价比排序，可增量验收）

### 任务 A：patch 回流记忆（断点 2，优先做）

- 在 `diagDone` 处理分支（tui.go:1973 附近）把 `d.FailedConditions` 与 `d.Patch`
  写入 `pkg/memory` 的 project 层（scope=project，类目可叫 `failed-conditions`），
  并审计一条非 EvDiagnosis 的记录说明写入发生；
- 写入前查重：与既有同类事实 hash 去重（可复用 distill.go 的 fnv 去重模式），
  防止同一错误刷屏；
- 保证下一次新会话的 `tools.MemoryFacts` 注入能看到这条教训（无需改注入逻辑，
  只要写进热层或能被 MemoryFacts 捞到的层）；
- 验收：mock 一次失败 → 检查 `~/.lyon/memory/` 出现对应事实 → 新会话 system prompt
  注入块包含该事实文本；纯本地、不依赖网关。
- 测试：`cmd/lyon` 加 tui 级测试（diagDone → 断言 store 落盘），`pkg/memory` 去重单测。

### 任务 B：TUI 自动固化提示（断点 4）

- 在会话内对「同类工具序列」计数（可复用 `flow.BumpStreak` 的 key 语义），
  连续 N 次（建议 N=3，与 CLI `autoFreezeStreak` 同值）成功后在答案区提示
  「该套路可固化：/flow save <slug>」；
- 不自动 save（固化前须过考核的设计约束保留），只提示；
- 验收：连续 3 轮同 slug 流程成功 → 第 3 轮后出现提示；换任务类计数清零。

### 任务 C：让路由有东西可换（断点 1，依赖用户决策）

- 跑 `/contest fix`（或任意类）：gateway 下 flash/pro 两个档位同题竞聘，
  `lb.Elect` 落座后 routing 表出现分歧；
- 之后 `routingSwap` 才可能在提交时换手。此项主要是**运行操作**而非改代码；
- 如需代码支持：确认 `runContest`（evolve.go:322）的候选列表来自
  `eval.Candidates`（`~/.lyon/agents/*.json`）而非硬编码，缺省时给出
  「无候选，请先创建」的明确提示而不是静默失败。

### 任务 D：记忆回路补全（断点 3，与任务 A 同属「回流」主题）

背景：记忆机制架构合理（三层作用域 + BM25 倒排 + 热层注入 + 自动晋升，优于
Claude Code 的纯 CLAUDE.md 常驻、与 Letta 同思路），但两个执行缺口导致冷热分层
在体验上失效。

- **D1 热层中途刷新（核心缺口）**：`session.go:185` 在会话创建时把
  `tools.MemoryFacts` 快照进 `memoryText`，之后不再更新——会话中途
  `save_memory`/distill 写入的记忆，**当前会话永远看不到**，只有新会话生效。
  修法：`pkg/tools/memory.go` 的 save/forget 成功后，若发生在会话内，回调刷新
  `s.memoryText`（`rollback.go:190` 已有现成的重读模式可复用）或
  `PushSystemNote` 一行「新记忆已存：name」；两条路至少做一条，建议都做
  （刷新保证内容可见，note 保证「记住了」可感知）。
  验收：会话内 save_memory → 同会话下一轮提问的 system prompt 注入块包含新事实。
- **D2 检索命中不再逐文件重写盘**：`store.go` `Search()` 每次命中 top-k 都
  `atomicWrite` 重写 k 个文件持久化 hits 计数器——查询频繁时是无谓磁盘写
  （代码注释自己说「persisted lazily」但实际是同步写）。修法：hits 改为内存
  累加 + 进程退出/ToggleHot/Save 时统一落盘，或加时间窗合并写。
  验收：`Search` 路径零写盘（可用测试里包一层 fs 断言）。
- **D3 记忆的可见通道**：`/memory` 面板之外，distill 蒸馏出的事实数 + 晋升热层
  的事实名应在退出/`/remember` 时回显一行（现有 distill 是静默的），
  让「今天学到了什么」有感知。
- 已知设计边界（不必修，写明即可）：hits 只增不减、无衰减淘汰——库到几百条后再
  考虑；BM25 词面召回对改述弱——没有 embedding 服务前是务实取舍，勿引入。

### 任务 E：MCP 配置持久化 + skill 最小支持（断点 5 附注）

- **E1 MCP 配置文件**：新增 `~/.lyon/mcp.json`，格式对齐 Claude 生态
  （`{"mcpServers": {name: {command: [...], args: [...]}}}` 或 `{url: ...}`），
  `buildEngine`（main.go:222）启动时自动加载，`-mcp`/`-mcp-http` flag 作覆盖
  （flag 优先）；加载失败逐条报错不 fatal（一个坏 server 不能拖垮启动）。
  验收：写一个 mcp.json 配假 server → 启动 TUI → 工具列表出现 `mcp_<srv>_<tool>`；
  不传任何 flag 也能连上。
- **E2 skill 最小支持**：不建新子系统，先做「可见 + 可读」——
  启动时扫描 `~/.claude/skills/*/SKILL.md`（路径可 env 覆盖），把
  「name: description」清单注入 system prompt（一段，限长），
  模型需要时用 read_file 取全文；不做自动挂载、不做执行器。
  验收：提问「你有什么 skill」→ 清单里的条目被正确列出并能复述某条内容。
- 长期方向（不在本次）：候选 agent 差异补齐设计四维（prompt_ref / tools 白名单 /
  params），Gen/Parent 接上错因回流（依赖任务 A）后才有代际演化。

## 四、禁区与注意

- 不要动消息守卫（`pkg/agent/preflight.go`）——刚修完 messages[69] 事故，勿碰。
- `cmd/lyon/tui.go` 现 3015+ 行（评审待修 #5「拆分」未做），新逻辑尽量放
  `evolve.go`/新文件，别继续往 tui.go 堆。
- 全量 `go test ./...` 存在已知 flaky（bench TestLoad/overhead、
  TestKickScoreFilesCard），单跑 PASS 即可，勿因并行 flaky 反复横跳。
- 涉及 `~/.lyon/` 真实数据的写入操作先在测试里用 scratchHome 验证。
