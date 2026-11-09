# 钉钉文档：用浏览器自动化「读下来 / 写回去」

> 场景：钉钉文档（alidocs.dingtalk.com）没有可用的导出按钮、或需要把改动写回原文档时，
> 用 CDP 接管一个**真人已登录的 Chrome** 来做，登录态不出浏览器。
>
> 实测对象：一篇「会议录制」类文档（2026-11，Chrome 157 / Node 20）

---

## 一、结论先行：两条通道，能力边界不同

| 通道 | 做什么 | 前提 |
|---|---|---|
| **A. UI 自动化** | 点按钮、点光标、打字、走官方导出 | 文档允许编辑/导出 |
| **B. 内部接口** | 批量取正文（HTML/Markdown）、核对改动 | 有登录态即可 |

**先判断该用哪条**：先探测页面里有没有 `export`/`download` 语义的节点。
- 有 → 走 A，能拿到真正 `.docx/.pdf`
- 没有（本例：会议录制类文档就没有导出入口）→ 走 B 取正文

⚠️ 别把「没找到按钮」当成「我没点到」。本例在编辑器 iframe 里穷举
`class/aria-label/title/data-testid` 含 `export|download` 的节点 = **0 个**，是真的没这个功能。

---

## 二、整体思路

```
1. 独立 profile 起 Chrome，开远程调试端口     ← 隔离，不动日常浏览器
2. playwright-core 用 connectOverCDP 接管      ← 复用登录态，不导出 cookie
3. 页面结构：应用外壳 + note/edit iframe        ← 所有操作要钻进 iframe
4. 读：内部接口拿 checkpoint + deltas           ← 正文是「快照 + 增量 op 日志」
5. 写：点进正文 → Ctrl+Home → type → Enter      ← 编辑器是隐藏 textarea 输入代理
6. 验：字数统计 + deltas op + DOM 渲染 三方对账
```

---

## 三、技术栈

| 组件 | 用途 | 说明 |
|---|---|---|
| **Chrome + `--remote-debugging-port=9222`** | 提供 CDP 入口 | 必须配**独立 `--user-data-dir`**，不碰日常配置 |
| **playwright-core** | 接管浏览器、操作 frame、发键盘事件 | 走 `chromium.connectOverCDP()`，不是 `launch()` |
| **Node.js 20** | 写胶水脚本、解析文档树 | 无额外依赖 |
| **已装包复用** | 省一次 `npm i` | 全局包 `mcp-dingtalk-doc@1.1.1` 里自带 `playwright-core@1.65` |

**零安装复用 playwright-core 的写法**（CommonJS 包，别用 ESM 具名导入）：

```js
const { chromium } = require('C:/Users/<you>/AppData/Roaming/npm/node_modules/mcp-dingtalk-doc/node_modules/playwright-core');
const b = await chromium.connectOverCDP('http://127.0.0.1:9222');
```

起浏览器（`start` 是为了不占住输出管道）：

```bash
cmd //c start "" "C:\Program Files\Google\Chrome\Application\chrome.exe" \
  --remote-debugging-port=9222 \
  --user-data-dir="D:\tmp\dingtalk-profile" \
  --no-first-run --no-default-browser-check "https://alidocs.dingtalk.com"
```

验证端口：`curl -s http://127.0.0.1:9222/json/version`

---

## 四、人工必须做的三件事

1. **勾选协议 + 点确定** —— 首次登录会弹「我已阅读并同意…（服务协议/隐私政策）」，这一步不代做
2. **手机钉钉扫码登录** —— 弹出的 Chrome 窗口里扫，登录态存进那个 profile 目录，有效期约 7–30 天，失效了再扫一次
3. **把文档链接交给 agent**（`https://alidocs.dingtalk.com/i/nodes/<id>`），或让它自己在「最近」列表里挑

之后所有点击、读取、写入都是自动的。

> 可选：不想每次扫码，可用 `mcp-dingtalk-doc` 自带的 cookie 管理（`cookie:login` / `cookie:check`），
> 它是把 cookie 落盘成 `dingtalk_cookies.json` 再走 axios 直连。
> **安全取向不同**：落盘 cookie 等于把凭证落到磁盘；CDP 接管则凭证始终只在浏览器进程里。本方案选了后者。

---

## 五、关键接口（通道 B）

正文不是普通 REST 一次拿全的，是**协作编辑模型**：

```
GET  /i/nodes/<nodeId>   → HTML，里面 grep 到 dentryKey
POST /api/document/data  → header: a-dentry-key: <dentryKey>，body: {"fetchBody": true}
```

返回结构：

```jsonc
{
  "data": {
    "documentContent": {
      "checkpoint": { "content": "<字符串，需再 JSON.parse>" },   // 基线快照
      "deltas": { "list": [ { "operations": "<JSON 字符串>" } ] }  // 增量 op 日志
    }
  }
}
```

- `checkpoint.content` 解析后是 **`[tag, props, children]` 轻量树**（sml 风格），
  本例 = `body` 里 210 个块，标签只有 `p / h2 / span`，样式靠 props（`blockquote`、`bold`…）
- ⚠️ **只看 checkpoint 会看漏刚写的改动**。新写入落在 `deltas.list`，op 类型：
  - `t:1` 插入节点、`t:2` 插入文本（`p` 路径 + `o` 偏移 + `txt`）、`t:6/7` 拆分节点/文本
  - 本例 3 组 op 就还原了「文首插一行 + 原首段下移」

**在浏览器上下文里 fetch，cookie 自动带，不用导出**：

```js
await page.evaluate(async () => {
  const r = await fetch('/api/document/data', {
    method: 'POST', credentials: 'include',
    headers: { 'a-dentry-key': '<key>', 'content-type': 'application/json' },
    body: JSON.stringify({ fetchBody: true }),
  });
  return await r.text();
});
```

---

## 六、写回正文（通道 A）怎么定位光标

编辑器**没有 `contenteditable`**，输入走隐藏 `<textarea>` 代理：

```js
const f = page.frames().find(x => x.url().includes('/note/edit')); // 钻进编辑器 iframe
await f.getByText('创建时间:', { exact: false }).first().click(); // 落光标（任一点得中的正文块都行）
await page.keyboard.press('Control+Home');   // 回到文首
await page.keyboard.type('要点其一', { delay: 25 });
await page.keyboard.press('Enter');          // 独立成行，避免和原首段粘连
```

落光标后验证一下：`document.activeElement` 应该是 `TEXTAREA`，不是 `BODY`。

---

## 七、坑清单（踩过的）

| 坑 | 现象 | 解法 |
|---|---|---|
| **iframe 坐标偏移** | 用 frame 内 `getBoundingClientRect` 的值直接喂 `page.mouse.click`，点空 | 本例 iframe 偏移 `x=289`；一律用 `frame.locator(...).click()`，playwright 自己算绝对坐标 |
| **frame 内坐标当页面坐标** | 点正文没反应，`activeElement` 仍是 `BODY` | 同上；且先用 DOM 断言焦点真的进去了再打字 |
| **默认 profile 起调试端口** | 另开实例会复用已在跑的进程，端口未必建得起来 | 用独立 `--user-data-dir`，顺便隔离日常配置 |
| **`start` 占住管道** | 启动命令挂死、报 `WaitDelay expired` | `cmd //c start "" ...` 或重定向输出 |
| **路径拼错** | bash 的 `/d/xxx` 交给 Node 变成 `D:\d\xxx` | Node 里用相对路径或 `D:/...` |
| **`mainsite_server_content` 找不到** | 老教程的正则匹配不到 | 页面结构已变，直接 grep `dentryKey":"([^"]+)"` 更快 |
| **只看 checkpoint** | 以为自己没写进去 | 同时看 `deltas.list` |
| **「编辑」下拉别乱点** | 里面是 `可编辑文档 / 阅读 / 修订`，可能影响共享权限 | 本例根本不用切，直接点正文就能编 |

---

## 八、安全与回滚

- **凭证**：cookie 全程留在 Chrome 进程内，脚本里只有相对 URL，日志不打印 cookie
- **写前备份**：先把改前的 `/api/document/data` 响应整份存盘（`document.json`），改完再存一份 `document.after.json`
- **三方对账**（缺一不可）：
  1. UI 状态 = `已保存`
  2. `deltas.list` 出现对应 op
  3. 字数/字符数增量 = 插入内容的长度（本例 字数 6449→6473、字符 7275→7319）
- **回滚**：文档里直接删掉那行 / `Ctrl+Z` / 右上角版本历史
- **样式副作用**：在引用块里回车拆分，新行会继承 `blockquote: true`；要纯正文行得显式改格式

---

## 九、脚本形态

胶水脚本当时写在仓库的 `tmp/` 下，属于一次性的临时草稿，用完即弃。按用途拆成这几类，自己写时照这个切分即可：

| 脚本 | 作用 |
|---|---|
| `probe*.cjs` / `state.cjs` | 页面、frame、登录态、焦点探测 |
| `list2.cjs` | 抓「最近」文档列表（标题/路径/创建人）|
| `open.cjs` | 按标题点开某篇 |
| `fetch.cjs` / `doc.cjs` | 取 dentryKey / 拉 document/data |
| `render.cjs` | 文档树 → Markdown |
| `write.cjs` | 文首插一行 |
| `verify.cjs` / `tidy.cjs` | 写入后核对 |