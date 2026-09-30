package translate

import (
	"encoding/json"
	"strings"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// ResponsesTranslator ports createResponsesSseTranslator: named SSE events
// with a monotonically increasing sequence_number.
type ResponsesTranslator struct {
	ResponseID string
	Created    int64
	Model      string

	LastCcEvent       string
	UpstreamError     *cc.MappedError
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64

	sawFinish    bool
	finishReason string
	createdSent  bool

	seq         int
	outputIndex int
	current     *respItem
	out         SSEBuf
	doneItems   []map[string]any
	textAcc     strings.Builder
}

type respItem struct {
	kind    string // message | reasoning | function_call
	index   int
	item    map[string]any
	textBuf strings.Builder
}

func NewResponsesTranslator(model, responseID string, created int64) *ResponsesTranslator {
	return &ResponsesTranslator{ResponseID: responseID, Created: created, Model: model}
}

func (t *ResponsesTranslator) Take() SSEBuf {
	out := t.out
	t.out = t.out[:0]
	return out
}

func (t *ResponsesTranslator) sse(typ string, data map[string]any) {
	data["type"] = typ
	data["sequence_number"] = t.seq
	t.seq++
	t.out = append(t.out, "event: "...)
	t.out = append(t.out, typ...)
	t.out = append(t.out, '\n')
	t.out.Data(data)
}

func (t *ResponsesTranslator) baseResponse(status string, output []map[string]any) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	return map[string]any{
		"id": t.ResponseID, "object": "response", "created_at": t.Created,
		"status": status, "output": output, "output_text": "", "model": t.Model,
		"error": nil, "incomplete_details": nil,
		"parallel_tool_calls": true, "previous_response_id": nil,
		"store": false, "tools": []any{}, "metadata": map[string]any{},
	}
}

func (t *ResponsesTranslator) startResponse() {
	t.createdSent = true
	t.sse("response.created", map[string]any{"response": t.baseResponse("in_progress", nil)})
	t.sse("response.in_progress", map[string]any{"response": t.baseResponse("in_progress", nil)})
}

// Started reports whether the 200/SSE header must go out.
func (t *ResponsesTranslator) Started() bool { return t.createdSent }

func (t *ResponsesTranslator) closeItem() {
	if t.current == nil {
		return
	}
	cur := t.current
	switch cur.kind {
	case "message":
		text := cur.textBuf.String()
		t.sse("response.output_text.done", map[string]any{
			"item_id": cur.item["id"], "output_index": cur.index, "content_index": 0, "text": text, "logprobs": []any{},
		})
		t.sse("response.content_part.done", map[string]any{
			"item_id": cur.item["id"], "output_index": cur.index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		cur.item["content"] = []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}
		cur.item["status"] = "completed"
	case "function_call":
		t.sse("response.function_call_arguments.done", map[string]any{
			"item_id": cur.item["id"], "output_index": cur.index, "arguments": cur.item["arguments"],
		})
		cur.item["status"] = "completed"
	case "reasoning":
		text := cur.textBuf.String()
		t.sse("response.reasoning_summary_text.done", map[string]any{
			"item_id": cur.item["id"], "output_index": cur.index, "summary_index": 0, "text": text,
		})
		t.sse("response.reasoning_summary_part.done", map[string]any{
			"item_id": cur.item["id"], "output_index": cur.index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
		cur.item["summary"] = []map[string]any{{"type": "summary_text", "text": text}}
		cur.item["status"] = "completed"
	}
	t.sse("response.output_item.done", map[string]any{"output_index": cur.index, "item": cur.item})
	t.doneItems = append(t.doneItems, cur.item)
	t.current = nil
}

func (t *ResponsesTranslator) openItem(kind string, item map[string]any) {
	t.closeItem()
	t.current = &respItem{kind: kind, index: t.seq0Index(), item: item}
	t.sse("response.output_item.added", map[string]any{"output_index": t.current.index, "item": item})
	switch kind {
	case "message":
		t.sse("response.content_part.added", map[string]any{
			"item_id": item["id"], "output_index": t.current.index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	case "reasoning":
		t.sse("response.reasoning_summary_part.added", map[string]any{
			"item_id": item["id"], "output_index": t.current.index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
}

func (t *ResponsesTranslator) seq0Index() int {
	idx := t.outputIndex
	t.outputIndex++
	return idx
}

// ParseLine consumes one NDJSON line.
func (t *ResponsesTranslator) ParseLine(line string) {
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
		if !t.createdSent {
			t.startResponse()
		}
		if t.current == nil || t.current.kind != "message" {
			t.openItem("message", map[string]any{
				"type": "message", "id": cc.NewResponsesID("msg_"), "status": "in_progress",
				"role": "assistant", "content": []any{},
			})
		}
		t.current.textBuf.WriteString(text)
		t.textAcc.WriteString(text)
		t.sse("response.output_text.delta", map[string]any{
			"item_id": t.current.item["id"], "output_index": t.current.index,
			"content_index": 0, "delta": text, "logprobs": []any{},
		})

	case "reasoning-delta":
		if event.Text == "" {
			return
		}
		if !t.createdSent {
			t.startResponse()
		}
		if t.current == nil || t.current.kind != "reasoning" {
			t.openItem("reasoning", map[string]any{
				"type": "reasoning", "id": cc.NewResponsesID("rs_"), "summary": []any{}, "status": "in_progress",
			})
		}
		t.current.textBuf.WriteString(event.Text)
		t.sse("response.reasoning_summary_text.delta", map[string]any{
			"item_id": t.current.item["id"], "output_index": t.current.index,
			"summary_index": 0, "delta": event.Text,
		})

	case "tool-call":
		if !t.createdSent {
			t.startResponse()
		}
		callID := event.ToolCallID
		if callID == "" {
			callID = cc.NewResponsesID("call_")
		}
		args := event.ArgsOf()
		t.openItem("function_call", map[string]any{
			"type": "function_call", "id": cc.NewResponsesID("fc_"), "call_id": callID,
			"name": event.ToolName, "arguments": "", "status": "in_progress",
		})
		t.current.item["arguments"] = args
		t.sse("response.function_call_arguments.delta", map[string]any{
			"item_id": t.current.item["id"], "output_index": t.current.index, "delta": args,
		})

	case "finish":
		t.sawFinish = true
		if event.FinishReason != "" {
			t.finishReason = cc.MapFinishReason(event.FinishReason)
		}
		u := event.TotalUsage
		if u == nil {
			u = event.Usage
		}
		if u != nil {
			cc.NormalizeUsage(u)
			t.InputTokens = u.InputTokens
			t.OutputTokens = u.OutputTokens
			t.CachedInputTokens = u.CachedInputTokens
		}

	case "error":
		t.UpstreamError = cc.MapCcEventError(&event)

	default:
		// Responses translator is silent on everything else (reference default)
	}
}

func (t *ResponsesTranslator) IncompleteDetail() string {
	return cc.IncompleteDetail(t.sawFinish, t.finishReason)
}

// Finish emits the terminal event. Call once the upstream body is drained.
func (t *ResponsesTranslator) Finish() {
	if !t.createdSent {
		return
	}
	t.closeItem()
	if detail := cc.IncompleteDetail(t.sawFinish, t.finishReason); detail != "" {
		logx.Warn("Upstream stream incomplete", map[string]any{"path": "/v1/responses", "reason": detail})
		resp := t.baseResponse("failed", nil)
		resp["error"] = map[string]any{
			"code":    "upstream_error",
			"message": cc.IncompleteUpstreamError(detail).Body.Error.Message,
		}
		t.sse("response.failed", map[string]any{"response": resp})
		return
	}
	truncated := t.finishReason == "length"
	paused := t.finishReason == "pause_turn"
	status := "completed"
	if truncated || paused {
		status = "incomplete"
	}
	resp := t.baseResponse(status, t.doneItems)
	resp["output_text"] = t.textAcc.String()
	usage := BuildResponsesUsage(&cc.CCUsage{
		InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CachedInputTokens: t.CachedInputTokens,
	}, t.OutputTokens)
	if truncated {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	} else if paused {
		resp["incomplete_details"] = map[string]any{"reason": "pause_turn"}
	}
	resp["usage"] = usage
	if truncated || paused {
		t.sse("response.incomplete", map[string]any{"response": resp})
	} else {
		t.sse("response.completed", map[string]any{"response": resp})
	}
}

// Fail emits response.failed mid-stream (upstream error after 200 was sent).
func (t *ResponsesTranslator) Fail(message string) {
	if !t.createdSent {
		return
	}
	resp := t.baseResponse("failed", nil)
	resp["error"] = map[string]any{"code": "upstream_error", "message": message}
	t.sse("response.failed", map[string]any{"response": resp})
}

// ErrorEvent is the bare error SSE event (timeout path).
func (t *ResponsesTranslator) ErrorEvent(message string) SSEBuf {
	var buf SSEBuf
	buf.sse("error", map[string]any{"code": nil, "message": message, "param": nil})
	return buf
}

func (b *SSEBuf) sse(typ string, data map[string]any) {
	*b = append(*b, "event: "...)
	*b = append(*b, typ...)
	*b = append(*b, '\n', 'd', 'a', 't', 'a', ':', ' ')
	*b = appendJSON(*b, data)
	*b = append(*b, '\n', '\n')
}

// BuildResponsesUsage ports buildResponsesUsage: input_tokens is the TOTAL
// (cached/cache_write are subsets) — opposite of Anthropic's semantics.
func BuildResponsesUsage(u *cc.CCUsage, fallbackOutputTokens int64) map[string]any {
	cc.NormalizeUsage(u)
	if u == nil {
		u = &cc.CCUsage{}
	}
	inTok := u.InputTokens
	outTok := u.OutputTokens
	if outTok == 0 {
		outTok = fallbackOutputTokens
	}
	cacheWrite := int64(0)
	if u.InputTokenDetails != nil {
		cacheWrite = u.InputTokenDetails.CacheWriteTokens
	}
	return map[string]any{
		"input_tokens": inTok,
		"input_tokens_details": map[string]any{
			"cached_tokens": u.CachedInputTokens, "cache_write_tokens": cacheWrite,
		},
		"output_tokens":         outTok,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          inTok + outTok,
	}
}

// BuildResponsesOutput ports buildResponsesOutput.
func BuildResponsesOutput(fullText, thinkingText string, toolCalls []ChatTool) []map[string]any {
	output := []map[string]any{}
	if thinkingText != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": cc.NewResponsesID("rs_"),
			"summary": []map[string]any{{"type": "summary_text", "text": thinkingText}},
		})
	}
	if fullText != "" {
		output = append(output, map[string]any{
			"type": "message", "id": cc.NewResponsesID("msg_"), "status": "completed",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": fullText, "annotations": []any{}}},
		})
	}
	for _, tc := range toolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "id": cc.NewResponsesID("fc_"), "call_id": tc.ID,
			"name": tc.Function.Name, "arguments": tc.Function.Arguments, "status": "completed",
		})
	}
	return output
}

// BuildResponsesObject ports buildResponsesObject (non-streaming response).
func BuildResponsesObject(responseID, model string, created int64, fullText, thinkingText string, toolCalls []ChatTool, usage *cc.CCUsage, finishReason string, opts map[string]any) map[string]any {
	truncated := finishReason == "length"
	paused := finishReason == "pause_turn"
	status := "completed"
	incomplete := any(nil)
	if truncated {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	} else if paused {
		status = "incomplete"
		incomplete = map[string]any{"reason": "pause_turn"}
	}

	get := func(key string, def any) any {
		if v, ok := opts[key]; ok && v != nil {
			return v
		}
		return def
	}

	return map[string]any{
		"id": responseID, "object": "response", "created_at": created,
		"status": status, "completed_at": cc.NowUnix(), "error": nil,
		"incomplete_details":   incomplete,
		"input":                get("input", []any{}),
		"instructions":         opts["instructions"],
		"max_output_tokens":    opts["max_output_tokens"],
		"model":                model,
		"output":               BuildResponsesOutput(fullText, thinkingText, toolCalls),
		"output_text":          fullText,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            opts["reasoning"],
		"store":                false,
		"temperature":          get("temperature", 1),
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          get("tool_choice", "auto"),
		"tools":                get("tools", []any{}),
		"top_p":                get("top_p", 1),
		"truncation":           "disabled",
		"usage":                BuildResponsesUsage(usage, 0),
		"user":                 nil,
		"metadata":             map[string]any{},
	}
}
