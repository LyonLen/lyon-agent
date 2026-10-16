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
import { DashboardService } from "./gen/lyon/v1/dashboard_pb";

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

const authInterceptor: Interceptor = (next) => async (req) => {
  const t = getToken();
  if (t) {
    req.header.set("Authorization", `Bearer ${t}`);
  }
  return next(req);
};

// baseUrl "" = same origin: dev goes through the vite proxy, production
// serves these assets from lyond itself.
export const transport = createConnectTransport({
  baseUrl: "",
  interceptors: [authInterceptor],
});

export const sessionClient = createClient(SessionService, transport);
export const fileClient = createClient(FileService, transport);
export const dashboardClient = createClient(DashboardService, transport);

export function errText(err: unknown): string {
  const ce = ConnectError.from(err);
  if (ce.code === Code.Unauthenticated) {
    return "unauthenticated — set the lyond token in the header";
  }
  return ce.message;
}
