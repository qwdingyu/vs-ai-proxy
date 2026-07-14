package proxy

import (
	"fmt"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type streamReasoningAccumulator struct {
	reasoning     strings.Builder
	toolCallIDs   []string
	toolCallNames []string
	hasContent    bool
	// hasRefusal 单独记录拒绝响应，避免 content 为空时把合法终态误判成空成功。
	hasRefusal   bool
	hasToolCalls bool
	finished     bool
	finishReason string
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
	if strings.TrimSpace(chunk.Content) != "" {
		a.hasContent = true
	}
	if strings.TrimSpace(chunk.Reasoning) != "" {
		a.reasoning.WriteString(chunk.Reasoning)
	}
	if strings.TrimSpace(chunk.Refusal) != "" {
		a.hasRefusal = true
	}
	if len(chunk.ToolCalls) > 0 {
		a.hasToolCalls = true
		for _, call := range chunk.ToolCalls {
			addToolCallID(a, call)
		}
	}
	if strings.TrimSpace(chunk.FinishReason) != "" {
		a.finished = true
		a.finishReason = strings.ToLower(strings.TrimSpace(chunk.FinishReason))
	}
}

func validateOpenAIStreamCompletion(acc *streamReasoningAccumulator, sanitizer *openAIStreamToolSanitizer) error {
	if acc == nil {
		return fmt.Errorf("OpenAI SSE 缺少响应状态")
	}
	sawDone := sanitizer != nil && sanitizer.sawDone
	if !acc.finished && !sawDone {
		return fmt.Errorf("OpenAI SSE 在 finish_reason 或 [DONE] 之前结束")
	}
	if !acc.hasResponsePayload() && !isOpenAITruncationFinishReason(acc.finishReason) {
		return fmt.Errorf("OpenAI SSE 没有文本、推理内容或工具调用")
	}
	return nil
}

func validateOllamaStreamCompletion(acc *streamReasoningAccumulator) error {
	if acc == nil {
		return fmt.Errorf("Ollama 流缺少响应状态")
	}
	if !acc.finished {
		return fmt.Errorf("Ollama 流在 done=true 或 [DONE] 之前结束")
	}
	if !acc.hasResponsePayload() && !isOpenAITruncationFinishReason(acc.finishReason) {
		return fmt.Errorf("Ollama 流没有文本、推理内容或工具调用")
	}
	return nil
}

func (a *streamReasoningAccumulator) hasResponsePayload() bool {
	if a == nil {
		return false
	}
	hasReasoning := strings.TrimSpace(a.reasoning.String()) != ""
	return a.hasContent || a.hasRefusal || hasReasoning || a.hasToolCalls
}

func (a *streamReasoningAccumulator) consumeOllamaChunk(chunk map[string]any) {
	if chunk == nil {
		return
	}

	if message, ok := chunk["message"].(map[string]any); ok && message != nil {
		if content, ok := message["content"].(string); ok && strings.TrimSpace(content) != "" {
			a.hasContent = true
		}
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
		a.finishReason, _ = chunk["done_reason"].(string)
		a.finishReason = strings.ToLower(strings.TrimSpace(a.finishReason))
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

	if function, ok := call["function"].(map[string]any); ok && function != nil {
		if name, ok := function["name"].(string); ok && strings.TrimSpace(name) != "" {
			a.toolCallNames = append(a.toolCallNames, strings.TrimSpace(name))
		}
	}
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
