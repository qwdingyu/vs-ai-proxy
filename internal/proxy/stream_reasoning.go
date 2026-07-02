package proxy

import (
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type streamReasoningAccumulator struct {
	reasoning    strings.Builder
	toolCallIDs  []string
	hasToolCalls bool
	finished     bool
}

func newStreamReasoningAccumulator() *streamReasoningAccumulator {
	return &streamReasoningAccumulator{}
}

func (a *streamReasoningAccumulator) consumeOpenAISSELine(line string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
		return
	}

	payload := strings.TrimSpace(line[5:])
	if payload == "" || payload == "[DONE]" {
		return
	}

	chunk, err := parseOpenAIStreamPayload(payload)
	if err != nil {
		return
	}
	a.consumeOpenAIChunk(chunk)
}

func (a *streamReasoningAccumulator) consumeOpenAIChunk(chunk openAIStreamChunk) {
	if strings.TrimSpace(chunk.Reasoning) != "" {
		a.reasoning.WriteString(chunk.Reasoning)
	}
	if len(chunk.ToolCalls) > 0 {
		a.hasToolCalls = true
		for _, call := range chunk.ToolCalls {
			addToolCallID(a, call)
		}
	}
	if strings.TrimSpace(chunk.FinishReason) != "" {
		a.finished = true
	}
}

func (a *streamReasoningAccumulator) consumeOllamaChunk(chunk map[string]any) {
	if chunk == nil {
		return
	}

	if message, ok := chunk["message"].(map[string]any); ok && message != nil {
		if reasoning, ok := message["thinking"].(string); ok && strings.TrimSpace(reasoning) != "" {
			a.reasoning.WriteString(reasoning)
		}
		if reasoning, ok := message["reasoning_content"].(string); ok && strings.TrimSpace(reasoning) != "" {
			a.reasoning.WriteString(reasoning)
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			a.hasToolCalls = true
			for _, call := range calls {
				addToolCallID(a, call)
			}
		}
	}

	if done, _ := chunk["done"].(bool); done {
		a.finished = true
	}
}

func addToolCallID(a *streamReasoningAccumulator, raw any) {
	call, ok := raw.(map[string]any)
	if !ok || call == nil {
		return
	}

	id, _ := call["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	for _, existing := range a.toolCallIDs {
		if existing == id {
			return
		}
	}
	a.toolCallIDs = append(a.toolCallIDs, id)
}

func (s *Server) cacheChatResponse(resp *provider.ChatResponse) {
	if s.reasoningCache == nil || resp == nil || len(resp.Choices) == 0 {
		return
	}
	s.reasoningCache.CacheMessage(resp.Choices[0].Message)
}

func (s *Server) cacheStreamAccumulator(acc *streamReasoningAccumulator) {
	if s.reasoningCache == nil || acc == nil || !acc.finished {
		return
	}

	reasoning := strings.TrimSpace(acc.reasoning.String())
	if reasoning == "" {
		return
	}

	key := ""
	if acc.hasToolCalls && len(acc.toolCallIDs) > 0 {
		key = "toolcall:" + strings.Join(acc.toolCallIDs, "|")
	}
	if key == "" {
		key = s.reasoningCache.NextAssistantKey()
	}
	s.reasoningCache.Set(key, reasoning)
}
