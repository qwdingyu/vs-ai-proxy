package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const dsmlStreamProbeLimit = 8 * 1024

func probeOpenAIStreamForDSML(scanner *bufio.Scanner, allowedTools map[string]struct{}) ([]string, *provider.ChatResponse, bool, error) {
	buffered := []string{}
	if len(allowedTools) == 0 {
		return buffered, nil, false, nil
	}
	var raw bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if len(buffered) == 0 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		buffered = append(buffered, line)
		raw.WriteString(line)
		raw.WriteByte('\n')
		canonical := canonicalizeDSML(raw.String())
		if strings.Contains(canonical, "<｜DSML｜tool_calls>") {
			if err := drainOpenAIStreamProbe(scanner, &raw, &buffered); err != nil {
				return buffered, nil, true, err
			}
			resp, err := collectOpenAIStreamReader(bytes.NewReader(raw.Bytes()), "", allowedTools)
			if err != nil {
				return buffered, nil, true, err
			}
			if !chatResponseHasToolCalls(resp) {
				return buffered, nil, false, nil
			}
			return buffered, resp, true, nil
		}
		if raw.Len() >= dsmlStreamProbeLimit || isOpenAIStreamDoneLine(line) {
			return buffered, nil, false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return buffered, nil, false, err
	}
	return buffered, nil, false, nil
}

func drainOpenAIStreamProbe(scanner *bufio.Scanner, raw *bytes.Buffer, buffered *[]string) error {
	return drainOpenAIStreamProbeWithLimit(
		scanner,
		raw,
		buffered,
		maxAggregatedOpenAIStreamBytes,
	)
}

func drainOpenAIStreamProbeWithLimit(
	scanner *bufio.Scanner,
	raw *bytes.Buffer,
	buffered *[]string,
	maxBytes int64,
) error {
	for scanner.Scan() {
		line := scanner.Text()
		if int64(raw.Len()+len(line)+1) > maxBytes {
			return fmt.Errorf("%w: maximum %d bytes", errOpenAIStreamTooLarge, maxBytes)
		}
		*buffered = append(*buffered, line)
		raw.WriteString(line)
		raw.WriteByte('\n')
	}
	return scanner.Err()
}

func chatResponseHasToolCalls(resp *provider.ChatResponse) bool {
	if resp == nil {
		return false
	}
	for _, choice := range resp.Choices {
		if len(choice.Message.ToolCalls) > 0 || choice.Message.FunctionCall != nil {
			return true
		}
	}
	return false
}

func isOpenAIStreamDoneLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "data:") && strings.TrimSpace(trimmed[5:]) == "[DONE]"
}

// openAIStreamEventProcessor 按 SSE 事件而不是物理行处理 direct stream。
// 普通内容事件即时写出；一旦看到工具调用，只缓存工具尾部，终态校验通过后再输出，
// 避免 length/content_filter、错误事件或 EOF 残缺参数先暴露给客户端执行。
type openAIStreamEventProcessor struct {
	w         io.Writer
	flusher   interface{ Flush() }
	acc       *streamReasoningAccumulator
	sanitizer *openAIStreamToolSanitizer

	eventLines []string
	dataLines  []string
	eventType  string
	held       bytes.Buffer
	eventBytes int64
	maxBytes   int64
	started    bool
}

func newOpenAIStreamEventProcessor(
	w io.Writer,
	flusher interface{ Flush() },
	acc *streamReasoningAccumulator,
	sanitizer *openAIStreamToolSanitizer,
) *openAIStreamEventProcessor {
	return &openAIStreamEventProcessor{
		w:         w,
		flusher:   flusher,
		acc:       acc,
		sanitizer: sanitizer,
		maxBytes:  maxAggregatedOpenAIStreamBytes,
	}
}

func (p *openAIStreamEventProcessor) consumeLine(line string) error {
	if !p.started {
		line = strings.TrimPrefix(line, "\ufeff")
		p.started = true
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return p.flushEvent("\n\n")
	}

	isData := strings.HasPrefix(trimmed, "data:")
	if isData && len(p.dataLines) > 0 {
		pending := strings.TrimSpace(strings.Join(p.dataLines, "\n"))
		if pending == "[DONE]" || json.Valid([]byte(pending)) {
			if err := p.flushEvent("\n"); err != nil {
				return err
			}
		}
	}
	if strings.HasPrefix(trimmed, "event:") && len(p.eventLines) > 0 {
		if err := p.flushEvent("\n"); err != nil {
			return err
		}
	}
	if p.maxBytes <= 0 {
		p.maxBytes = maxAggregatedOpenAIStreamBytes
	}
	if p.eventBytes+int64(len(line)+1) > p.maxBytes {
		return fmt.Errorf("%w: maximum %d bytes", errOpenAIStreamTooLarge, p.maxBytes)
	}

	p.eventLines = append(p.eventLines, line)
	p.eventBytes += int64(len(line) + 1)
	switch {
	case isData:
		p.dataLines = append(p.dataLines, strings.TrimSpace(trimmed[5:]))
	case strings.HasPrefix(trimmed, "event:"):
		p.eventType = strings.TrimSpace(trimmed[6:])
	}
	return nil
}

func (p *openAIStreamEventProcessor) finish() error {
	if err := p.flushEvent("\n"); err != nil {
		return err
	}
	if p.sanitizer != nil {
		if err := p.sanitizer.validateFinal(); err != nil {
			return err
		}
	}
	if p.held.Len() == 0 {
		return nil
	}
	if _, err := p.w.Write(p.held.Bytes()); err != nil {
		return err
	}
	p.flusher.Flush()
	return nil
}

func (p *openAIStreamEventProcessor) flushPendingBeforeReadError() error {
	return p.flushEvent("\n")
}

func (p *openAIStreamEventProcessor) flushEvent(separator string) error {
	if len(p.eventLines) == 0 {
		return nil
	}
	defer p.resetEvent()

	if len(p.dataLines) == 0 {
		return p.writeEvent(strings.Join(p.eventLines, "\n") + separator)
	}
	payload := strings.TrimSpace(strings.Join(p.dataLines, "\n"))
	if strings.EqualFold(p.eventType, "error") {
		var detail any
		if json.Unmarshal([]byte(payload), &detail) == nil {
			encoded, _ := json.Marshal(detail)
			payload = string(encoded)
		}
		return fmt.Errorf("upstream SSE error: %s", sanitizeDiagnosticMessage(payload))
	}

	normalized := "data: " + payload
	if p.sanitizer != nil {
		normalized = normalizeOpenAIStreamLineForVisualStudioWithToolState(normalized, p.sanitizer)
		if p.sanitizer.err != nil {
			return p.sanitizer.err
		}
	}
	for _, line := range strings.Split(normalized, "\n") {
		p.acc.consumeOpenAISSELine(line)
	}

	var event strings.Builder
	for _, line := range p.eventLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			continue
		}
		event.WriteString(line)
		event.WriteByte('\n')
	}
	event.WriteString(normalized)
	event.WriteString(separator)
	return p.writeEvent(event.String())
}

func (p *openAIStreamEventProcessor) writeEvent(event string) error {
	if p.held.Len() > 0 || (p.sanitizer != nil && p.sanitizer.hasTrackedToolCalls()) {
		if int64(p.held.Len()+len(event)) > p.maxBytes {
			return fmt.Errorf("%w: maximum %d bytes", errOpenAIStreamTooLarge, p.maxBytes)
		}
		p.held.WriteString(event)
		return nil
	}
	if _, err := io.WriteString(p.w, event); err != nil {
		return err
	}
	p.flusher.Flush()
	return nil
}

func (p *openAIStreamEventProcessor) resetEvent() {
	p.eventLines = p.eventLines[:0]
	p.dataLines = p.dataLines[:0]
	p.eventType = ""
	p.eventBytes = 0
}

func fillMissingStreamResponseModel(resp *provider.ChatResponse, model string) {
	if resp == nil || strings.TrimSpace(resp.Model) != "" {
		return
	}
	resp.Model = model
	if strings.TrimSpace(resp.ID) == "" {
		resp.ID = "chatcmpl-dsml-stream"
	}
}
