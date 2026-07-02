package provider

import "encoding/json"

var chatRequestKnownFields = map[string]struct{}{
	"model":            {},
	"messages":         {},
	"temperature":      {},
	"top_p":            {},
	"top_k":            {},
	"max_tokens":       {},
	"reasoning_effort": {},
	"stream":           {},
	"tools":            {},
	"stop":             {},
}

var messageKnownFields = map[string]struct{}{
	"role":              {},
	"content":           {},
	"tool_calls":        {},
	"tool_call_id":      {},
	"reasoning_content": {},
}

var toolCallKnownFields = map[string]struct{}{
	"id":       {},
	"type":     {},
	"function": {},
}

var functionCallKnownFields = map[string]struct{}{
	"name":      {},
	"arguments": {},
}

var toolKnownFields = map[string]struct{}{
	"type":     {},
	"function": {},
}

var toolFuncKnownFields = map[string]struct{}{
	"name":        {},
	"description": {},
	"parameters":  {},
}

var ollamaOptionKnownFields = map[string]struct{}{
	"temperature":      {},
	"top_p":            {},
	"top_k":            {},
	"max_tokens":       {},
	"reasoning_effort": {},
}

type chatRequestAlias ChatRequest
type messageAlias Message
type toolCallAlias ToolCall
type functionCallAlias FunctionCall
type toolAlias Tool
type toolFuncAlias ToolFunc

// UnmarshalJSON keeps provider-specific top-level request fields intact.
// Visual Studio and aggregator providers may send extension fields this proxy
// does not model yet; dropping them would make the proxy less transparent.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	type alias chatRequestAlias
	var req alias
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for field := range chatRequestKnownFields {
		delete(raw, field)
	}

	*r = ChatRequest(req)
	if len(raw) > 0 {
		r.Extra = raw
	} else {
		r.Extra = map[string]json.RawMessage{}
	}
	return nil
}

func (r ChatRequest) MarshalJSON() ([]byte, error) {
	type alias chatRequestAlias
	known, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(known, &out); err != nil {
		return nil, err
	}
	for key, value := range r.Extra {
		if _, exists := chatRequestKnownFields[key]; exists || len(value) == 0 {
			continue
		}
		out[key] = value
	}
	return json.Marshal(out)
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var msg struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		Reasoning  string          `json:"reasoning_content,omitempty"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for field := range messageKnownFields {
		delete(raw, field)
	}

	*m = Message{
		Role:       msg.Role,
		ToolCalls:  msg.ToolCalls,
		ToolCallID: msg.ToolCallID,
		Reasoning:  msg.Reasoning,
	}
	if len(msg.Content) > 0 {
		var content string
		if err := json.Unmarshal(msg.Content, &content); err == nil {
			m.Content = content
		} else {
			m.ContentRaw = append(json.RawMessage(nil), msg.Content...)
		}
	}
	if len(raw) > 0 {
		m.Extra = raw
	} else {
		m.Extra = map[string]json.RawMessage{}
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	type alias messageAlias
	known, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(known, &out); err != nil {
		return nil, err
	}
	if len(m.ContentRaw) > 0 && m.Content == "" {
		out["content"] = append(json.RawMessage(nil), m.ContentRaw...)
	}
	for key, value := range m.Extra {
		if _, exists := messageKnownFields[key]; exists || len(value) == 0 {
			continue
		}
		out[key] = value
	}
	return json.Marshal(out)
}

func (t *ToolCall) UnmarshalJSON(data []byte) error {
	type alias toolCallAlias
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	raw, err := rawExtra(data, toolCallKnownFields)
	if err != nil {
		return err
	}

	*t = ToolCall(value)
	t.Extra = raw
	return nil
}

func (t ToolCall) MarshalJSON() ([]byte, error) {
	type alias toolCallAlias
	return marshalWithExtra(alias(t), t.Extra, toolCallKnownFields)
}

func (f *FunctionCall) UnmarshalJSON(data []byte) error {
	type alias functionCallAlias
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	raw, err := rawExtra(data, functionCallKnownFields)
	if err != nil {
		return err
	}

	*f = FunctionCall(value)
	f.Extra = raw
	return nil
}

func (f FunctionCall) MarshalJSON() ([]byte, error) {
	type alias functionCallAlias
	return marshalWithExtra(alias(f), f.Extra, functionCallKnownFields)
}

func (t *Tool) UnmarshalJSON(data []byte) error {
	type alias toolAlias
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	raw, err := rawExtra(data, toolKnownFields)
	if err != nil {
		return err
	}

	*t = Tool(value)
	t.Extra = raw
	return nil
}

func (t Tool) MarshalJSON() ([]byte, error) {
	type alias toolAlias
	return marshalWithExtra(alias(t), t.Extra, toolKnownFields)
}

func (f *ToolFunc) UnmarshalJSON(data []byte) error {
	type alias toolFuncAlias
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	raw, err := rawExtra(data, toolFuncKnownFields)
	if err != nil {
		return err
	}

	*f = ToolFunc(value)
	f.Extra = raw
	return nil
}

func (f ToolFunc) MarshalJSON() ([]byte, error) {
	type alias toolFuncAlias
	return marshalWithExtra(alias(f), f.Extra, toolFuncKnownFields)
}

func rawExtra(data []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for field := range known {
		delete(raw, field)
	}
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	return raw, nil
}

func marshalWithExtra(value any, extra map[string]json.RawMessage, knownFields map[string]struct{}) ([]byte, error) {
	known, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(known, &out); err != nil {
		return nil, err
	}
	for key, raw := range extra {
		if _, exists := knownFields[key]; exists || len(raw) == 0 {
			continue
		}
		out[key] = raw
	}
	return json.Marshal(out)
}
