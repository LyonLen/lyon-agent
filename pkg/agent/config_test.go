package agent

import "testing"

func TestWebToolModeAutoDetection(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{"https://gateway.example.com/anthropic", "client"},
		{"https://api.anthropic.com", "server"},
		{"", "server"},
	}
	for _, tc := range cases {
		t.Run("base="+tc.baseURL, func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", tc.baseURL)
			eng := NewEngine(Config{}, nil)
			if got := eng.Config().WebToolMode; got != tc.want {
				t.Fatalf("mode = %q, want %q", got, tc.want)
			}
			_, hasWeb := eng.Registry().Get("web_search")
			if wantWeb := tc.want == "client"; hasWeb != wantWeb {
				t.Fatalf("web_search registered = %v, want %v", hasWeb, wantWeb)
			}
		})
	}
}

func TestWebToolModeExplicitOverrides(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.com/anthropic")

	off := NewEngine(Config{WebToolMode: "off"}, nil)
	if off.Config().WebToolMode != "off" {
		t.Fatalf("off override lost: %q", off.Config().WebToolMode)
	}
	if _, has := off.Registry().Get("web_search"); has {
		t.Fatal("off mode must not register web tools")
	}

	server := NewEngine(Config{WebToolMode: "server"}, nil)
	if _, has := server.Registry().Get("web_search"); has {
		t.Fatal("server mode must not register client web tools")
	}
}

// TestMaxIterationsEnv: LYON_MAX_ITER raises or lowers the per-prompt
// budget, garbage falls back to the default, and an explicit Config value
// (flag) still wins over the env.
func TestMaxIterationsEnv(t *testing.T) {
	t.Setenv("LYON_MAX_ITER", "80")
	if got := NewEngine(Config{}, nil).Config().MaxIterations; got != 80 {
		t.Fatalf("LYON_MAX_ITER=80 → %d", got)
	}
	t.Setenv("LYON_MAX_ITER", "many")
	if got := NewEngine(Config{}, nil).Config().MaxIterations; got != defaultMaxIterations {
		t.Fatalf("garbage env → %d, want %d", got, defaultMaxIterations)
	}
	t.Setenv("LYON_MAX_ITER", "80")
	if got := NewEngine(Config{MaxIterations: 7}, nil).Config().MaxIterations; got != 7 {
		t.Fatalf("explicit value lost to env: %d", got)
	}
}

// TestDefaultIterationsUnlimited: the ReAct loop runs until the model
// stops — no per-prompt cap unless one is asked for. A cap is opt-in
// (LYON_MAX_ITER / -max-iter, positive integer); 0 keeps it open.
func TestDefaultIterationsUnlimited(t *testing.T) {
	t.Setenv("LYON_MAX_ITER", "")
	if got := NewEngine(Config{}, nil).Config().MaxIterations; got != 0 {
		t.Fatalf("default MaxIterations = %d, want 0 (unlimited)", got)
	}
	t.Setenv("LYON_MAX_ITER", "0")
	if got := NewEngine(Config{}, nil).Config().MaxIterations; got != 0 {
		t.Fatalf("LYON_MAX_ITER=0 → %d, want 0 (unlimited)", got)
	}
	t.Setenv("LYON_MAX_ITER", "40")
	if got := NewEngine(Config{}, nil).Config().MaxIterations; got != 40 {
		t.Fatalf("LYON_MAX_ITER=40 → %d, want 40", got)
	}
}

// TestOpenAIBaseDefaults: LYON_OPENAI_BASE enables the translation layer
// with gateway-friendly defaults (client web tools, single subagent model),
// explicit values win, and leaving it unset keeps the Anthropic path.
func TestOpenAIBaseDefaults(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("LYON_SUBAGENT_MODEL", "")

	// Env enables; trailing slash trimmed; subagent falls back to the
	// main model because single-model gateways reject a second id.
	t.Setenv("LYON_OPENAI_BASE", "https://gw.example.com/v1/")
	eng := NewEngine(Config{Model: "m-both"}, nil)
	cfg := eng.Config()
	if cfg.OpenAIBase != "https://gw.example.com/v1" {
		t.Fatalf("OpenAIBase = %q (env + trailing-slash trim)", cfg.OpenAIBase)
	}
	if cfg.WebToolMode != "client" {
		t.Fatalf("WebToolMode = %q, want client under an OpenAI gateway", cfg.WebToolMode)
	}
	if cfg.SubagentModel != "m-both" {
		t.Fatalf("SubagentModel = %q, want main model", cfg.SubagentModel)
	}

	// An explicit base (flag) wins over the env var.
	t.Setenv("LYON_OPENAI_BASE", "https://env.example/v1")
	eng2 := NewEngine(Config{OpenAIBase: "https://flag.example/v1"}, nil)
	if got := eng2.Config().OpenAIBase; got != "https://flag.example/v1" {
		t.Fatalf("flag base lost: %q", got)
	}

	// Explicit subagent model still wins over the single-model fallback.
	t.Setenv("LYON_SUBAGENT_MODEL", "haiku-x")
	eng3 := NewEngine(Config{Model: "m-main"}, nil)
	if got := eng3.Config().SubagentModel; got != "haiku-x" {
		t.Fatalf("LYON_SUBAGENT_MODEL ignored: %q", got)
	}

	// Unset: Anthropic path untouched, stock subagent tier.
	t.Setenv("LYON_OPENAI_BASE", "")
	t.Setenv("LYON_SUBAGENT_MODEL", "")
	eng4 := NewEngine(Config{Model: "m4"}, nil)
	cfg4 := eng4.Config()
	if cfg4.OpenAIBase != "" {
		t.Fatalf("OpenAIBase leaked: %q", cfg4.OpenAIBase)
	}
	if cfg4.SubagentModel != DefaultSubagentModel {
		t.Fatalf("SubagentModel = %q, want default tier", cfg4.SubagentModel)
	}
}

// TestCapToModelOutput: the configured output cap is a ceiling, not a
// promise — a model with a known lower MaxOutput must clamp it (otherwise
// the gateway truncates answers mid-sentence with no stop_reason), while
// unknown models and already-safe caps keep the configured value.
func TestCapToModelOutput(t *testing.T) {
	if got := capToModelOutput("deepseek-chat", 64000); got != 8192 {
		t.Fatalf("deepseek-chat@64000 → %d, want the 8192 card ceiling", got)
	}
	if got := capToModelOutput("deepseek-chat", 4096); got != 4096 {
		t.Fatalf("already-safe cap must pass through, got %d", got)
	}
	if got := capToModelOutput("no-such-model-xyz", 64000); got != 64000 {
		t.Fatalf("unknown model must keep the configured cap, got %d", got)
	}

	// End to end: withDefaults applies the clamp for both loops.
	eng := NewEngine(Config{Model: "deepseek-chat", SubagentModel: "deepseek-chat"}, nil)
	cfg := eng.Config()
	if cfg.MaxTokens != 8192 {
		t.Fatalf("engine MaxTokens = %d, want 8192", cfg.MaxTokens)
	}
	if cfg.SubagentMaxTokens != 8192 {
		t.Fatalf("engine SubagentMaxTokens = %d, want 8192", cfg.SubagentMaxTokens)
	}
}
