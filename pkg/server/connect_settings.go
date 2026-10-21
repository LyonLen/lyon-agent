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
	"fmt"
	"os"
	"strings"

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

func (s *settingsService) GetSettings(_ context.Context, _ *connect.Request[lyonv1.GetSettingsRequest]) (*connect.Response[lyonv1.GetSettingsResponse], error) {
	infos := models.All()
	list := make([]*lyonv1.ModelInfo, 0, len(infos))
	for i := range infos {
		list = append(list, modelProto(&infos[i]))
	}
	env := make([]*lyonv1.EnvVar, 0, len(envCatalog))
	for _, e := range envCatalog {
		env = append(env, envProto(e))
	}
	return connect.NewResponse(&lyonv1.GetSettingsResponse{
		Engine: s.engineProto(),
		Models: list,
		Env:    env,
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
