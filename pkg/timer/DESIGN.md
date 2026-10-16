# pkg/timer 设计：双定时器（进程内 + 操作系统级）

用户需求 #8：「进程内定时器、操作系统定时器，两种都要，要做日志机制」。
两种定时器共享**同一个 Task 定义**与同一份 JSON 持久化；差别只在「谁来数表」：

| | 进程内 Scheduler | OS 级注册 |
|---|---|---|
| 存活范围 | lyon 进程活着才走拍 | lyon 退出后 OS 继续触发 |
| 调度方 | 单 goroutine + 最小堆 | Windows 任务计划 / POSIX crontab |
| 执行体 | 注入的 Runner（默认拉起 `lyon run` 子进程） | OS 直接拉起 `lyon run` |
| 适用 | 会话期间的提醒/轮询/节拍任务 | 每日报告、夜间构建这类无人值守任务 |

## 1. Task 定义与持久化（一切状态皆文件）

- 一个任务一个文件：`$LYON_HOME/timers/<id>.json`（`$LYON_HOME` 缺省 `~/.lyon`）。
- `kind ∈ {once, interval, cron}`：
  - `once` — `at`（RFC3339）触发一次，触发后自动 `enabled=false`；
  - `interval` — `every_ms`（毫秒，进程内粒度；OS 级要求 ≥ 1 分钟）；
  - `cron` — 简式 5 段 `分 时 日 月 周`，支持 `*` `a` `a-b` `*\/n` `a-b/n` 列表，
    月/周支持 `JAN..DEC` `SUN..SAT` 名字；日/周语义遵循 POSIX（两者都受限时取 OR）。
- 载荷二选一：`command`（shell 命令）或 `prompt`（交给 `lyon run` 的提示词，可带 `workspace`）。
- 运行时字段随跑随写：`last_run` / `last_status` / `last_error` / `run_count`。
- `Save` 原子写（tmp + rename）；`Load` / `Delete` 与 `Save` 同走一把**包级互斥锁**——
  Windows 上读与 rename-over 并发会直接报「文件被占用」共享冲突，串行后彻底消失。
- `LoadAll` 对损坏文件**跳过并记日志**，绝不因一个坏文件拖死全部定时器。

## 2. 进程内 Scheduler

- 单 goroutine 事件循环 + `container/heap` 最小堆；`Start(ctx)` / `Stop()`（均幂等）。
- 触发回调（Runner）**在独立 goroutine 执行**，调度循环永不被回调阻塞；
  回调带 `context` 超时（任务级 `timeout_ms` 覆盖全局默认），超时/报错全部落日志。
- **Stop 是排空式屏障**：取消 ctx → 等循环退出 → 用 `WaitGroup` **等在飞回调收尾**
  （有界 2s 宽限，超时记 WARN 后放其在后台自行结束）。Stop 返回后保证：
  不再有新的触发落地、在飞回调的记账已写盘。被 Stop 打断的回调只记一条
  **aborted** 日志、**不改写** `last_status`——停机不是任务失败，不能用
  `context canceled` 覆盖上一次真实的 `timeout`/`ok` 状态。
- **Remove / Enable(false) 语义**：不等待在飞回调（否则一个卡死的 runner 能把
  `/timer disable` 卡到超时上限），已在飞的那一次允许跑完（running 标志保证
  至多一次）；之后不再有新触发。Remove 的防复活双保险：在飞期间 id 已从注册表
  删除 → 记账保存被抑制；一拍内烧完的 one-shot 走 `spent` 标记仍会落最终记账
  （并自动 `enabled=false`），但 `deleted` 台账兜底——Remove 之后任何保存都
  不会让文件复活。
- **并发触发策略（上一拍没跑完，下一拍来了）**：
  - `overlap=skip`（默认）：跳过这一拍并记 `WARN` 日志。理由：多数任务是
    「刷新状态/抓取数据」，重复并发执行既浪费 token 又可能互相踩文件；
    跳过永远安全，语义上「最近一次还在跑 = 结果已有」。
  - `overlap=queue`：挂起**一次**补跑（多拍合并为一次，不排队堆积），上一拍结束
    后立刻执行。适合「必须每拍都有结果」的任务。选默认 skip 是因为它是无脑安全项；
    queue 的合并语义避免了无界堆积。
- **崩溃恢复**：`Start` 时 `LoadAll` 加载全部任务，下一拍一律从 **now** 计算，
  **默认不补跑**（miss=skip）——错过的拍记一条 INFO 日志。可选 `miss=once`：
  若宕机期间确实错过拍，启动时**补跑一次**（只一次，不逐拍补）。理由：默认不补
  是因为补跑风暴（停机一夜后连跑 200 个 cron）比漏一拍伤害大得多。
- **载荷 interval 下限 1000ms（fork-storm 防线）**：带 prompt/command 载荷的
  interval 任务每触发一次就 fork 一个 `lyon run`/shell 子进程，亚秒级间隔 =
  每秒几十个子进程。真实事故（2026-10-16）：一个 15ms 测试夹具泄漏进真实
  `~/.lyon/timers/`，用户 TUI 加载后每 15ms fork 一个真 `lyon run -- x`，
  机器上堆出 9 个子进程才被杀掉。因此三处设防，全部拒绝入堆：
  `Add` 直接报错**不落盘**（坏参数无法持久化）；`Start` 加载与 `Enable(true`
  遇到既有低于下限的任务时**落盘 `enabled=false` + 一行 WARN**
  （防复活——否则每次启动都会重新卷起风暴）；`Enable` 同样拒绝并保持落盘
  disabled。`Save` 本身**不钳**：它是存储原语，在飞记账必须原样写回。
  无载荷的纯 tick 不受限（不 fork 任何东西），once/cron 本就一分钟粒度或
  一次性；测试经 `scratchHome` 临时调低下限以注入非 fork 的快速 runner。

## 3. OS 级定时器

统一入口 `Register/Unregister/List`，内部按 `runtime.GOOS` 分发；命令行**构造函数是纯函数**，
在任何平台都可测试。

### Windows（`schtasks`，任务名前缀 `lyon-`）

- 创建：`schtasks /Create /TN lyon-<id> /TR <cmdline> /SC <type> [/MO n] [/D days] /ST HH:MM [/SD yyyy/mm/dd] /F`
  （`/F` 幂等覆盖；语法已对照本机 `schtasks /Create /?` 输出核实）。
- 映射：interval（整分钟、1..1439）→ `/SC MINUTE /MO n`；
  cron `m h * * *` → `/SC DAILY /ST m:h`；`m h * * <dow集>` → `/SC WEEKLY /D MON,TUE.. /ST`；
  `m h d * *` → `/SC MONTHLY /D d /ST`；`once` → `/SC ONCE /ST /SD`。
  其余 cron 形态**拒绝并报清晰错误**（提示「保持进程内」），不半残注册。
- 删除：`schtasks /Delete /TN lyon-<id> /F`（不存在视为成功，幂等）。
- 列举：`schtasks /Query /FO CSV /NH`，筛 `TaskName` 前缀 `\lyon-`。
- 无权限（Access denied / 拒绝访问）：原样返回带提示的错误，**不做任何半程状态**。
- **输出按 ANSI 代码页解码**（`ansiToUTF8`，kernel32 `MultiByteToWideChar(CP_ACP)`）：
  中文 Windows 的 schtasks 走管道时输出 GBK（ACP 936），`找不到`/`拒绝访问` 的
  UTF-8 字节匹配永不命中——幂等删除与权限识别在本地化系统上会失效。解码后统一
  在 UTF-8 文本上匹配，错误消息也因此可读；纯 ASCII 原样通过。
- **`command` 载荷的 /TR 是 `%COMSPEC% /C "<cmd>"`**（注册时展开为绝对路径，缺省
  `cmd.exe`）：lyon 没有 `/C` 动词，`<lyon.exe> /C ...` 注册成功但触发必败。
  `/C` 后用**裸双引号**包裹命令——cmd.exe 自己剥引号、不认反斜杠转义，命令必须
  原样到达（内嵌引号按「首尾引号剥离」规则正确还原）。

### POSIX（crontab，`# lyon:` 注释锚点）

- 行形态：`<cron> <command|lyon-run> >> "<logs>/timer-os-$(date +$(printf '\045F')).log" 2>&1 # lyon:<id>`
  —— 锚点在行尾、在 shell 里是注释，cron 实际执行时被 `sh` 剥掉，零副作用。
- **% 逃逸**：Vixie 系 cron（vixie/cronie/anacron、macOS）会把命令段里裸的 `%`
  变成换行再交给 sh——载荷里的 `%` 统一在注册时替换成文档惯用的 `\%`（恰好一次，
  用户不要自己预先转义）；我们自己的日期重定向则用 `printf '\045'` 在运行时拼出
  `%`，整行不含 `%`，不依赖任何一种 cron 对反斜杠的处理语义。
- 注册 = `crontab -l`（无 crontab 视为空）→ 按锚点幂等去重 → `crontab -` 写回。
- 删除/列举按 `# lyon:<id>` 锚点识别自己的条目，绝不动用户的其他行。
- interval→cron 只接受能整除 60 的分钟数（`*/n` 在 n 不整除 60 时不是严格每 n 分钟），
  `once` 在 POSIX 无原生对应 → 报清晰错误，提示用进程内。

## 4. OS 级执行的命令行形态

`lyon run` 是非交互子命令（`lyon run [flags] "prompt"`，prompt 也支持 stdin），
所以 OS 任务的拉起命令是：

- Windows：`"C:\path\lyon.exe" run -- "提示词"` —— /TR 里 exe 带空格要整体加引号，
  prompt 按 CommandLineToArgvW 规则转义（`\"`、尾部反斜杠加倍）；`--` 保证破折号
  开头的 prompt 不会被 `lyon run` 的 flag 解析吞掉（Go flag 在 `--` 处终止）。
- POSIX：`'/path/lyon' run -- '提示词'` —— sh 单引号转义。
- `command` 载荷不经过 lyon：Windows 是 `%COMSPEC% /C "<cmd>"`、POSIX 直接交给
  cron 的 sh（crontab 行本身就是 shell 命令），见 §3。
- exe 路径取 `os.Executable()`（EvalSymlinks 尽力解析），也可显式指定便于测试。
- prompt 必须单行（schtasks /TR 上限 261 字符；crontab 是一行）——校验并给清晰错误。

## 5. 日志机制

- 文件：`$LYON_HOME/logs/timer-YYYY-MM-DD.log`，append + 全局 mutex，每行
  `2006-01-02 15:04:05.000 [LEVEL] msg`。
- 每次**触发 / 跳过（overlap、miss）/ 成功（含耗时）/ 失败 / 超时 / OS 注册与删除**都落盘。
- OS 级任务的 stdout/stderr 重定向到 `$LYON_HOME/logs/timer-os-$(date +%F).log`（POSIX）；
  Windows 的输出在 schtasks 历史里。
- log-dev 正在做统一的 `pkg/logx`：当前 log.go 是极简自有实现，
  **全部日志都走 `logLine()` 一个漏斗**，logx 落地后把漏斗体换成转发即可。


## 6. cmd/lyon 接线清单（issue #8 接线批次）

af98bcd 的提交信息曾误称本文件已含此清单；接线实际在后续批次落进 cmd/lyon——此节为落定后的真本。

### 6.1 Runner 注入（cmd/lyon/timerctl.go · timerRunner）

- `command` 任务 → 平台 shell 子进程：Windows `cmd /C`（`%COMSPEC%`，缺省 `cmd.exe`）、POSIX `sh -c`；
- `prompt` 任务 → `lyon run [-workspace <绝对路径>] -- <prompt>` 子进程——argv 与 OS 级注册同形，
  `--` 保证破折号开头的 prompt 不会被当 flag；
- 子进程经 `exec.CommandContext` 绑定本次 fire 的超时 ctx：Stop/timeout 直接杀进程，
  runner 满足「ctx 结束立即返回」的契约，调度循环与 TUI 永不被它阻塞；
- 输出：成功把尾部（≤1200 rune，按 rune 安全截断）落 INFO 日志；失败把尾部折进返回错误，
  由调度器落 ERROR 与 `last_status=error` / `last_error`。

### 6.2 Scheduler 生命周期（cmd/lyon/main.go · cmdChat）

- `startTimers()` 挂在 cmdChat：engine/session 建好后 Start（加载 `$LYON_HOME/timers`，
  加载完才返回），`defer stopTimers()` 在退出时 Stop 排空（包内 2s 宽限；被停机中止的
  fire 只记 aborted，不改写上一次真实状态）；
- TUI 与 `-plain` REPL 同生命周期（都走 cmdChat）；
- `lyon run` / `lyon tools` **不**启动进程内调度：一次性子命令里再开一个 scheduler，每个被
  OS 拉起的 prompt 任务都会二次加载同一 store 造成重复触发；
- 单例指针 `timerSched`（互斥保护）由 start/stop 发布/摘除；slash 处理读它走 scheduler-aware
  路径（Add/Enable/Remove 连堆一起改），会话外 nil 自动退化为纯文件操作。

### 6.3 用户可见面

- **TUI `/timer`**（cmd/lyon/timerpanel.go）：`add` / `list`（裸命令即 list）/ `enable` /
  `disable` / `rm`。list 是 `timer.LoadAll()` 本地快照 → `id · kind · schedule · next ·
  status · runs · payload` 表格（next 为相对时窗，远期给绝对时刻）——全程零 OS 调用，
  渲染与 slash 循环不被 schtasks/crontab 阻塞。
- **CLI `lyon timer`**（cmd/lyon/timercli.go）：与 TUI 同一条 add 行、同一组 store helper
  （退出码 0 成功 / 1 操作失败 / 2 用法错），外加 OS 面 `register` / `unregister` /
  `os-list`（`timer.NewBackend()`：Windows schtasks、POSIX crontab）。`rm` 对仍注册的 OS 任务
  best-effort 提醒；`register` 对仍 enabled 的任务提示「进程内也会打，disable 可只走 OS」；
  `enable/disable` 提示运行中的 chat 只在启动时装载（见 6.4-2）。
- **add 行语法（两面共用一个 parser）**：`<id> <once|every|cron> <spec…> <command|prompt>
  <payload…> [k=v…]`，k=v ∈ {ws=, overlap=, miss=, timeout=}。once 支持 `+duration` 与
  RFC3339（带/不带时区，必须在未来）；every 为 Go duration；cron 简式 5 段。prompt 缺省
  workspace：TUI 取会话 cfg.Workspace，CLI 取启动目录——存绝对路径，OS 拉起时同源。

### 6.4 已知边界（接线批次如实记录）

1. **跨进程互斥缺失**：saveMu 只护单进程；两个并发 `lyon chat` 会对同一 store 双重触发。
   进程内调度的诚实前提 = 恰好一个交互会话。
2. **`lyon timer disable` 对运行中的 chat 不生效、还可能被回写**：运行中的 scheduler 只在
   Start 时装载，fire 后按内存态（enabled=true）回写文件。live 状态一律用 `/timer` 管；
   CLI 面向离线/单进程（输出已带提示）。后续工作：mtime 感知或跨进程心跳。
3. **Windows `command` 载荷的 OS 注册动作有上游缺陷（已实测）→ 已修复**：`SchtasksTaskRun`
   曾生成 `<lyon.exe> /C "<cmd>"`，而 lyon 没有 `/C` 动词——注册成功、触发必败。现改为
   `%COMSPEC% /C "<cmd>"`（注册时展开绝对路径，裸双引号包裹）；osreg_test.go 的钉死用例
   已改为 cmd.exe 形态。真机触发回归：`LYON_TIMER_ITEST=2` 门控的
   `TestSchtasksCommandTriggerIntegration` 注册真实 ONCE 任务并等探针文件落盘。
4. **中文 Windows 的 schtasks 输出是 GBK → 已修复**：`找不到` / `拒绝访问` 的 UTF-8 字节
   匹配在本地化系统上永不命中（实测二次 unregister 报 rc=1）。现所有 schtasks 输出先过
   `ansiToUTF8`（CP_ACP→UTF-8，kernel32）再匹配：幂等删除与 access-denied 识别在
   936/1252 等任何 ANSI 代码页上都成立；GBK 固定样本的解码有 windows-only 单测钉住
   （非 936 机器跳过）。
5. **进程树与回灌**：CommandContext 只杀直接子进程；prompt/command 的输出只落
   `$LYON_HOME/logs/timer-*.log`，不回灌 TUI（fire 时的 TUI 提示列为后续工作）。
6. **`-plain` REPL 没有 `/timer`**（调度器照常跑）：管理走 `lyon timer` 或全屏 TUI。

### 6.5 测试

- `cmd/lyon/timers_test.go`：parse 全表（含拒绝路径）、/timer slash 生命周期（无 scheduler
  的 store 退化 + 有 scheduler 的 live 路径，均 `t.Setenv("LYON_HOME", …)`）、`lyon timer`
  CLI 退出码与落盘、Runner 的 shell 成功/失败与日志落盘、`lyon run` argv、popup/help 发现性、
  任务文件 JSON 形态；
- OS 级：pkg/timer 的集成测试仍以 `LYON_TIMER_ITEST=1` 门控；接线批次另做了 Windows 实测
  （`lyon timer register → schtasks /Query 证实 → unregister → /Query 证实已删`，
  全程 scratch LYON_HOME，结束时系统内零 `lyon-` 任务残留）。
