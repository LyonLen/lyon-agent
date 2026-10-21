// SettingsService — "which brain is answering, and what can it do".
//
// lyond is configured at launch (flags + LYON_*/ANTHROPIC_* environment) and
// does not read ~/.lyon/config.toml, so this panel reports the process's
// EFFECTIVE configuration, not a file that claims something else. Model
// values come from s.eng.Config(), which overlays the live model tier — a
// switch through UpdateSettings shows up here (and in the ctx gauge, the
// session proto, /healthz, /metrics) without a restart.
//
// Credentials are never echoed: names that hold a secret report set/unset
// with masked=true and an empty value, in both directions.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/models"
)

type settingsService struct {
	s *Server
}

var _ lyonv1connect.SettingsServiceHandler = (*settingsService)(nil)

// envEntry is one row of the environment inventory: a name the process
// actually reads, plus the one line that explains what it does. Ordering in
// envCatalog is the panel's ordering — model first, credentials last.
type envEntry struct {
	name string
	note string
}

// envCatalog is the curated inventory of variables lyon and lyond read.
// Test-only knobs and hook-helper plumbing are deliberately absent: the
// panel documents operating knobs, not the test harness.
var envCatalog = []envEntry{
	{"LYON_MODEL", "主循环模型（优先于 ANTHROPIC_MODEL）"},
	{"LYON_SUBAGENT_MODEL", "子代理 / 低成本档模型"},
	{"LYON_BASE_URL", "Anthropic 协议网关地址"},
	{"ANTHROPIC_BASE_URL", "Anthropic 官方 / 网关地址"},
	{"LYON_OPENAI_BASE", "OpenAI 协议转换层地址（设置后走 chat/completions）"},
	{"LYON_MAX_ITER", "单个 prompt 的 API 调用上限（0 = 不限）"},
	{"LYON_LANG", "界面 / 回复语言"},
	{"LYON_HOME", "lyon 数据根目录（会话、dump、记忆）"},
	{"LYON_SKILLS_DIR", "技能目录"},
	{"LYON_HOOKS", "hook 配置路径"},
	{"LYON_AUDIT", "审计日志开关"},
	{"LYON_DISTILL", "记忆蒸馏开关"},
	{"LYON_DIAGNOSE", "失败自诊断开关"},
	{"LYON_DEBUG", "调试输出开关"},
	{"LYON_DUMP", "运行 dump 开关"},
	{"LYON_NOTIFY", "完成通知开关"},
	{"LYON_AUTOFREEZE", "空闲会话自动冻结"},
	{"LYON_TITLE", "自动标题开关"},
	{"LYON_PRICE_IN", "输入侧价格覆盖（USD / 百万 token）"},
	{"LYON_PRICE_OUT", "输出侧价格覆盖（USD / 百万 token）"},
	{"LYON_DASHBOARD", "dashboard 开关"},
	{"LYON_SYNC_URL", "远端同步地址"},
	{"LYON_SYNC_TOKEN", "远端同步令牌"},
	{"LYON_AUTH_TOKEN", "lyon 服务鉴权令牌"},
	{"LYON_API_KEY", "模型 API 密钥"},
	{"LYON_TOKEN", "lyond 访问令牌"},
	{"ANTHROPIC_API_KEY", "Anthropic API 密钥"},
	{"ANTHROPIC_AUTH_TOKEN", "Anthropic 鉴权令牌"},
	{"ANTHROPIC_MODEL", "模型 id（LYON_MODEL 的兜底）"},
}

// credentialEnv names values that must never leave the process. Matched by
// substring on the upper-cased name so a new *_TOKEN / *_KEY / *_SECRET
// variable cannot leak by being forgotten in the list above.
func credentialEnv(name string) bool {
	u := strings.ToUpper(name)
	for _, frag := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(u, frag) {
			return true
		}
	}
	return false
}

// envProto renders one catalog entry. A credential reports whether it is
// set and nothing else — set/unset is what an operator needs to debug a
// missing login, the value is what an attacker needs.
func envProto(e envEntry) *lyonv1.EnvVar {
	v, set := os.LookupEnv(e.name)
	v = strings.TrimSpace(v)
	set = set && v != ""
	out := &lyonv1.EnvVar{Name: e.name, Set: set, Note: e.note}
	if credentialEnv(e.name) {
		out.Masked = true
		return out
	}
	if set {
		out.Value = v
	}
	return out
}

// modelProto flattens one knowledge-base entry for the picker table.
func modelProto(i *models.Info) *lyonv1.ModelInfo {
	p := i.CapProfile()
	pr := i.Pricing
	return &lyonv1.ModelInfo{
		Id:          i.ID,
		Family:      i.Family,
		Provider:    i.Provider,
		Context:     i.Context,
		MaxOutput:   i.MaxOutput,
		Vision:      i.Vision,
		Reasoning:   i.Reasoning,
		Temperature: i.Temperature,
		TopP:        i.TopP,
		Cutoff:      i.Cutoff,
		Notes:       i.Notes,
		Pricing: &lyonv1.ModelPricing{
			Input:      pr.Input,
			Output:     pr.Output,
			CacheRead:  pr.CacheRead,
			CacheWrite: pr.CacheWrite,
			Known:      pr.Known(),
		},
		Profile: &lyonv1.ModelProfile{
			Reasoning: int32(p.Reasoning),
			LongHaul:  int32(p.LongHaul),
			Openness:  int32(p.Openness),
			Speed:     int32(p.Speed),
			Thrift:    int32(p.Thrift),
		},
	}
}

// engineProto reports the effective config. gauge_model_source names where
// the gauge's denominator came from: "knowledge-base" when the knowledge
// base knows the live model, "config" when the launch-time ContextWindow
// stands in for an unknown id — the two are different numbers and the panel
// must not pretend otherwise.
func (s *settingsService) engineProto() *lyonv1.EngineSettings {
	cfg := s.s.eng.Config()
	source := "config"
	if info := models.Lookup(cfg.Model); info != nil && info.Context > 0 {
		source = "knowledge-base"
	}
	return &lyonv1.EngineSettings{
		Model:              cfg.Model,
		SubagentModel:      cfg.SubagentModel,
		Effort:             cfg.Effort,
		SubagentEffort:     cfg.SubagentEffort,
		MaxTokens:          cfg.MaxTokens,
		SubagentMaxTokens:  cfg.SubagentMaxTokens,
		MaxIterations:      int32(cfg.MaxIterations),
		CompactThreshold:   cfg.CompactThreshold,
		ContextWindow:      cfg.ContextWindow,
		Workspace:          cfg.Workspace,
		WebToolMode:        cfg.WebToolMode,
		OpenaiBase:         cfg.OpenAIBase,
		MaxConcurrentTurns: int32(cfg.MaxConcurrentTurns),
		ShellAllow:         cfg.ShellAllow,
		ApprovalBridge:     s.s.appr != nil,
		GaugeModelSource:   source,
	}
}

// envFirst returns the first of keys set to a non-empty value — mirrors
// pkg/agent's unexported helper so the panel resolves the endpoint and its
// credential exactly the way the engine's client does.
func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// gatewayClient is package-level so repeated panel loads reuse one pool.
var gatewayClient = &http.Client{Timeout: 8 * time.Second}

// gatewayModel is one entry from an endpoint's model list, kept to the two
// fields Anthropic's and OpenAI's shapes agree on.
type gatewayModel struct {
	id    string
	owner string
}

// modelsEndpoint resolves the URL that lists the endpoint's models. An
// OpenAI-protocol base already carries its /v1, so models hang directly off
// it; an Anthropic base is a bare host (gateways like New API) or a
// /v1-less URL, so /v1/models is appended. No base at all means the
// Anthropic default host.
func modelsEndpoint(openaiBase string) string {
	if b := strings.TrimRight(strings.TrimSpace(openaiBase), "/"); b != "" {
		return b + "/models"
	}
	base := envFirst("LYON_BASE_URL", "ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return strings.TrimRight(base, "/") + "/v1/models"
}

// gatewayKey is the credential the engine would send: an API key wins over
// an auth token, LYON_* over ANTHROPIC_* (mirrors agent.endpointOptions).
func gatewayKey() string {
	if k := envFirst("LYON_API_KEY", "ANTHROPIC_API_KEY"); k != "" {
		return k
	}
	return envFirst("LYON_AUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN")
}

// fetchGatewayModels asks the endpoint for its model list. Both credential
// headers ride along so one path serves Anthropic-protocol gateways
// (x-api-key) and OpenAI-protocol ones (Authorization: Bearer).
func fetchGatewayModels(ctx context.Context, url, key string) ([]gatewayModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := gatewayClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, firstLine(body))
	}
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]gatewayModel, 0, len(doc.Data))
	for _, m := range doc.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, gatewayModel{id: id, owner: strings.TrimSpace(m.OwnedBy)})
		}
	}
	return out, nil
}

// firstLine trims an error body to one readable line for the panel.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// buildModels sources the picker from the endpoint's own list, enriching
// the ids the knowledge base recognizes. An unknown gateway id still gets a
// row (bare; provider = owned_by). A failed fetch yields no rows plus the
// reason — never a preset table, which would name models this endpoint may
// not serve.
func buildModels(ctx context.Context, openaiBase string) ([]*lyonv1.ModelInfo, string, string) {
	gw, err := fetchGatewayModels(ctx, modelsEndpoint(openaiBase), gatewayKey())
	if err != nil {
		return nil, "error", err.Error()
	}
	list := make([]*lyonv1.ModelInfo, 0, len(gw))
	for _, m := range gw {
		if info := models.Lookup(m.id); info != nil {
			list = append(list, modelProto(info))
			continue
		}
		owner := m.owner
		if owner == "" {
			owner = "网关"
		}
		list = append(list, &lyonv1.ModelInfo{
			Id:       m.id,
			Provider: owner,
			Notes:    []string{"来自网关；知识库无该模型（窗口与价格未注明）"},
		})
	}
	return list, "gateway", ""
}

func (s *settingsService) GetSettings(ctx context.Context, _ *connect.Request[lyonv1.GetSettingsRequest]) (*connect.Response[lyonv1.GetSettingsResponse], error) {
	list, source, mErr := buildModels(ctx, s.s.eng.Config().OpenAIBase)
	env := make([]*lyonv1.EnvVar, 0, len(envCatalog))
	for _, e := range envCatalog {
		env = append(env, envProto(e))
	}
	return connect.NewResponse(&lyonv1.GetSettingsResponse{
		Engine:       s.engineProto(),
		Models:       list,
		Env:          env,
		ModelsSource: source,
		ModelsError:  mErr,
	}), nil
}

func (s *settingsService) UpdateSettings(_ context.Context, req *connect.Request[lyonv1.UpdateSettingsRequest]) (*connect.Response[lyonv1.UpdateSettingsResponse], error) {
	msg := req.Msg
	// An absent field keeps its value; only a present one is applied. The
	// pair moves together (SetModels validates them as one tier), so a
	// subagent-only change re-sends the current main model.
	main, sub := s.s.eng.Models()
	if msg.Model != nil {
		main = *msg.Model
	}
	if msg.SubagentModel != nil {
		sub = *msg.SubagentModel
	} else if msg.Model != nil && strings.TrimSpace(sub) == "" {
		sub = main // single-model gateway: follow the main tier
	}
	if err := s.s.eng.SetModels(main, sub); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("update settings: %w", err))
	}
	return connect.NewResponse(&lyonv1.UpdateSettingsResponse{Engine: s.engineProto()}), nil
}
