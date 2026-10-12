// Command lyond serves the lyon engine over HTTP + SSE.
//
// Endpoints:
//
//	GET    /healthz                      liveness + session count
//	GET    /metrics                      aggregate usage/cache/tool latency JSON
//	POST   /v1/sessions                  create {"message"?} — with message: one-shot SSE; without: JSON id
//	POST   /v1/sessions/{id}/messages    run {"message"} as SSE
//	GET    /v1/sessions/{id}/stats       per-turn usage JSON
//	DELETE /v1/sessions/{id}             close and evict
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/openaix"
	"lyon-agent/pkg/server"
	"lyon-agent/pkg/tools"
)

func main() {
	var (
		addr       = flag.String("addr", ":8080", "listen address")
		workspace  = flag.String("workspace", ".", "tool confinement root")
		model      = flag.String("model", "", "model id (default: engine default / ANTHROPIC_MODEL)")
		effort     = flag.String("effort", "", "output effort: low|medium|high|xhigh|max")
		maxIter    = flag.Int("max-iter", 0, "max API iterations per prompt (default 50, env LYON_MAX_ITER)")
		maxSess    = flag.Int("max-sessions", 1024, "admission cap (429 beyond)")
		ttl        = flag.Duration("session-ttl", 30*time.Minute, "idle session eviction (0 disables)")
		shellAllow = flag.String("shell-allow", "", "comma-separated shell command allowlist")
		webTools   = flag.String("web-tools", "", "web tool mode: server|client|off (default: auto)")
		maxTurns   = flag.Int("max-turns", 0, "global cap on in-flight API turns across sessions (0 = unlimited)")
		prewarm    = flag.Bool("prewarm", false, "warm the prompt cache (one tiny request) before serving")
		openai     = flag.Bool("openai", false, "speak OpenAI protocol to the gateway at "+openaix.DefaultBase+" (same API key as Bearer auth)")
		openaiBase = flag.String("openai-base", "", "OpenAI-compatible base URL, e.g. https://host/v1 (implies -openai; env LYON_OPENAI_BASE)")
	)
	flag.Parse()

	cfg := agent.DefaultConfig()
	cfg.Workspace = *workspace
	if *model != "" {
		cfg.Model = *model
	}
	if *effort != "" {
		cfg.Effort = *effort
	}
	if *maxTurns > 0 {
		cfg.MaxConcurrentTurns = *maxTurns
	}
	if *maxIter > 0 {
		cfg.MaxIterations = *maxIter
	}
	if *shellAllow != "" {
		for _, name := range strings.Split(*shellAllow, ",") {
			if name = strings.TrimSpace(name); name != "" {
				cfg.ShellAllow = append(cfg.ShellAllow, name)
			}
		}
	}
	if *webTools != "" {
		switch *webTools {
		case "server", "client", "off":
			cfg.WebToolMode = *webTools
		default:
			fmt.Fprintln(os.Stderr, "-web-tools must be server, client, or off")
			os.Exit(2)
		}
	}
	if *openaiBase != "" {
		cfg.OpenAIBase = *openaiBase
	} else if *openai {
		cfg.OpenAIBase = openaix.DefaultBase
	}

	eng := agent.NewEngine(cfg, nil)
	store := server.NewStore(*maxSess, *ttl)
	srv := server.New(eng, store)
	// One shared Metrics collector feeds /metrics with per-tool latency for
	// every session of this process.
	metrics := tools.NewMetrics()
	srv.SetObserver(metrics)

	stopReaper := make(chan struct{})
	go store.RunReaper(time.Minute, stopReaper)

	if *prewarm {
		wctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := eng.Warmup(wctx); err != nil {
			fmt.Fprintln(os.Stderr, "prewarm:", err)
		} else {
			log.Printf("prompt cache warmed (model=%s)", eng.Config().Model)
		}
		cancel()
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are long-lived. Slow clients are
		// bounded per-event by the write deadline in streamPrompt.
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("lyond listening on %s (model=%s workspace=%s max_sessions=%d max_turns=%d)",
			*addr, eng.Config().Model, eng.Config().Workspace, *maxSess, *maxTurns)
		errCh <- httpSrv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("signal %v: shutting down", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
		return
	}

	close(stopReaper)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown: %v\n", err)
	}
	store.CloseAll()
}
