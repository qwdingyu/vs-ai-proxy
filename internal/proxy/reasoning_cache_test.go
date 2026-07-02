package proxy

import (
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func TestCacheChatResponseStoresAssistantReasoning(t *testing.T) {
	server := &Server{reasoningCache: newReasoningCache()}
	server.cacheChatResponse(&provider.ChatResponse{
		Choices: []provider.Choice{{
			Message: provider.Message{
				Role:      "assistant",
				Content:   "ok",
				Reasoning: "chain of thought",
			},
		}},
	})

	value, ok := server.reasoningCache.TryGet("assistant:0")
	if !ok {
		t.Fatalf("expected cached reasoning to exist")
	}
	if value != "chain of thought" {
		t.Fatalf("cached reasoning = %q, want chain of thought", value)
	}
}

func TestCacheStreamAccumulatorStoresToolCallReasoning(t *testing.T) {
	server := &Server{reasoningCache: newReasoningCache()}
	acc := newStreamReasoningAccumulator()
	acc.consumeOpenAIChunk(openAIStreamChunk{
		Reasoning:    "step1",
		FinishReason: "stop",
		ToolCalls: []any{
			map[string]any{"id": "tool_123"},
		},
	})

	server.cacheStreamAccumulator(acc)

	value, ok := server.reasoningCache.TryGet("toolcall:tool_123")
	if !ok {
		t.Fatalf("expected cached tool call reasoning to exist")
	}
	if value != "step1" {
		t.Fatalf("cached reasoning = %q, want step1", value)
	}
}
