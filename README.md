# lyon-agent

高性能通用 AI Agent，Go 编写，**服务 + CLI 双形态**。一个进程内跑完整的
agent 循环（流式输出、工具调用、上下文压缩、子代理），对外提供 HTTP/SSE 服务
和交互式 CLI；自带压力验收套件，性能阈值不达标直接 FAIL。

> 📘 **文档站**：浏览器打开 [`docs/index.html`](docs/index.html)（产品介绍 + 截图）
> 与 [`docs/manual.html`](docs/manual.html)（使用手册：上手 → 界面 → 演化机制 →
> 配置参考 → FAQ），纯本地静态页、无网络资源，双击即可读。
> **English**: [`README.en.md`](README.en.md) ·
> [`docs/index.en.html`](docs/index.en.html) · [`docs/manual.en.html`](docs/manual.en.html)。
> 界面本身也双语：`/lang zh|en` 或启动时 `LYON_LANG=zh`（详见手册「界面语言」）。

```
go build ./...          # 构建
go test ./... -count=1  # 全量测试（含 bench 验收）
go test ./bench -run 'TestLoad|TestCache|TestSSE' -v   # 性能验收（打印结果表）
```

## 架构

```
cmd/lyon            CLI：run（一次性）/ chat（全屏 TUI）/ tools（工具清单）
  tui.go             chat 的 bubbletea TUI：流式转录、滚动视口、y/n 审批浮层、thinking 折叠、斜杠候选弹层、alt+v 剪贴板贴图
cmd/lyond           HTTP 服务：SSE 流式会话、/metrics、准入控制
pkg/agent           引擎核心
  engine.go           Engine：单进程调优 HTTP 连接池、全局并发 turn 信号量、Warmup
  session.go          Session：每会话一个驱动 goroutine，串行工具锁，
                      事件流 (text_delta → turn_end → prompt_end)、压缩触发、历史组装
  compact.go          ★ 客户端压缩：渲染历史 → 独立摘要请求 → 摘要替换历史（任意端点可用）
  snapshot.go         会话快照：Export/Restore + ~/.lyon/sessions 多会话存档
                      （最新槽位 + 20 份归档轮转，/sessions /resume N /new）
  subagent.go         run_subagent：低配额模型的只读子代理
  events.go           事件协议（服务端 SSE 与 CLI 共用同一事件流）
pkg/tools           工具系统
  registry.go         注册/适配/并行安全标注
  shell.go files.go   run_shell / read_file / write_file / edit_file（工作区禁闭）
  image.go            view_image / @path 图片附件（magic 校验、大小与像素上限）
  paste.go            ★ PasteImage/PasteFile：alt+v 落盘 .lyon/paste + 超限重编码 + 轮转
  web.go              web_search / web_fetch（客户端实现，共享连接池）
  memory.go           ★ save/read/forget_memory：跨会话记忆（<工作区>/.lyon/memory.json）
  stable.go           ★ prompt-cache 字节稳定化（见下文）
  metrics.go          每工具 p50/p95 + last_error
pkg/mcp             MCP 客户端：stdio 与 streamable HTTP，工具映射为 mcp_<srv>_<tool>
pkg/server          HTTP/SSE 服务：/v1/sessions、/metrics、/healthz、会话存储与 TTL
internal/mockapi    验收用假 API：真实 SSE 帧序 + 前缀哈希 prompt-cache 模拟器
bench               性能验收套件（阈值强制）
```

默认配置：模型 `claude-opus-5-5`（`ANTHROPIC_MODEL` 可覆盖）、effort medium、
`MaxIterations=0`（单 prompt 的 ReAct 轮次**默认不设上限**，跑到模型自己收工为止；
要硬顶就 `-max-iter N` 或 `LYON_MAX_ITER=N`，到顶以
`prompt_end.stop_reason="max_iterations"` 如实收尾）、压缩阈值 0.7×200k、
子代理默认低配额档（轮次同样不设上限，跑到模型收工为止）。

## 快速开始

### CLI

```bash
export ANTHROPIC_AUTH_TOKEN=...        # 密钥，勿提交
go run ./cmd/lyon run "在 shell 里跑 echo hello 然后总结"   # 一次性
go run ./cmd/lyon chat                                      # 全屏 TUI（见下；退出自动存档）
go run ./cmd/lyon chat -c                                   # 恢复本工作区上次会话继续聊
go run ./cmd/lyon tools                                     # 列出全部工具
```

常用旗标：`-model` `-effort` `-workspace`（shell/文件禁闭根）`-approve`（shell/写
文件前人工确认；**chat 交互式默认开启**，`-approve=false` 关、管道 stdin 不弹防脚本
卡死，`lyon run` 仅显式 `-approve` 时开启）`-shell-allow echo,ls,git`（白名单）`-mcp name=cmd args`（接 MCP 服
务器）`-mcp-http name=url` `-prewarm`（首 prompt 前预热缓存）`-max-turns N`（全局
并发 turn 上限）`-web-tools server|client|off`。

**图片输入**：prompt 里用 `@路径` 引用工作区内的 png/jpeg/gif/webp，图片会作为
image block 随消息一起发给模型（`看看 @screenshot.png`；文件名含空格写成
`@"my shot.png"`；`a@b.com` 这类邮箱不受影响）。模型侧对应 `view_image` 工具，
随时可把工作区图片读进上下文——`read_file` 只读 UTF-8 文本，遇到二进制会提示改用
它。大小/像素上限按 API 限制执行（约 3.5 MB、单边 8000 px）。

**`alt+v` 贴图**（或 `/paste`）：从系统剪贴板读图——截屏、浏览器「复制图片」，
或在资源管理器里复制的图片文件——存进 `<workspace>/.lyon/paste/` 并在输入框里
插入对应 `@path`，回车即随消息发出。读取在后台线程（剪贴板可能被别的程序短暂
占用），超限图片会**先重编码缩小**再落盘（PNG 优先保真，实在压不下才转 JPEG），
目录按 32 份轮转、只删最旧，历史回放不会因此断链；剪贴板没有图时在状态行如实
说明，不留半截 token。目前仅 Windows（Win32 直读，无子进程），其他平台给出
明确提示并建议直接 `@path`。

### chat TUI

`lyon chat` 在真实终端里进全屏 TUI：流式转录（markdown-lite 高亮、代码块着色、
CJK 自动换行）、底部单行输入、跟随滚动。非 TTY（管道/重定向）或 `-plain` 时自动
退化为行式 REPL。

**分区改造**：宽终端（≥46 列 × ≥26 行）顶部固定 Lyon 大 logo（6 行 36 列的
█ 字形 `LYON` 字标 + 身份行 + 全宽分隔线），logo 之下一条全宽细线把**会话区与
输入区分隔开**（`──── 会话区 │ 输入区 ────`式的规则行）；窄终端自动降级为
「身份行 + 细线」，帧几何不变量（恰好 height 行、每行 ≤ width）照常保持。

**thinking 折叠**：思考过程按连续块分组，流式期间实时显示（带 `▾ thinking · N
lines` 头部），下一个非思考内容一到就把整块折成一行
`▸ thinking · N lines  <首行预览>`；**严格折叠**——对话末尾的最后一个思考块
在 prompt 结束时同样折起，不会因为后面没有内容而一直摊开；`/resume` 回放的
历史思考块也照折。`ctrl+t`（或 `/think`）全局展开/收起。

| 按键 | 行为 |
|---|---|
| `enter` | 发送（`/` 开头走本地命令；唯一匹配的前缀直接执行，`/st` → `/stats`） |
| `tab` | 完成候选弹层里选中的斜杠命令 |
| `esc` | 忙时中断当前 prompt（历史自动修复，可继续聊）；空闲且弹层开着先关弹层 |
| `ctrl+c` | 忙时：先中断，再按一次退出；空闲：退出 |
| `ctrl+t` | 展开/折叠全部 thinking 块 |
| `↑` / `↓` | 弹层打开时在候选间移动，否则输入历史回忆 |
| `alt+↑` / `alt+↓` | 步骤面板选步（首下选中最新，走到底回到实时跟随） |
| `alt+enter` | 展开/折叠选中步骤的完整操作 |
| `alt+←` / `alt+→` | 面板加宽/变窄（28–72，转录列同步重排） |
| `alt+e` | 打开/关闭**进化面板**（在岗榜单 + 考试时钟，等价 `/evolve`） |
| `alt+v` | 粘贴剪贴板图片：落到 `<workspace>/.lyon/paste/` 并在输入框插入 `@path`（等价命令 `/paste`） |
| `pgup` / `pgdn` / 滚轮 | 滚动转录（滚离底部即停止跟随，回底部恢复） |

**shift 变体**：`alt` 组合在 Trae 等终端里常被占用，因此步骤面板的四个键都接受
`alt+shift+…` 变体（`alt+shift+↑/↓/←/→`，以及 `alt+shift+enter`）——检测逻辑
`KeyMsg.String()` 会把 shift 修饰拼成 `alt+shift+up` 这类标签，大小写不敏感，
两种按法等价。**常驻键位提示**：状态行空闲时固定显示
`enter send · esc interrupt · pgup/pgdn scroll · ctrl+t think · alt+↑↓ steps ·
alt+e evolve · alt+v paste · /help`（`/help` 里还有完整键位表），窄终端裁掉
尾巴也不影响发送。

**busy 时排队**：prompt 正在跑时直接输入下一条问题**不再丢失**——回车即入队，
状态行显示 `queued (N) — sends when the current prompt ends`，当前 prompt 的
`prompt_end` 落地后自动按 FIFO 逐条发出（中断也照常排空）。

**界面语言**：`/lang zh` / `/lang en` 运行时切换整套界面文案（状态行、命令帮助、
弹窗描述、面板标签、审批行、console 提示），`LYON_LANG=zh` 启动即中文；
转录内容（提问、回答、工具输出）是数据，永不翻译。

**斜杠候选弹层**：输入 `/` 即在输入框上方弹出命令列表，随输入实时过滤，末行提示
`↑/↓ choose, enter runs · tab complete · esc close`；视口同步让出行数，布局不被
顶掉。`enter` 直接**执行当前高亮的命令**（一个匹配就是它，多个匹配按 ↑/↓ 挑），
tab 只是把名字补进输入框；无匹配则照常作为本地命令处理；`esc` 关闭后继续输入会
重新弹出。

**聊天区治理**：聊天转录里只有**问题、模型回答与调用活动**（工具行、轮次、
compact、审批与回滚审计）；指令说明与查询结果（`/stats` `/help` `/model`
`/memory` `/sessions` `/rollback` 列表、模式切换反馈、★ 检查点、记忆保存/同步）
渲染在**转录下方的临时输出区**（`── output ──` 分隔，最多 10 行 + `…(+N more)`
截断）。新 prompt、`esc`、下一条指令都会清空该区——永远只显示最新一次回答。
**斜杠命令留痕**：执行过的 `/xxx` 会作为一条提问行写进转录（后跟空行分隔），
向上滚动翻得到「我执行过什么」；清屏 `/clear` 会连自己的留痕一起清掉（清的就是
视图本身）。用量类信息不再逐轮往转录里打 `[turn N] in=… out=…` 行——token
账本交给 `/stats` 与右下角常驻仪表。

命令：`/stats` 用量与缓存命中表 · `/clear` 清屏（会话历史保留）·
`/resume` 恢复存档（多份时**直接弹列表选**，`/resume N` 指定第 N 份）·
`/sessions` 列出全部存档 · `/new` 开新会话（当前对话先归档）· `/think` 同
`ctrl+t` · `/steps` `/evolve` 侧面板（或 `alt+↑↓` / `alt+e`）· `/lang zh|en`
界面语言 · `/help` · `/quit`（`/memory` `/remember` `/rollback` `/route`
`/exam` `/flow` `/model` `/mode` `/paste` 见后文各节）。交互式 chat **默认**对 shell/写文件工具在状态行弹
y/n 浮层（`-approve=false` 关闭、管道 stdin 自动关闭）：`y` 一次放行 ·
**`a` 本会话该工具全部放行**（审批默认开后长任务不被同一问题打断十次）· `n` 拒绝
（超时/关窗一律拒绝，fail closed；拒绝不持久化）。`lyon run` 默认不弹，
`-approve` 开启（stdin 关闭即拒绝，fail closed）。

### 会话恢复与记忆

**会话恢复（多会话存档）**：`lyon chat` 退出时把对话存进
`~/.lyon/sessions/`（`LYON_HOME` 可整体改写存储根——容器/CI/测试不碰真实
home），**每个工作区一个槽位 + 一个归档目录**
（`<sha256(工作区绝对路径)[:16]>.json` 是最新槽位，同名不带 `.json` 的目录是
归档，各存最近 20 份，tmp+rename 原子写，空对话不写）。保存是**轮转**不是
覆盖：对话变了 → 旧槽位先挪进归档再写新的；对话没变（看了眼旧会话直接退出）
→ 什么也不写，不产生重复。所以**每一次不同的会话都永远可恢复**：

- `/resume` — 恢复存档：只有一份就直接恢复，多份**弹出选择列表**
  （↑/↓ 选、enter 恢复、esc 关闭）；`lyon chat -c` 启动时恢复最新一份；
- `/sessions` — 列出全部存档（序号、时间、消息数，新在前）；
- `/resume N` — 指定恢复第 N 份，旧会话随便翻；
- `/new` — 就地开新会话：**先把当前对话归档**（与库里重复则跳过），再清空
  历史/turn 统计/转录；忙时与 `/resume` 一样先打断、等 prompt_end 再执行。

恢复行为：TUI 转录按 prompt/回答/工具行重放并加 `restored N messages · 时间`
一行，行式 REPL 打同款提示；之后的 prompt 接在旧历史上，turn 编号延续不重置；
以空白会话开场且库里有存档时启动会提示数量与 `/resume` 用法。`/resume` 遇到
正在跑的 prompt 会先打断、等 prompt_end 落地再恢复——裸 `/resume` 此时弹出
选择列表（绝不和驱动 goroutine 抢历史锁）。保存顺序固定为 `Interrupt → Export → Save`：Export 落在历史锁上，
等在跑的 prompt 收完尾才截快照，拿到的永远是一份完整历史。

**记忆**：内置 `save_memory` / `read_memory` / `forget_memory` 把事实写进
`<工作区>/.lyon/memory.json`（按 title 无大小写 upsert，上限 500 条 / 单条
8KB，最旧淘汰，原子写）。每个会话的首次 prompt 自动注入一次
`[persistent-memory]` 快照；用 `chat -c` 恢复的会话若历史里已含当前记忆块则
跳过注入。注入走 user 消息块而非 `role:"system"`——后者在各网关上未经验证，
前者 100% 兼容。

**压缩（客户端实现，任意端点可用）**：单轮用量达到阈值×上下文窗口（默认
0.7×200k，`-compact-threshold` 调节、负数禁用）即触发：把历史渲染成带角色
标签的文本（thinking 丢弃、单块截 4000 字符），发一次独立摘要请求，再用单条
摘要 user 消息**整体替换**历史。不依赖 SDK 的 `CompactBeforeNextTurn`——网关
不返回原生 compaction block 时那条路只白烧一次请求、历史原样保留。细节：
`tool_use` 轮触发会延迟一拍（`compactArmed`），等工具结果落地后摘要才生成，
工具产出不丢；历史停在悬空 `tool_use` 上时重建 runner（旧 runner 会拿摘要去
派发被放弃的调用），否则只换 `Params.Messages` 让本轮自然收尾；进度护栏
（`lastCompactInput`）保证摘要没把用量压下去就绝不重试，失败也不会死循环。
TUI 里显示为 `📦 compaction ... (in=... out=...)`，`/stats` 与 SSE 事件流里
对应 `stop=compaction` 轮。

### 服务

```bash
go run ./cmd/lyond -addr :8080 -workspace . -prewarm
bash examples/sse-client.sh http://127.0.0.1:8080   # 验收客户端
```

| 路由 | 说明 |
|---|---|
| `POST /v1/sessions` | 建会话；带 `message` 字段则直接开跑，SSE 返回 |
| `POST /v1/sessions/{id}/messages` | 后续 prompt，SSE 流式事件 |
| `GET /v1/sessions/{id}/stats` | 逐轮 usage（含 cache_read/cache_creation） |
| `DELETE /v1/sessions/{id}` | 立即销毁 |
| `GET /metrics` | 吞吐、cache_share、工具 p50/p95、last_error |
| `GET /healthz` | 存活探针 |

服务端防御：`-max-sessions` 准入上限（超出 429）、`-session-ttl` 空闲驱逐、
`-max-turns` 全局在飞 API turn 硬顶、SSE 写超时 15s + `X-Accel-Buffering: no`。

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
吞吐/分位延迟只记录不设卡。真实端点报告（`LYON_LIVE_TEST=1`）：稳态命中率
100%，ttft p50 3.5s（网关模型，非 Anthropic 原生端点）。

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
- 网关不返回原生 compaction block——因此压缩走客户端实现（见上"会话恢复与
  记忆"），不依赖该能力，任何端点都能缩历史。

## 健壮性设计

**shell 探测**（`pkg/tools/shell.go`）：Windows 上 `exec.LookPath` 会命中 WSL
占位符 `C:\Windows\System32\bash.exe`——能解析却跑不了。因此每个候选 shell 都
真实执行一次 `echo lyon-shell-probe-ok`（5s 超时、校验 stdout 标记与退出码），
坏二进制直接淘汰；PATH 里没有 Git Bash 时还会探测
`%ProgramFiles%\Git\bin\bash.exe` 等常见安装路径。全部失败才报
`no working shell found (tried: ...)`。

**可中断 prompt**（`Session.Interrupt`）：`esc`/`ctrl+c` 取消的是本次 prompt 派生
的 context——API 流与运行中的工具一起停，会话本身存活，prompt_end 以
`interrupted` 收尾（不发红色 error 事件）。若历史停在 assistant 的 `tool_use`
上（工具没跑完就被打断，或 `MaxIterations` 在派发前切断），`repairInterrupted-
History` 会给每个悬空调用排入合成 `tool_result`，与下一条用户消息合并进同一条
user message，保证 role 交替完整、API 不会拒绝。若干净收尾却仍需补偿，说明是
迭代预算到顶（`prompt_end.stop_reason="max_iterations"`，只在设了
`-max-iter`/`LYON_MAX_ITER` 时会发生——默认无上限）：TUI 显示
`⚡ budget reached` 并提示回车继续，被跳过的 step 如实标 `over budget`。
busy 锁在发出 prompt_end **之前**
释放，避免事件缓冲消费者立刻重投时撞上 `ErrPromptInProgress`。

## 统计、审计与安全控制（hooks）

`pkg/hooks` 在每个会话生命周期上留了钩子点，并实现三个消费方——事件表里
`prompt_start/end`、`turn_end`、`tool_use/exec_start/end`、`approve_request/result`、
`compaction`、`subagent_end`、`error`、`security_block` 等都会经过：

- **审计**：`~/.lyon/audit/<日期>.jsonl`，每行一个 JSON Record（时间、事件、工具、
  截断后的输入、耗时、审批结果、token 用量、拦截原因）。写盘在独立 goroutine，
  热路径零 I/O；保留 30 天自动清理。`LYON_AUDIT=off` 关闭。
- **统计**：`~/.lyon/stats.json` 持久化聚合——prompt/迭代/compaction/subagent 次数、
  审批 yes/no、安全拦截数、token 累计、每工具调用/失败/耗时。TUI 里 `/stats`
  在原有会话表之后追加该全局段。prompt_end 落盘（≤2s 延迟），`Session.Close`
  时 `Flush` 同步收尾。
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

- **检索**：本地 BM25 倒排索引（CJK 二元分词，无需分词器），字段加权 name 3.0 / tags 2.5 / description 2.0 / body 1.0；平局时近层优先（project > personal > team）。纯内存索引，毫秒级，召回与排序强于 grep。
- **热层**：`hot: true` 的事实随会话首条 prompt 常驻系统提示（默认 4KB 预算、整条渲染不截断）。`/memory N` 切换冷热；已存事实被检索/精读命中累计 hits≥2 后，再蒸馏时自动晋级热层。
- **模型工具**：`save_memory`（按标题 upsert，`scope`/`hot` 参数）、`read_memory`（`query`=BM25 检索 / `title`=精读 / 空=紧凑列表）、`forget_memory`（按名删除）。
- **自进化（distill）**：`/remember` 手动触发或退出会话时自动跑一次——用低价位子模型（SubagentModel）从近期对话提取耐久事实入库。幂等（内容没变不重写）、限时 20s 不拖住退出、`LYON_DISTILL=off` 关闭；每次写入记 `memory_save` 审计事件。
- **云同步**：设置 `LYON_SYNC_URL`（可选 `LYON_SYNC_TOKEN`）后启用，`POST /memory/push {"facts":[…]}`、`POST /memory/pull {"scopes":["team","personal"]}`，`updated` 新者胜；只同步 team+personal，project 层永不出机。`/memory sync` 手动触发。
- **旧格式迁移**：`<workspace>/.lyon/memory.json` 首次打开时自动拆为 project 层单条事实，原文件改名 `memory.json.imported`。
- **TUI/REPL**：`/memory` 渲染编号表格（层、命中数、更新时间、摘要），`/memory N` 置热、`/memory forget N` 删除、`/memory reload` 重读磁盘（其他 agent 的改动）、`/memory sync` 云同步；plain REPL 同样支持。

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

- 每次 prompt 结束后，用低价位子模型做一次意图判定：同话题回 `{"changed":false}` 保持原题（不写不花），换题才重出标题（3-8 个词，跟随用户语言）。
- 判定输入只有「首条 prompt + 当前标题 + 最新 prompt」（几百 token，绝不带整段历史），回复上限 64 token；`LYON_TITLE=off` 关闭。
- 展示与持久化：TUI header 常驻 `lyon chat · <标题>`；`/sessions` 和 resume 选择器列出各会话标题；快照保存标题，`chat -c` 恢复后延续。plain REPL 在标题变化时打印 `🏷 title …`。
- 换题检测在 prompt_end 异步进行，UI 不等待；检查在途时新来的 prompt_end 会补跑，保证最新话题总被识别。

## 右侧会话任务执行面板（步骤审计）

- **任务树结构**：面板按**父子任务**组织，一眼看清「哪几步在解决哪个问题、新增
  的部分是为了解决什么」——每条 prompt 生成一个**分组标题行**（`▸ <prompt 压成一行>`，
  加粗、零缩进），其下的动作缩进一级；`run_subagent` 是**任务节点**，子代理内部的
  执行（事件 `Sub` 标记）再缩进一级挂在它下面，`subagent_end` 把节点收尾为
  `task · N API calls`（合并不产生重复条目）。
- **可读参数**：面板与聊天区的参数行显示模型真正要求的内容——`run_shell` 显示
  命令、`web_search` 显示 query、`web_fetch` 显示 URL、文件工具显示 path、
  `run_subagent` 显示任务——多行压成一行（`&` 之类转义还原成人话），
  不再出现 `{"command":"…"}` 裸 JSON。
- **工具配色**：shell 绿 · 子代理蓝 · 写入类黄 · web/MCP 品红 · 读取类青 ·
  prompt 标题加粗青，扫一眼就能按类别分清。
- **实时动作审计**：每步带状态标记：`⟳` 进行中（实时显示耗时）· `✓` 完成
  （带耗时）· `✗` 失败/中断。数据全部来自引擎事件流（tool_use_start /
  tool_exec_* / server_tool_result / subagent_end / compaction / prompt_end），
  use_start 记决策、exec_start 配对执行 ID、exec_end 按 ID 收尾（并行同名工具
  FIFO 配对）；prompt_end 收尾时仍在跑的步骤如实记为 interrupted/aborted。
- **可操作**：`alt+↑/↓` 在历史里走步（选中行反白、表头标 `@#n`，窗口跟随选择，
  走过最新回到实时跟随）；`alt+enter` 展开选中步骤——完整命令换行显示（最多 6 行，
  超出截断）、错误全文、开始时刻；`alt+←/→` 调整面板宽度 28–72，转录列同步重排。
  `/steps` 切换开关，`/steps open|close|width N` 与按键等价。
- **审计可追溯**：步骤号会话内连续递增（与 hooks 审计日志对得上），compact 失败、
  工具报错文本直接显示在行尾；/new /resume 重置面板（历史动作审计在 hooks 日志与转录里）。
- **几何**：窄终端（<110 列）自动隐藏且 `/steps` 无法越过 110 下限；布局为
  `[转录 tw] │ [面板 40]`，转录列最小 40 列不受挤压，帧几何不变量（恰好 height 行、
  每行 ≤ width）全程保持。

## 回滚机制（/rollback，含 compact 前回滚）

- **检查点**：每次进入 prompt 前（历史非空时）自动捕获一份完整历史副本；
  **compact 替换历史前**也捕获一份——所以 compact 永远可以撤销。
  手动标记：`/rollback save [text]`（比如动危险文件前先标一个）。
- **恢复**：`/rollback` 列出检查点（新→旧，带消息数/时间/标签，compact 标 📦），
  `/rollback N` 恢复第 N 个（与 /sessions 同序号习惯）。恢复 = 下一 prompt 从
  检查点重建历史；**轮次统计、花费、标题保留**（钱花了就是花了，只有对话回退）。
- **compact 后回滚到 compact 前**：compact 检查点存的是替换前的完整原文，
  恢复后下一次请求重新携带原始对话（`TestCompactRollback` 断言摘要彻底消失、
  原文回归）；compact 检查点在淘汰时最长寿（undo 栈上限 20，先删 prompt 检查点）。
- **安全语义**：恢复只在空闲时执行（busy 自动 interrupt 并在 prompt_end 后执行，
  与 /resume 同一延迟机制）；目标之后的检查点随之作废；未知 id 报错不 panic；
  每次恢复写一条 `rollback` 审计记录（hooks 日志）。转录保留被丢弃的行 +
  `↺ rolled back` 标记，审计痕迹不抹。

## 内置模型参数知识库（/model）

- **离线知识库**（`pkg/models`）：常见模型家族的上下文窗口、输出上限、官方推荐
  参数（温度/top_p/推理模型约束）、知识截止与**参考价**（USD/百万 token）——
  Anthropic Claude、OpenAI GPT/o 系、Google Gemini、DeepSeek、Qwen、Llama、
  Mistral、**Xiaomi MiMo（网关默认 `mimo-v2.6-flash`）**，无网络查询，编译进
  二进制。缓存命中/写入价按厂商各自记录（MiMo 命中 2%、写入限时期免费），
  未记录的条目沿用 Anthropic 的 0.1×/1.25×。
- **查找规则**：先精确匹配（不分大小写），再最长通配前缀
  （`gpt-4o-mini-2024-07-18` → `gpt-4o-mini*` 卡片而不是 `gpt-4o*`）；
  **未收录返回 nil，绝不猜窗口**——调用方回退到配置值。
- **`/model` 命令**（TUI 与 plain REPL 同款）：
  - `/model` — 当前模型知识卡 + `effective:` 行（实际生效的 model/effort/
    max_tokens/ctx 配置/compact 阈值）；
  - `/model list` — 全库表（id · context · 价格 · 家族）；
  - `/model <id>` — 查任意型号，**不切换模型**；
  - 未收录型号显示「知识库未收录 — 按配置执行，不猜参数」。
- **价格是参考价**：厂商随时调价，以官网为准（卡片明示）；价格未知的条目
  显示「未知（以官网为准）」而不是编数字。
- **为「上下文占比 + 花费」铺路**：知识库的 context 窗口与单价是后续常驻
  百分比与花费展示的数据源（Lookup(nil) 回退配置窗口）。



## 上下文占比与花费（常驻显示）

- **右下角常驻仪表**：贴在输入行最右端固定
  `ctx 68.2% · 136,400/200,000 · $0.412`——上下文占窗口百分比 + 当前占用/窗口 +
  会话累计花费（从 header 挪到右下角：打字时眼角一扫就是「本会话的油量表」）。
  仪表的座位从输入框自身宽度里**切出来**（给仪表留宽 + 3 格间隙，输入区不足
  40 格时整段让位——输入被挤比少个数字更伤）；颜色随水位走：≤60% 青、≥60% 黄
  （逼近 compact 阈值）、≥85% 红；帧几何不变量照常保持。
- **plain REPL**：每次 prompt 结束后打一行 `── context 68.2% · … tok · cost …`，
  恢复存档启动时也打（续上会话的仪表）；`/stats` 末尾附同款汇总行。
- **占用估算**：取最后一轮的请求侧（input+cache_read+cache_write）加回复——回复
  已进历史，就是下一次请求的增量；**compact 轮特判**（该轮请求侧是被丢弃的压缩
  前历史，只有它的回复即摘要留在上下文里）。窗口数取知识库（`/model` 那套），
  未收录回退配置值。
- **花费**：逐轮 input/output 按面值、cache read ×0.1、cache write ×1.25（Anthropic
  公布的倍率）计价累加；**子代理用量按模型单独记账**（`Session.SubUsage()`，常是
  更便宜的档），子模型价格在库用其价、否则跟随主模型。单价来自知识库（参考价）；
  **未收录的模型绝不编价格**——显示 `cost 未知`，或用环境变量声明网关 tariff：
  `LYON_PRICE_IN` / `LYON_PRICE_OUT`（USD/百万 token，两个都设才生效）。
- 子代理行随会话存档不落盘（恢复后续跑会补上）；`/new` 归零与 turn 统计同进同退。



## plan mode / bypass mode（/mode 切换）

- **三种执行模式**（会话级，引擎强制，TUI/REPL 同款）：
  - `default` — **人工审批**：chat 交互式默认弹 y/n（`-approve=false` 关、管道
    不弹），`lyon run` 默认自动（`-approve` 开）——bypass 对照放行的正是这层；
  - `plan` — **只读探索**：写文件、shell、记忆写入、MCP 工具与未注册工具
    一律 `blocked by plan mode`（fail closed：放行名单 = registry 的
    ParallelSafe 只读标注，`mcp_*` 因副作用未知额外排除，`run_subagent`
    因子代理只拿只读工具且过同一道门而放行）。拦截以
    `security_block(blocked_by=mode:plan)` 落审计，**不打扰审批、不触发
    外部 hook**；system prompt 同步注入 `[MODE: PLAN]` 合约（模型先知道
    规则，切换各花一次缓存 MISS，切回 default 字节复原可再命中）；
  - `bypass` — **审批自动放行**：需要审批的工具不再弹给人，直接 yes，
    approve_request/result 两条审计照写（auto-grant 必留痕）。
    **policy.json 与 blocking hooks 照常执行**——bypass 只跳过人工，
    不削弱安全底线。
- **切换**：`/mode`（裸=查状态）· `/mode plan|bypass|default` ·
  **shift+tab 循环** default→plan→bypass→default；每次切换写一条
  `mode` 审计记录（`default → plan`）。
- **常驻可见**：TUI header 徽章 `· [plan]`（黄）/ `· [bypass]`（红），
  转录落 `mode → …` 确认行；mode 不随 `/new` `/resume` 偷偷改——
  徽章一直在，切了就看得见。
- 运行中的 prompt 实时生效：下一秒的工具调用就按新模式走（原子切换，
  UI 线程与驱动线程安全）。



## OpenAI 兼容网关（协议翻译层）

- **单点翻译**：`pkg/openaix` 实现一个 `http.RoundTripper`，在 HTTP 层把 Anthropic Messages 协议⇄
  OpenAI `chat/completions` 双向翻译。工具系统、历史构建、tool runner、compact、子代理、
  标题生成……所有调用点共用一个 Engine client，**零业务代码改动**，不开启时零开销。
- **开启方式**（任选其一）：
  - `lyon chat -openai` — 默认网关 `https://token-plan-cn.xiaomimimo.com/v1`
  - `lyon chat -openai-base https://host/v1` — 任意 OpenAI 兼容端点（隐含 `-openai`）
  - 环境变量 `LYON_OPENAI_BASE`（flag 优先，尾部 `/` 自动去掉）
  - `lyond -openai` / `lyond -openai-base …` 同样支持
- **同一个 key**：请求仍带 `x-api-key`，翻译层自动换成 `Authorization: Bearer …`
  发给网关（网关若本身用 Bearer 则原样透传）；**鉴权头/Anthropic 专有头不会泄漏给网关**。
- **网关友好默认值**：`WebToolMode` 强制 `client`（网关跑不了 Anthropic 服务端 web 工具）；
  未设 `LYON_SUBAGENT_MODEL` 时子代理自动用主模型（单模型网关会拒绝第二个模型 id）。
- **协议要点**：
  - 请求：system→首条 system 消息；`tool_result` 提升为独立 `role:"tool"` 消息并排在
    user 文本之前（OpenAI 要求 tool 消息紧跟 assistant tool_calls）；相邻同角色合并；
    thinking/未知块丢弃；`tool_choice` none/any/tool/auto→none/required/function/auto；
    流式请求带 `stream_options:{include_usage:true}`。
  - 响应：网关 SSE 逐帧合成为 Anthropic `message_start/content_block_delta/message_stop`
    事件流（SDK Accumulate 严格按事件名分发，缺 `event:` 行的帧会被丢弃——全部补齐）；
    usage 在 message_start 置零、message_delta 带全四个字段；JSON 响应同样可喂流式 SDK。
  - 错误：非 2xx 保留原 HTTP 状态码（重试策略不变），body 包成 Anthropic
    `{"type":"error",…}` 结构（401→authentication_error、429→rate_limit_error、
    529→overloaded_error、≥500→api_error），`retry-after`/`x-request-id` 透传；
    200 但 body 是 HTML/错误对象时映射为 502/500 而不是静默空消息。
- **验收**：翻译单测、RoundTrip 往返（流式/非流式/错误/透传/鉴权）、真实
  anthropic-sdk-go 客户端穿层、引擎级真实 tool runner 两轮循环 e2e
  （`pkg/agent/openai_gateway_test.go`：tool_call→本地执行→role:"tool" 回传→文本收尾）。

## Agent 演化机制（评分 · 考核 · 路由 · workflow 冻结）

设计文档 `docs/design/agent-evolution.md`。四层代码：`pkg/eval`（评分/榜单/考核/
覆盖率/错因归纳）、`pkg/routing`（任务分类与推荐网络）、`pkg/models`（模型能力
画像）、`pkg/flow`（机械执行器）；接线在 TUI 命令与 `lyon run` 标志上。

### 生产评分与在岗榜单

- **四维评分** `eval.ScoreWith`：Correctness / Efficiency / Cost / Compliance
  加权出 Total，`DefaultThresholds` 四维全过才 Passed——在岗的都是达标的 Agent。
- **榜单** `$LYON_HOME/leaderboard.json` 按任务类（coding/fix/review/research/
  writing/design/data/general）分别记账：每个 Agent 保留最近 50 张记分卡，
  每张生产记录都会重新推选 on-duty（证据最新且达标的当选）。
- 生产记分来源：**普通 prompt 跑完自动入榜**——无标准答案按设计 §1.2 交
  **盲评裁判**（`eval.JudgeRun`，一次低价调用给 0..1 的 correctness）：
  TUI 每次成功 prompt_end 后台判分（步骤面板 `score` 行），`lyon run`
  收尾同款打印 `✎ score … → leaderboard [class]`；`LYON_SCORE=off` 关
  （考核轮次照记），判分失败**不入账**（宁缺毋滥）。
  `lyon run -flow` 的流程结果以 `source=flow` 入榜（correctness = 节点通过率，
  steps 对比节点数出效率分）。

### 闭卷考核（每 10 轮）

- **手写题库**：8 个任务类各 4 题，随二进制冻结分发（绝不由被考核的生产任务
  生成），首次使用落盘 `$LYON_HOME/exams/<class>.json`——可检查、可改。
- **闭卷** `ClosedBookSystem`：无工具、不读文件，只考权重里的知识；单题答案
  期望串命中或关键词过半即得分，`DefaultExamPass = 0.75`（4 题过 3）。
- **时钟**：每 10 次成功 prompt 记一轮（`ExamInterval=10`），到点且席位有人时
  自动开考（`LYON_EXAM=off` 关触发、轮次照记）；连挂 2 次 → 撤职、席位空缺，
  下一张生产记分卡重新选举。
- **入口**：TUI 自动 / `/exam [class]`（裸命令从上一条 prompt 分类）；
  `lyon run -exam auto|<class>` 先考再跑（考挂只警告，记分照存）。

### 任务分类与推荐路由

- `routing.Classify`：中英双语关键词计分（同分保留靠前者，顺序 fix → review →
  research → design → coding → writing → data → general）；`Openness(text)` 给
  任务打 0–5 开放度。
- **推荐表** `$LYON_HOME/routing.json`：每个 (class, agent) 记运行均值分与
  置信度 `mean × samples / (samples + 3)`——样本越多置信越高，新玩家要靠
  真实分数挣位置。
- **能力画像** `pkg/models`：注册表 32 个模型全部标注 Reasoning/LongHaul/
  Openness/Speed/Thrift 五维 0–5 画像。
- `/route` 裸命令 = 证据表 + 考核时钟 + 当前模型画像；`/route <text>` =
  分类 + 开放度 + 该类推荐 + 画像对比（任务开放度超过模型画像时提示换将）
  + workflow 匹配。

### 知识覆盖率门禁（设计 §2）

- **两段闭卷测验**：`GenSystem` 从任务生成可核对的 Q/A 清单 → `BookSystem`
  盲答 → 对照评分；`CoverageThreshold = 0.6` 以下算缺口。
- **定向注入**：缺口行（`问题 → 期望答案`）拼进任务正文；flow 里经 `{{input}}`
  进入**每个节点**的 prompt。门禁本身出错只记 `gate skipped`，**永不阻断**。
- **入口**：`lyon run -coverage`（普通 prompt 前置门禁并打印得分）；
  `lyon run -flow` 默认挂门（`LYON_FLOW_GATE=off` 关）；`/flow save` 强制过门
  且把缺口**烙进节点**，冻结后的文件自带这份知识。

### workflow 冻结与机械执行（设计 §3）

- **控制流是数据**：`Workflow{slug, title, trigger, nodes}`，节点退出判据
  `always | command | command 退出码 | contains 标记 | judge 窄问`；失败策略
  `retry（默认，诊断回灌后有界重试 ≤3）| abort | continue | goto:<name>`。
  状态机选节点、判出口、限重试——**LLM 只做节点内填空（`{{input}}`），不决定
  控制流**；tick 预算杀死 goto 死循环。
- **内置 4 条**（`go:embed`）：`feature-dev`（澄清→设计→实现→自测→评审→交付）、
  `bugfix`（复现→定位→修复→回归→总结）、`code-review`（盘点→逐文件→结论→
  自动修复）、`release`（状态检查→版本→changelog→构建验证→打 tag）。
  用户文件 `$LYON_HOME/workflows/*.json` 按 slug 覆盖内置；坏文件跳过不致命。
- **冻结入口** `/flow save <slug>`：把本会话的 prompt 组序列化为节点——
  前置两道门：**该任务类闭卷考核通过** + 覆盖率门禁（缺口烙入）——才落盘。
- **执行入口**：
  - `lyon run -flow <slug> "任务"` — CLI 机械执行：gate → 节点循环
    （drainText 收集模型输出、`Shell.Run` 跑 command 判据、窄 judge、
    诊断回灌）→ 结果打分入榜 + 推荐表 → 非全过退出码 1。
  - TUI：`/flow list|show <slug>|match <text>|export <slug>` 本地即时；
    `/flow run` 的 TUI 内执行暂未接（CLI 已验证同一执行器）。
- **错因归纳回灌**：失败的 prompt 异步跑 `SummarizeFailure`，产出可核对
  条件 + patch——面板显示、audit 留痕（`LYON_DIAGNOSE=off` 关）；flow 重试
  时同一机制把约束块追加进下一次节点 prompt。

### 进化面板（alt+e / /evolve）

- **steps 式侧边弹窗**：`alt+e`（或 `/evolve`）在右侧列打开/关闭**进化面板**——
  与步骤面板共用分栏几何（宽 40，`alt+←/→` 与 `/steps width` 同步可调 28–72，
  窄终端 <110 列自动隐藏并如实提示）。
- **一行一类**：8 个任务类固定行序（coding/fix/review/research/writing/design/
  data/general，账本新增类排尾），每行 = 类名 · 在岗席位（模型短名，空缺显示
  `(空缺)`）· 考试时钟 `轮次/10` · 在岗近期 10 次均分，右端贴告警标记：
  `EXAM`（到点应考，黄）· `fN`（连挂 N 次，红）· `open`（席位空缺，暗）。
- **数据安全**：全部读自 `eval.Leaderboard.Views` 的**加锁快照**——评分在
  后台 goroutine 落榜，面板绝不去直接读 map（竞态）。
- **空态提示**：账本无据时显示「每次完成的提问都会记一张评分卡并记一轮」的
  指引块；页脚常驻 `/exam · /route · alt+e close`。

### 命令与标志速查

| 入口 | 作用 |
| --- | --- |
| `/route [text]` | 路由证据表 / 任务分类 + 推荐 + 画像对比 |
| `/exam [class]` | 手动闭卷考核（每 10 轮自动，`LYON_EXAM=off`） |
| `/flow list\|show\|match\|export\|save <slug>` | workflow 浏览与冻结 |
| `lyon run -exam auto\|<class>` | 先考核后执行 |
| `lyon run -coverage` | 覆盖率门禁 + 缺口注入 |
| `lyon run -flow <slug>` | 机械执行 workflow，结果入榜 |
| `LYON_SCORE=off` | 关闭普通 prompt 的盲评入榜（考核轮次照记） |

## 测试

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

覆盖：工具系统（schema 稳定性、审批、白名单、指标、shell 探测、记忆读写/上限/
注册）、引擎（事件协议、客户端压缩终局/延迟重建/阈值谓词、快照 Save↔Load↔
Restore 往返与 turn 延续、回滚（多轮撤销/compact 撤销/淘汰策略）、记忆注入、
子代理、真实缓存命中、中断与历史修复）、
TUI（转录渲染、状态机、命令、审批浮层、输入历史、thinking 折叠、斜杠候选弹层、
ctrl+t 按键端到端、恢复回放、步骤面板生命周期/渲染尺寸/分栏几何、/model 卡片/
列表/未收录回退、上下文/花费仪表与 header 常驻几何、/mode 与 shift+tab 循环）、
模式与审批（plan 门放行/拦截矩阵、bypass 短路人工、system 注入、mode 审计）、
模型知识库（精确/通配/未收录查找、注册表健全性、价格格式化、CostUSD 缓存
倍率）、子代理用量记账、OpenAI 翻译层
（协议互转、SSE 合成、错误映射、
SDK 穿层、引擎级工具循环 e2e）、MCP 客户端、服务（SSE、metrics、会话存储）、
演化机制（评分四维/榜单持久与连挂撤职/考核出题与判分/seed 题库落盘、
分类与推荐表置信度、覆盖率门禁注入、flow 执行器全部分支——重试诊断回灌、
耗尽中止、continue/goto、tick 预算、内置 workflow 结构与用户覆盖、
TUI /route /exam /flow 与 10 轮考核时钟、CLI -exam/-coverage/-flow 端到端
mock 打分入榜）、mockapi（缓存模拟器可信度）、bench（性能验收）。
全部无网络、无真实密钥依赖。
