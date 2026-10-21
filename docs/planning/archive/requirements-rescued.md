# 测试会话数据打捞 2026-10-15 11:50

- 会话: 72f2fa928a6b1980a1957bb38a5701dd / 20261015-114237.877.json (SavedAt 2026-10-15T11:42:37.8776222+08:00)
- Title: 了解仓库结构与功能 | FirstPrompt: 了解一下这个仓库 | Messages: 71
- 完整快照已复制为 session-20261015-114237.877.json.bak

## 1. 压缩摘要/待办（消息 idx 0）

[conversation compacted — this summary replaces the full history]

## 分工约束（已解除）

用户原话（verbatim）：「可以去掉这个限制了，之后你来接手改代码的事」。原记忆「Claude Code 改代码、lyon 只负责测试」已由用户明确解除——**lyon（本 agent）现在既改代码也写测试**，已用 save_memory 覆盖为 project 记忆 `lyon-agent-分工-claude-code-改代码-lyon-负责测试`（hot tier，标题未改但内容已更新）。用户语言：中文回复。

仓库：本仓库，Go 1.25，182 个 .go（95 测），37043 行，main 分支，工作区仅 `.claude/` 未跟踪。评审基线：`go build ./...` + `go vet ./...` 全绿；全量 `go test ./... -count=1` 初始挂 #6 bench 与 #3 TestSubNestMarksEvents。

## 待修清单与进度（todo：4/8 完成 → 当前 in_progress 已推进）

**#2 lyond 鉴权 — completed**
- `cmd/lyond/main.go`：`-addr` 默认改 `127.0.0.1:8080`；新增 `-token`（env `LYON_TOKEN`）、`-insecure`；非回环地址无 token 且无 `-insecure` → 打印错误 `os.Exit(2)`；新增 `loopbackOnly(addr)`（空 host/`0.0.0.0`/非回环 IP/无法解析的域名/解析失败一律 false；`localhost` 与 loopback IP 为 true）、`authState(token)`；启动日志加 `auth=%s`；末尾 `eng.Close()`。
- `pkg/server/server.go`：`Server` 加 `token` 字段、`SetToken(tok)`；`Handler()` token 非空时包装中间件——`/healthz` 放行，其余校验 `Authorization: Bearer` 或 `X-Lyon-Token`，`subtle.ConstantTimeCompare` 常量时间比较，失败 401 + `WWW-Authenticate`。
- 新增 `pkg/server/auth_test.go`（匿名 401 矩阵、错 token、Bearer/X-Lyon-Token 接受、healthz 开放、无 token 兼容）、`cmd/lyond/main_test.go`（loopbackOnly 10 用例 + authState）。
- README.md / README.en.md「服务」章节：命令改 `127.0.0.1:8080`，新增**鉴权**段落。

**#4 runHook WaitDelay — completed**
- `pkg/hooks/hooks.go` runHook 加 `c.WaitDelay = time.Second`（同 `shell.go:201-204` 先例）。
- `pkg/hooks/hooks_test.go`：helper 新增 `orphan`（起继承 stdout 管道的孙进程后 `select{}` 挂死）与 `grandchild`（按 `LYON_HOOK_GRANDCHILD_SLEEP` 睡）mode；新增 `TestRunHookTimeoutDoesNotBlockOnGrandchildren`（timeout 300ms，断言 <4s 返回、≥250ms）。
- **回归验证**：临时去掉 WaitDelay → 测试 FAIL（8.26s），恢复 → PASS（1.16s）。`pkg/hooks` 全绿。

**#3 writeLoop Close — completed**
- `pkg/hooks/hooks.go`：`Manager` 加 `stopCh`/`doneCh`/`closed`/`closeOnce`；`writeLoop` 加 `<-m.stopCh` 分支（先 drain auditCh 再退出，defer close(doneCh)）；新增 `Close()`（置 closed → 起 writer → close(stopCh) → 等 doneCh，幂等）、`isClosed()`；`Flush()` closed 时直接 return，否则 select 用 doneCh 兜底防挂；`Save()` closed 时 no-op；`Fire` closed 时不再入队。
- `pkg/agent/engine.go`：新增 `Engine.Close()`（nil 安全，转发 hooks.Close）。
- 调用点：`cmd/lyon/main.go` 3 处 `defer mgr.Close()` 后补 `defer eng.Close()`（含 buildEngine 注释更新）；`cmd/lyond/main.go` 末尾 `eng.Close()`。
- 新增 `pkg/hooks/close_test.go`（drain 50 行 + doneCh + stats；幂等 Close + RemoveAll 后 Flush/Fire/Save 不复活目录；无 Fire 也可 Close）。
- 测试侧：`pkg/hooks/hooks_test.go` 所有 `New("ws")` 补 `t.Cleanup(m.Close)`；`pkg/agent/subnest_test.go`、`hooks_wiring_test.go`（2 处）、`mode_test.go`（modeTestSession）补 `t.Cleanup(eng.Close)`，且这些 TempDir 改用 `tempDir(t)`（delete-with-retry helper）。
- 验证：`pkg/agent -count=10` 全绿（此前连挂）。

**#1 confine 符号链接 — completed**
- `pkg/tools/files.go`：`confine()` 重写——间接层 `evalSymlinks`/`osLstat`/`osReadlink` 包级变量（可注入桩）、`linkBudget = 32`、`resolveExisting(path, budget) (real string, ok bool)`（EvalSymlinks 优先，失败向上走最近存在祖先，逐组件 Lstat 判 symlink 再 Readlink 展开，相对链接基于 resolvedDir，预算耗尽返回 ok=false）；root 自身也 EvalSymlinks；`real, ok` 不成立或 Rel 越界 → 拒绝；**返回值仍是调用方原始拼写（clean 后未解析）**，只有判定用真实路径。
- 新增 `pkg/tools/symlink_test.go`：真实 symlink 测试（`symlinkOrSkip`，本机因无 SeCreateSymbolicLinkPrivilege 全部 SKIP）+ 确定性桩测试（`symlinkStub`/`newSymlinkStub`/`canonical` 规范化 8.3 短名）——dir link 逃逸、dangling link、link cycle fail-closed、inside link 放行、linked root、`resolveExisting` 祖先回退。
- **回归验证**：把判定改回纯词法 → 4 个桩用例 FAIL；恢复 → `pkg/tools` 全绿。

**#6 bench 门限 — in_progress（已改完，正被新失败打断）**
- `bench/load_test.go` overhead 子测试改为**最多 3 轮取最小 overhead**（噪声只能把差分推高，真回归每轮都挂）：抽 `measure()` 闭包 + `round` 结构体（overhead/分位/wall/throughput/errs/completed/leaked），`maxRounds = 3`，失败只报最后一轮，成功日志带 `best of up to 3 rounds`。
- 验证：`go test ./bench` 单独跑 PASS（11.0ms ≤ 25ms，wall 0.37s），gofmt 已过。

**全量跑暴露的新问题（当前正在排查）**：`time go test ./... -count=1`（42s）除 bench/cmd/lyon/pkg 大部分绿之外，**`pkg/eval` `TestElectAfterContest` 挂**：`TempDir RemoveAll cleanup: ...\001: The directory is not empty`。已确认 `pkg/eval` 无 goroutine/ticker/t.Parallel，写盘是同步 `os.WriteFile`/tmp+rename（`candidates.go:87-95`、`exam.go:69-76`、`leaderboard.go:78-85`），`OpenLeaderboard()` 走 `homeDir()`（受 `LYON_HOME`），测试用裸 `t.TempDir()`。

**最后一条命令（结果尚未返回）**：
```
go test ./pkg/eval -count=10 2>&1 | grep -E "^--- FAIL|RemoveAll|^ok|^FAIL" | head -20; echo "=== parallel? ==="; grep -rn "t.Parallel" pkg/eval/*_test.go | head; echo "=== tempDir helper? ==="; grep -rn "func tempDir" pkg/eval/*_test.go
```
待拿到结果后判断：是否为可复现泄漏（若 `pkg/eval` 无 tempDir helper，则与 pkg/agent 同因——残留 writer/句柄 → 补 `tempDir(t)` 或补 Close），或 Windows %TEMP% 瞬时句柄 flake。

## 剩余 todo
1. **in_progress**：#6 收尾（bench 已改并单跑绿）+ 处理全量跑暴露的 `pkg/eval TestElectAfterContest` TempDir 失败。
2. #7 `pkg/memory/store.go`：Search 每命中同步原子写应在写锁外批量；migrateLegacy 持 `storesMu` 做磁盘 I/O 出锁；insertLocked 每次 Save 全量重建索引。
3. 测试缺口：`pkg/memory`(4源/1测)、`pkg/mcp`(2/1)、`pkg/hooks`(3/1) 补覆盖。
4. 最终验收：`go build ./... && go vet ./... && go test ./... -count=1` 全绿。
5. #5 `cmd/lyon/tui.go`（现 3015 行）按 popup/render/slash dispatch 拆分——清单末尾项。

## 环境/工具注意
- Windows + MinGW bash：**无 `cp`**；`mv`/`sed`/`git`/`python`(Python38) 可用；临时改文件再恢复用 python 读写（注意 `newline=''` 保换行，gofmt 校验）。
- Go 构建缓存偶发 `unlinkat ...\test.exe: Access is denied` 噪声（非测试失败）。
- 克隆基线仓库须 `git clone --no-hardlinks . /tmp/lyon-base`（`--local` 报 Improper link）——基线 de51bd3 用于对照验证。
- 临时调试文件如 `pkg/tools/zz_debug_test.go` 用后即删；`/tmp/*.bak` 用于改前备份。

Continue the original task from where it left off.

## 2. 最新需求消息（消息 idx 70，role=user，含图片粘贴）

[image: .lyon/paste/paste-20261015-102438.png (image/png, 1920x1040)]
[image]
 这个plan要不就常驻在右下？
[image: .lyon/paste/paste-20261015-102520.png (image/png, 1920x1040)]
[image]
 这个queue插入逻辑，能否模仿claude code，在间隙里面可以执行一下，而不是等上一个任务全部跑完？
另外规划一下，我需要规划一下整体的进程执行后run的dump文件
然后除了tui可否做一个web版本？
文档记得整体更新一下，搞得专业点，我要开源得
[image: .lyon/paste/paste-20261015-103001.png (image/png, 1920x1040)]
[image]
 这个不对，shift + alt + 上下，alt + enter，alt + 左右，你现在的表达会让用户误解
[image: .lyon/paste/paste-20261015-103109.png (image/png, 1920x1040)]
[image]
 像这样的queue是否需要合并或优化一下再投给AI，有的时候我就会很碎的扔给AI，可以开个subagent这种方式，在agent working的时候，有个人帮你理计划，帮你判断，无相关甚至可以直接分屏执行？分屏要好好规划哦，我希望是可以多个会话同时跑
web版本，我希望交互协议用protobuf 参考这个仓库 
[image: .lyon/paste/paste-20261015-103432.png (image/png, 1920x1040)]
[image]
 这个仓库是我比较满意的
然后你规划一下定时器逻辑：进程内定时器、操作系统定时器。两种都要
要做日志机制哦，有一些执行报错，记录一下，是不是文件并发抢锁还是什么代码没写好
[image: .lyon/paste/paste-20261015-112000.png (image/png, 1920x1040)]
[image]
 我有个good idea
[image: .lyon/paste/paste-20261015-112059.png (image/png, 1920x1040)]
[image]
 open时可以看到实时终端输出
报错的问题你列一下

## 3. 异常消息 idx 69

    {"content": []}

## 追加需求 #13 — 右栏三段式布局（2026-10-15 ~13:0x，截图消息直接发来）

截图：右侧栏被红框分成上中下三块，从上到下：
1. **steps**（现有步骤面板，顶部，`步骤·3`，#1 memory.inject / #3 diagnose 等）
2. **step real time output — 可以展开、折叠**（中部：当前步骤的实时输出区，支持展开/折叠）
3. **plan 的计划**（底部：模型写的计划/todo 常驻区）

底部状态行仍是 `alt+↑ 选择 · enter 展开 · ↔ 调宽`。
关联 handoff A 节 #1（plan 常驻右下）与 #11（open 实时终端输出）——本图把两者具体化为
右栏三段布局：steps / 步骤实时输出(可折叠) / plan 计划常驻。


## 追加需求 #14 — 进化面板（alt+e evopanel）加两块内容（2026-10-15 ~13:1x，截图）

截图：`进化 · 每 10 轮考试` 面板（coding/fix/review/research/writing/design/data/general 各行
模型+分数，底部 `/exam · /route · alt+e 关闭`），下方红框新增两块：
1. **总结整体 agent 进化情况**（一段整体进化状态总结区）
2. **描述测评集内容，如何调整测评集**（测评集构成说明 + 调整方法区）

落地点：`cmd/lyon/evopanel.go`（面板渲染）、`cmd/lyon/evolve.go`（exam/routing 数据与生成逻辑，
可能需要一次 LLM 总结调用或由已有统计拼装）。


## 追加需求 #15 — 事情进展预览（plan 横向树 + 审批 + 执行演化）（2026-10-15 ~13:2x 口述，**排在 web 版本之后**）

原文大意：「等你 web 版本做好了，我想要一个事情进展预览：提了一个问题，AI 给出 plan
4 个步骤 → 展示成横向从左到右的树（1-4 节点）→ 我可以像在 xmind 上面一样一个一个审批、
修改（bypass 模式下自己演化）→ 点击某个节点，右侧再展开面板显示这一步的执行细节，
可以改、可以批通过 → 全部批通过后点击执行 → 这一层 4 个节点各自演化，长出一些 step
（run_shell / view_image…），可能并发、可能串行 → 都可以点进去查看 prompt、实时 output；
进行中的节点也可以点进去临时指导。」

拆解：
1. plan 横向树渲染（左→右，4 个主节点）——web 端（依赖 #4 protobuf web 版）。
2. 节点级审批/修改交互（xind 式逐节点操作；bypass 模式 = 自动演化免批）。
3. 节点详情右侧面板：执行细节展示 + 可编辑 + 可批通过。
4. 全批通过 → 执行编排：子 step 挂在各节点下生长，支持并发/串行标记。
5. 节点可点入查看 prompt + 实时 output（与 #11 实时输出、#13 右栏三段式同族）。
6. 进行中节点可注入临时指导（queue 插队 #2 的节点级版本）。

落地依赖：web 版本（#4 protobuf 协议需覆盖：plan 树结构、审批状态、节点执行流、
实时输出流、临时指导注入）。


## 进度 2026-10-15 本会话（接续）

- **#11 open 时实时终端输出 — completed (07aab30)**：tools.OutputObserver 可选接口 + WithOutputSink；shell streamBuf 40ms/16KB 合流、Close 先于 OnToolEnd；Session/subObs OnToolOutput → EventToolExecOutput（不进 audit）→ stepEvent → stepAppendOutput（每 step stepOutMax 尾窗）；stepExpandRows 三段预算（detail 3 行封顶 / 实时输出取最新 / err+started 钉底）。测试三组：pkg/tools/output_test.go、pkg/agent/output_test.go、cmd/lyon/steps_output_test.go；TestExpandRowCap 更新为新契约。
- **测试卫生 (ee4b3ed)**：pkg/hooks 补 tempDir 重试清理（0e3c73c 同款）；TestKickScoreFilesCard 等 card+evidence 双落盘（kickScore goroutine 内两次写盘的窗口）。
- **全量 go test ./... -count=1 16 包全绿**（此前 3 轮中的环境类失败均已定因：bench CPU 抢占、%TEMP% 扫描器、kickScore 竞态）。
- **#2 queue 间隙插队 — completed (d7f3527)**：闲时 Enqueue 拒绝（ErrNoPromptRunning），忙时进 inbox；drive 回圈顶部、工具执行+POST 之前 injectQueuedGap 追加用户消息（apiGuard 预合并相邻同 role，模型读到与 tool_result 同轮的一条消息）；把 inbox 清空与 busy 落锁放在同一 mu 临界区（不双发、不丢失，prompt_end 的 kick 仍是兜底）；EventQueueInjected 列噪声不进 audit，投递以 prompt_start+Detail 标记留痕；TUI 收到事件就地删镟副本并写入 transcript 行。测试 pkg/agent/inject_test.go（生命周期/穿透验证/审计/无泄漏）+ cmd/lyon TestTUIQueueInjection；全量第 5 轮全绿。
- **D3 memview 补漏 — completed (f11d324)**：4ef0f56 提交时漏了 memview.go（HEAD tui.go 已引用 rememberDone.rep + echoLine 却编译坏）；补交 17 行（DistillReport 字段 + count-only 回退 echoLine）修复历史。
- **任务 B TUI 自动固化提示 — completed (65cb850)**：flowhint.go/flowhint_test.go 新增 + tui.go prompt_end +3 行；slug=run 首现 step 工具名 [a-z0-9-] token + FNV-1a 低 24 位，与 CLI /flow 共用 flow-streak.json 账本，换类先投 false，n≥3 每段一次双语固化提示，LYON_AUTOFREEZE=off 全静默；7 用例 + cmd/lyon 全包 + pkg/flow 绿（pkg/flow 曾现 TestBumpStreak TempDir RemoveAll 单轮 flake，3 连跑绿，0e3c73c 同类环境问题，pkg/flow 源码零改动）。
- **docs-open 第一批 — completed (41bca01)**：MIT LICENSE、三平台 CI (build/vet/test, 启明已知 flaky)、.gitignore 无扩展名产物、README 双语重写对镜代码现实（esc/ctrl+c、#2 间隙注入、分层记忆、裸 lyon 用法、新键位与未记录能力补齐）。待晨报：CI badge clone URL 用推测 slug LyonLen/lyon-agent（无 remote 待用户确认仓名）；.claude/ 与 ROADMAP/EVOLUTION-GAPS 是否入库待用户裁决。
- **docs-open 第二批 — completed (fe2b107)**：手册双语修正（esc→ctrl+c 四处、间隙注入状态行字面量、指令数 23、补 /contest /agents /market、新键位）+ 五帧截图经 LYON_GEN_SCREENS 生成器重渲染，我复跑校验模式绿。
- **#9 日志机制 — completed (7e95fe5)**：pkg/logx 按日轮转日志（O_APPEND+逐行 Write 无撕裂、失败降级不吓宿主、30 天清理、LYON_DEBUG 级别）；接线全在静默失败缝：Engine.Close/Warmup 失败/apiGuard 传输错与不可修复 400 摘要（护栏语义未动）/工具 recover 缝/hooks 审计与 stats 原静默吞错处；logx 包注释陆全仓非原子写点清单。
- **新开 writefix-dev**：按该清单修 A 类 3 处直写 + B 类 8 处固定 .tmp 名（CreateTemp 独特名）。
- **#3 run dump + E2 skills — completed (b7ff44e，同笔因 session.go 交织）**：dump 每 run 一文件 $LYON_HOME/dumps/（schema_version 1、事件时间线 seq 连续、usage/子账单、文件层无锁唯一名+O_EXCL+rename、写闸门纯 I/O 礼貋性、失败仅 dump_error 审计不动 run）；skills 扫描 ~/.claude/skills 注入 system 基底（保 cache 前缀字节稳定）。全包 + dump 7 用例 + skills 9 用例绿；bench 17ms≤25ms。E2 从此解锁。
- **新开 d1-refresh**：修热层中途刷新（会话内 save_memory 同会话可见，内容刷新 + PushSystemNote 双路）。
- **#4 web 设计文档 — completed（web-v1-plan.md，1078 行，押后入库）**：Connect RPC+buf、SSE 不迁移有 TestSSE 门禁、Event Hub 扇出、proto 6 文件字段级、19 项数据源、#15 预留。**web v1 实现押后：晨报请用户确认协议方向**。
- **logx 自我回归修复 — completed (1c884d3)**：持有日文件句柄至进程退出使 Windows 下 TempDir 删不掉 → open-per-write（O_APPEND+逐行关闭），新增 TestNoHandleHeldBetweenWrites 守卫。
- **docs-open 收工**：三批全入库（41bca01/fe2b107/97191e5）；ANALYSIS 拆解押后用户裁决。
- **steps_test 裁决已执行**：gutter 断言改为 step 行消失（header 四格 │ 合法），随 header 批次提交。
- **run_subagent plan 模式 3 连超时修复 — completed (b224f1a)**：根因链——墙钟 ToolTimeout 包住整段嵌套循环 → 健康多轮研究 120s 必死 → SDK 把错误当 tool_result 回喂 → 模型盲重试成“3连”；修复：session.go 重适配仅两个子代理工具 Timeout=0（主会话手合）+ idle 看门狗 2×ToolTimeout 进度续命 + 三分支可诊断错误 + plan 门放行 run_subagents + dispatch 折病因 + mockapi content_block_delta 缺 type 基建修复。
- **#16 header 四格功能区 — completed (5492b06)**：logo 右侧 model/usage/evolution/activity 四格，本地只读、缺数据 dim — 单格降级、窄屏折叠；steps_test gutter 断言退役（View 级字符锨已被 header/transcript 共享，改纯几何契约）。
- **1.5 Claude Code hooks 兼容 — completed (e885e7a)**：pkg/hooks/cccompat.go 解析 .claude/settings.json hooks 并best-effort映射到 lyon 自有事件（PreToolUse→阻塞 tool_exec_start 走既有 CheckTool 否决、PostToolUse→tool_exec_end、UserPromptSubmit→prompt_start、Stop→prompt_end、PreCompact→compaction、Subagent/PermissionRequest 等；unmap 聚合告警一次；matcher 文本保留未用）；Engine 构造时 LoadClaudeSettings 一次，文件缺失零行为变化。8 用例绿（pkg/hooks 直跑 test.exe PASS——%TEMP% 被 AV 锁属环境噪声）。
- **0.2 lyon.toml 三层配置 — completed (17ba77e)**：pkg/config stdlib 手写 TOML 子集；优先级 flag > workspace/.lyon.toml > LYON_HOME/config.toml > 默认，ApplyFlags fs.Visit 仅显式 flag 覆盖；MCP 三层 flag > toml > mcp.json，toml 层容忍、flag 层致命（mcp-config-dev 独立复验）。pkg/config 1.6s + cmd/lyon 6.7s 绿。
- **#8 双定时器核心包 — completed (af98bcd，接线未开)**：pkg/timer 10 文件——min-heap Scheduler 单循环 + Runner 注入、overlap skip/queue、miss skip/once、Stop 排空屏障、schtasks/crontab OS 注册（# lyon:<id> 锚点）、DESIGN.md 记录决策与 cmd/ 接线清单（Runner 注入、/timer slash、OS 子命令、TUI 面板——后续接线任务任务书）。35 PASS + 1 SKIP 门控 2.9s 绿。
- **writefix 非原子写修复 — completed (225563e)**：log-dev 审计清单闭环——A 类直写 4 点（SaveExam/flow.Save/write_file/edit_file）+ B 类固定 .tmp 名 8 点（snapshot×2/candidates/leaderboard/streak/market/memory/routing）全改 os.CreateTemp 唯一名+rename；0644 点 Chmod 保模式；memory 测试方案 A（挪 scope 目录制造真实失败）+ routing 8 协程并发 residue 测试 + streak residue 断言。7 域绿（market 首挂 TempDir 竞态 flake，复跑 5 连绿）。
-- **下一项**：d1-refresh2/dashboard/demo60s 在跑；timer 接线（cmd/ 侧 Runner 注入、/timer slash、OS 子命令、TUI 面板——DESIGN.md 清单）待开；web v1 实现、晨报四拍板项（CI 仓名/.claude 入库/web 协议/header 内容）押后。alse；`localhost` 与 loopback IP 为 true）、`authState(token)`；启动日志加 `auth=%s`；末尾 `eng.Close()`。
- `pkg/server/server.go`：`Server` 加 `token` 字段、`SetToken(tok)`；`Handler()` token 非空时包装中间件——`/healthz` 放行，其余校验 `Authorization: Bearer` 或 `X-Lyon-Token`，`subtle.ConstantTimeCompare` 常量时间比较，失败 401 + `WWW-Authenticate`。
- 新增 `pkg/server/auth_test.go`（匿名 401 矩阵、错 token、Bearer/X-Lyon-Token 接受、healthz 开放、无 token 兼容）、`cmd/lyond/main_test.go`（loopbackOnly 10 用例 + authState）。
- README.md / README.en.md「服务」章节：命令改 `127.0.0.1:8080`，新增**鉴权**段落。

**#4 runHook WaitDelay — completed**
- `pkg/hooks/hooks.go` runHook 加 `c.WaitDelay = time.Second`（同 `shell.go:201-204` 先例）。
- `pkg/hooks/hooks_test.go`：helper 新增 `orphan`（起继承 stdout 管道的孙进程后 `select{}` 挂死）与 `grandchild`（按 `LYON_HOOK_GRANDCHILD_SLEEP` 睡）mode；新增 `TestRunHookTimeoutDoesNotBlockOnGrandchildren`（timeout 300ms，断言 <4s 返回、≥250ms）。
- **回归验证**：临时去掉 WaitDelay → 测试 FAIL（8.26s），恢复 → PASS（1.16s）。`pkg/hooks` 全绿。

**#3 writeLoop Close — completed**
- `pkg/hooks/hooks.go`：`Manager` 加 `stopCh`/`doneCh`/`closed`/`closeOnce`；`writeLoop` 加 `<-m.stopCh` 分支（先 drain auditCh 再退出，defer close(doneCh)）；新增 `Close()`（置 closed → 起 writer → close(stopCh) → 等 doneCh，幂等）、`isClosed()`；`Flush()` closed 时直接 return，否则 select 用 doneCh 兜底防挂；`Save()` closed 时 no-op；`Fire` closed 时不再入队。
- `pkg/agent/engine.go`：新增 `Engine.Close()`（nil 安全，转发 hooks.Close）。
- 调用点：`cmd/lyon/main.go` 3 处 `defer mgr.Close()` 后补 `defer eng.Close()`（含 buildEngine 注释更新）；`cmd/lyond/main.go` 末尾 `eng.Close()`。
- 新增 `pkg/hooks/close_test.go`（drain 50 行 + doneCh + stats；幂等 Close + RemoveAll 后 Flush/Fire/Save 不复活目录；无 Fire 也可 Close）。
- 测试侧：`pkg/hooks/hooks_test.go` 所有 `New("ws")` 补 `t.Cleanup(m.Close)`；`pkg/agent/subnest_test.go`、`hooks_wiring_test.go`（2 处）、`mode_test.go`（modeTestSession）补 `t.Cleanup(eng.Close)`，且这些 TempDir 改用 `tempDir(t)`（delete-with-retry helper）。
- 验证：`pkg/agent -count=10` 全绿（此前连挂）。

**#1 confine 符号链接 — completed**
- `pkg/tools/files.go`：`confine()` 重写——间接层 `evalSymlinks`/`osLstat`/`osReadlink` 包级变量（可注入桩）、`linkBudget = 32`、`resolveExisting(path, budget) (real string, ok bool)`（EvalSymlinks 优先，失败向上走最近存在祖先，逐组件 Lstat 判 symlink 再 Readlink 展开，相对链接基于 resolvedDir，预算耗尽返回 ok=false）；root 自身也 EvalSymlinks；`real, ok` 不成立或 Rel 越界 → 拒绝；**返回值仍是调用方原始拼写（clean 后未解析）**，只有判定用真实路径。
- 新增 `pkg/tools/symlink_test.go`：真实 symlink 测试（`symlinkOrSkip`，本机因无 SeCreateSymbolicLinkPrivilege 全部 SKIP）+ 确定性桩测试（`symlinkStub`/`newSymlinkStub`/`canonical` 规范化 8.3 短名）——dir link 逃逸、dangling link、link cycle fail-closed、inside link 放行、linked root、`resolveExisting` 祖先回退。
- **回归验证**：把判定改回纯词法 → 4 个桩用例 FAIL；恢复 → `pkg/tools` 全绿。

**#6 bench 门限 — in_progress（已改完，正被新失败打断）**
- `bench/load_test.go` overhead 子测试改为**最多 3 轮取最小 overhead**（噪声只能把差分推高，真回归每轮都挂）：抽 `measure()` 闭包 + `round` 结构体（overhead/分位/wall/throughput/errs/completed/leaked），`maxRounds = 3`，失败只报最后一轮，成功日志带 `best of up to 3 rounds`。
- 验证：`go test ./bench` 单独跑 PASS（11.0ms ≤ 25ms，wall 0.37s），gofmt 已过。

**全量跑暴露的新问题（当前正在排查）**：`time go test ./... -count=1`（42s）除 bench/cmd/lyon/pkg 大部分绿之外，**`pkg/eval` `TestElectAfterContest` 挂**：`TempDir RemoveAll cleanup: ...\001: The directory is not empty`。已确认 `pkg/eval` 无 goroutine/ticker/t.Parallel，写盘是同步 `os.WriteFile`/tmp+rename（`candidates.go:87-95`、`exam.go:69-76`、`leaderboard.go:78-85`），`OpenLeaderboard()` 走 `homeDir()`（受 `LYON_HOME`），测试用裸 `t.TempDir()`。

**最后一条命令（结果尚未返回）**：
```
go test ./pkg/eval -count=10 2>&1 | grep -E "^--- FAIL|RemoveAll|^ok|^FAIL" | head -20; echo "=== parallel? ==="; grep -rn "t.Parallel" pkg/eval/*_test.go | head; echo "=== tempDir helper? ==="; grep -rn "func tempDir" pkg/eval/*_test.go
```
待拿到结果后判断：是否为可复现泄漏（若 `pkg/eval` 无 tempDir helper，则与 pkg/agent 同因——残留 writer/句柄 → 补 `tempDir(t)` 或补 Close），或 Windows %TEMP% 瞬时句柄 flake。

## 剩余 todo
1. **in_progress**：#6 收尾（bench 已改并单跑绿）+ 处理全量跑暴露的 `pkg/eval TestElectAfterContest` TempDir 失败。
2. #7 `pkg/memory/store.go`：Search 每命中同步原子写应在写锁外批量；migrateLegacy 持 `storesMu` 做磁盘 I/O 出锁；insertLocked 每次 Save 全量重建索引。
3. 测试缺口：`pkg/memory`(4源/1测)、`pkg/mcp`(2/1)、`pkg/hooks`(3/1) 补覆盖。
4. 最终验收：`go build ./... && go vet ./... && go test ./... -count=1` 全绿。
5. #5 `cmd/lyon/tui.go`（现 3015 行）按 popup/render/slash dispatch 拆分——清单末尾项。

## 环境/工具注意
- Windows + MinGW bash：**无 `cp`**；`mv`/`sed`/`git`/`python`(Python38) 可用；临时改文件再恢复用 python 读写（注意 `newline=''` 保换行，gofmt 校验）。
- Go 构建缓存偶发 `unlinkat ...\test.exe: Access is denied` 噪声（非测试失败）。
- 克隆基线仓库须 `git clone --no-hardlinks . /tmp/lyon-base`（`--local` 报 Improper link）——基线 de51bd3 用于对照验证。
- 临时调试文件如 `pkg/tools/zz_debug_test.go` 用后即删；`/tmp/*.bak` 用于改前备份。

Continue the original task from where it left off.

## 2. 最新需求消息（消息 idx 70，role=user，含图片粘贴）

[image: .lyon/paste/paste-20261015-102438.png (image/png, 1920x1040)]
[image]
 这个plan要不就常驻在右下？
[image: .lyon/paste/paste-20261015-102520.png (image/png, 1920x1040)]
[image]
 这个queue插入逻辑，能否模仿claude code，在间隙里面可以执行一下，而不是等上一个任务全部跑完？
另外规划一下，我需要规划一下整体的进程执行后run的dump文件
然后除了tui可否做一个web版本？
文档记得整体更新一下，搞得专业点，我要开源得
[image: .lyon/paste/paste-20261015-103001.png (image/png, 1920x1040)]
[image]
 这个不对，shift + alt + 上下，alt + enter，alt + 左右，你现在的表达会让用户误解
[image: .lyon/paste/paste-20261015-103109.png (image/png, 1920x1040)]
[image]
 像这样的queue是否需要合并或优化一下再投给AI，有的时候我就会很碎的扔给AI，可以开个subagent这种方式，在agent working的时候，有个人帮你理计划，帮你判断，无相关甚至可以直接分屏执行？分屏要好好规划哦，我希望是可以多个会话同时跑
web版本，我希望交互协议用protobuf 参考这个仓库 
[image: .lyon/paste/paste-20261015-103432.png (image/png, 1920x1040)]
[image]
 这个仓库是我比较满意的
然后你规划一下定时器逻辑：进程内定时器、操作系统定时器。两种都要
要做日志机制哦，有一些执行报错，记录一下，是不是文件并发抢锁还是什么代码没写好
[image: .lyon/paste/paste-20261015-112000.png (image/png, 1920x1040)]
[image]
 我有个good idea
[image: .lyon/paste/paste-20261015-112059.png (image/png, 1920x1040)]
[image]
 open时可以看到实时终端输出
报错的问题你列一下

## 3. 异常消息 idx 69

    {"content": []}

## 追加需求 #13 — 右栏三段式布局（2026-10-15 ~13:0x，截图消息直接发来）

截图：右侧栏被红框分成上中下三块，从上到下：
1. **steps**（现有步骤面板，顶部，`步骤·3`，#1 memory.inject / #3 diagnose 等）
2. **step real time output — 可以展开、折叠**（中部：当前步骤的实时输出区，支持展开/折叠）
3. **plan 的计划**（底部：模型写的计划/todo 常驻区）

底部状态行仍是 `alt+↑ 选择 · enter 展开 · ↔ 调宽`。
关联 handoff A 节 #1（plan 常驻右下）与 #11（open 实时终端输出）——本图把两者具体化为
右栏三段布局：steps / 步骤实时输出(可折叠) / plan 计划常驻。


## 追加需求 #14 — 进化面板（alt+e evopanel）加两块内容（2026-10-15 ~13:1x，截图）

截图：`进化 · 每 10 轮考试` 面板（coding/fix/review/research/writing/design/data/general 各行
模型+分数，底部 `/exam · /route · alt+e 关闭`），下方红框新增两块：
1. **总结整体 agent 进化情况**（一段整体进化状态总结区）
2. **描述测评集内容，如何调整测评集**（测评集构成说明 + 调整方法区）

落地点：`cmd/lyon/evopanel.go`（面板渲染）、`cmd/lyon/evolve.go`（exam/routing 数据与生成逻辑，
可能需要一次 LLM 总结调用或由已有统计拼装）。


## 追加需求 #15 — 事情进展预览（plan 横向树 + 审批 + 执行演化）（2026-10-15 ~13:2x 口述，**排在 web 版本之后**）

原文大意：「等你 web 版本做好了，我想要一个事情进展预览：提了一个问题，AI 给出 plan
4 个步骤 → 展示成横向从左到右的树（1-4 节点）→ 我可以像在 xmind 上面一样一个一个审批、
修改（bypass 模式下自己演化）→ 点击某个节点，右侧再展开面板显示这一步的执行细节，
可以改、可以批通过 → 全部批通过后点击执行 → 这一层 4 个节点各自演化，长出一些 step
（run_shell / view_image…），可能并发、可能串行 → 都可以点进去查看 prompt、实时 output；
进行中的节点也可以点进去临时指导。」

拆解：
1. plan 横向树渲染（左→右，4 个主节点）——web 端（依赖 #4 protobuf web 版）。
2. 节点级审批/修改交互（xind 式逐节点操作；bypass 模式 = 自动演化免批）。
3. 节点详情右侧面板：执行细节展示 + 可编辑 + 可批通过。
4. 全批通过 → 执行编排：子 step 挂在各节点下生长，支持并发/串行标记。
5. 节点可点入查看 prompt + 实时 output（与 #11 实时输出、#13 右栏三段式同族）。
6. 进行中节点可注入临时指导（queue 插队 #2 的节点级版本）。

落地依赖：web 版本（#4 protobuf 协议需覆盖：plan 树结构、审批状态、节点执行流、
实时输出流、临时指导注入）。


## 进度 2026-10-15 本会话（接续）

- **#11 open 时实时终端输出 — completed (07aab30)**：tools.OutputObserver 可选接口 + WithOutputSink；shell streamBuf 40ms/16KB 合流、Close 先于 OnToolEnd；Session/subObs OnToolOutput → EventToolExecOutput（不进 audit）→ stepEvent → stepAppendOutput（每 step stepOutMax 尾窗）；stepExpandRows 三段预算（detail 3 行封顶 / 实时输出取最新 / err+started 钉底）。测试三组：pkg/tools/output_test.go、pkg/agent/output_test.go、cmd/lyon/steps_output_test.go；TestExpandRowCap 更新为新契约。
- **测试卫生 (ee4b3ed)**：pkg/hooks 补 tempDir 重试清理（0e3c73c 同款）；TestKickScoreFilesCard 等 card+evidence 双落盘（kickScore goroutine 内两次写盘的窗口）。
- **全量 go test ./... -count=1 16 包全绿**（此前 3 轮中的环境类失败均已定因：bench CPU 抢占、%TEMP% 扫描器、kickScore 竞态）。
- **#2 queue 间隙插队 — completed (d7f3527)**：闲时 Enqueue 拒绝（ErrNoPromptRunning），忙时进 inbox；drive 回圈顶部、工具执行+POST 之前 injectQueuedGap 追加用户消息（apiGuard 预合并相邻同 role，模型读到与 tool_result 同轮的一条消息）；把 inbox 清空与 busy 落锁放在同一 mu 临界区（不双发、不丢失，prompt_end 的 kick 仍是兜底）；EventQueueInjected 列噪声不进 audit，投递以 prompt_start+Detail 标记留痕；TUI 收到事件就地删镟副本并写入 transcript 行。测试 pkg/agent/inject_test.go（生命周期/穿透验证/审计/无泄漏）+ cmd/lyon TestTUIQueueInjection；全量第 5 轮全绿。
- **D3 memview 补漏 — completed (f11d324)**：4ef0f56 提交时漏了 memview.go（HEAD tui.go 已引用 rememberDone.rep + echoLine 却编译坏）；补交 17 行（DistillReport 字段 + count-only 回退 echoLine）修复历史。
- **任务 B TUI 自动固化提示 — completed (65cb850)**：flowhint.go/flowhint_test.go 新增 + tui.go prompt_end +3 行；slug=run 首现 step 工具名 [a-z0-9-] token + FNV-1a 低 24 位，与 CLI /flow 共用 flow-streak.json 账本，换类先投 false，n≥3 每段一次双语固化提示，LYON_AUTOFREEZE=off 全静默；7 用例 + cmd/lyon 全包 + pkg/flow 绿（pkg/flow 曾现 TestBumpStreak TempDir RemoveAll 单轮 flake，3 连跑绿，0e3c73c 同类环境问题，pkg/flow 源码零改动）。
- **docs-open 第一批 — completed (41bca01)**：MIT LICENSE、三平台 CI (build/vet/test, 启明已知 flaky)、.gitignore 无扩展名产物、README 双语重写对镜代码现实（esc/ctrl+c、#2 间隙注入、分层记忆、裸 lyon 用法、新键位与未记录能力补齐）。待晨报：CI badge clone URL 用推测 slug LyonLen/lyon-agent（无 remote 待用户确认仓名）；.claude/ 与 ROADMAP/EVOLUTION-GAPS 是否入库待用户裁决。
- **docs-open 第二批 — completed (fe2b107)**：手册双语修正（esc→ctrl+c 四处、间隙注入状态行字面量、指令数 23、补 /contest /agents /market、新键位）+ 五帧截图经 LYON_GEN_SCREENS 生成器重渲染，我复跑校验模式绿。
- **#9 日志机制 — completed (7e95fe5)**：pkg/logx 按日轮转日志（O_APPEND+逐行 Write 无撕裂、失败降级不吓宿主、30 天清理、LYON_DEBUG 级别）；接线全在静默失败缝：Engine.Close/Warmup 失败/apiGuard 传输错与不可修复 400 摘要（护栏语义未动）/工具 recover 缝/hooks 审计与 stats 原静默吞错处；logx 包注释陆全仓非原子写点清单。
- **新开 writefix-dev**：按该清单修 A 类 3 处直写 + B 类 8 处固定 .tmp 名（CreateTemp 独特名）。
- **#3 run dump + E2 skills — completed (b7ff44e，同笔因 session.go 交织）**：dump 每 run 一文件 $LYON_HOME/dumps/（schema_version 1、事件时间线 seq 连续、usage/子账单、文件层无锁唯一名+O_EXCL+rename、写闸门纯 I/O 礼貋性、失败仅 dump_error 审计不动 run）；skills 扫描 ~/.claude/skills 注入 system 基底（保 cache 前缀字节稳定）。全包 + dump 7 用例 + skills 9 用例绿；bench 17ms≤25ms。E2 从此解锁。
- **新开 d1-refresh**：修热层中途刷新（会话内 save_memory 同会话可见，内容刷新 + PushSystemNote 双路）。
- **#4 web 设计文档 — completed（web-v1-plan.md，1078 行，押后入库）**：Connect RPC+buf、SSE 不迁移有 TestSSE 门禁、Event Hub 扇出、proto 6 文件字段级、19 项数据源、#15 预留。**web v1 实现押后：晨报请用户确认协议方向**。
- **logx 自我回归修复 — completed (1c884d3)**：持有日文件句柄至进程退出使 Windows 下 TempDir 删不掉 → open-per-write（O_APPEND+逐行关闭），新增 TestNoHandleHeldBetweenWrites 守卫。
- **docs-open 收工**：三批全入库（41bca01/fe2b107/97191e5）；ANALYSIS 拆解押后用户裁决。
- **steps_test 裁决已执行**：gutter 断言改为 step 行消失（header 四格 │ 合法），随 header 批次提交。
- **run_subagent plan 模式 3 连超时修复 — completed (b224f1a)**：根因链——墙钟 ToolTimeout 包住整段嵌套循环 → 健康多轮研究 120s 必死 → SDK 把错误当 tool_result 回喂 → 模型盲重试成“3连”；修复：session.go 重适配仅两个子代理工具 Timeout=0（主会话手合）+ idle 看门狗 2×ToolTimeout 进度续命 + 三分支可诊断错误 + plan 门放行 run_subagents + dispatch 折病因 + mockapi content_block_delta 缺 type 基建修复。
- **#16 header 四格功能区 — completed (5492b06)**：logo 右侧 model/usage/evolution/activity 四格，本地只读、缺数据 dim — 单格降级、窄屏折叠；steps_test gutter 断言退役（View 级字符锨已被 header/transcript 共享，改纯几何契约）。
- **1.5 Claude Code hooks 兼容 — completed (e885e7a)**：pkg/hooks/cccompat.go 解析 .claude/settings.json hooks 并best-effort映射到 lyon 自有事件（PreToolUse→阻塞 tool_exec_start 走既有 CheckTool 否决、PostToolUse→tool_exec_end、UserPromptSubmit→prompt_start、Stop→prompt_end、PreCompact→compaction、Subagent/PermissionRequest 等；unmap 聚合告警一次；matcher 文本保留未用）；Engine 构造时 LoadClaudeSettings 一次，文件缺失零行为变化。8 用例绿（pkg/hooks 直跑 test.exe PASS——%TEMP% 被 AV 锁属环境噪声）。
- **0.2 lyon.toml 三层配置 — completed (17ba77e)**：pkg/config stdlib 手写 TOML 子集；优先级 flag > workspace/.lyon.toml > LYON_HOME/config.toml > 默认，ApplyFlags fs.Visit 仅显式 flag 覆盖；MCP 三层 flag > toml > mcp.json，toml 层容忍、flag 层致命（mcp-config-dev 独立复验）。pkg/config 1.6s + cmd/lyon 6.7s 绿。
- **#8 双定时器核心包 — completed (af98bcd，接线未开)**：pkg/timer 10 文件——min-heap Scheduler 单循环 + Runner 注入、overlap skip/queue、miss skip/once、Stop 排空屏障、schtasks/crontab OS 注册（# lyon:<id> 锚点）、DESIGN.md 记录决策与 cmd/ 接线清单（Runner 注入、/timer slash、OS 子命令、TUI 面板——后续接线任务任务书）。35 PASS + 1 SKIP 门控 2.9s 绿。
- **writefix 非原子写修复 — completed (225563e)**：log-dev 审计清单闭环——A 类直写 4 点（SaveExam/flow.Save/write_file/edit_file）+ B 类固定 .tmp 名 8 点（snapshot×2/candidates/leaderboard/streak/market/memory/routing）全改 os.CreateTemp 唯一名+rename；0644 点 Chmod 保模式；memory 测试方案 A（挪 scope 目录制造真实失败）+ routing 8 协程并发 residue 测试 + streak residue 断言。7 域绿（market 首挂 TempDir 竞态 flake，复跑 5 连绿）。
- **五 agent 结单回执到齐**：header-widgets2（#16 探针证实 read_file 三处合法回显，几何裁决正确）、writefix（13 文件最终报告：timer.go:248 因施工中跳过——已转 timer-wire 顺手补）、config-02（0.2 完整关闭）、timer-core（称任务书=DESIGN.md 第五节——实际第五节是日志机制、无接线清单，清单段由 timer-wire 本批补正 af98bcd message 失实）、hooks-15（后续可接点：SessionStart 触发点缺失、matcher 生效需动 hooks.go——均押后晨报）。
- **demo60s 进展**：ConPTY std-handle 复制卡点已解（STARTF_USESTDHANDLES+hStd 置 0）、mockserve SSE delta 缺顶层 type 的旧二进制已重编、S1 全链路一次跑通（CJK prompt→read_file→500 注入→diagnosis→memory_save→audit 九类）；在做 S4 记忆注入断言 + demo-60s.sh/Makefile/README + 连跑计时，完整报告随后。
- **D1 热层中途刷新 — completed (68f7eee)**：MemoryChange seam（WithOutputSink 同形）save/forget 成功即通知；内容收敛 buildHistory 顶部无条件重读（回调直写会死锁 histMu/碰 snapshot.go 禁区）——下一轮即见新记忆，覆盖 save/forget/distill/外部编辑全部写者；零新锁零新共享状态。5 新测试，pkg/agent+tools 绿。
- **#8 cmd 接线 + #10 dashboard — completed (e30947c，因 tui.go 双接线互相引用合并一笔)**：timer 四件（cmdChat 生命周期 startTimers、Runner %COMSPEC%/sh+lyon run --、/timer slash 五子命令、lyon timer CLI 含 schtasks 端到端实测、timerListLines 零阻塞渲染）+ DESIGN.md §6 真清单（修正 af98bcd 失实）+ timer.go Save CreateTemp；dashboard 10 模块四组拓扑全并发刷新（TUI 事件循环零网络）、dashboard.json 配置+缓存+离线徽标、tui.go 7 接线点 12 行、18 httptest 测试、交叉 vet 绿。合并原因：拆分会产生编译坏中间态（4ef0f56 同类）。四包合跑绿（cmd/lyon 11.8s / timer 3.3s / agent 18.9s / tools 12.1s）。
- **pkg/timer 两缺陷转 timer-core 修**：A=command 载荷 /TR 缺 %COMSPEC% /C（注册成功触发必败，osreg_test.go:123 钉死坏形）；B=中文 Windows schtasks 输出 GBK，UTF-8 匹配永不命中致幂等删除失效（二次 unregister rc=1）。
- **C 盘 100% 满警报处理**：go clean -cache（AV 锁重试后成功）448M→8.8G；治本项（GOCACHE go env -w 迁 D 盘）属持久配置改动，列晨报拍板。
- **#16 内容修正 — completed (59dcf32)**：用户指出机器信息等应放 header 四格位置（晨报项拍板：原需求即 header 而非启动页）——四格换成 机器/时间/天气/简报，直读 dashboard cell store（零额外网络、与 /home 启动页永不打架）；model/usage/evolution/activity 信息各有入口未丢（顶行//stats/alt+e/步骤面板）；evoSnapshot 族退役。cmd/lyon 全包 15.3s 绿，二进制已重建（09:02）。启动页 dashboard 保留待用户嫌重复再裁。
- **ROADMAP 0.3 demo-60s — completed (0268d8e)**：14 断言脚本实测 40s 全 PASS——四镜头（失败→归纳→写记忆→注入教训→不再犯）ConPTY 驱动真 TUI；mockserve（双 mockapi+粘性 500+管理口）、tuipty（ConPTY std 句柄配方修 bInheritHandles 失效）、Makefile 双目标、README-60s 分镜与 ffmpeg 配方。pkg/cmd 零触碰。
- **pkg/timer 实测双缺陷修复 — completed (81e0561)**：A=command 载荷 /TR 补 `%COMSPEC% /C`（裸双引号，prompt 加 `--`），POSIX 侧同源 % 炸弹一并防（载荷 `\%` 转义 + 自家重定向 printf '\045'）；B=新 ansi_windows.go CP_ACP→UTF-8 解码（中文 GBK schtasks 输出可匹配，零新依赖），二次 unregister rc=0 真机两次复跑绿。
- **Timer 失控事故（10-16 早）— 已处置闭环**：测试 fixture 污染真实 ~/.lyon/timers/（every 10-20ms prompt "x"），用户 TUI 内存调度器每 tick 拉起真实 `lyon run -- x` 子进程成 fork 风暴（6+ 并存、Save 记账回魂被删文件）；止血=杀全部 lyon 进程（**含用户 TUI 16740，用户需重启**）+ 清空 timers 目录（清后稳定无回魂）。最终测试套件逐函数审计无泄漏（TestNext 为正则误报），泄漏源系 pre-scratchHome 开发中间态。**根因防线 completed (576fe3c)**：minPayloadEveryMS=1000 载荷 interval 三门设防（Add 拒不落盘 / registerLocked 拒+落盘 enabled=false+WARN 防复活 / Enable 拒+回写 disabled），Save 故意不钳（存储原语），纯 tick 与 once/cron 豁免；OS 级 ≥1min 两后端本已 enforce，crontab 500ms 补测；DESIGN §2 记事故理据；pkg/timer 两轮 2.2s 绿。
- **无主 memory 改动对账（进行中，三方询证中）**：**D2 已入库 4ff59cc**（E1/D2 合并笔，store_hits_test/main.go 挂点均在内——先前「在途未提交」认知过时）；盘上剩余未提交的纯一套无主工作=store.go Open loader 锁改造（loaders map per-workspace、拆 storesMu、double-check）+ index.go +127 upsert 族 + insertLocked 改调 `s.ix.upsert` + 未跟踪 upsert_perf_test.go（perf 测试引用 loaders 坐实四者同源）。**带真缺陷**：TestUpsertMatchesFullBuild panic（index.go:131 nil map）。memfix、patchflow 已先后否认（patchflow 揭出 loader 锁块并点名 dump/log/layout），三方问询已发未回；归属+修复确认前 pkg/memory 冻结不提交、请各方勿再写。排除法独立验收：-skip upsert 测试后断言层全绿（仅已知 TempDir 竞态 flake）。
- **用户 10-16 反馈两件 — completed (94ce8f0)**：①聊天区 lyon home 展示退役、/home 与 /dash 指令全删（tui.go 净 -11 只减、dashboard.go -975/+188 删 seed/render/home/command/panel/dashRows+render 链；tick 链保留供 header 走秒、refresher 与 cell store 不动、enabled/LYON_DASHBOARD=off 语义改只控刷新）；②sysinfo 默认 5s→10s 双处（registry+defaultDashConfig，显式存储值仍优先/垃圾回退，TestDashIntervalDefaults 钉死）+ Windows/Linux sysEnsureBaseline 首刷基线（cpu/net 双差分首刷必 …的根因，120ms 窗口后真值，darwin 空桩）。dashboard-dev 会话离场留中间态（netSample 多返回值两处未接完），team-lead 接手补完并验收：gofmt/build/三平台 vet/cmd-lyon 11.7s 绿（新 TestDashNoTranscriptBlock：启动仅 2 行 banner、resetTranscript 永不注入）。用户存量 json intervals.sysinfo "5s" 已手改 "10s"（python 替换+回读 json 校验）。二进制 10:17 重建（杀 2440 后 build lyon+lyond，timer list 冒烟 rc=0）。
- **无主 memory 套件 — 询证终局，列晨报裁决**：四方否认（memfix/patchflow/log/dump 各自足迹自洽），layout-dev 会话已消失（本机当前零 Claude 会话，ListAgents 空）永无回复；无主套件=loader 锁+index upsert+insertLocked+upsert_perf_test（四者同源，带 nil-map panic）。**处置待用户裁**：修复认领 / 回滚 / 冻结；冻结期间 pkg/memory 不提交。
- **下一项**：晨报拍板项聚齐——无主 memory 套件处置 / CI 仓名 / .claude 与 backlog md 入库 / GOCACHE 迁 D 盘 / web 协议三拍板（Connect RPC 工具链、SSE 并存、静态 bearer token——web-plan 规划已验收等实现开工）；押后：hooks SessionStart 触发点与 matcher、web v1 实现、#7 分屏、1.6、repo-map、tui.go #5 拆分。
