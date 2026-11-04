# 产出卡片（files_changed）验收记录 · 2026-11-03

对象：聊天里的「产出文件 · N」卡片（baa21fa 引入，5776ab6 补变更模式 / 归属过滤 / CJK 路径）。

## 结论

**通过。** 服务端链路（快照 → 差分 → 注入 → 重放）与前端渲染类型检查全部为绿；
本文件本身就是一次实机用例：由 shell 写入、不经过任何文件工具，正是这张卡片存在的理由。

## 证据

### 1. 单元测试（go test ./pkg/server，8 项全 PASS，无 skip）

| 用例 | 断言的行为 |
|---|---|
| TestDiffFileCards | 差分只收「新路径 / XY 变了 / 同 XY 但 mtime 动了」，输出按路径排序 |
| TestFileCardsFlow | 提交时拍基线，prompt_end 注入一张卡：已脏文件再编辑 + 全新文件 = 恰好 2 行；卡片 seq 在 prompt_end 之上 |
| TestFileCardsSkipWithoutBaseline | 半路接管的 seat 无基线 → 不发卡（否则整仓脏文件都会被当成产出） |
| TestFileCardsForeignWriterExcluded | 并行会话写过的路径，从本次 run 的卡里剔除（writeLog 归属，#41-2） |
| TestNoteWriteAndFilter | 归属账本的记录 / 过期 / 本 seat 写入不过滤 |
| TestFileCardsNotARepo | 非 git 工作区不发卡 |
| TestFileCardsOverTheWire | 真 ConnectRPC + 真 engine + 真 run_shell 写文件 → WatchEvents 收到 files_changed，attrs.files 可解析 |
| TestFileCardsOverTheWireClassification / NoChange | 暂存 added 状态正确；一次什么都没写的 run 保持静默 |

### 2. 相邻能力（同批改动）

- TestGitDiff / TestParseBranchHeader：变更模式的 unified diff（staged+unstaged 合并、256KB 上限、`-c core.quotepath=false` 修 CJK 路径乱码）
- 前端 `npx tsc --noEmit` 通过；卡片行点击走 `onOpenDiff`（变更行）或 `onOpenFile`（untracked 行）

### 3. 性能实测（本仓 D:\lyon-agent）

`git status --porcelain=v1 -uall` 实测 0.40s，卡片采集预算 `fileCardTimeout = 2s`，余量充足。

## 已知盲区（设计如此，非缺陷，但要知道）

1. **非 git 工作区永远没有卡片** —— 基线采集把「不是仓库」当成空基线。
2. **被 .gitignore 忽略的产物不进卡** —— `bin/`、`tmp/`、`*.exe` 等；只产出构建物的 run 会静默无卡。
3. **工作区外的写入不进卡** —— `$HOME/.lyon`、`.lyon/paste/` 这类缓存目录。
4. **短于一次 git status 的 run 不发卡** —— prompt_end 到来时基线还没落地，按「诚实跳过」处理；下一次提交会重试采集。
5. **卡片是本次 run 的 delta，不是累计清单** —— 跨轮次只靠注入事件在 ring 里重放，ring 淘汰后不会重建。

## 需要人眼确认的一项

本轮结束后，聊天里应出现一张 1 行的卡片，内容为：

    产出文件 · 1
    docs/design/files-changed-card-acceptance.md   ??

上一轮的 `2027年规划汇报/产出/卡片验收-1103-1748.md` **不应**再出现 —— 卡片描述的是增量。
