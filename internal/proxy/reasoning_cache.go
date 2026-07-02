package proxy

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// reasoningCache 维护多轮对话中的 reasoning_content。
// 这是一个进程内 best-effort cache，用于提升推理模型在多轮和工具调用场景下的兼容性。
type reasoningCache struct {
	values  sync.Map
	counter atomic.Int64
}

func newReasoningCache() *reasoningCache {
	return &reasoningCache{}
}

func (c *reasoningCache) TryGet(key string) (string, bool) {
	if strings.TrimSpace(key) == "" {
		return "", false
	}
	value, ok := c.values.Load(key)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", false
	}
	return text, true
}

func (c *reasoningCache) Set(key, value string) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	c.values.Store(key, value)
}

func (c *reasoningCache) NextAssistantKey() string {
	index := c.counter.Add(1) - 1
	return fmt.Sprintf("assistant:%d", index)
}

func (c *reasoningCache) CacheMessage(message provider.Message) {
	reasoning := strings.TrimSpace(message.Reasoning)
	if reasoning == "" {
		return
	}

	key := reasoningCacheKeyForMessage(message)
	if key == "" {
		key = c.NextAssistantKey()
	}
	c.Set(key, reasoning)
}

func reasoningCacheKeyForMessage(message provider.Message) string {
	if len(message.ToolCalls) == 0 {
		return ""
	}

	ids := make([]string, 0, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		id := strings.TrimSpace(call.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return "toolcall:" + strings.Join(ids, "|")
}
