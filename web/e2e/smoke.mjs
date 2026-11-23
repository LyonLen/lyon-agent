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
//     schedules it (tray count down, grid chip up, flash);
//   G. real mouse drag on a SCHEDULED chip: grabbing its label and
//     landing two days right moves it (the 点进详情页改时间 report);
//   H. quick-add popover discipline: a day click opens it, ESC closes
//     just it (overlay survives), and a press that STARTED on a chip
//     never opens it (the 输入样式-without-typing report);
//   I. chip edge resize: dragging a scheduled chip's RIGHT edge extends
//     the span to the drop day (the 拖右边界改日期 report);
//   J. gantt interactions: double-click an undated row schedules it,
//     bar body drag moves the span, bar right edge resizes it, and a
//     double-click on a dated row's track opens the inline quick-add
//     (ESC closes just the input — the 甘特图同理 report);
//   K. sidebar fold survives a session switch: the sessions pane
//     remounts on selection change (per-session tiling layout → fresh
//     leaf keys), which used to reset the fold set and pop every
//     workspace group open (the 点击后别的project展开 report).
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

  // G — REAL mouse drag on the scheduled chip (page.dragAndDrop drives
  // native HTML5 DnD in Chromium): press on the chip's label button —
  // where a user would grab it — and land two cells right. A miss shows
  // up as no 已移动到 flash; the press/release span different elements,
  // so a failed drag cannot open the edit modal by accident.
  const gBefore = await page.locator(".td-grid .td-chip").count();
  const gPos = await page.evaluate(() => {
    const src = document.querySelector(".td-grid .td-chip");
    if (!src) return null;
    const cells = [...document.querySelectorAll(".td-cell")];
    const i = cells.indexOf(src.closest(".td-cell"));
    return { i, dst: Math.min(i + 2, cells.length - 1) };
  });
  if (!gPos || gPos.dst <= gPos.i) {
    bad(`grid-move setup failed (pos=${JSON.stringify(gPos)})`);
  } else {
    await page
      .locator(".td-grid .td-chip")
      .first()
      .locator(".td-ct")
      .dragTo(page.locator(".td-cell").nth(gPos.dst));
    try {
      await page.getByText("已移动到", { exact: false }).waitFor({ timeout: 5000 });
      ok("real drag moves a scheduled chip to a later day");
    } catch {
      bad("real drag did not move the chip (no 已移动到 flash)");
    }
    const srcN = await page
      .locator(".td-cell")
      .nth(gPos.i)
      .locator(".td-chip")
      .count();
    if (srcN === 0) ok("source day emptied by the move");
    else bad(`source day still holds ${srcN} chip(s)`);
    const dstN = await page
      .locator(".td-cell")
      .nth(gPos.dst)
      .locator(".td-chip")
      .count();
    if (dstN >= 1) ok("target day holds the moved chip");
    else bad("target day has no chip after the move");
    const gAfter = await page.locator(".td-grid .td-chip").count();
    if (gAfter === gBefore) ok("chip count unchanged (pure move)");
    else bad(`chip count = ${gAfter}, want ${gBefore} (pure move)`);
  }

  // H — quick-add popover: a direct day click opens it (click the same
  // day again to close), but a press that STARTED on a chip must not
  // open it — the stray click after a drag attempt popped an empty
  // focused input over the grid ("我没有输入，但是这里好像是输入样式").
  const pop0 = await page.locator(".td-pop").count();
  if (pop0 !== 0) bad(`popover already open after the drag (count=${pop0})`);
  const day0 = page.locator(".td-cell").first();
  await day0.click();
  try {
    await page.locator(".td-pop").first().waitFor({ timeout: 3000 });
    ok("day click opens the quick-add popover");
  } catch {
    bad("day click did not open the quick-add popover");
  }
  // ESC must collapse the quick-add only — the pane itself survives
  // (the ladder used to skip popDate and closed the whole overlay).
  await page.keyboard.press("Escape");
  await page.waitForTimeout(150);
  if ((await page.locator(".td-pop").count()) === 0)
    ok("ESC closes the quick-add popover");
  else bad("ESC did not close the quick-add popover");
  if ((await page.locator(".td-tbar").count()) > 0)
    ok("overlay survives ESC (popover-only close)");
  else bad("ESC closed the whole overlay instead of the popover");
  await page.evaluate(() => {
    const chip = document.querySelector(".td-grid .td-chip");
    const cell = chip && chip.closest(".td-cell");
    if (!cell) return;
    chip.dispatchEvent(
      new PointerEvent("pointerdown", { bubbles: true, cancelable: true }),
    );
    cell.dispatchEvent(
      new MouseEvent("click", { bubbles: true, cancelable: true }),
    );
  });
  await page.waitForTimeout(200);
  if ((await page.locator(".td-pop").count()) === 0)
    ok("press starting on a chip never opens the popover");
  else bad("chip-origin press opened the day popover (stray click)");

  // I — chip edge resize: grab the scheduled chip's right handle and
  // drag two cells right; the span must extend to the drop day while
  // the start day keeps its chip. Pointer-capture based — a real mouse
  // drag drives it end to end.
  const iPos = await page.evaluate(() => {
    const chip = document.querySelector(".td-grid .td-chip");
    const cells = [...document.querySelectorAll(".td-cell")];
    if (!chip) return null;
    return { i: cells.indexOf(chip.closest(".td-cell")) };
  });
  if (!iPos || iPos.i < 0) {
    bad("edge-resize setup: no scheduled chip in the grid");
  } else {
    const iDst = Math.min(iPos.i + 2, 41);
    await page
      .locator(".td-grid .td-chip")
      .first()
      .locator(".td-rz.r")
      .dragTo(page.locator(".td-cell").nth(iDst));
    try {
      await page.getByText("已改到", { exact: false }).waitFor({ timeout: 5000 });
      ok("dragging a chip's right edge resizes the span");
    } catch {
      bad("right-edge drag did not commit (no 已改到 flash)");
    }
    const iDstN = await page.locator(".td-cell").nth(iDst).locator(".td-chip").count();
    if (iDstN >= 1) ok("resized span reaches the drop day");
    else bad(`drop day holds ${iDstN} chip(s), want ≥1`);
    const iSrcN = await page.locator(".td-cell").nth(iPos.i).locator(".td-chip").count();
    if (iSrcN >= 1) ok("span start day untouched by the end-drag");
    else bad("start day lost its chip after the end-drag");
  }

  // J — gantt interactions. Switch views, then: double-click the
  // undated row's track (schedule that day), press-drag the first bar
  // left (span moves), drag its right edge +1 day (span widens),
  // double-click a dated row's empty track (inline quick-add → Enter
  // creates → ESC closes just the input).
  await page.getByRole("tab", { name: "甘特" }).click();
  await page.waitForTimeout(200);

  const jBars0 = await page.locator(".td-bar").count();
  const jUnd = page.locator(".td-g-sec").filter({ hasText: "未排期" });
  if ((await jUnd.locator(".td-g-row").count()) === 0) {
    bad("no undated gantt row to double-click");
  } else {
    await jUnd
      .locator(".td-g-row")
      .first()
      .locator(".td-g-track")
      .dblclick({ position: { x: 120, y: 15 } });
    try {
      await page.getByText("已排期", { exact: false }).waitFor({ timeout: 5000 });
      ok("double-clicking an undated gantt row schedules it");
    } catch {
      bad("undated-row double-click did not schedule (no 已排期 flash)");
    }
    const jBars1 = await page.locator(".td-bar").count();
    if (jBars1 === jBars0 + 1) ok("scheduled row gains a timeline bar");
    else bad(`bar count = ${jBars1}, want ${jBars0 + 1}`);
  }

  // Bar body drag — the range's right edge is data-driven, so this bar
  // (already at it) can only travel LEFT: −80px = two days. The
  // preview follows the pointer; commit snaps and flashes 已移动到.
  const jBar = page.locator(".td-bar").first();
  const jb = await jBar.boundingBox();
  if (!jb) {
    bad("gantt bar has no box to drag");
  } else {
    const jx = jb.x + jb.width / 2;
    const jy = jb.y + jb.height / 2;
    await page.mouse.move(jx, jy);
    await page.mouse.down();
    await page.mouse.move(jx - 80, jy, { steps: 6 });
    await page.mouse.up();
    try {
      await page.getByText("已移动到", { exact: false }).waitFor({ timeout: 5000 });
      ok("press-dragging a gantt bar moves the span");
    } catch {
      bad("bar body drag did not move (no 已移动到 flash)");
    }
    const jLeft = await page.locator(".td-bar").first().evaluate((el) => el.style.left);
    if (jLeft === "120px") ok("bar snapped two days left (left=120px)");
    else bad(`bar left = ${jLeft}, want 120px`);
  }

  // Right-edge resize: +1 day widens the bar by one column (40px).
  const jRh = page.locator(".td-bar").first().locator(".td-h.r");
  const jhb = await jRh.boundingBox();
  if (!jhb) {
    bad("bar right handle has no box");
  } else {
    await page.mouse.move(jhb.x + jhb.width / 2, jhb.y + jhb.height / 2);
    await page.mouse.down();
    await page.mouse.move(jhb.x + jhb.width / 2 + 40, jhb.y + jhb.height / 2, {
      steps: 5,
    });
    await page.mouse.up();
    try {
      await page.getByText("已改到", { exact: false }).waitFor({ timeout: 5000 });
      ok("dragging a bar's right edge resizes the span");
    } catch {
      bad("bar edge drag did not commit (no 已改到 flash)");
    }
    const jW = await page.locator(".td-bar").first().evaluate((el) => el.style.width);
    if (jW === "157px") ok("bar widened by one day (width=157px)");
    else bad(`bar width = ${jW}, want 157px`);
  }

  // Inline quick-add: double-click the SECOND dated row's empty track
  // (its single-day bar leaves room), Enter creates, ESC must close the
  // input alone — the overlay survives (same ladder discipline as H).
  const jRow2 = page.locator(".td-g-row", { has: page.locator(".td-bar") }).nth(1);
  const jTr2 = jRow2.locator(".td-g-track");
  const jtb = await jTr2.boundingBox();
  if (!jtb) {
    bad("second dated row has no track to double-click");
  } else {
    await jTr2.dblclick({ position: { x: Math.min(200, jtb.width - 10), y: 15 } });
    try {
      await page.locator(".td-g-add").waitFor({ timeout: 3000 });
      ok("double-clicking a dated row's track opens inline quick-add");
    } catch {
      bad("no inline quick-add after track double-click");
    }
    await page.locator(".td-g-add input").fill("联调甘特快速添加");
    const jBars2 = await page.locator(".td-bar").count();
    await page.keyboard.press("Enter");
    await page.waitForTimeout(300);
    const jBars3 = await page.locator(".td-bar").count();
    if (jBars3 === jBars2 + 1) ok("quick-add creates a bar in the row's section");
    else bad(`bar count ${jBars2} → ${jBars3}, want +1`);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(150);
    if ((await page.locator(".td-g-add").count()) === 0)
      ok("ESC closes only the inline add");
    else bad("ESC did not close the inline quick-add");
    if ((await page.locator(".td-tbar").count()) > 0)
      ok("overlay survives the inline-add ESC");
    else bad("ESC closed the whole overlay instead of the input");
  }

  // K — fold state survives the session-switch remount. Fold every
  // workspace group, then create + select a new session: the selection
  // change rehydrates the per-session tile layout with fresh leaf
  // nodeIds, remounting the sessions pane. Plain component state would
  // reset to all-expanded here; the fold set now rides localStorage.
  await page.keyboard.press("Escape"); // close the todos overlay
  await page.waitForTimeout(200);
  const heads = page.locator(".ws-head");
  if ((await heads.count()) === 0) {
    bad("no workspace group header to fold");
  } else {
    // Fold groups until no row is left (one group today; loop stays
    // correct if the fixture ever grows a second workspace).
    for (let i = 0; i < 8 && (await page.locator(".session-row").count()) > 0; i++) {
      const h = page.locator(".ws-head").nth(i);
      if ((await h.count()) === 0) break;
      await h.click();
      await page.waitForTimeout(100);
    }
    if ((await page.locator(".session-row").count()) === 0)
      ok("clicking a group header folds its sessions");
    else bad("fold did not hide the group's rows");

    await page.getByText("+ new session", { exact: true }).click();
    await page.getByText("当前目录", { exact: true }).click();
    await page.waitForSelector('[placeholder*="prompt"]', { timeout: 10000 });
    await page.waitForTimeout(400); // let the layout hydrate + remount
    if ((await page.locator(".session-row").count()) === 0)
      ok("fold survives the session-switch remount");
    else bad("workspace groups re-expanded after switching session");

    await page.locator(".ws-head").first().click();
    await page.waitForTimeout(150);
    if ((await page.locator(".session-row").count()) > 0)
      ok("fold toggle still works after the remount");
    else bad("cannot unfold the group after the remount");
  }
} catch (e) {
  failed++;
  console.error("FAIL - exception:", e && e.message);
}

await browser.close();
process.exit(failed ? 1 : 0);
