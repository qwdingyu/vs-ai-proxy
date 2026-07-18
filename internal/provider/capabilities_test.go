package provider

import "testing"

func TestCompatibilityProfileForXiaomiMiMo(t *testing.T) {
	profile := CompatibilityProfileFor(
		"custom-mimo",
		"自定义 MiMo",
		"https://api.xiaomimimo.com/v1",
		string(ApiFormatOpenAi),
	)

	if profile.Capability != "xiaomimimo" {
		t.Fatalf("capability = %q, want xiaomimimo", profile.Capability)
	}
	if profile.ChatPath != "v1/chat/completions" {
		t.Fatalf("chat_path = %q, want v1/chat/completions", profile.ChatPath)
	}
	if profile.OutputTokenParam != "max_completion_tokens" {
		t.Fatalf("output_token_param = %q, want max_completion_tokens", profile.OutputTokenParam)
	}
}

func TestCompatibilityProfileForKimiVersionedBaseURL(t *testing.T) {
	profile := CompatibilityProfileFor(
		"kimi",
		"Kimi",
		"https://api.kimi.com/coding/v1",
		string(ApiFormatOpenAi),
	)

	if profile.Capability != "kimi" {
		t.Fatalf("capability = %q, want kimi", profile.Capability)
	}
	if profile.ChatPath != "chat/completions" {
		t.Fatalf("chat_path = %q, want chat/completions", profile.ChatPath)
	}
	if profile.ModelsPath != "models" {
		t.Fatalf("models_path = %q, want models", profile.ModelsPath)
	}
	if profile.OutputTokenParam != "max_tokens" {
		t.Fatalf("output_token_param = %q, want max_tokens", profile.OutputTokenParam)
	}
}

func TestOutputTokenParamForDefaultsToMaxTokens(t *testing.T) {
	if got := OutputTokenParamFor("unknown-openai-compatible"); got != "max_tokens" {
		t.Fatalf("OutputTokenParamFor unknown = %q, want max_tokens", got)
	}
}
