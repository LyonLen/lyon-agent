# lyon ROADMAP：从「机制齐全」到「好用 + 被看见」

> 2026-10-15 由 lyon 产出，与 `EVOLUTION-GAPS.md` 配套（那份讲「哪里断了」，
> 这份讲「怎么变成一个好产品」）。总体判断：
>
> **lyon 有三张别人没有的牌——自进化系统、可重放的证据主义、中文原生；
> 但这三张牌目前都是扣着的（不闭环、demo 要 11 小时、MCP/skill 不可见）。
> 别在功能面上追 OpenCode/Crush，把「闭环的进化」做成 60 秒可见的事实，
> 其余都是分发问题。**

## 0. 定位与差异化

- **一句话定位**：the agent that grades itself and gets better
  ——给自己打分、考试、淘汰、路由，并且**越用越强**的 coding agent。
- **同行对照**：OpenCode/Crush/Codex CLI 的强项是 TUI 与 provider 接入；
  Aider 强在 repo-map 与 git 集成；Letta/mem0 强在向量记忆。
  lyon 的护城河是**进化系统**（考试/淘汰/路由/固化，全 GitHub 无第二家）
  与**全文件账本 + 可离线重放**（由此可产出月度《模型竞技榜》这种
  自带引用与讨论的公共内容）。
- **不做的事**（保护隐性优势「单二进制零依赖」）：
  - 不上向量/embedding 记忆（BM25 + CJK 二元组在无 embedding 服务的世界里
    就是最优解，词面召回够用）；
  - 不做云端、不做 SaaS、不做插件平台化；
  - 不追功能数量，每个新机制必须满足「机械归机制、聪明归模型」哲学。

## 阶段 0：能用（约 2 周）——先翻牌

目标：让「第二次比第一次强」真实发生且**一眼可见**。

- **0.1 闭环五任务落地**：`EVOLUTION-GAPS.md` 任务 A–E 全部完成。
  优先级 A（patch 回流记忆）> D（记忆回路）> E（MCP/skill 可见）
  > B（自动固化提示）> C（激活路由，运行操作）。
- **0.2 一份配置文件收编所有 flag**：新增 `lyon.toml`（`~/.lyon/config.toml`
  与 `<workspace>/.lyon.toml`，后者覆盖前者）：model、provider、effort、
  MCP servers、审批模式、shell 白名单……全部进配置；CLI flag 保留作覆盖。
  MCP 配置格式对齐 Claude 生态（`mcpServers`），能直接复用 `.mcp.json`。
- **0.3 60 秒 demo（本阶段核心交付物）**：
  - `make demo-60s`：mockapi 假模型故意在同类任务上犯同一个错 3 次；
  - 镜头依次给到：失败 → 错因归纳 → 写入记忆 → **下一次会话注入教训 → 不再犯**；
  - 录屏 + GIF 进 README 第一屏。这 60 秒就是项目的电梯演讲。
  - 现有 11 小时整夜 demo（`demo/evolve-loop.sh`）降级为「深度验证」保留。
- **验收**：新用户装完后 5 分钟内能亲眼看到「agent 因为上次的错而做对了」；
  `lyon.toml` 一个文件可完整复现配置；全量 test 绿。

## 阶段 1：好用（约 1 个月）——上手即达

目标：消除「装上 → 不知道干嘛 → 关掉」的流失。

- **1.1 首跑体验**：
  - 空目录直接 `lyon` 进 TUI，不报错不空转；
  - `/help` 分级：新手三问（我在哪、能干什么、怎么退出）一屏内回答；
  - `lyon doctor` 一键体检：网关连通性、MCP server 状态、记忆库规模、
    leaderboard/routing 摘要，输出可直接贴 issue。
- **1.2 安装一行命令**：`brew tap` / `curl -fsSL | sh` / scoop；
  发 GitHub Release（tag + 三平台产物 + checksum）。
- **1.3 git 即后悔药**（借 Aider）：workspace 改动随 session 自动
  commit（message = session id + prompt 摘要），`/undo` 一键回滚；
  白嫖整个 git 生态的信任感，也让 snapshot 有了第二重保险。
- **1.4 找代码不再是最贵的动作**（repo-map + 搜索工具，借 Aider，控制规模）：
  - **repo-map 轻量上下文**：cttags/简单解析生成文件+符号表，system note 注入
    「本仓库地图」，省掉模型盲目 grep/read 的探索轮；不做完整索引引擎；
  - **新增 `search_files` 工具**：pattern + glob + 上下文行数，**ParallelSafe=true**
    （内部 Go 扫描或调 rg）。现状搜索只能走 `run_shell`：每次 spawn bash
    （Windows ~100ms）且 shell 是非并行安全——模型批量搜索被 serialMu 串行排队，
    「并发倾向」在搜索场景适得其反。几十行代码，解锁并发搜索；
  - **`read_file` 加 offset/limit**：现状只有 Path，大文件整读或被 64KB
    ResultCap 截断、翻页靠 `sed` 迂回（多烧轮次）。十行代码，大文件取段
    从 3 轮变 1 轮；
  - 验收：5 个独立搜索并发发出时无排队（工具级并行可观察）；读 5000 行文件
    的指定段落一次 read_file 完成。
- **1.5 兼容生态的两个格式**：
  - 读 `~/.claude/skills/*/SKILL.md`（E2 已含），清单注入 + read_file 按需取；
  - 读 `.claude/settings.json` 的 hooks 段（能解析多少算多少），
    把「从 Claude Code 迁移」的成本压到接近零。
- **1.6 并发规划增强（并发倾向需求的收尾）**：
  引擎原语已就位（只读工具并行、`run_subagents` 扇出 2–6 子任务、dispatch.go
  明文写着「并发倾向 requirement」），缺的是让模型**养成规划并发的习惯**：
  - **习惯回路进 prompt**：todo_write 写完计划后——「计划中互相独立的项用
    run_subagents 一次派发，不要串行做」。一行 prompt，零代码风险；
  - **计划感知并发**：≥3 项独立任务时，首条 todo 即派发本身；steps 面板
    能显示「N 项并行中」；
  - **度量闭环**：stats 记 fan-out 次数/扇出子任务数，evolve 面板加「并发度」
    列——不度量就不会改善（闭环原则）。
  - 性能叙事纪律：对外不吹「快」（端到端是模型/网关绑定，本地 overhead 已有
    25ms p95 硬门限），吹「等待被重叠、轮次被省掉」——可验证的承诺。
  - 验收：≥3 独立子项的任务，模型首次尝试即用 run_subagents 扇出而非串行；
    stats 出现 fan-out 计数；steps 面板可见并行指示。
- **1.7 仓库卫生**：LICENSE（MIT）；CI（build+vet+test，Windows/Linux/macOS
  三平台）+ badge；`lyon.exe`/`lyond.exe` 移出 git（Release 分发）；
  docs/screens 截图换新；README 双语结构统一。
- **验收**：从「听说 → 装上 → 第一次正反馈」全程 <10 分钟；
  陌生人 clone 后 `make test` 绿、CI badge 绿。

## 阶段 2：被看见（持续）——内容与社区

目标：把机制优势变成公共内容资产。

- **2.1 月度《模型竞技榜》**：用自家 leaderboard 机制跑公开题集
  （SWE-bench-lite 子集 / 自建中文题集），每月一篇博客：
  谁在 fix 类上岗、谁被撤职、路由置信度怎么变。
  评估内容天然自带引用与讨论，且**只有 lyon 能持续产出**（机制现成）。
- **2.2 狗粮叙事**：项目自身的 issue/PR 用 lyon 跑，定期发
  「lyon 用自己开发自己」复盘（128 轮会话、错因归纳实例都是现成素材）；
  demo-60s 的真实版（不是 mock）作为第二弹。
- **2.3 分发渠道**：Show HN（标题带 grading/self-improving，别叫 agent 拼音）·
  r/LocalLLaMA · r/ClaudeAI · awesome-claude-code 等清单收录申请 ·
  中文：V2EX / 掘金 / 即刻（中文原生是地理优势，Claude Code/Codex 的
  中文体验都是二等公民）。
- **2.4 节奏**：大版本（带可见变化）+ 月度竞技榜交替出现；
  每次 Release 附 changelog 与升级一行命令。

## 贯穿原则（每个阶段都适用）

1. **闭环原则**：任何新机制必须同时交付「写端 + 读回端」——只写账本不喂回
   上下文的功能，等于没做（五个断点的共同根因，勿再重蹈）。
2. **哲学一致**：机械的归机制、聪明的归模型；一切状态皆文件、可重放；
   证据治国（上岗靠考分）；每个机制可被环境变量关掉且系统照常可用。
3. **60 秒法则**：用户感知不到的效果 = 不存在。任何「进化/记忆/固化」类
   改进，都要能在 60 秒内演示出 before/after。
4. **禁区不动**：`pkg/agent/preflight.go` 消息守卫勿碰；`tui.go` 只减不增
   （拆分是评审待修 #5）；不引外部服务依赖。
