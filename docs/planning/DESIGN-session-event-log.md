# 设计：把 runs / steps 并成一份会话事件日志

> 照 DeepSeek Harness（`deepseek-ai/deepseek-harness`，简称 dsh）的做法重做
> lyon 的「总账 + 流水」。**这不是改面板，是改数据模型** —— 面板只是最后的投影。
>
> 依据（我实读过的两页）：
> `https://deepseek-harness.github.io/deepseek-harness/reference/`（架构）
> 与 `.../reference/agent-lifecycle`（Agent 轮次与步骤生命周期）。
> **只照搬"一份日志 + 投影"这一条思路，不照搬它的 Cordis 插件框架**——那是一个
> 独立的大工程，与本题无关。

---

## 0. 一句话

dsh 里没有"运行记录"和"步骤记录"两套数据：只有一份**只追加的会话事件日志**，
`turn`（一次提问，含零或多个 `step`）和 `step`（一次模型请求 + 它调用的工具）
都是日志里的事件；界面是这份日志的**投影**。

lyon 现在是三份事实、三种格式、四个消费者。本设计把它们并成一份。

---

## 1. 现状：三份事实

| # | 是什么 | 落在哪 | 谁在读 |
|---|---|---|---|
| A | 对话历史（模型看到的消息） | `$LYON_HOME/sessions/<hash16>.json` 的 history 快照（`settleLocked`，`session.go:1128`） | 恢复会话、构建下一次请求、转录渲染 |
| B | 单次 run 的成文（事件时间线 + 用量 + history 尾巴） | `$LYON_HOME/dumps/<日期>/<HHMMSS>-<id12>.json`（`dump.go` 的 `writeRunDump`，:381）；本机现存 689 个，最大一个 4.6 MB / 30296 条事件 | dashboard 的 `ListRuns`/`GetRun`（`connect_dashboard.go`） |
| C | 安全/审批审计 | `$LYON_HOME/audit/<日期>.jsonl`（hooks 独立写） | dashboard 的 `ListAudit` |
| D | 实时事件流 | 内存 hub：单调 `seq` + 16384 条 ring（`pkg/server/hub.go`，`hubRingSize` :92；溢出即 `ErrRingGap` :96） | 浏览器（SSE/Connect 订阅）、转录重建 |

三个后果：

1. **同一件事写三遍**（A 的消息、B 的 history 尾巴、D 的流），格式各不相同，任何一处改了要同步三处。
2. **dashboard 是第三份读取路径**：它绕开 A 和 D，直接从 B 的磁盘文件重新拼装视图——所以它必然是"另一个界面"，不可能和聊天界面共享语义。
3. **重放有天花板**：ring 只有 16384 条，溢出后客户端必须从 A 重建，A 里又没有工具级的细节（谁调了什么、花了多少）——细节只存在于 B，而 B 是 run 边界成文的，不在会话时间轴上。

---

## 2. 目标模型

### 2.1 词汇

- **turn**：一次人提交的提问。从领取输入开始，到不再欠任何工作为止。
  （lyon 现在叫 run / prompt；日志里统一叫 `turn`。）
- **step**：一次模型请求 + 它调用的一批工具。一个 turn 由 0..N 个 step 组成。
- **settlement**：一个不可再变的结算事实（模型消息、工具结果、轮次边界）。
- **chunk**：流式增量（文本/思考 delta、工具实时输出）。**瞬时，永不入日志**。

### 2.2 三域（照 dsh 的划分，语义直接可搬）

| 域 | 内容 | 是否持久 | lyon 对应 |
|---|---|---|---|
| **会话事件** | 消息、工具结果、turn/step 边界、用量 | 是（日志） | 现在的 SSE 事件主体 + dump 的事件 |
| **实时事件** | 收件箱、当前步骤、状态、请求准备、打断 | 否（内存） | hub 的 ring + session 的内存态 |
| **能力事件** | 安全策略、审批、遥测 | 是（各自） | hooks 的 audit（保持独立，见 §6.3） |

### 2.3 铁律（dsh 的原话："模型可见即已记录"）

> **每个模型请求都必须能从日志重建。**

落成三条可检验的约束：

1. 凡是能让模型看到不同内容的东西，必须对应一条日志事件（含压缩摘要替换、记忆注入、
   技能注入、队列插队、图片粘贴）。
2. 日志是唯一事实源；history、转录、步骤面板、run 账本**全部是投影**。
3. 流式 chunk 不进日志（否则体积失控且无回放价值）；它只活在实时域。

### 2.4 终止条件不需要改

dsh 的循环是**欠账驱动**：还欠工具请求或有新输入就再来一步，否则关 turn。
lyon 已经一致 —— `for !s.runner.IsCompleted()`（`session.go`）+ `empty > 2`
的"没有进展就退出"。**这块不动**，跑飞靠 `WORKORDER-runaway-guard.md` 那一层解决。

---

## 3. 映射表：dsh → lyon 今天 → lyon 目标

| dsh | lyon 今天 | lyon 目标 |
|---|---|---|
| `SessionEvent` 追加日志 | hub 内存 ring + `sessions/<hash>.json` + `dumps/<day>/*.json` | `sessions/<hash>/events.v1.jsonl`（只追加） |
| `turn/start`、`step/start`、`step/end`、`turn/end` | `prompt_start`/`prompt_end`、`turn_end`、`tool_exec_*` | 同一套事件名沿用，补 `turn`/`step` 归属字段 |
| `assistant/message` 内嵌精确紧凑 stream | `settleLocked` 存消息；dump 另存一段 history | 消息与它的组装依据同存一条事件 |
| 投影（`ctx.sessionProjections` / `snapshot()`） | 前端各自拼装；dashboard 独立读盘 | 单一投影函数：`turn → step → 工具行`，前端只渲染 |
| `agent/pre-step` waterfall | `preTool` 闸门 | 保留，等价物已具备 |
| JSONL v0 / vN + generation 不会被改名或删除 | 单文件 `<hash>.json`，直接覆盖写 | `events.v1.jsonl` 只追加；迁移时新 generation 另起文件 |

---

## 4. 数据格式

### 4.1 位置与命名

```
$LYON_HOME/sessions/<hash16>.json          ← 保留：会话索引/标题/模型等"头信息"
$LYON_HOME/sessions/<hash16>/
    events.v1.jsonl                        ← 新增：只追加事实日志
    drafts/                                ← 已存在
    20261012-140600.041.json               ← 已存在：rollback 快照
```

**命名约束（很重要）**：`pkg/agent/snapshot.go:490` 用 `strings.HasSuffix(name, ".json")`
枚举会话目录里的快照文件。日志必须用 `.jsonl` 后缀，否则会被误当成快照读进去。

### 4.2 行格式

每行一个 JSON 对象，首字段固定：

```json
{"seq":123,"ts":"2026-11-16T12:00:00.123+08:00","turn":7,"step":3,"type":"tool_exec_end","name":"read_file","dur_ms":12,"ok":true,"attrs":{}}
```

- `seq`：会话内单调递增，**从会话创建起永不回退**（今天 seq 由 hub 内存持有，
  `hub.go:37`；要挪到日志/会话，否则重启就归零）。
- `turn` / `step`：归属。`turn` 从 1 起；`step` 在 turn 内从 1 起。
- `type`：沿用 `pkg/agent/events.go` 的既有类型名（`prompt_start`、`tool_exec_start`、
  `tool_exec_end`、`turn_end`、`prompt_end`、`compaction`、`memory_injected` …），
  **不要另造一套词汇**——前端、dump、测试都认得它们。
- `attrs`：现有 `Event.Attrs` 原样落盘（记忆注入、技能命中、图片路径等都走这里）。

### 4.3 写哪些、不写哪些

**写**：`emit` 现在就已经有一张"哪些事件值得留痕"的过滤表（`session.go:1276`，用于 hooks
审计）——直接复用同一张表，把命中的事件追加到日志。豁免的类型（文本/思考 delta、
`message_start`、工具实时输出 chunk、`queue_injected`）**不入日志**。

> 注意 `queue_injected` 现在被审计豁免，但它是**模型可见的**（插队的话会进下一轮上下文），
> 按 §2.3 的铁律它必须入日志。执行时要把它从豁免名单里摘出来，只留纯流噪声。

**不写**：所有 chunk 类事件（逐条 delta 不入盘）。

### 4.4 消息事件内嵌"紧凑 stream"（照 dsh，也是对仓库自己那条 TODO 的答复）

`dump.go` 的 "Future extensions (schema_version 2)" 里已经写过一条候选：
「Full event stream: untruncated deltas/text as a sidecar `.jsonl`」。
本设计的取舍**不是**照那条做一个旁路文件，而是 dsh 的做法：

> 每条 `assistant` 消息事件**内嵌**产生它的那段紧凑 stream（文本与思考片段 + 各自时间戳，
> 单段裁剪上限照 `dumpEventCap` = 2000 字符），原始 delta 仍然不逐条落盘。

理由：一份事实、一个文件；回放时既有原文（在消息里），又不会因为逐条 delta
把日志撑成今天 dump 那个量级。仓库那条 TODO 可以就此勾掉，改写成本节结论。

---

## 5. 分阶段落地

> 每个阶段都能独立发布；P0 先做，失败就停，成本控制在一天内。

### P0 探针（半天，只读不改）

- 确认 `emit`（`session.go:1263`）确实是事件的唯一出口（工具、子代理、压缩都经过它）。
- 拿一个真实 run，把 `emit` 看到的事件序列与同一 run 的 dump 事件序列逐条比对，
  量出差异（哪些事件 dump 有、实时流没有，反之亦然）。
- **产出**：一页差异清单。若差异超过预期（比如子代理内层事件只有 dump 有），
  重新评估工作量再决定是否继续。

### P1 写日志（可独立发布，零行为变化）

- `Session` 打开时挂一个追加写入器（fail-open：写失败只累加一个 drop 计数并记一条日志，
  **绝不因为写日志失败而中断 run**——照 `hooks.Fire` 的既有原则）。
- `emit` 里按 §4.3 的表追加一行；每 `turn_end` / `prompt_end` 刷一次盘，不做每条 fsync。
- `seq` 的来源从 hub 挪到会话/日志（hub 读它）；重启后从日志尾行恢复。
- **验收**：跑一次真实任务后，`events.v1.jsonl` 里 seq 连续、turn 单调、无 chunk 行；
  单次典型 turn 的日志体积 < 50 KB。

### P2 读日志：history 变成投影（**关键一步**）

- 新增 `deriveMessages(log []Event) []Message`：从日志投影出模型历史。
- `settleLocked`/恢复路径改为调用它；`sessions/<hash>.json` 里的 history 降级为
  "旧格式只读兼容"，新会话不再写。
- **验收（本设计的核心回归测试）**：对同一会话，`deriveMessages(日志)` 与
  今天 `settleLocked` 得到的历史**逐条相等**（消息数、角色、内容、工具块）。
  这条测试不通过，后面都不用做。

### P3 dump 降级

- `writeRunDump` 改成"从日志切片导出"的**显式导出物**（默认关闭），或直接保留但
  在文档和代码注释里明确：**它不是事实源，只是给外部工具看的快照**。
- 不再有新代码从 dump 读数据（`dumpread.go` 保留给"让 agent 复盘"用，见
  `WORKORDER-runaway-guard.md` §3）。

### P4 投影进 UI：steps 面板按 turn 分组

- 前端把现在平铺的工具行按 `turn` 折叠：每次提问一行表头（提示词摘要、N step、
  token、时长、终止原因、成败），展开是这次的工具行。
- 数据源：当前会话走实时流，历史会话走日志投影——**同一个折叠函数**。
- 这一步之后，"runs 面板"这个概念自然消失：它就是同一份投影的另一层。

### P5 删 dashboard

- 删 `ListRuns` / `GetRun` / `ListAudit` 三个 RPC 及 `DashboardPanel`（详见
  `WORKORDER-runaway-guard.md` §2；那份工单可以独立先做，与本设计不冲突）。
- 保留 `GetOverview`（演化面板用）、`ListSnapshots`（存档弹窗用）。

---

## 6. 迁移、兼容与取舍

### 6.1 老数据

- 老 `<hash>.json`（含 history）与老 dump：写一个一次性导入器，把
  history → 一组 `turn/step` 骨架事件（没有工具细节就只造轮次与消息事件），
  然后 `deriveMessages` 必须能还原出同样的消息。
- 导入完成的会话改名 `events.v1.jsonl`；**旧文件不删**（dsh 的规矩：已提交的
  generation 绝不重命名或删除），下个版本再清。
- 兼容窗口：一个版本内同时支持"有日志"和"只有 JSON"两种会话；判断依据是文件存在性。

### 6.2 日志会无限增长

dsh 用 generation + 迁移 + 压缩解决。lyon 先做**最简单的**：

- 每 N 个 turn 在日志里追加一条 `snapshot` 事件（内含投影后的完整消息数组），
  投影时从最后一条 snapshot 开始回放。
- 前缀截断：`snapshot` 之前的行可以归档（移动到 `events.v0.archive.jsonl`），
  **不做原地重写**（只追加的规矩不破）。
- 先不做跨会话全局压缩；`archiveKeep` 之类的保留策略照旧只管整会话。

### 6.3 hooks 的 audit 保持独立

`$LYON_HOME/audit/*.jsonl` 是**安全取证**流（审批、阻断、模式切换），格式稳定、体积小、
与界面无关。**不要**把它并进会话日志：安全审计不该因为会话日志的压缩策略被截断。
dsh 里它对应"能力事件"，也是各管各的。

### 6.4 明确不做的

- **不引入插件框架**。dsh 的价值在"一份日志 + 投影"，不在 Cordis。lyon 的
  `preTool` 闸门已经等价于它的 pre-execute 拦截点。
- **不改循环终止逻辑**（欠账驱动已经一致）。
- **不为日志新增 RPC**：前端读的是同一份事件流/投影，不需要额外接口。
- **不做跨会话的 run 检索**：那件事交给 agent 读日志（工具化），不是 UI。

---

## 7. 验收测试清单

| 测试 | 断言 |
|---|---|
| `TestEventLogAppendSeqMonotonic` | 连续写 N 条，seq 严格递增；重启后从尾行续上 |
| `TestEventLogSkipsChunks` | delta / message_start / tool_exec_output 不落盘 |
| `TestEventLogQueueInjectedPersisted` | `queue_injected` **必须**落盘（模型可见） |
| `TestDeriveMessagesMatchesSettle` | 同一会话：日志投影出的历史 == 旧 settleLocked 历史（逐条） |
| `TestEventLogTruncatedTailTolerated` | 手动截断文件末尾半行 → 打开会话成功，忽略残行 |
| `TestEventLogFailOpen` | 写入器返回错误 → run 正常完成，drop 计数 +1 |
| `TestSnapshotEnumerationIgnoresLog` | `sessions/<hash>/` 下快照枚举不把 `events.v1.jsonl` 当快照 |
| `TestMigrationFromLegacyJSON` | 老 `<hash>.json` → 导入 → 投影，消息数与内容一致 |
| `TestCompactionSnapshotReplay` | 有 snapshot 事件时，投影从它开始，结果与全量回放一致 |
| `TestTurnStepAttribution` | 一次含子代理的 run：每个工具行都能归到正确的 turn/step |

性能门槛（放 `bench/`）：一次典型 turn（20 步、50 次工具调用）的追加写开销
< 单轮请求耗时的 1%，日志体积 < 200 KB。

---

## 8. 与其他工单的关系

| 工单 | 关系 |
|---|---|
| `WORKORDER-runaway-guard.md` | **独立、先做**。它只加一层判据，不碰数据模型；本设计依赖它的 `OnToolResult` 钩子（P1 之后的日志记录正好复用同一个结果回调） |
| 本设计的 P4/P5 | 面板与 RPC 的删除可以先在旧模型上完成（那份工单的 §2），不必等 P1–P3 |

顺序建议：`runaway-guard` → 本设计 P0 → P1 → P2（到这里收益才开始兑现）→ P3/P4 → P5。
如果 P0 或 P2 的验收过不了，**停在 P1**：你仍然得到一份可回放的事实日志，
只是 history 还没切过来——那是无损的中间态。