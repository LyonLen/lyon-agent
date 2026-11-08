// smoke.mjs — real-browser joint smoke (playwright-core on system Edge).
// Driven by pkg/server/joint_e2e_test.go: isolated lyond + scripted mock
// model + vite dev. Asserts the regressions we keep re-breaking:
//   A. new-session chooser rows respond to clicks (the 点不动 report);
//   B. a session creates from 当前目录;
//   C. the mock reply renders EXACTLY once (StrictMode double-fold bug);
//   D. breadcrumbs live under the 文件 tab and hide under git.
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
} catch (e) {
  failed++;
  console.error("FAIL - exception:", e && e.message);
}

await browser.close();
process.exit(failed ? 1 : 0);
