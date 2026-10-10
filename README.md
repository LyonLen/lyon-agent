# lyon-agent

高性能通用 AI Agent，Go 编写，**服务 + CLI 双形态**。一个进程内跑完整的
agent 循环（流式输出、工具调用、上下文压缩、子代理），对外提供 HTTP/SSE 服务
和交互式 CLI；自带压力验收套件，性能阈值不达标直接 FAIL。

```
go build ./...          # 构建
go test ./... -count=1  # 全量测试（含 bench 验收）
go test ./bench -run 'TestLoad|TestCache|TestSSE' -v   # 性能验收（打印结果表）
```

## 架构

```
cmd/lyon            CLI：run（一次性）/ chat（全屏 TUI）/ tools（工具清单）
  tui.go             chat 的 bubbletea TUI：流式转录、滚动视口、y/n 审批浮层、thinking 折叠、斜杠候选弹层
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
`MaxIterations=20`、压缩阈值 0.7×200k、子代理默认低配额档。

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
文件前人工确认）`-shell-allow echo,ls,git`（白名单）`-mcp name=cmd args`（接 MCP 服
务器）`-mcp-http name=url` `-prewarm`（首 prompt 前预热缓存）`-max-turns N`（全局
并发 turn 上限）`-web-tools server|client|off`。

**图片输入**：prompt 里用 `@路径` 引用工作区内的 png/jpeg/gif/webp，图片会作为
image block 随消息一起发给模型（`看看 @screenshot.png`；文件名含空格写成
`@"my shot.png"`；`a@b.com` 这类邮箱不受影响）。模型侧对应 `view_image` 工具，
随时可把工作区图片读进上下文——`read_file` 只读 UTF-8 文本，遇到二进制会提示改用
它。大小/像素上限按 API 限制执行（约 3.5 MB、单边 8000 px）。

### chat TUI

`lyon chat` 在真实终端里进全屏 TUI：流式转录（markdown-lite 高亮、代码块着色、
CJK 自动换行）、底部单行输入、跟随滚动。非 TTY（管道/重定向）或 `-plain` 时自动
退化为行式 REPL。

**thinking 折叠**：思考过程按连续块分组，流式期间实时显示（带 `▾ thinking · N
lines` 头部），下一个非思考内容一到就把整块折成一行
`▸ thinking · N lines  <首行预览>`；`ctrl+t`（或 `/think`）全局展开/收起。

| 按键 | 行为 |
|---|---|
| `enter` | 发送（`/` 开头走本地命令；唯一匹配的前缀直接执行，`/st` → `/stats`） |
| `tab` | 完成候选弹层里选中的斜杠命令 |
| `esc` | 忙时中断当前 prompt（历史自动修复，可继续聊）；空闲且弹层开着先关弹层 |
| `ctrl+c` | 忙时：先中断，再按一次退出；空闲：退出 |
| `ctrl+t` | 展开/折叠全部 thinking 块 |
| `↑` / `↓` | 弹层打开时在候选间移动，否则输入历史回忆 |
| `pgup` / `pgdn` / 滚轮 | 滚动转录（滚离底部即停止跟随，回底部恢复） |

**斜杠候选弹层**：输入 `/` 即在输入框上方弹出命令列表，随输入实时过滤，末行提示
`tab complete · ↑/↓ choose · esc close`；视口同步让出行数，布局不被顶掉。只剩
一个匹配时 `enter` 直接执行，多个匹配时 `enter` 提示补全（不报 unknown
command），无匹配则照常作为本地命令处理；`esc` 关闭后继续输入会重新弹出。

命令：`/stats` 用量与缓存命中表 · `/clear` 清屏（会话历史保留）·
`/resume` 恢复存档（多份时**直接弹列表选**，`/resume N` 指定第 N 份）·
`/sessions` 列出全部存档 · `/new` 开新会话（当前对话先归档）· `/think` 同
`ctrl+t` · `/help` · `/quit`。带 `-approve` 时 shell/写文件工具会在状态行弹
y/n 浮层（超时/关窗一律拒绝，fail closed）。

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
user message，保证 role 交替完整、API 不会拒绝。busy 锁在发出 prompt_end **之前**
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

## 测试

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

覆盖：工具系统（schema 稳定性、审批、白名单、指标、shell 探测、记忆读写/上限/
注册）、引擎（事件协议、客户端压缩终局/延迟重建/阈值谓词、快照 Save↔Load↔
Restore 往返与 turn 延续、记忆注入、子代理、真实缓存命中、中断与历史修复）、
TUI（转录渲染、状态机、命令、审批浮层、输入历史、thinking 折叠、斜杠候选弹层、
ctrl+t 按键端到端、恢复回放）、MCP 客户端、服务（SSE、metrics、会话存储）、
mockapi（缓存模拟器可信度）、bench（性能验收）。全部无网络、无真实密钥依赖。
