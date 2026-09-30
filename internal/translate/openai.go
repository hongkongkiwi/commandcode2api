// Package translate holds the wire codecs for the three downstream protocols
// (OpenAI Chat, Anthropic Messages, OpenAI Responses) and the shared NDJSON
// event pump.
package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// SSEBuf accumulates SSE frames; handlers flush it to the client.
type SSEBuf []byte

func (b *SSEBuf) Data(obj any) {
	(*b) = append(*b, "data: "...)
	(*b) = appendJSON(*b, obj)
	*b = append(*b, '\n', '\n')
}

func (b *SSEBuf) Raw(s string) { *b = append(*b, s...) }

func appendJSON(dst []byte, obj any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return append(dst, "null"...)
	}
	return append(dst, bytes.TrimRight(buf.Bytes(), "\n")...)
}

// ChatDelta is the choices[0].delta of a chat chunk. Fields stay in a
// deterministic order via explicit struct encoding.
type ChatDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatDeltaTool `json:"tool_calls,omitempty"`
}

type ChatDeltaTool struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function ChatDeltaFn `json:"function"`
}

type ChatDeltaFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ChatChoice struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type ChatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

type ChatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type ChatMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatTool `json:"tool_calls,omitempty"`
}

type ChatTool struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function ChatToolFn `json:"function"`
}

type ChatToolFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAIUsage builds the usage object from CC usage (after normalization).
func OpenAIUsage(u *cc.CCUsage) *ChatUsage {
	cc.NormalizeUsage(u)
	if u == nil {
		u = &cc.CCUsage{}
	}
	return &ChatUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
		PromptTokensDetails: struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{CachedTokens: u.CachedInputTokens},
	}
}

// ChatTranslator is the port of createSseTranslator: CC NDJSON lines in,
// OpenAI SSE chunks out.
type ChatTranslator struct {
	ID      string
	Created int64
	Model   string

	LastCcEvent       string
	UpstreamError     *cc.MappedError
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64

	sawFinish       bool
	finishReason    string
	hasFinishReason bool
	chunkIndex      int
	toolCallIndex   int
	usage           *cc.CCUsage
	out             SSEBuf
}

func NewChatTranslator(model, completionID string, created int64) *ChatTranslator {
	return &ChatTranslator{ID: completionID, Created: created, Model: model, finishReason: ""}
}

func (t *ChatTranslator) Take() SSEBuf {
	out := t.out
	t.out = t.out[:0]
	return out
}

func (t *ChatTranslator) makeChunk(delta ChatDelta, finishReason *string, usage *ChatUsage) {
	chunk := ChatChunk{
		ID:      t.ID,
		Object:  "chat.completion.chunk",
		Created: t.Created,
		Model:   t.Model,
		Choices: []ChatChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
	chunk.Usage = usage
	t.out.Data(chunk)
}

// ParseLine consumes one NDJSON line, appending any SSE chunks to the
// internal buffer (retrieved with Take).
func (t *ChatTranslator) ParseLine(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "[DONE]" || strings.HasPrefix(trimmed, ":") {
		return
	}
	var event cc.CCEvent
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil || event.Type == "" {
		return
	}
	t.LastCcEvent = event.Type

	switch event.Type {
	case "text-start", "reasoning-start", "start", "start-step":
		// silent

	case "text-delta":
		text := event.TextOf()
		if text == "" {
			return
		}
		delta := ChatDelta{Content: &text}
		if t.chunkIndex == 0 {
			delta.Role = "assistant"
		}
		t.chunkIndex++
		t.makeChunk(delta, nil, nil)

	case "reasoning-delta":
		if event.Text == "" {
			return
		}
		delta := ChatDelta{ReasoningContent: event.Text}
		if t.chunkIndex == 0 {
			delta.Role = "assistant"
		}
		t.chunkIndex++
		t.makeChunk(delta, nil, nil)

	case "tool-call":
		id := event.ToolCallID
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", timeNowUnixMilli(), t.toolCallIndex)
		}
		tc := ChatDeltaTool{
			Index: t.toolCallIndex,
			ID:    id,
			Type:  "function",
			Function: ChatDeltaFn{
				Name:      event.ToolName,
				Arguments: event.ArgsOf(),
			},
		}
		delta := ChatDelta{ToolCalls: []ChatDeltaTool{tc}}
		if t.chunkIndex == 0 {
			delta.Role = "assistant"
			delta.Content = nil
		}
		t.chunkIndex++
		t.toolCallIndex++
		t.makeChunk(delta, nil, nil)

	case "finish-step":
		t.sawFinish = true
		if event.FinishReason != "" {
			t.finishReason = cc.MapFinishReason(event.FinishReason)
			t.hasFinishReason = true
		}
		if event.Usage != nil {
			t.usage = event.Usage
			t.InputTokens = event.Usage.InputTokens
			t.OutputTokens = event.Usage.OutputTokens
			t.CachedInputTokens = event.Usage.CachedInputTokens
		}

	case "finish":
		t.sawFinish = true
		fr := t.finishReason
		if !t.hasFinishReason || t.finishReason == "" {
			fr = cc.MapFinishReason(event.FinishReason)
			if fr == "" {
				fr = cc.MapFinishReason("stop")
			}
		}
		u := event.TotalUsage
		if u == nil {
			u = t.usage
		}
		if u == nil {
			u = &cc.CCUsage{}
		}
		cc.NormalizeUsage(u)
		t.InputTokens = u.InputTokens
		t.OutputTokens = u.OutputTokens
		t.CachedInputTokens = u.CachedInputTokens
		final := fr
		t.makeChunk(ChatDelta{}, &final, OpenAIUsage(u))

	case "error":
		t.UpstreamError = cc.MapCcEventError(&event)
		logx.Warn("CC stream error", map[string]any{
			"message":           eventErrorMessage(&event),
			"upstreamStatus":    t.UpstreamError.ReportedStatus,
			"upstreamRetryable": eventRetryable(&event),
			"code":              t.UpstreamError.Code,
			"mappedTo":          t.UpstreamError.Status,
		})
		// No finish_reason chunk — natural termination handles it, so
		// downstream agent loops don't stop at a premature finish_reason.

	case "reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
		"tool-input-end", "tool-error", "text-end":
		// silent

	default:
		logx.Warn("Unknown CC event type", map[string]any{"type": event.Type})
	}
}

func (t *ChatTranslator) IncompleteDetail() string {
	return cc.IncompleteDetail(t.sawFinish, t.finishReason)
}

func (t *ChatTranslator) DoneEvent() string { return "data: [DONE]\n\n" }

func eventErrorMessage(e *cc.CCEvent) string {
	if e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return e.Message
}

func eventRetryable(e *cc.CCEvent) bool {
	if e.Error != nil && e.Error.IsRetryable != nil {
		return *e.Error.IsRetryable
	}
	return false
}

func timeNowUnixMilli() int64 {
	return time.Now().UnixMilli()
}

// ChatAggregate is the non-streaming accumulator (port of the non-stream
// branch of handleChatCompletions).
type ChatAggregate struct {
	FullText         string
	ReasoningContent string
	FinishReason     string
	SawFinish        bool
	Usage            *cc.CCUsage
	ToolCalls        []ChatTool
	UpstreamError    *cc.MappedError
	LastCcEvent      string
}

func NewChatAggregate() *ChatAggregate {
	return &ChatAggregate{FinishReason: "stop"}
}

// ProcessLine consumes one NDJSON line into the aggregate.
func (a *ChatAggregate) ProcessLine(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "[DONE]" || strings.HasPrefix(trimmed, ":") {
		return
	}
	var event cc.CCEvent
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return
	}
	switch event.Type {
	case "text-delta":
		a.LastCcEvent = event.Type
		a.FullText += event.TextOf()
	case "reasoning-delta":
		a.LastCcEvent = event.Type
		a.ReasoningContent += event.Text
	case "tool-call":
		a.LastCcEvent = event.Type
		id := event.ToolCallID
		if id == "" {
			id = "call_" + cc.NewUUID()[:8]
		}
		a.ToolCalls = append(a.ToolCalls, ChatTool{
			ID:   id,
			Type: "function",
			Function: ChatToolFn{
				Name:      event.ToolName,
				Arguments: event.ArgsOf(),
			},
		})
	case "finish-step", "finish":
		a.LastCcEvent = event.Type
		a.SawFinish = true
		a.FinishReason = cc.MapFinishReason(event.FinishReason)
		if event.TotalUsage != nil {
			a.Usage = event.TotalUsage
		}
	case "error":
		a.LastCcEvent = event.Type
		a.UpstreamError = cc.MapCcEventError(&event)
		logx.Warn("CC stream error (non-stream)", map[string]any{
			"message":           eventErrorMessage(&event),
			"upstreamStatus":    a.UpstreamError.ReportedStatus,
			"upstreamRetryable": eventRetryable(&event),
			"code":              a.UpstreamError.Code,
			"mappedTo":          a.UpstreamError.Status,
		})
	case "text-start", "text-end", "start", "start-step",
		"reasoning-start", "reasoning-end",
		"provider-metadata", "tool-input-start", "tool-input-delta", "tool-input-end",
		"tool-error":
		// silent — same list as the streaming translator
	default:
		logx.Warn("Unknown CC event type", map[string]any{"type": event.Type})
	}
}
