package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

func setResponseUsage(w http.ResponseWriter, usage *provider.Usage) {
	normalized := provider.NormalizeUsage(usage)
	if normalized == nil {
		return
	}
	logged := &store.TokenUsage{
		PromptTokens:     normalized.PromptTokens,
		CompletionTokens: normalized.CompletionTokens,
		TotalTokens:      normalized.TotalTokens,
		Source:           "upstream",
	}
	if normalized.PromptTokensDetails != nil {
		logged.CachedTokens = normalized.PromptTokensDetails.CachedTokens
	}
	if normalized.CompletionTokensDetails != nil {
		logged.ReasoningTokens = normalized.CompletionTokensDetails.ReasoningTokens
	}
	setResponseWriterUsage(w, logged)
}

func setRawOpenAIResponseUsage(w http.ResponseWriter, body []byte) {
	var envelope struct {
		Usage *provider.Usage `json:"usage"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		setResponseUsage(w, envelope.Usage)
	}
}

func setRawOllamaResponseUsage(w http.ResponseWriter, body []byte) {
	var envelope struct {
		PromptEvalCount *int64 `json:"prompt_eval_count"`
		EvalCount       *int64 `json:"eval_count"`
	}
	if json.Unmarshal(body, &envelope) != nil || (envelope.PromptEvalCount == nil && envelope.EvalCount == nil) {
		return
	}
	promptTokens := int64(0)
	completionTokens := int64(0)
	if envelope.PromptEvalCount != nil {
		promptTokens = *envelope.PromptEvalCount
	}
	if envelope.EvalCount != nil {
		completionTokens = *envelope.EvalCount
	}
	setResponseUsage(w, &provider.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	})
}

func setResponseWriterUsage(w http.ResponseWriter, usage *store.TokenUsage) {
	if usage == nil {
		return
	}
	switch target := w.(type) {
	case *responseWriter:
		copy := *usage
		target.usage = &copy
	case *streamAttemptWriter:
		setResponseWriterUsage(target.ResponseWriter, usage)
	}
}
