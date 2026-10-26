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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/evolve"
	"lyon-agent/pkg/openaix"
	"lyon-agent/pkg/server"
	"lyon-agent/pkg/tools"
)

func main() {
	var (
		addr       = flag.String("addr", "127.0.0.1:8080", "listen address (non-loopback requires -token or -insecure)")
		workspace  = flag.String("workspace", ".", "tool confinement root")
		token      = flag.String("token", "", "bearer token for all routes but /healthz (default: env LYON_TOKEN)")
		insecure   = flag.Bool("insecure", false, "allow serving on a non-loopback address without a token")
		model      = flag.String("model", "", "model id (default: engine default / LYON_MODEL or ANTHROPIC_MODEL)")
		effort     = flag.String("effort", "", "output effort: low|medium|high|xhigh|max")
		maxIter    = flag.Int("max-iter", 0, "max API iterations per prompt (default unlimited, env LYON_MAX_ITER)")
		maxSess    = flag.Int("max-sessions", 1024, "admission cap (429 beyond)")
		ttl        = flag.Duration("session-ttl", 30*time.Minute, "idle session eviction (0 disables)")
		shellAllow = flag.String("shell-allow", "", "comma-separated shell command allowlist")
		webTools   = flag.String("web-tools", "", "web tool mode: server|client|off (default: auto)")
		maxTurns   = flag.Int("max-turns", 0, "global cap on in-flight API turns across sessions (0 = unlimited)")
		prewarm    = flag.Bool("prewarm", false, "warm the prompt cache (one tiny request) before serving")
		approve    = flag.Bool("approve", false, "require human approval for WriteFile/EditFile/Shell over the web ApprovalService (default: auto-approve, matching a non-TTY lyon run)")
		openai     = flag.Bool("openai", false, "speak OpenAI protocol to the gateway at "+openaix.DefaultBase+" (same API key as Bearer auth)")
		openaiBase = flag.String("openai-base", "", "OpenAI-compatible base URL, e.g. https://host/v1 (implies -openai; env LYON_OPENAI_BASE)")
	)
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("LYON_TOKEN")
	}
	// These routes run shell commands and write files inside the workspace:
	// handing them to the network unauthenticated is remote code execution.
	if !loopbackOnly(*addr) && *token == "" && !*insecure {
		fmt.Fprintf(os.Stderr, "lyond: -addr %s is reachable off this machine but no token is set; pass -token/-insecure or bind 127.0.0.1\n", *addr)
		os.Exit(2)
	}

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

	// -approve parks NeedsApproval tools until a web client decides. The
	// bridge must exist before the engine (cfg.Approve rides every
	// session's adapter), and SetApproval below attaches the store for
	// seat back-resolution — approvals before that would fail closed.
	var appr *server.ApprovalBridge
	if *approve {
		appr = server.NewApprovalBridge()
		cfg.Approve = appr.ApproveFunc()
	}

	eng := agent.NewEngine(cfg, nil)
	store := server.NewStore(*maxSess, *ttl)
	// The evolution loop rides every daemon prompt_end (BACKLOG §2.5):
	// rounds booked, due exams kicked, failures post-mortemed — the same
	// computation the TUI drives, one pkg/evolve implementation. The
	// observer must be set before the first session registers; each
	// leg honors its own gate (LYON_SCORE / LYON_EXAM / LYON_DIAGNOSE).
	if !strings.EqualFold(os.Getenv("LYON_EVOLVE"), "off") {
		store.SetPromptEndObserver(evolve.Hook)
	}
	// The dump is the seat. A session mirrors itself to disk on every state
	// change, and whatever a previous process left behind is indexed here —
	// not swept into a handful of archive blobs. Each draft keeps its own id,
	// so the list shows the same conversations under the same ids and the
	// first RPC to one reads it back (Server.acquire) instead of 404ing or
	// minting a duplicate row.
	if n, err := store.LoadDrafts(eng.Config().Workspace); err != nil {
		log.Printf("sessions: loaded %d, then: %v", n, err)
	} else if n > 0 {
		log.Printf("sessions: %d saved conversation(s) indexed from disk", n)
	}
	srv := server.New(eng, store)
	if appr != nil {
		srv.SetApproval(appr)
	}
	srv.SetToken(*token)
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
		log.Printf("lyond listening on %s (model=%s workspace=%s max_sessions=%d max_turns=%d auth=%s)",
			*addr, eng.Config().Model, eng.Config().Workspace, *maxSess, *maxTurns, authState(*token))
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
	srv.CloseTerminal() // ConPTY shells don't die with the process — kill them
	eng.Close()         // retire the hooks writer goroutine
}

// loopbackOnly reports whether addr binds solely to the loopback interface.
// An empty host means "all interfaces"; an unresolvable hostname is treated
// as reachable so it fails the safe way (require a token).
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authState(token string) string {
	if token == "" {
		return "off"
	}
	return "bearer"
}
