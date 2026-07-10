package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const (
	dsmlStreamProbeLimit = 8 * 1024
	// SSE 通常每个事件包含 data 行和空行。最多探测两个事件，避免为了兼容
	// provider-specific DSML 而显著增加普通文本流的首 token 延迟。
	dsmlStreamProbeLines = 4
)

func probeOpenAIStreamForDSML(scanner *bufio.Scanner, allowedTools map[string]struct{}) ([]string, *provider.ChatResponse, bool, error) {
	buffered := []string{}
	if len(allowedTools) == 0 {
		return buffered, nil, false, nil
	}
	var raw bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		buffered = append(buffered, line)
		raw.WriteString(line)
		raw.WriteByte('\n')
		canonical := canonicalizeDSML(raw.String())
		if strings.Contains(canonical, "<｜DSML｜tool_calls>") {
			for scanner.Scan() {
				line = scanner.Text()
				buffered = append(buffered, line)
				raw.WriteString(line)
				raw.WriteByte('\n')
			}
			if err := scanner.Err(); err != nil {
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
		if raw.Len() >= dsmlStreamProbeLimit || len(buffered) >= dsmlStreamProbeLines || isOpenAIStreamDoneLine(line) {
			return buffered, nil, false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return buffered, nil, false, err
	}
	return buffered, nil, false, nil
}

func chatResponseHasToolCalls(resp *provider.ChatResponse) bool {
	if resp == nil {
		return false
	}
	for _, choice := range resp.Choices {
		if len(choice.Message.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func isOpenAIStreamDoneLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "data:") && strings.TrimSpace(trimmed[5:]) == "[DONE]"
}

func writeBufferedOpenAIStreamLines(w io.Writer, flusher interface{ Flush() }, lines []string, acc *streamReasoningAccumulator) error {
	return writeBufferedOpenAIStreamLinesWithTools(w, flusher, lines, acc, nil)
}

func writeBufferedOpenAIStreamLinesWithTools(w io.Writer, flusher interface{ Flush() }, lines []string, acc *streamReasoningAccumulator, allowedTools map[string]struct{}) error {
	for _, line := range lines {
		line = normalizeOpenAIStreamLineForVisualStudioWithTools(line, allowedTools)
		acc.consumeOpenAISSELine(line)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		flusher.Flush()
	}
	return nil
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
