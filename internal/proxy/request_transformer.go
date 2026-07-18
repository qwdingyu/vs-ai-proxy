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

	preserveReasoningOnly := shouldInjectCachedReasoning(prov)
	if preserveReasoningOnly {
		s.injectCachedReasoning(req)
	}
	// VS/Copilot 历史里可能出现空 assistant 占位；Kimi 这类 provider 会把
	// 空 assistant 判为非法消息。只有 direct reasoning provider 才保留纯
	// reasoning assistant，用于 reasoning cache；其它 provider 一律按空占位清理。
	dropEmptyAssistantPlaceholders(req, preserveReasoningOnly)
	s.applyExecutionDefaults(cfg, req, requestedModel, prov)
}

func shouldInjectCachedReasoning(prov provider.Provider) bool {
	if prov == nil {
		return false
	}
	caps := provider.GetCapabilities(provider.CapabilityNameOf(prov))
	// reasoning_content 是模型/厂商私有语义，不应无条件注入到 new-api/sub2api
	// 这类多模型聚合网关。聚合网关不同渠道的 body/context 限制不一致，
	// 隐式注入可能把 VS/Copilot 的大工具请求放大到 413，且排障时看不到来源。
	return caps.Category == provider.ProviderCategoryDirect && caps.SupportsReasoningEffort
}

func (s *Server) injectCachedReasoning(req *provider.ChatRequest) {
	if req == nil || s.reasoningCache == nil || len(req.Messages) == 0 {
		return
	}

	assistantIndex := 0
	for i := range req.Messages {
		msg := &req.Messages[i]
		if msg.Role != "assistant" {
			continue
		}

		key := reasoningCacheKeyForMessage(*msg)
		if key == "" {
			key = assistantKey(assistantIndex)
			assistantIndex++
		}

		if strings.TrimSpace(msg.Reasoning) == "" {
			if reasoning, ok := s.reasoningCache.TryGet(key); ok {
				msg.Reasoning = reasoning
			}
		}
	}
}

func dropEmptyAssistantPlaceholders(req *provider.ChatRequest, preserveReasoningOnly bool) {
	if req == nil || len(req.Messages) == 0 {
		return
	}

	messages := make([]provider.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		candidate := msg
		if !preserveReasoningOnly {
			candidate.Reasoning = ""
		}
		if !shouldDropAssistantPlaceholder(candidate) {
			messages = append(messages, msg)
		}
	}
	req.Messages = messages
}

func shouldDropAssistantPlaceholder(msg provider.Message) bool {
	if msg.Role != "assistant" {
		return false
	}
	if len(msg.ToolCalls) > 0 {
		return false
	}
	if msg.FunctionCall != nil {
		return false
	}
	if strings.TrimSpace(msg.Reasoning) != "" {
		return false
	}
	if strings.TrimSpace(msg.Refusal) != "" {
		return false
	}
	if len(msg.ContentRaw) > 0 {
		return false
	}
	if len(msg.Extra) > 0 {
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
	caps := provider.GetCapabilities(provider.CapabilityNameOf(prov))
	hasDeclaredTools := requestDeclaresTools(req)
	if !ok {
		if !hasDeclaredTools {
			// 普通聊天没有模型配置时给一个保守输出上限；工具请求不能这样做，
			// 否则大工具参数可能被 max_tokens=4096 截断，表现为 VS 工具无法执行。
			s.applyGlobalDefaults(req)
		}
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
		// 用户显式选择“覆盖客户端参数”时，模型配置优先；但仍必须经过
		// provider capability 过滤，避免把 unsupported top_k/reasoning_effort 透传给严格上游。
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
		// 默认策略是“客户端显式值优先，模型配置只补缺省值”。这保护 VS/Copilot
		// 针对工具调用和长上下文动态设置的参数，不让管理页配置无意覆盖客户端意图。
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

	if !hasDeclaredTools {
		s.applyGlobalDefaults(req)
	}
}

func (s *Server) applyGlobalDefaults(req *provider.ChatRequest) {
	// Sampling parameters are model-specific. Leave them unset unless the
	// client or a declarative model profile supplies a compatible value.
	if req.MaxTokens == nil {
		req.MaxTokens = intPtr(4096)
	}
}

// requestDeclaresTools 只在 modern tools 或 legacy functions 实际非空时
// 关闭代理猜测的输出/采样默认值；[] 和 null 仍属于普通聊天，必须保持旧行为。
func requestDeclaresTools(req *provider.ChatRequest) bool {
	if req == nil {
		return false
	}
	if len(req.Tools) > 0 {
		return true
	}
	var functions []json.RawMessage
	if err := json.Unmarshal(req.Extra["functions"], &functions); err != nil {
		return false
	}
	return len(functions) > 0
}

func (s *Server) applyProfileDefaults(
	req *provider.ChatRequest,
	profile provider.ModelProfile,
	prov provider.Provider,
) {
	if req == nil {
		return
	}
	caps := provider.GetCapabilities(provider.CapabilityNameOf(prov))
	isNativeReasoner := caps.SupportsReasoningEffort && strings.TrimSpace(profile.ReasoningEffort) != ""
	if profile.ContextLength != nil && *profile.ContextLength > 0 &&
		(req.ContextLength == nil || profile.OverrideClientParams) {
		contextLength := *profile.ContextLength
		req.ContextLength = &contextLength
	}

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
	if profile.FixedTemperature != nil {
		fixedTemperature := *profile.FixedTemperature
		req.Temperature = &fixedTemperature
	}
	outputLimit := 0
	if profile.ContextLength != nil && *profile.ContextLength > 0 {
		outputLimit = *profile.ContextLength
	}
	if profile.MaxOutputTokens != nil && *profile.MaxOutputTokens > 0 &&
		(outputLimit == 0 || *profile.MaxOutputTokens < outputLimit) {
		outputLimit = *profile.MaxOutputTokens
	}
	if outputLimit > 0 && (req.MaxTokens == nil || *req.MaxTokens > outputLimit) {
		req.MaxTokens = intPtr(outputLimit)
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
	if req.ContextLength != nil {
		v := *req.ContextLength
		out.ContextLength = &v
	}
	out.Messages = append([]provider.Message(nil), req.Messages...)
	for i := range out.Messages {
		out.Messages[i].ContentRaw = append(json.RawMessage(nil), req.Messages[i].ContentRaw...)
		out.Messages[i].ToolCalls = append([]provider.ToolCall(nil), req.Messages[i].ToolCalls...)
		for j := range out.Messages[i].ToolCalls {
			out.Messages[i].ToolCalls[j].Extra = cloneRawMessages(req.Messages[i].ToolCalls[j].Extra)
			out.Messages[i].ToolCalls[j].Function.Extra = cloneRawMessages(req.Messages[i].ToolCalls[j].Function.Extra)
		}
		if req.Messages[i].FunctionCall != nil {
			functionCall := *req.Messages[i].FunctionCall
			functionCall.Extra = cloneRawMessages(req.Messages[i].FunctionCall.Extra)
			out.Messages[i].FunctionCall = &functionCall
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

	bestScore := -1
	var best config.ModelConfig
	for _, m := range cfg.Models {
		if !m.Enabled || !strings.EqualFold(modelProviderKey(m), providerName) {
			continue
		}
		if score := modelConfigMatchScore(m, requestedModel, upstreamModel); score > bestScore {
			best = m
			bestScore = score
		}
	}
	if bestScore >= 0 {
		return best, true
	}

	bestScore = -1
	for _, m := range cfg.Models {
		if !m.Enabled || (modelProviderKey(m) != "" && providerName != "") {
			continue
		}
		if score := modelConfigMatchScore(m, requestedModel, upstreamModel); score > bestScore {
			best = m
			bestScore = score
		}
	}
	return best, bestScore >= 0
}

func mergeModelConfigProfile(profile provider.ModelProfile, model config.ModelConfig) provider.ModelProfile {
	override := provider.ModelProfile{
		Model:                strings.TrimSpace(model.Name),
		Provider:             modelProviderKey(model),
		SupportsTools:        model.SupportsTools,
		SupportsVision:       model.SupportsVision,
		Temperature:          model.Temperature,
		TopP:                 model.TopP,
		ReasoningEffort:      strings.TrimSpace(model.ReasoningEffort),
		OverrideClientParams: model.OverrideClientParams,
		Enabled:              model.Enabled,
	}
	if model.ContextLength != nil && *model.ContextLength > 0 {
		override.ContextLength = model.ContextLength
		override.InputTokenLimit = model.ContextLength
	}
	if model.MaxOutputTokens != nil && *model.MaxOutputTokens > 0 {
		override.MaxOutputTokens = model.MaxOutputTokens
	}
	if model.MaxTokens != nil && *model.MaxTokens > 0 {
		override.MaxTokens = model.MaxTokens
	}
	if model.TimeoutSeconds != nil && *model.TimeoutSeconds > 0 {
		override.TimeoutSeconds = model.TimeoutSeconds
	}

	merged := provider.MergeModelProfiles(profile, override)
	merged.OverrideClientParams = model.OverrideClientParams
	return merged
}

func modelProviderKey(m config.ModelConfig) string {
	key := strings.TrimSpace(m.ProviderID)
	if key == "" {
		key = strings.TrimSpace(m.Provider)
	}
	return key
}

func modelConfigMatchScore(m config.ModelConfig, requestedModel, upstreamModel string) int {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return -1
	}

	best := -1
	for _, candidate := range []string{
		requestedModel,
		upstreamModel,
		provider.DisplayNameModelSuffix(requestedModel),
		provider.DisplayNameModelSuffix(upstreamModel),
	} {
		if score := provider.ProfileNameMatchScore(candidate, name); score > best {
			best = score
		}
	}
	return best
}
