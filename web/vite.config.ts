import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev proxy: the browser talks same-origin, so the bearer token flows to
// lyond without CORS preflight noise. Production flips the model: lyond
// serves these built assets itself (single-binary philosophy), and api.ts
// keeps baseUrl "" — same origin in both worlds.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5183,
    proxy: {
      "/lyon.v1": "http://127.0.0.1:8080",
      "/v1": "http://127.0.0.1:8080",
      "/healthz": "http://127.0.0.1:8080",
    },
  },
});
