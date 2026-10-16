# 60 秒演示：从失败中学习（demo-60s）

> 草稿：供主 README 合并。对应 ROADMAP 0.3。

lyon 连续三次在同类任务上栽跟头（假模型被脚本刻意带偏）：报错 → 自动错因归纳 → 教训写入项目记忆；下一个**全新会话**里，教训随记忆注入上下文，agent 读对了文件、算对了答案。全程离线（mock 网关，无真实模型调用），四个会话不到 10 秒。

## 快跑

```bash
bash demo/demo-60s.sh        # 或 make demo-60s
```

Windows + MinGW（Git Bash）环境；内部用 ConPTY（`demo/tuipty`）驱动真正的 `lyon chat` TUI——错因归纳的写回只存在于 TUI 事件环里，普通 `lyon run` 走不到。脚本自建三个二进制到 `tmp/demo60/`（tuipty、mockserve、lyon-demo），起一次性 `LYON_HOME` 与工作区，跑完打印 PASS/FAIL 清单；产物留在 `.demo/demo-60s/`，完整日志在 `.demo/demo-60s.log`。

## 分镜（脚本断言逐条对应）

| # | 镜头 | 屏幕上发生什么 | 硬证据（脚本断言） |
|---|------|----------------|--------------------|
| 1 | 失败 | 输入「读取 notes.txt 里的 port 数值，翻倍后告诉我。」→ 模型固执地读了 config.txt → 网关注入 500（fake model repeated its mistake）→ 状态行报错 | audit `prompt_start` 带完整中文 prompt；带 error 的 `prompt_end` |
| 2 | 错因归纳 | 报错后自动触发 post-mortem，诊断面板列出 failed conditions 与 patch | audit `diagnosis` 事件，patch 含 `demo60s-lesson` 标记 |
| 3 | 写入记忆 | 状态行出现 memory writeback 提示 | audit `memory_save`：项目级 fact `failed-conditions-<fnv64a>`；`.lyon/memory/` 落盘（第 2、3 次失败去重，只有一条） |
| 4 | 注入教训 | —（新会话的第一次请求）| mock 网关 `/__demo/last_body` 里出现同一教训文本（记忆热层注入）|
| 5 | 不再犯 | 模型这次读 notes.txt（port=8080），回答「端口翻倍后是 16160。」 | transcript 含 `16160`；tool_use 指向 notes.txt；末次 prompt_end 无 error；diagnosis 仍为 3 条 |

镜头 1–3 重复三次（同一 prompt、同一错、同一归纳），证明“同类错误全场可见”且记忆去重；镜头 4–5 是一个**新进程新会话**，证明教训跨会话存活。

## 拍摄（--live 模式）

```bash
bash demo/demo-60s.sh --live     # 或 make demo-60s-live
```

脚本只做准备（构建、起 mock 网关、按镜头装载脚本），把一段可直接粘贴的 `export …; lyon-demo.exe chat …` 命令交给你：在**自己的真实终端**（Windows Terminal / VS Code 集成终端均可）里执行它，按屏幕提示逐镜头操作（输入 prompt → 看报错与归纳 → `/quit`），回车进入下一镜头。第 4 镜头注意观察右下状态区的 memory 注入痕迹与最终答案。

### 转 GIF（ffmpeg）

现场画面建议用终端录制 + ffmpeg 两段式转 GIF（体积小、文字锐利）：

1. 录制：Xbox Game Bar（Win+G）或 OBS 录制终端窗口；或直接 `asciinema cast` + `agg`（Linux 侧工具链）。
2. ffmpeg 高质量 GIF（两遍 palette）：

```bash
ffmpeg -i capture.mp4 -vf "fps=12,scale=1200:-1:flags=lanczos,split[s0][s1];[s0]palettegen=max_colors=128[p];[s1][p]paletteuse=dither=bayer" lyon-60s.gif
```

字幕卡文案（叠加在每镜头上沿）：`失败 → 错因归纳 → 写入记忆 → 下次会话注入教训 → 不再犯`。

## 原理速览

- `demo/mockserve`：双 `internal/mockapi` 实例的前置网关。按 system prompt 内容分流（诊断请求 / 标题请求不会挤占任务脚本的序号），并支持**粘性 500 注入**（fail_at 之后任务请求全失败，SDK 重试也失败，保证 prompt 以 error 收尾且脚本不错位）。
- `demo/tuipty`：Windows ConPTY 驱动器。steps 文件（`send` / `wait-audit` / `wait-exit`）驱动 TUI；`wait-audit` 轮询 `$LYON_HOME/audit/*.jsonl` 的新增完整行，跨会话不误配。子进程 std 句柄靠 `STARTF_USESTDHANDLES` 置空交给 conhost 接管——否则 CreateProcess 会把父进程（脚本）的管道句柄复制给 child，输出绕过伪终端，`lyon chat` 的 isTTY 门也会把 TUI 降级成普通 REPL。
- 教练判据的注入点：`pkg/memory` 的热层事实（`MemoryFacts`）在新会话第一条 prompt 的请求体里随上下文下发；`demo60s-lesson` 是埋在 patch 里的哨兵串，只可能来自记忆。

## 故障排查

| 症状 | 原因 / 处理 |
|------|-------------|
| mockserve 起不来 | 端口由系统分配（127.0.0.1:0），不会冲突；看 `.demo/demo-60s/mockserve.out` |
| wait-audit 超时 | 首次运行杀毒扫描拖慢构建属正常；重跑一次，或看 `.demo/demo-60s/transcripts/` 里 TUI 实际渲染 |
| 断言「last_body 没有 lesson」 | 检查 `ws/.lyon/memory/failed-conditions-*.md` 是否存在；存在则说明记忆没进热层（trimDiagHot 上限 3 条） |
| 非 Windows | 脚本直接退出 2；其他平台用 `--live` 从真实终端手动驱动 |

## 相关文件

- `demo/demo-60s.sh` — 脚本化入口（`--live` 拍摄模式）
- `demo/mockserve/` — mock 网关（内容路由 + 粘性失败注入 + `/__demo/*` 管理口）
- `demo/tuipty/` — ConPTY TUI 驱动器（仅 Windows，其他平台编译为提示性 stub）
- `demo/evolve-loop.sh` — 通宵进化的另一个 demo（真实模型），与本 demo 互不影响
