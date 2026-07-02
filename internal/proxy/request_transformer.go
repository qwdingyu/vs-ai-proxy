package proxy

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func (s *Server) transformRequest(
	cfg *config.AppConfig,
	req *provider.ChatRequest,
	requestedModel string,
	prov provider.Provider,
) {
	if req == nil {
		return
	}

	s.injectCachedReasoning(req)
	s.applyExecutionDefaults(cfg, req, requestedModel, prov)
}

func (s *Server) injectCachedReasoning(req *provider.ChatRequest) {
	if req == nil || s.reasoningCache == nil || len(req.Messages) == 0 {
		return
	}

	assistantIndex := 0
	trimmed := make([]provider.Message, 0, len(req.Messages))

	for _, msg := range req.Messages {
		if msg.Role != "assistant" {
			trimmed = append(trimmed, msg)
			continue
		}

		key := reasoningCacheKeyForMessage(msg)
		if key == "" {
			key = assistantKey(assistantIndex)
			assistantIndex++
		}

		if strings.TrimSpace(msg.Reasoning) == "" {
			if reasoning, ok := s.reasoningCache.TryGet(key); ok {
				msg.Reasoning = reasoning
			}
		}

		if shouldDropAssistantPlaceholder(msg) {
			continue
		}

		trimmed = append(trimmed, msg)
	}

	req.Messages = trimmed
}

func shouldDropAssistantPlaceholder(msg provider.Message) bool {
	if msg.Role != "assistant" {
		return false
	}
	if len(msg.ToolCalls) > 0 {
		return false
	}
	if strings.TrimSpace(msg.Reasoning) != "" {
		return false
	}
	if len(msg.ContentRaw) > 0 {
		return false
	}
	return strings.TrimSpace(msg.Content) == ""
}

func assistantKey(index int) string {
	return "assistant:" + strconv.Itoa(index)
}

func (s *Server) applyExecutionDefaults(
	cfg *config.AppConfig,
	req *provider.ChatRequest,
	requestedModel string,
	prov provider.Provider,
) {
	if req == nil {
		return
	}

	modelCfg, ok := findModelConfig(cfg, requestedModel, req.Model, prov.Name())
	caps := provider.GetCapabilities(prov.Name())
	if !ok {
		s.applyGlobalDefaults(req)
		if !caps.SupportsTopK {
			req.TopK = nil
		}
		if !caps.SupportsReasoningEffort {
			req.ReasoningEffort = ""
		}
		return
	}

	isNativeReasoner := caps.SupportsReasoningEffort && strings.TrimSpace(modelCfg.ReasoningEffort) != ""

	if modelCfg.OverrideClientParams {
		if modelCfg.Temperature != nil {
			req.Temperature = modelCfg.Temperature
		}
		if modelCfg.TopP != nil && !isNativeReasoner {
			req.TopP = modelCfg.TopP
		}
		if modelCfg.MaxTokens != nil {
			req.MaxTokens = modelCfg.MaxTokens
		}
		if strings.TrimSpace(modelCfg.ReasoningEffort) != "" && caps.SupportsReasoningEffort {
			req.ReasoningEffort = modelCfg.ReasoningEffort
		}
	} else {
		if req.Temperature == nil && modelCfg.Temperature != nil {
			req.Temperature = modelCfg.Temperature
		}
		if req.TopP == nil && modelCfg.TopP != nil && !isNativeReasoner {
			req.TopP = modelCfg.TopP
		}
		if req.MaxTokens == nil && modelCfg.MaxTokens != nil {
			req.MaxTokens = modelCfg.MaxTokens
		}
		if strings.TrimSpace(req.ReasoningEffort) == "" && strings.TrimSpace(modelCfg.ReasoningEffort) != "" && caps.SupportsReasoningEffort {
			req.ReasoningEffort = modelCfg.ReasoningEffort
		}
	}

	if !caps.SupportsTopK {
		req.TopK = nil
	}
	if !caps.SupportsReasoningEffort {
		req.ReasoningEffort = ""
	}

	s.applyGlobalDefaults(req)
}

func (s *Server) applyGlobalDefaults(req *provider.ChatRequest) {
	if req.Temperature == nil {
		req.Temperature = float64Ptr(0.7)
	}
	if req.MaxTokens == nil {
		req.MaxTokens = intPtr(4096)
	}
}

func (s *Server) applyProfileDefaults(
	req *provider.ChatRequest,
	profile provider.ModelProfile,
	prov provider.Provider,
) {
	if req == nil {
		return
	}
	caps := provider.GetCapabilities(prov.Name())
	isNativeReasoner := caps.SupportsReasoningEffort && strings.TrimSpace(profile.ReasoningEffort) != ""

	if profile.OverrideClientParams {
		if profile.Temperature != nil {
			req.Temperature = profile.Temperature
		}
		if profile.TopP != nil && !isNativeReasoner {
			req.TopP = profile.TopP
		}
		if profile.MaxTokens != nil {
			req.MaxTokens = profile.MaxTokens
		}
		if strings.TrimSpace(profile.ReasoningEffort) != "" && caps.SupportsReasoningEffort {
			req.ReasoningEffort = profile.ReasoningEffort
		}
	} else {
		if req.Temperature == nil && profile.Temperature != nil {
			req.Temperature = profile.Temperature
		}
		if req.TopP == nil && profile.TopP != nil && !isNativeReasoner {
			req.TopP = profile.TopP
		}
		if req.MaxTokens == nil && profile.MaxTokens != nil {
			req.MaxTokens = profile.MaxTokens
		}
		if strings.TrimSpace(req.ReasoningEffort) == "" && strings.TrimSpace(profile.ReasoningEffort) != "" && caps.SupportsReasoningEffort {
			req.ReasoningEffort = profile.ReasoningEffort
		}
	}
	if profile.ContextLength != nil && *profile.ContextLength > 0 {
		if req.MaxTokens == nil || *req.MaxTokens > *profile.ContextLength {
			req.MaxTokens = intPtr(*profile.ContextLength)
		}
	}
	if !caps.SupportsReasoningEffort {
		req.ReasoningEffort = ""
	}
}

func cloneChatRequest(req *provider.ChatRequest) *provider.ChatRequest {
	if req == nil {
		return nil
	}

	out := *req
	if req.Temperature != nil {
		v := *req.Temperature
		out.Temperature = &v
	}
	if req.TopP != nil {
		v := *req.TopP
		out.TopP = &v
	}
	if req.TopK != nil {
		v := *req.TopK
		out.TopK = &v
	}
	if req.MaxTokens != nil {
		v := *req.MaxTokens
		out.MaxTokens = &v
	}
	out.Messages = append([]provider.Message(nil), req.Messages...)
	for i := range out.Messages {
		out.Messages[i].ContentRaw = append(json.RawMessage(nil), req.Messages[i].ContentRaw...)
		out.Messages[i].ToolCalls = append([]provider.ToolCall(nil), req.Messages[i].ToolCalls...)
		for j := range out.Messages[i].ToolCalls {
			out.Messages[i].ToolCalls[j].Extra = cloneRawMessages(req.Messages[i].ToolCalls[j].Extra)
			out.Messages[i].ToolCalls[j].Function.Extra = cloneRawMessages(req.Messages[i].ToolCalls[j].Function.Extra)
		}
		out.Messages[i].Extra = cloneRawMessages(req.Messages[i].Extra)
	}
	out.Tools = append([]provider.Tool(nil), req.Tools...)
	for i := range out.Tools {
		out.Tools[i].Extra = cloneRawMessages(req.Tools[i].Extra)
		out.Tools[i].Function.Extra = cloneRawMessages(req.Tools[i].Function.Extra)
	}
	out.Stop = append([]string(nil), req.Stop...)
	out.Extra = cloneRawMessages(req.Extra)
	out.OptionsExtra = cloneRawMessages(req.OptionsExtra)
	return &out
}

func cloneRawMessages(src map[string]json.RawMessage) map[string]json.RawMessage {
	if len(src) == 0 {
		return map[string]json.RawMessage{}
	}

	out := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func findModelConfig(
	cfg *config.AppConfig,
	requestedModel string,
	upstreamModel string,
	providerName string,
) (config.ModelConfig, bool) {
	if cfg == nil {
		return config.ModelConfig{}, false
	}

	requestedModel = strings.TrimSpace(requestedModel)
	upstreamModel = strings.TrimSpace(upstreamModel)
	providerName = strings.TrimSpace(providerName)

	for _, m := range cfg.Models {
		if !m.Enabled || !modelConfigNameMatches(m, requestedModel, upstreamModel) {
			continue
		}
		if providerName != "" && strings.TrimSpace(m.Provider) == providerName {
			return m, true
		}
	}

	for _, m := range cfg.Models {
		if !m.Enabled || !modelConfigNameMatches(m, requestedModel, upstreamModel) {
			continue
		}
		if strings.TrimSpace(m.Provider) == "" || providerName == "" {
			return m, true
		}
	}
	return config.ModelConfig{}, false
}

func modelConfigNameMatches(m config.ModelConfig, requestedModel, upstreamModel string) bool {
	name := strings.TrimSpace(m.Name)
	return name != "" && (name == requestedModel || name == upstreamModel)
}
