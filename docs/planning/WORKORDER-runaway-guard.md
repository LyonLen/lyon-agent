# 工单：把「跑飞」修进代码，并删掉 dashboard

> 交给执行者（Claude Code）的施工单。按 §4 的提交顺序做，每节都有可验证的验收；
> 行号是 2026-11-16 的定位提示，**以符号名为准**（改完会漂）。

## 0. 为什么是这三件事

从本机 `~/.lyon/dumps/` 的 689 个 run dump 实测：

| 事实 | 数字 |
|---|---|
| 总 run 数 | 689（2026-10-16 → 2026-11-16） |
| 失败 | 54 = `interrupted` 39（用户主动打断）+ 配置/网络错 15（401 / 503 model_not_found / 端口连不上 / 400） |
| 跑飞实例 | 迭代 174 轮「工作区还有很多没提交的东西」、117 轮「我发现会被截断」、116 轮「一张图片都没有？」、86 / 81 / 77 轮 |
| 单次成本 | 中位数 8557 token；top10 run 占全量原始 token 的 65% |

两条结论决定了方案：

1. **没有一类失败需要事后翻时间线才能定位**——39 次是自己打断，15 次是配置错，
   `prompt_err` 一行字符串就是全部信息。为它们建回放面板是给错字配显微镜。
2. **跑飞是代码问题，不是运维问题**：`pkg/agent/config.go` 的
   `defaultMaxIterations = 0`（主循环 API 调用**默认无上限**），同一份文件里子代理
   上限是 12。作者的理由写在 `config.go` 的注释里：主循环「a main-loop user drives
   the session and can see it stall」——但实测证明，用户只能意识到「截断了？」「死循环了？」，
   看不到轮数，处置方式是打断重开（那 39 次 interrupted 就是证据）。

所以：**在主循环装一个「无进展」守卫（§1），删掉只为这 689 个文件建的 UI（§2），
可选给 agent 一个自己读 dump 的工具（§3）。** 不新建任何面板。

**不要做的事**：不要建 dashboard 面板 / pane / 路由；**不要动 `MaxIterations`**
（默认 0 是刻意的设计立场，`config.go` 注释有论证——而且这个仓库真的试过把它设成 **5**，
结果正常的长任务被物理截断，用户只能反复重开 prompt 重做一遍；所以本工单的任何判据都
不得以「轮数」为尺度）；不要给 dump 加新 RPC；不要改 dump 的落盘格式
（`pkg/agent/dump.go` 的 clip/schema 是复盘与渲染共同依赖的）；
不要在 `preTool` 里做阻塞的长操作（每一轮工具执行都会经过它）。

---

## 1. 任务 A：无进展守卫（核心，必须做）

### 1.1 接缝在哪：两个钩子，一前一后

**执行前** —— `pkg/agent/session.go` 的 `Session.preTool(ctx, e tools.ExecEvent) error`（当前 :642）：

```go
func (s *Session) preTool(ctx context.Context, e tools.ExecEvent) error {
	if err := s.modeGate(ctx, e); err != nil {   // plan 模式闸门
		return err
	}
	return s.eng.hooks.CheckTool(ctx, e.Name, e.Input)  // 本地策略 + 阻塞钩子
}
```

- 返回值非 nil 时，**模型收到的是「工具报错」这个工具结果**，不是崩溃——这是既有的
  契约（`pkg/hooks/hooks.go` 的 `CheckTool` 注释：「A non-nil error must be returned
  to the model as the tool result」）。
- 子代理走同一个闸门：`pkg/agent/subagent.go:142` `PreTool: s.preTool`。
  所以**守卫写在这里，主循环和子代理同时覆盖**，不需要第二处实现。
- `modeGate`（`pkg/agent/mode.go` 的 `func (s *Session) modeGate`）就是现成模板：
  Fire 一条 `hooks.EvSecurityBlock` 审计 + 返回 `fmt.Errorf("blocked by ...")`。照抄这个形状。

**执行后** —— 结果本身。`pkg/tools/tool.go` 的 `adaptedTool.Execute` 里，`content` 是命名
返回值，`capResults` 之后能取到最终给模型的文本；defer 块（当前 :347–365）已经在把结果
交给 `OutputObserver` 了。**这就是判定「固定点」的数据来源**：加一个同范式的
`ResultObserver`（type-assert，老 observer 不受影响——`OutputObserver` 当初就是这么加的），
`Session` 实现它，把 result 交给守卫。

> 一句话：`preTool` 决定「要不要执行」，`OnToolResult` 决定「这个调用还有没有信息量」。
> Layer 1 要两个钩子都用上。

### 1.2 Layer 1（核心）：固定点封盘 —— 用「证」而不是「猜」

判据不是次数，是**结果**。定义：一次调用是「原地打转」，当且仅当
**同 `(工具名, 规范化 input)` 调用返回了与上一次完全相同的结果** —— 这不是启发式，
这是「零信息增益」的可观测证据：结果没变，再调一次在逻辑上不可能推进。

所以第一层在**证据层面**终止，不是数到几就刹车。

**状态**（run 级，随 prompt 重置）：`last map[key]record`，`record = {resultHash string, count int, closed bool}`。

**规则**：

| 情形 | 动作 |
|---|---|
| 该 key 首次出现 | 正常执行，记 `resultHash`，`count=1` |
| 该 key 再次出现 | **必须再执行一次**（结果可能因外部状态变化而不同，这一次是给「文件被别的进程改了」的），执行完比对：结果**变了** ⇒ 视为进展，覆盖记录、`count=1`；结果**相同** ⇒ 固定点成立，`closed=true`、`count=2` |
| 已 `closed` 且工具**只读**（`Tool.ParallelSafe() == true`） | **短路，不执行**，直接把上次结果原样返回，并在末尾追加标记：`[unchanged · 与第 2 次调用结果完全相同 · 本 run 内该调用已封盘：参数与结果都没变，再调不会得到新信息。要重试请先改变状态或换参数]` |
| 已 `closed` 且工具**会改变状态**（`ParallelSafe() == false`） | **照常执行**（副作用可能是必要的），执行后重新比对：结果变了 ⇒ 解封、`count=1`；仍相同 ⇒ 保持封盘并追加同一标记 |

**为什么零误报**：正常干活的结果每次都不同——改完文件再读是新的 bytes，换词搜索是
不同的 input（不同 key），跑测试第二轮输出变了。唯一「同参数 + 同结果」的场合，
就是真的没有进展。而第 2 次**必须真的执行**才能确认结果没变，这一次执行是这套设计的
最小成本，无法再省。

**为什么这才回答「不该出现无限循环」**：封盘之后，只读工具那个调用**一次都不会再真正执行**，
模型每次拿到的是「结果未变」这个事实本身（而不是一句「被拦住了」的报错）。模型的下一步
自然转向——这是把不变量写进上下文，而不是在它身后拉闸。

### 1.3 Layer 2（兜底）：非确定性工具才需要次数

Layer 1 抓不到一类：**参数不变、结果每次都在变**的工具——`date`、带时间戳的日志尾、
随机数、网络抖动（注意 `ls` 不属此类：静止目录的 `ls` 结果不变，会被 Layer 1 抓住）。
这类每次 resultHash 都不同，固定点永远不成立。

- 判据：同 key 在本次 prompt 内**连续出现 ≥ 8 次**，且期间**没有产生任何新的 key**
  （既没换参数，也没调别的工具）——即「原地踏步」。
- 动作：`s.PushSystemNote(...)` 发一次**收尾指令**：「我会继续执行，但你已用相同参数调用
  同一个工具 8 次。请基于现有信息给出结论 / 交付当前成果。」**不置 `stop_reason`、
  不取消 run、不清历史。**
- ≥ 15 次 → 才兜底停：`stop_reason: "no_progress"`，`Err` 保持空（不是失败，与
  `max_iterations` 同语义），**历史与工具结果一律不清理**，用户可在同一会话直接追问「继续」。

阈值之所以能放到 8/15 这么宽，正是因为 Layer 1 已经把真循环在第 2 次就封了：
Layer 2 只防「结果在抖动」的稀疏情形，而正常任务不会用同一参数连调 8 次。

**这一层就是你踩过的坑的反面**：`MaxIterations=5` 砍的是**所有**轮次（正常活也被砍），
这里砍的只是**同一个调用连续 8 次仍无新 key** 的病态情形。

### 1.4 Layer 3（最便宜）：让模型一开始就不这么干

在冻结系统提示的规则区加一条硬规则（`pkg/agent/config.go:236` 的 `DefaultSystemPrompt` 常量，
「How you work」段落）：

> 结果已经在上下文里的调用，不要用完全相同的参数再调一次。要重试，先在正文里说明
> 什么改变了（文件被外部修改 / 上次超时 / 你需要不同参数）。

- 成本：一次性的 prompt cache 冷启动（`DefaultSystemPrompt` 是缓存前缀的一部分）。
- 已核对：现有测试（`mode_test.go:155`、`skills_test.go:162`）都是取 `cfg.SystemPrompt`
  的值做比较，不硬编码文本，改常量不会让它们红。
- 这一层单独不可靠（模型会漂），但和 Layer 1 叠加能显著降低封盘触发频率。

### 1.5 落地清单（按文件）

1. **`pkg/tools/tool.go`：加结果观察者**（Layer 1 的前提）
   - 新增可选接口，照 `OutputObserver` 的既有范式（type-assert，老 observer 不受影响）：
     ```go
     // ResultObserver receives the capped result text of one execution.
     type ResultObserver interface {
         OnToolResult(e ExecEvent, result string)
     }
     ```
   - 在 `adaptedTool.Execute` 的 defer 里、`OnToolEnd` 之前调用
     （`content` 是命名返回值，`capResults` 之后取 `contentText(content)`；
     位置在 `pkg/tools/tool.go` 的 `Execute` defer 块，当前 :347–365）。
   - 空结果或报错（`err != nil`）**不回调**：错误不构成固定点证据。
2. **新建 `pkg/agent/repeat.go`**
   - `type repeatGuard struct`：`mu sync.Mutex`（`preTool` 会被多个 goroutine 并发调用——
     `ParallelSafe` 工具在同一 turn 里并发跑）、`last map[string]*repeatRecord`、
     `blockStreak int`、`trip string`。
   - `func (g *repeatGuard) before(name string, input json.RawMessage) (verdict, cached string)`：
     `verdict` ∈ `repeatNone / repeatShortCircuit / repeatWrapUp / repeatTrip`；
     `cached` 只在 `repeatShortCircuit` 时非空（封盘时为 nil）。
   - `func (g *repeatGuard) after(name string, input json.RawMessage, result string)`：
     记录/比对 resultHash，产出 `closed` 状态与新 key 计数。
   - `func (g *repeatGuard) reset()`（prompt 入口调用）。
   - `func canonicalInput(raw json.RawMessage) string` 与 `resultHash(string) string`（见 §1.6）。
3. **`pkg/agent/session.go`**
   - `preTool`：在 `s.eng.hooks.CheckTool(...)` **之后**插入 `before(...)`——安全策略先跑，
     被策略拒掉的调用不该进守卫。
   - 实现 `OnToolResult(e tools.ExecEvent, result string)`（`Session` 已经是
     `tools.Observer`，加一个方法即可）→ 转给 `repeat.after(...)`。
   - `repeatShortCircuit`：不执行工具，把 `cached` 作为结果返回。注意 `preTool` 的返回值类型是
     `error`，**短路必须走别的口子**：在 `preTool` 里返回一个哨兵错误
     `errRepeatShortCircuit`，在 `adaptedTool.Execute` 里识别它并直接构造成功结果
     （或在 adapter 增加 `PreToolResult(ctx, ev) (result string, ok bool)` 钩子）。
     **执行者必须二选一并说明理由，不要发明第三种。**
   - `repeatWrapUp` → `s.PushSystemNote(...)`，返回 nil。
   - `repeatTrip` → `s.trip = "no_progress"`，返回 nil（让当前这轮正常跑完），
     并在 `prompt_end` 前的 `budgetStop` 判断（当前 :1065 附近）加：
     ```go
     } else if promptErr == "" && s.trip == "no_progress" {
         budgetStop = "no_progress"
     }
     ```
   - 守卫实例挂 `Session`：`repeat *repeatGuard`，**prompt 入口处 `reset()`**。
   - **不要**在触发时调用 `Interrupt()` / `cancel()` / `repairInterruptedHistory()` 或任何
     清理历史的动作。
4. **`pkg/agent/config.go`**：`RepeatGuard int`（0 = 关闭；默认 **8**，即 Layer 2 的收尾阈值）
   + `repeatGuard()` 解析函数，**照抄同文件 `maxIterations()`（当前 :143）的写法**：
   `LYON_REPEAT_GUARD` 环境变量；解析失败往 stderr 报一句再回退默认值，不要静默吞掉。
   Layer 1 没有阈值，不受此开关影响（固定点是事实判定，不该可关）——
   若确实要全关，`LYON_REPEAT_GUARD=off` 同时关两层。
5. **`pkg/hooks/hooks.go`**：新增审计事件 `EvRepetition = "repetition_block"`
   （挨着 `EvSecurityBlock`，当前 :55），`BlockedBy: "guard:repeat"`，
   `Detail` 写证据（`identical result ×2` / `short-circuited`）。
6. **前端一处小改（建议做）**：`web/src/components/ChatView.tsx:539` 已有
   `if (ev.stopReason === "max_tokens")` 分支，照它加 `no_progress`
   （文案：`这次 run 判定为原地打转，已停止 · 历史完整，可继续追问`）。
7. **文档**：`README.md` / `README.en.md` 环境变量表加 `LYON_REPEAT_GUARD`；
   `docs/manual.html` / `docs/manual.en.html` 同表（当前 :375 附近）同步。

### 1.6 取舍（照写，别自作聪明扩范围）

- **第 2 次一定要真的执行**。想省掉这次、直接看「参数是否重复」就封盘，会误杀
  「文件刚被外部改过，我要重读」这类正当行为。一次执行换零误报，值得。
- **短路的只有只读工具**。写文件/shell 有副作用，即使结果相同也不能替用户跳过。
- **不比较流式增量**。`OnToolResult` 拿的是 `capResults` 之后的完整结果文本；
  流式工具的中间 chunk 不参与 hash（`OnToolOutput` 不动）。
- **规范化**：input 与 result 都照 `pkg/tools/stable.go` 的 `stableSchema` 思路做
  `marshal → map[string]any(UseNumber) → marshal → sha256[:16]`。result 不是 JSON
  （普通文本）时直接 `sha256` 全文，不做 JSON 化。
- **守卫不是修 bug**。根因（上下文压缩后反复重试、图片附件路径没生效）该单独修；
  这一层只保证「同参数同结果」的循环在第 2 次结束。
- **子代理与主循环共用一份状态**：两者都过 `preTool`（`subagent.go:142`），
  共用 Session 上的同一个守卫。扇出里各子任务参数通常不同，不会互相误伤；
  若将来需要隔离，按 `ExecEvent.Sub` 分桶即可，现在不要提前做。
- **不要用轮数**。`MaxIterations` 保持默认 0；这个仓库已经证明 5 会砍断正常长任务。

### 1.7 验收（`go test ./pkg/agent ./pkg/tools -race -run 'TestRepeatGuard|TestResultObserver'`）

需要一个**假工具**：`fakeTool{name, results []string}`，按调用次序返回预设结果，
`ParallelSafe` 可配。用例：

| 用例 | 断言 |
|---|---|
| `TestResultObserverSeesCappedResult` | `OnToolResult` 收到的是 `capResults` 之后的文本；`err != nil` 时不回调 |
| `TestRepeatGuardClosedOnIdenticalResult` | 同 key 调 2 次、结果相同 → `closed`；第 3 次 `before` 返回 `repeatShortCircuit` 且工具**未被调用**（假工具调用计数 = 2） |
| `TestRepeatGuardReopensWhenResultChanges` | 第 2 次结果不同 → 不封盘；第 3 次照常执行 |
| `TestRepeatGuardMutatingNeverShortCircuits` | `ParallelSafe=false` 同结果 5 次 → 每次都真的执行（计数 = 5），但结果尾巴带 `unchanged` 标记 |
| `TestRepeatGuardWrapUpAtEight` | 假工具每次结果都不同（模拟 date）→ 第 8 次后 `pendingNote` 含「给出结论」，且本次 prompt **未结束**（StopReason 为空） |
| `TestRepeatGuardTripsAtFifteen` | 同上到第 15 次 → `EventPromptEnd.StopReason == "no_progress"` 且 `Err == ""`，历史条数不变（断言 `Export()` 前后一致） |
| `TestRepeatGuardResetsPerPrompt` | prompt1 封盘后，prompt2 同 key → `repeatNone` |
| `TestRepeatGuardOff` | `LYON_REPEAT_GUARD=off` → 两层都不触发 |
| `TestRepeatGuardCanonicalInput` | 键序/空白不同、`1` vs `1.0` 的等价 JSON → 同一 key |
| `TestRepeatGuardConcurrent` | 8 goroutine 并发 `before/after` 同 key → 状态一致（`-race` 干净） |
| **`TestRepeatGuardLongLegitRunNeverTrips`** | 连续 40 次 `read_file` **不同** path（模拟正常长任务）→ 全程 `repeatNone`，一次提醒都没有 |

最后一条是这个工单的回归测试：它就是 `MaxIterations=5` 那个坑的显式反面。

模板：`pkg/agent/mode_test.go` 顶部 `modeTestSession(t)`；`DefaultConfig()` +
`cfg.Workspace = t.TempDir()` + `cfg.WebToolMode = "off"` 让工具集确定。

端到端冒烟（手工）：让它读同一个文件——第 2 次后应看到结果尾部的 `[unchanged · 已封盘]`，
第 3 次起不再有工具执行事件（steps 面板能看到），run 正常结束、`failed: false`、
历史完整、可继续追问。

## 2. 任务 B：删掉 dashboard（UI + 只服务它的三个 RPC）

先明确**留什么**：

- `GetOverview` **留**：`web/src/components/EvolvePanel.tsx:46` 用它渲染路由表与榜单。
- `ListSnapshots` **留**：`web/src/App.tsx:198` 的「已保存会话」弹窗用它。
- `RoutingEntry` / `LeaderboardView` / `GetOverviewRequest|Response` /
  `SnapshotRef` / `ListSnapshotsRequest|Response` **全部留**。
- `pkg/agent/dumpread.go` **全部留**（§3 要用；本次不动）。

删 `ListRuns` / `GetRun` / `ListAudit` 三个 RPC 及其消息类型。

### 2.1 前端

- 删 `web/src/components/DashboardPanel.tsx`（311 行）。
- `web/src/App.tsx`：
  - 删 `const DashboardPanel = lazy(...)`（当前 :30）与那条注释。
  - 删 `const [view, setView] = useState<"chat" | "dashboard">("chat")`（当前 :42）。
  - 删侧栏两个导航按钮 `sessions` / `dashboard`（当前 :1105–1117）。
  - 删 `{view === "dashboard" ? <Suspense …>…</Suspense> : ( … )}` 这个三元
    （当前 :1127–1141），直接渲染 `<TilingWorkspace ws={ws} panes={panes} shakeLeaf={shakeLeaf} />`。
  - **保留** `dashboardClient.listSnapshots(...)`（:198）与 `dashboardClient` 本身。
  - 若 `Suspense` 因此变成未使用 import，一起删掉。
- `web/src/index.css`：**不要删 `.dash` 系列规则**——`EvolvePanel.tsx:71` 也在用
  `className="dash"`。只删确认「只有 DashboardPanel 用过」的孤立类。已核对，
  可删（行号约 :1049–1085）：`.run-replay`、`.run-head`、`.ev-list`、`.ev-seq`、
  `.ev-type`、`.ev-body`、`.ev-input`、`.audit-time`、`.day-input`（含 `.dash h3 .day-input`）。
  **保留**：`.failed`、`.placeholder`、`.run-prompt`、`.k`/`.v` —— 别处都在用。
- 删完自检：`grep -rn 'className="dash' web/src` 应只剩 `EvolvePanel.tsx`。

### 2.2 proto + 生成（两端必须一起提交）

- `proto/lyon/v1/dashboard.proto`：
  - 删 3 个 rpc：`ListRuns` / `GetRun` / `ListAudit`。
  - 删 9 个消息：`RunRef`、`ListRunsRequest`、`ListRunsResponse`、`DumpEvent`、
    `GetRunRequest`、`GetRunResponse`、`AuditRecord`、`ListAuditRequest`、`ListAuditResponse`
    （共 9 个，全部只被那 3 个 rpc 引用）。
  - 保留：`RoutingEntry`、`LeaderboardView`、`GetOverviewRequest|Response`、
    `SnapshotRef`、`ListSnapshotsRequest|Response`。
  删完 `import "lyon/v1/event.proto"` 变成未使用（`DumpEvent` 是唯一消费者：
  它引用了 `Usage`），`buf lint` 会报，删掉这行 import。
- 重新生成（`buf.gen.yaml` 头部写的命令，`buf.exe` 已在 `$(go env GOPATH)/bin`）：
  ```bash
  PATH="$(go env GOPATH)/bin:$PATH" buf lint && buf generate
  ```
  Go（`gen/go/lyon/v1/dashboard.pb.go`、`gen/go/lyon/v1/lyonv1connect/dashboard.connect.go`）
  与 TS（`web/src/gen/lyon/v1/dashboard_pb.ts`）**两边都要提交**——只落一边会让另一边编不过。

### 2.3 服务端

- `pkg/server/connect_dashboard.go`：删 `ListRuns` / `GetRun` / `ListAudit` 三个方法、
  `dumpEventProto`，以及只被它们用到的辅助（`dashDay` 在删完后应无消费者 → 删；
  `dashErr` 若 overview/snapshots 仍在用则保留，`go vet` 与编译为准）。
- `pkg/server/connect_dashboard_test.go`：删 `TestConnectDashboardRuns`（当前 :124）
  与 `TestConnectDashboardAudit`（当前 :194）两个函数；保留
  `TestConnectDashboardOverview`（:81）与 `TestConnectDashboardSnapshots`（:252）。
- `pkg/server/connect_settings.go:72`：`{"LYON_DASHBOARD", "dashboard 开关"}` ——
  **代码里没有任何地方读这个环境变量**，是死文档。删掉这一行
  （或改成 `{"LYON_REPEAT_GUARD", "重复调用守卫，0/off 关闭"}`，与 §1 对齐）。

### 2.4 文档

- `README.md`：删或改写 :27（特性列表的「另有 Dashboard 总览视图」）、:54、
  **:157–162（整节 `### Dashboard（总览视图）`）**、:213（文件树里的 DashboardPanel）、
  :409（「Web 的 Dashboard『审计尾』直接读它展示」→ 改成说明审计日志路径即可）、
  :613、:646（「路由表与榜单在 Dashboard / evolve 面板直接可读」→ 只留 evolve 面板）。
- `README.en.md`：对应行 :34、:73、**:195（整节）**、:257、:273、:491、:746、:785、:835、:860。
- `docs/manual.html`：:132（「顶部可切换 dashboard 视图」整句删）、:375（`LYON_DASHBOARD` 行）、
  :492（文件树里的 Dashboard 字样）。`docs/manual.en.html` 同步。
- 本机残留的 `~/.lyon/dashboard.json`（clock/weather/stocks 那套已移除 TUI 的配置）
  不在仓库里，**不属于本工单范围**。

### 2.5 验收

```bash
go build ./... && go vet ./...
go test ./pkg/server ./pkg/agent ./cmd/...
cd web && npx tsc --noEmit && npm run build
PATH="$(go env GOPATH)/bin:$PATH" buf lint
```

- `git diff --stat` 里 `gen/go/` 与 `web/src/gen/` **同时**有改动。
- 手工起 `lyond` + `npm run dev`：① 侧栏没有 dashboard 按钮 ② evolve 面板的路由表
  与榜单照常（`getOverview` 未受影响）③ 已保存会话弹窗仍能列出快照
  （`listSnapshots` 未受影响）④ 控制台无 404/未实现 RPC 报错。

---

## 3. 任务 C（可选，建议做）：让 agent 自己读 dump

删掉 UI 之后，「我上次哪次跑飞了」这个问题该由 agent 回答，而不是由人翻页面。
数据读取函数已经现成：`pkg/agent/dumpread.go` 的 `ListRunDumps(day string, limit int)`
与 `LoadRunDump(runID string)`。

**注意 import 方向**：`pkg/tools` 不能 import `pkg/agent`（`pkg/agent` 依赖 `pkg/tools`）。
所以这个工具写在 **`pkg/agent` 侧**，用 `tools.RegisterFunc`（`pkg/tools/register.go:20`）
注册到 engine 的 registry 上——`pkg/agent/engine.go` 的 `NewEngine`（当前 :152 建 registry、
:191 附近是 builtins 列表）就是注册点。

```go
// pkg/agent/runstool.go
func registerRunsTool(reg *tools.Registry) error   // 在 NewEngine 里调用
```

工具名 `runs`，`parallelSafe = true`（只读），两个 op：

- `op=list`：`day`（可空 = 今天）、`limit`（默认 50）、`failed`（bool）、
  `sort`（`iterations` | `cost`）。返回 markdown 表格：run_id、iterations、
  `input`/`output`/`cache_read`、stop_reason、prompt 前 40 字。
  **排序必须能按去缓存成本**：`input - cache_read + 4*output`（4 倍是输出比输入贵的
  粗略假设，输出里要写明这是估算，别当精确账单）。只按原始 token 排会骗人：
  实测「一张图片都没有？」原始 2327 万（第 2），去缓存只有 29.6 万当量（第 8）。
- `op=read`：`run_id` → 返回**摘要而非全文**：`usage` / `sub_usage` / `stop_reason` /
  `iterations` / `prompt_err`，从 `Events` 里提 `turn_end` 拼出逐轮表
  （turn、stop_reason、usage——注意 `RunDumpDoc` 没有 `Turns` 字段，逐轮必须从事件里算），
  再列出所有 `is_error` 事件与重复出现的 `(tool_name, input 前 120 字)`。
  **硬性上限**：单次返回 < 16 KB。最大的单个 dump 有 4.6 MB / 30296 个事件，
  整档回吐会直接把上下文打爆。

验收：

- `TestRunsToolListSortsByIterations`（造 3 个 dump 文件，断言行序）
- `TestRunsToolReadCapsOutput`（对最大的 dump 断言返回长度 < 16 KB 且含逐轮表）
- `TestRunsToolBadDayBadRunID`（`../..` 与 28 位格式外 → 报错不 panic；复用
  `dumpread.go` 的 `validDay` / `validRunID`）
- 手工：在会话里问「我最近哪次 run 迭代最多」，工具被调用并答出 174 轮那次。

---

## 4. 提交拆分

| commit | 内容 | 可独立发布 |
|---|---|---|
| 1 | `pkg/tools` 的 `ResultObserver` + 一个 fake-tool 单测（纯增量，没人用也不改变行为） | 是 |
| 2 | §1 Layer 1 固定点封盘（`repeat.go` + `preTool`/`OnToolResult` 接线）+ 测试 | 是 |
| 3 | §1 Layer 2 次数兜底 + Layer 3 系统提示一条 + `LYON_REPEAT_GUARD` + 文档 | 是 |
| 4 | §2 删 dashboard（前端 + proto + 生成 + 服务端 + 文档） | 是 |
| 5 | §3 `runs` 工具（可选） | 是 |

commit 1 与 2 分开的理由：`ResultObserver` 是纯接口增量，先落地、先被 review，
再接上改变行为的判据——出问题时能一眼定位是「观察者错了」还是「判据错了」。

如果只做一件，做 **commit 2**（Layer 1）：它把「同参数同结果」的循环在第 2 次就封盘，
是这套设计里唯一「结构性」的部分，其余都是兜底。做完观察一两周，再决定 3/4/5 是否还要做。
`docs/planning/BACKLOG.md` 里可各记一行，做完勾掉。