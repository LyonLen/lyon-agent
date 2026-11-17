// smoke.mjs — real-browser joint smoke (playwright-core on system Edge).
// Driven by pkg/server/joint_e2e_test.go: isolated lyond + scripted mock
// model + vite dev. Asserts the regressions we keep re-breaking:
//   A. new-session chooser rows respond to clicks (the 点不动 report);
//   B. a session creates from 当前目录;
//   C. the mock reply renders EXACTLY once (StrictMode double-fold bug);
//   D. breadcrumbs live under the 文件 tab and hide under git;
//   E. 拿会话填充待办: sidebar「＋」/ popover fill into the global
//     todolist, per-session dedupe, reload round-trip;
//   F. drag-to-schedule: a 未排期 tray chip dropped on a day cell
//     schedules it (tray count down, grid chip up, flash).
import { chromium } from "playwright-core";

const BASE = process.env.BASE_URL;
if (!BASE) {
  console.error("BASE_URL required");
  process.exit(2);
}
const TOKEN = process.env.LYON_TOKEN || "e2e";
const PHRASE = "联调回复一二三四五六七八九十。";

let failed = 0;
const ok = (m) => console.log("ok   -", m);
const bad = (m) => {
  failed++;
  console.error("FAIL -", m);
};

const browser = await chromium.launch({ channel: "msedge", headless: true });
const page = await browser.newPage({ viewport: { width: 1500, height: 900 } });
await page.addInitScript((t) => localStorage.setItem("lyon-token", t), TOKEN);
await page.goto(BASE, { waitUntil: "domcontentloaded" });

try {
  // A — the chooser: rows must expand on click, repeatedly (the stale
  // busy / remount bug made every row dead after the first click).
  await page.getByText("+ new session", { exact: true }).click();
  await page.getByText("从 Git 克隆…").click();
  await page.waitForSelector('input[placeholder*="github.com"]', { timeout: 5000 });
  ok("git-clone row expands on click");
  await page.keyboard.press("Escape");

  // B — create from 当前目录.
  await page.getByText("+ new session", { exact: true }).click();
  await page.getByText("当前目录", { exact: true }).click();
  await page.waitForSelector('[placeholder*="prompt"]', { timeout: 10000 });
  ok("session created from 当前目录");

  // C — the mock reply must land exactly once: per-delta doubling (the
  // StrictMode mutation bug) breaks the exact phrase and duplicates it.
  await page.fill('[placeholder*="prompt"]', "测一句");
  await page.keyboard.press("Enter");
  await page.getByText("联调回复", { exact: false }).first().waitFor({ timeout: 20000 });
  await page.waitForTimeout(800); // let the tail settle
  const feed = await page.textContent(".chat-feed");
  const once = feed.split(PHRASE).length - 1;
  if (once === 1) ok("reply text renders exactly once");
  else bad(`reply phrase count = ${once}, want 1 (duplicated deltas?)`);

  // D — breadcrumbs under the 文件 tab only.
  await page.getByText("sub", { exact: true }).first().click();
  try {
    await page.waitForSelector(".files-crumbs", { timeout: 5000 });
    ok("breadcrumb shows under the 文件 tab");
  } catch {
    bad("breadcrumb missing under the 文件 tab");
  }
  await page.getByRole("button", { name: "git", exact: true }).click();
  await page.waitForTimeout(300);
  if ((await page.locator(".files-crumbs").count()) === 0)
    ok("breadcrumb hidden under the git tab");
  else bad("breadcrumb visible under the git tab");

  // E — 拿会话填充待办 (global overlay). Two sessions are needed so the
  // sidebar can fill one while the popover still finds an unlinked row.
  await page.getByText("+ new session", { exact: true }).click();
  await page.getByText("当前目录", { exact: true }).click();
  await page.waitForSelector('[placeholder*="prompt"]', { timeout: 10000 });

  // The sidebar「＋」queues the session; TodosPane (the single doc
  // writer) consumes it, opens via the queue, flashes. The ＋ overlays
  // the row edge and only joins hit-testing on hover — hover first.
  await page.locator(".session-row").first().hover();
  await page.locator(".session-fill").first().click();
  await page.getByText("已填充 1 条会话", { exact: false }).waitFor({ timeout: 10000 });
  ok("sidebar ＋ fills a session into the todolist");
  let gn = await page.locator(".td-goal").first().locator(".td-gn").textContent();
  if (gn === "1") ok("rail count = 1 after sidebar fill");
  else bad(`rail count = ${gn}, want 1`);

  // The popover adds the other (still unlinked) session — the row
  // picker skips linked rows (:not(.on)), so this stays order-proof.
  await page.getByText("从会话填充", { exact: true }).click();
  await page.locator(".td-fillrow:not(.on)").first().click();
  await page.getByText("已加入待办", { exact: false }).waitFor({ timeout: 5000 });
  ok("popover row adds a session");
  gn = await page.locator(".td-goal").first().locator(".td-gn").textContent();
  if (gn === "2") ok("rail count = 2 after popover add");
  else bad(`rail count = ${gn}, want 2`);
  // The overlay backdrop covers the sidebar — dismiss the popover, then
  // the overlay itself, before reaching for the sidebar「＋」again.
  await page.keyboard.press("Escape"); // close the popover
  await page.keyboard.press("Escape"); // close the overlay

  // Dedupe: every session is linked now — re-filling must not double-add.
  await page.locator(".session-row").first().hover();
  await page.locator(".session-fill").first().click();
  await page.getByText("会话已在待办中", { exact: true }).waitFor({ timeout: 10000 });
  ok("re-filling a linked session is deduped");

  // Persistence: reload, then the valve still reports already-in-list —
  // the entries round-tripped through todos.json.
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByText("+ new session", { exact: true }).waitFor({ timeout: 10000 });
  await page.locator(".session-row").first().hover();
  await page.locator(".session-fill").first().click();
  await page.getByText("会话已在待办中", { exact: true }).waitFor({ timeout: 10000 });
  ok("filled entries survive reload (todos.json round-trip)");

  // F — drag-to-schedule: the 未排期 tray chip drops onto a day cell
  // (the post-reload sidebar fill above reopened the overlay). Playwright's
  // mouse drag does not fire native HTML5 DnD, so dispatch the DragEvent
  // sequence synthetically with a real DataTransfer.
  await page.locator(".td-uchip").first().waitFor({ timeout: 10000 });
  const trayN = await page.locator(".td-uchip").count();
  const gridN = await page.locator(".td-grid .td-chip").count();
  await page.evaluate(() => {
    const src = document.querySelector(".td-uchip");
    const dst = document.querySelector(".td-cell.now") ?? document.querySelector(".td-cell");
    const dt = new DataTransfer();
    src.dispatchEvent(new DragEvent("dragstart", { dataTransfer: dt, bubbles: true }));
    dst.dispatchEvent(
      new DragEvent("dragover", { dataTransfer: dt, bubbles: true, cancelable: true }),
    );
    dst.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true, cancelable: true }));
    src.dispatchEvent(new DragEvent("dragend", { dataTransfer: dt, bubbles: true }));
  });
  await page.getByText("已排期", { exact: false }).waitFor({ timeout: 5000 });
  ok("dropping a tray chip onto a day schedules it");
  const trayAfter = await page.locator(".td-uchip").count();
  if (trayAfter === trayN - 1) ok("scheduled chip leaves the 未排期 tray");
  else bad(`tray count = ${trayAfter}, want ${trayN - 1}`);
  const gridAfter = await page.locator(".td-grid .td-chip").count();
  if (gridAfter === gridN + 1) ok("scheduled chip renders in the grid");
  else bad(`grid chip count = ${gridAfter}, want ${gridN + 1}`);
} catch (e) {
  failed++;
  console.error("FAIL - exception:", e && e.message);
}

await browser.close();
process.exit(failed ? 1 : 0);
