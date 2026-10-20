import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev proxy: the browser talks same-origin, so the bearer token flows to
// lyond without CORS preflight noise. Production flips the model: lyond
// serves these built assets itself (single-binary philosophy), and api.ts
// keeps baseUrl "" — same origin in both worlds.
//
// Beyond the bare string-proxy target, each entry adds upstream teardown:
// streaming calls (WatchEvents/WatchPending) keep the upstream socket
// open until the server ends them, so when the browser aborts —
// session switch, watchdog retry — http-proxy would otherwise leave
// lyond's half of the connection pinned. Destroying the upstream
// response on client close makes aborts real end to end.
const lyondProxy = (target: string) => ({
  target,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  configure: (proxy: any) => {
    proxy.on("proxyRes", (proxyRes: any, _req: any, res: any) => {
      if (res.destroyed) {
        proxyRes.destroy();
        return;
      }
      res.on("close", () => {
        if (!proxyRes.destroyed) proxyRes.destroy();
      });
    });
  },
});

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5183,
    proxy: {
      "/lyon.v1": lyondProxy("http://127.0.0.1:8080"),
      "/v1": lyondProxy("http://127.0.0.1:8080"),
      "/healthz": lyondProxy("http://127.0.0.1:8080"),
      // Terminal WebSocket (terminal.go): ws:true so the upgrade proxies;
      // the bearer token rides ?token= (browser WS cannot set headers).
      "/terminal": { ...lyondProxy("http://127.0.0.1:8080"), ws: true },
    },
  },
});
