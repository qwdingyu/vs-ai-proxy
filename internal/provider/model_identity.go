package provider

import "strings"

// ModelIdentity 是代理内部统一的模型身份描述。
//
// Visual Studio Copilot、Ollama-compatible 客户端和 OpenAI-compatible provider
// 对同一个模型会使用不同名字：用户看到 display，客户端可能回传 alias，
// 上游只能接受 upstream。所有模型名转换必须先落到这层，避免在 endpoint
// 和 registry 中各自拼接字符串。
type ModelIdentity struct {
	Upstream  string
	Provider  string
	Qualified string
	Display   string
	Basename  string
	Aliases   []string
}

func NewModelIdentity(upstream, provider string) ModelIdentity {
	return NewModelIdentityWithDisplay(upstream, provider, provider)
}

func NewModelIdentityWithDisplay(upstream, provider, providerDisplay string) ModelIdentity {
	upstream = strings.TrimSpace(upstream)
	provider = strings.TrimSpace(provider)
	providerDisplay = strings.TrimSpace(providerDisplay)
	basename := ModelBasename(upstream)
	qualified := upstream
	if provider != "" {
		qualified = upstream + "@" + provider
	}

	display := basename
	if provider != "" {
		if providerDisplay == "" {
			providerDisplay = provider
		}
		display = providerDisplay + " - " + basename
	}

	return ModelIdentity{
		Upstream:  upstream,
		Provider:  provider,
		Qualified: qualified,
		Display:   display,
		Basename:  basename,
		Aliases:   ModelAliases(upstream, provider),
	}
}

func ModelAliases(upstream, provider string) []string {
	upstream = strings.TrimSpace(upstream)
	provider = strings.TrimSpace(provider)
	qualified := upstream
	if provider != "" {
		qualified = upstream + "@" + provider
	}

	basename := ModelBasename(upstream)
	candidates := []string{
		upstream,
		upstream + ":latest",
		qualified,
		qualified + ":latest",
	}
	if basename != "" && !strings.EqualFold(basename, upstream) {
		candidates = append(candidates, basename, basename+":latest")
	}

	out := []string{}
	seen := map[string]struct{}{}
	for _, alias := range candidates {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, alias)
	}
	return out
}

func ModelBasename(model string) string {
	model = StripModelTag(strings.TrimSpace(model))
	if at := strings.Index(model, "@"); at > 0 {
		model = model[:at]
	}
	if slash := strings.LastIndex(model, "/"); slash > 0 && slash < len(model)-1 {
		return model[slash+1:]
	}
	return model
}

func DisplayNameModelSuffix(model string) string {
	clean := StripModelTag(strings.TrimSpace(model))
	sep := " - "
	idx := strings.LastIndex(clean, sep)
	if idx < 0 || idx+len(sep) >= len(clean) {
		return ""
	}
	return strings.TrimSpace(clean[idx+len(sep):])
}

func StripModelTag(model string) string {
	model = strings.TrimSpace(model)
	colon := strings.LastIndex(model, ":")
	if colon <= 0 {
		return model
	}
	if !strings.EqualFold(model[colon+1:], "latest") {
		return model
	}
	if strings.Contains(model[colon+1:], "/") {
		return model
	}
	return model[:colon]
}
