package converter

import (
	"encoding/json"
	"testing"
)

func TestParseOllamaStreamChunkAcceptsNativeNDJSON(t *testing.T) {
	chunk, err := ParseOllamaStreamChunk(`{"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}`)
	if err != nil {
		t.Fatalf("ParseOllamaStreamChunk returned error: %v", err)
	}
	if chunk["model"] != "llama" {
		t.Fatalf("unexpected model: %#v", chunk["model"])
	}
}

func TestParseOllamaStreamChunkAcceptsSSEDataLine(t *testing.T) {
	chunk, err := ParseOllamaStreamChunk(`data: {"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}`)
	if err != nil {
		t.Fatalf("ParseOllamaStreamChunk returned error: %v", err)
	}
	if chunk["model"] != "llama" {
		t.Fatalf("unexpected model: %#v", chunk["model"])
	}
}

func TestOllamaChatResponse2OpenAIReadsNestedMessageAndThinking(t *testing.T) {
	out, err := OllamaChatResponse2OpenAI([]byte(`{
		"model":"llama",
		"message":{"role":"assistant","content":"answer","thinking":"reason"},
		"done":true
	}`), "llama")
	if err != nil {
		t.Fatalf("OllamaChatResponse2OpenAI returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "answer" {
		t.Fatalf("content = %#v, want answer", message["content"])
	}
	if message["reasoning_content"] != "reason" {
		t.Fatalf("reasoning_content = %#v, want reason", message["reasoning_content"])
	}
}
