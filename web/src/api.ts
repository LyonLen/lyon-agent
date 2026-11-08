import {
  Code,
  ConnectError,
  createClient,
  type Interceptor,
} from "@connectrpc/connect";
// connect-es v2 split browser transports into their own package; same 2.2.0
// pin as the runtime (generator/runtime/transports share one version line).
import { createConnectTransport } from "@connectrpc/connect-web";
import { SessionService } from "./gen/lyon/v1/session_pb";
import { FileService } from "./gen/lyon/v1/file_pb";
import { MemoryService } from "./gen/lyon/v1/memory_pb";
import { DashboardService } from "./gen/lyon/v1/dashboard_pb";
import { EvolveService } from "./gen/lyon/v1/evolve_pb";
import { GitService } from "./gen/lyon/v1/git_pb";
import { ApprovalService } from "./gen/lyon/v1/approval_pb";
import { SettingsService } from "./gen/lyon/v1/settings_pb";
import { MindmapService } from "./gen/lyon/v1/mindmap_pb";

// v1 auth is the static lyond bearer token, kept in localStorage and
// injected by a transport interceptor (plan §4.7 — one interceptor covers
// unary and streaming alike; the server-side wrapper does the rest).
export function getToken(): string {
  return localStorage.getItem("lyon-token") ?? "";
}

export function setToken(t: string): void {
  if (t) {
    localStorage.setItem("lyon-token", t);
  } else {
    localStorage.removeItem("lyon-token");
  }
}

// Legacy /v1 JSON routes (no proto — the connect transport can't reach
// them): plain fetch with the same bearer token the interceptor injects.
export async function v1Json<T>(path: string, body: unknown): Promise<T> {
  const t = getToken();
  const res = await fetch(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(t ? { Authorization: `Bearer ${t}` } : {}),
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return (await res.json()) as T;
}

const authInterceptor: Interceptor = (next) => async (req) => {
  const t = getToken();
  if (t) {
    req.header.set("Authorization", `Bearer ${t}`);
  }
  if (currentWs) {
    req.header.set("x-lyon-workspace", currentWs);
  }
  return next(req);
};

// Per-session workspace selection: the selected session's root rides
// every RPC as the x-lyon-workspace header so file/git/memory resolve
// against it — a header, not a proto field (no wire regen needed).
let currentWs = "";
export function setWorkspace(ws: string): void {
  currentWs = ws;
}
export function getWorkspace(): string {
  return currentWs;
}

// cloneRepo clones a git repository and answers the clone's absolute
// path — the new-session workspace chooser's "clone from git" branch.
export async function cloneRepo(url: string, dir: string): Promise<string> {
  const r = await v1Json<{ workspace: string }>("/v1/git/clone", { url, dir });
  return r.workspace;
}

const WS_HISTORY_KEY = "lyon_workspace_history";

// rememberWorkspace records a chosen root for the chooser defaults.
export function rememberWorkspace(ws: string): void {
  try {
    localStorage.setItem("lyon_last_workspace", ws);
    const prev = JSON.parse(localStorage.getItem(WS_HISTORY_KEY) ?? "[]") as string[];
    const next = [ws, ...prev.filter((x) => x !== ws)].slice(0, 8);
    localStorage.setItem(WS_HISTORY_KEY, JSON.stringify(next));
  } catch {
    // storage blocked: defaults just won't be remembered
  }
}

export function workspaceHistory(): string[] {
  try {
    return JSON.parse(localStorage.getItem(WS_HISTORY_KEY) ?? "[]") as string[];
  } catch {
    return [];
  }
}

// fetchSessionWorkspace / saveSessionWorkspace are the JSON seam for one
// session's workspace root (GET/POST /api/session/{id}/workspace).
export async function fetchSessionWorkspace(id: string): Promise<string> {
  const t = getToken();
  const res = await fetch(`/v1/sessions/${id}/workspace`, {
    headers: t ? { Authorization: `Bearer ${t}` } : {},
  });
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return ((await res.json()) as { workspace: string }).workspace;
}

export async function saveSessionWorkspace(id: string, ws: string): Promise<string> {
  const t = getToken();
  const res = await fetch(`/v1/sessions/${id}/workspace`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(t ? { Authorization: `Bearer ${t}` } : {}),
    },
    body: JSON.stringify({ workspace: ws }),
  });
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return ((await res.json()) as { workspace: string }).workspace;
}

// baseUrl "" = same origin: dev goes through the vite proxy, production
// serves these assets from lyond itself.
export const transport = createConnectTransport({
  baseUrl: "",
  interceptors: [authInterceptor],
});

export const sessionClient = createClient(SessionService, transport);
export const fileClient = createClient(FileService, transport);
export const memoryClient = createClient(MemoryService, transport);
export const dashboardClient = createClient(DashboardService, transport);
// EvolveService is the evolution panel's read-only state (lineage,
// spaces, flows, exam banks); routing/leaderboard ride dashboardClient.
export const evolveClient = createClient(EvolveService, transport);
// GitService powers the files panel's source-control view (changes,
// commit, sync, outgoing graph) over the workspace repository.
export const gitClient = createClient(GitService, transport);
// ApprovalService answers Unimplemented until lyond starts with -approve;
// the pending watcher in App.tsx stops its retry loop on exactly that code.
export const approvalClient = createClient(ApprovalService, transport);
// SettingsService reads the effective engine config + model knowledge base
// and switches the live model tier — no restart, next turn picks it up.
export const settingsClient = createClient(SettingsService, transport);
// MindmapService: the chat-tail mindmap card's read side (GetMindmap —
// the map is NOT in the session snapshot, so /resume rebuilds through
// this) and the human edit channel (UpdateMindmap, origin="user").
export const mindmapClient = createClient(MindmapService, transport);

export function errText(err: unknown): string {
  const ce = ConnectError.from(err);
  if (ce.code === Code.Unauthenticated) {
    return "unauthenticated — set the lyond token in the header";
  }
  if (ce.code === Code.Unimplemented) {
    // approval/plan are mounted v1 stubs — degrade to a readable note
    // instead of surfacing the raw connect error (plan Phase 5).
    return "not available in this build — " + ce.message;
  }
  return ce.message;
}
