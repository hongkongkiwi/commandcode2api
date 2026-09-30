// Package ir defines the provider-neutral internal request format.
//
// It is the Go equivalent of the plain "openaiReq" object the reference
// proxy.mjs passes between protocol converters and buildCcRequest: an
// OpenAI-Chat-shaped request that /v1/messages and /v1/responses both
// normalize into before hitting the CC envelope builder.
package ir

import (
	"bytes"
	"encoding/json"
	"strconv"
)

type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	ReasoningEffort   *string         `json:"reasoning_effort,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"` // "auto" | "none" | "required" | {type:"function",function:{name}}
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	User              string          `json:"user,omitempty"`
}

// Message content is polymorphic: string | []Part | absent.
type Message struct {
	Role             string     `json:"role"`
	Content          Content    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

type Content struct {
	Str    *string
	Parts  []Part
	IsNull bool // explicit null vs absent
}

func StringContent(s string) Content { return Content{Str: &s} }

func (c Content) IsString() bool { return c.Str != nil }

// String returns the flattened text (string content, or "" for parts).
func (c Content) String() string {
	if c.Str != nil {
		return *c.Str
	}
	return ""
}

func (c *Content) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.IsNull = len(trimmed) > 0
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		c.Str = &s
		return nil
	}
	var parts []Part
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return err
	}
	c.Parts = parts
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.Parts == nil {
		if c.IsNull {
			return []byte("null"), nil
		}
		return []byte("null"), nil
	}
	return json.Marshal(c.Parts)
}

// Part is one content block. Known fields are decoded for the converters;
// Raw preserves the original bytes for upstream passthrough (the reference
// forwards text/reasoning parts verbatim, extra keys included).
type Part struct {
	Type         string
	Text         string
	ImageURL     *ImageURL
	CacheControl json.RawMessage
	Raw          json.RawMessage
}

type ImageURL struct {
	URL string `json:"url"`
}

func (p *Part) UnmarshalJSON(b []byte) error {
	p.Raw = append(p.Raw[:0:0], b...)
	var probe struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		Content      json.RawMessage `json:"content"`
		ImageURL     *ImageURL       `json:"image_url"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	p.Type, p.ImageURL, p.CacheControl = probe.Type, probe.ImageURL, probe.CacheControl
	p.Text = probe.Text
	// c?.text ?? c?.content fallback used when extracting system blocks.
	if p.Text == "" && len(probe.Content) > 0 && probe.Content[0] == '"' {
		var s string
		if json.Unmarshal(probe.Content, &s) == nil {
			p.Text = s
		}
	}
	return nil
}

func (p Part) MarshalJSON() ([]byte, error) {
	if p.Raw != nil {
		return p.Raw, nil
	}
	// Programmatic part (built by a converter) — emit canonical fields.
	var buf bytes.Buffer
	buf.WriteString(`{"type":`)
	buf.WriteString(strconv.Quote(p.Type))
	if p.Text != "" {
		buf.WriteString(`,"text":`)
		b, _ := json.Marshal(p.Text)
		buf.Write(b)
	}
	if p.ImageURL != nil {
		buf.WriteString(`,"image_url":`)
		b, _ := json.Marshal(p.ImageURL)
		buf.Write(b)
	}
	if len(p.CacheControl) > 0 {
		buf.WriteString(`,"cache_control":`)
		buf.Write(p.CacheControl)
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// ToolCall on an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // string or object
}

// StringArguments renders arguments the way the reference reads them
// (tc.function?.arguments as string) — object form is re-marshaled compact.
func (f FunctionCall) StringArguments() string {
	raw := f.Arguments
	if len(raw) == 0 {
		return "{}"
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return string(raw)
}

// ObjectArguments parses string-encoded arguments into a JSON value,
// returning {} on parse failure (port of tryParseJSON).
func (f FunctionCall) ObjectArguments() json.RawMessage {
	raw := f.Arguments
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			return json.RawMessage("{}")
		}
		trimmed := bytes.TrimSpace([]byte(s))
		if len(trimmed) == 0 {
			return json.RawMessage("{}")
		}
		var check json.RawMessage
		if json.Unmarshal(trimmed, &check) != nil {
			return json.RawMessage("{}")
		}
		return check
	}
	return raw
}

// Tool definition — accepts OpenAI function style and CC/Anthropic
// {name, description, input_schema} style, like the reference does.
type Tool struct {
	Type        string          `json:"type,omitempty"`
	Function    *FunctionDef    `json:"function,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// WireName mirrors toWireToolName(t.function?.name || t.name || ”).
func (t Tool) WireName() string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

func (t Tool) WireDescription() string {
	if t.Function != nil {
		return t.Function.Description
	}
	return t.Description
}

func (t Tool) WireParameters() json.RawMessage {
	if t.Function != nil && len(t.Function.Parameters) > 0 {
		return t.Function.Parameters
	}
	if len(t.InputSchema) > 0 {
		return t.InputSchema
	}
	if len(t.Parameters) > 0 {
		return t.Parameters
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// ToolChoice helpers.
func ToolChoiceString(raw json.RawMessage) (string, bool) {
	s := bytes.TrimSpace(raw)
	if len(s) > 0 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(s, &v); err == nil {
			return v, true
		}
	}
	return "", false
}

type toolChoiceObj struct {
	Type     string `json:"type"`
	Function *struct {
		Name string `json:"name"`
	} `json:"function"`
	Name string `json:"name"`
}

// ToolChoiceFunction returns the named-tool form {type:"function",function:{name}}.
func ToolChoiceFunction(raw json.RawMessage) (string, bool) {
	var o toolChoiceObj
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", false
	}
	if o.Function != nil && o.Function.Name != "" {
		return o.Function.Name, true
	}
	if o.Type == "function" && o.Name != "" {
		return o.Name, true
	}
	return "", false
}
