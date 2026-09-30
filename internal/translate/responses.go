package translate

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/ir"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// Responses API codec (port of the /v1/responses section of proxy.mjs).

type ResponsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"` // string | items
	Instructions      json.RawMessage `json:"instructions"`
	MaxOutputTokens   *int            `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Tools             json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Reasoning         *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	PreviousResponseID string `json:"previous_response_id"`
}

// responsesTextOf flattens string | [{text}] content.
func responsesTextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// responsesReasoningOf: summary[].text || content[].text || item.text.
func responsesReasoningOf(item map[string]any) string {
	if s, ok := item["summary"].([]any); ok && len(s) > 0 {
		return joinTextParts(s)
	}
	if s, ok := item["content"].([]any); ok && len(s) > 0 {
		return joinTextParts(s)
	}
	if s, ok := item["text"].(string); ok {
		return s
	}
	return ""
}

func joinTextParts(parts []any) string {
	var b strings.Builder
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// ConvertResponsesToIR ports convertResponsesToChat. The pending-assistant
// accumulation merges reasoning/message/function_call items that Responses
// keeps as siblings but Chat attaches to one assistant message.
func ConvertResponsesToIR(req *ResponsesRequest) (*ir.ChatRequest, error) {
	out := &ir.ChatRequest{Model: req.Model}

	if len(req.Instructions) > 0 {
		if sys := responsesTextOf(req.Instructions); sys != "" {
			out.Messages = append(out.Messages, ir.Message{Role: "system", Content: ir.StringContent(sys)})
		}
	}

	type pendingAssistant struct {
		hasContent bool
		content    string
		reasoning  string
		toolCalls  []ir.ToolCall
	}
	var pending *pendingAssistant
	ensure := func() *pendingAssistant {
		if pending == nil {
			pending = &pendingAssistant{}
		}
		return pending
	}
	flush := func() {
		if pending == nil {
			return
		}
		if !pending.hasContent && len(pending.toolCalls) == 0 && pending.reasoning == "" {
			pending = nil
			return
		}
		m := ir.Message{Role: "assistant"}
		if pending.hasContent {
			m.Content = ir.StringContent(pending.content)
		} else {
			m.Content = ir.Content{IsNull: true}
		}
		if pending.reasoning != "" {
			m.ReasoningContent = pending.reasoning
		}
		if len(pending.toolCalls) > 0 {
			m.ToolCalls = pending.toolCalls
		}
		out.Messages = append(out.Messages, m)
		pending = nil
	}

	input := strings.TrimSpace(string(req.Input))
	switch {
	case strings.HasPrefix(input, `"`):
		var s string
		if json.Unmarshal(req.Input, &s) == nil {
			out.Messages = append(out.Messages, ir.Message{Role: "user", Content: ir.StringContent(s)})
		}
	case strings.HasPrefix(input, "["):
		var items []map[string]any
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, err
		}
		rawItems := []json.RawMessage{}
		if err := json.Unmarshal(req.Input, &rawItems); err != nil {
			return nil, err
		}
		for i, item := range items {
			typ, _ := item["type"].(string)
			if typ == "" {
				if _, hasRole := item["role"]; hasRole {
					typ = "message" // EasyInputMessage omits type
				} else {
					logx.Warn("Unknown Responses input item type", map[string]any{"index": i})
					continue
				}
			}
			switch typ {
			case "reasoning":
				if t := responsesReasoningOf(item); t != "" {
					ensure().reasoning = t
				}
			case "message":
				content, _ := item["content"]
				cb, _ := json.Marshal(content)
				text := responsesTextOf(cb)
				role, _ := item["role"].(string)
				switch role {
				case "assistant":
					if text != "" {
						p := ensure()
						p.content = text
						p.hasContent = true
					}
				case "system", "developer":
					flush()
					out.Messages = append(out.Messages, ir.Message{Role: "system", Content: ir.StringContent(text)})
				default:
					flush()
					out.Messages = append(out.Messages, ir.Message{Role: "user", Content: ir.StringContent(text)})
				}
			case "function_call":
				callID, _ := item["call_id"].(string)
				if callID == "" {
					callID, _ = item["id"].(string)
				}
				if callID == "" {
					callID = "call_" + cc.NewUUID()[:8]
				}
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				if args == "" {
					args = "{}"
				}
				ensure().toolCalls = append(ensure().toolCalls, ir.ToolCall{
					ID:   callID,
					Type: "function",
					Function: ir.FunctionCall{
						Name:      name,
						Arguments: json.RawMessage(jsonString(args)),
					},
				})
			case "function_call_output":
				flush()
				callID, _ := item["call_id"].(string)
				output, hasOutput := item["output"]
				outText := ""
				if hasOutput {
					if s, ok := output.(string); ok {
						outText = s
					} else if output != nil {
						ob, _ := json.Marshal(output)
						outText = string(ob)
					}
				}
				out.Messages = append(out.Messages, ir.Message{
					Role:       "tool",
					ToolCallID: callID,
					Content:    ir.StringContent(outText),
				})
			default:
				logx.Warn("Unknown Responses input item type", map[string]any{"type": typ})
			}
		}
	case input == "" || input == "null":
		// handled by the empty check in the handler
	default:
		// opaque shape → user message with raw JSON (reference falls through
		// to content stringification only for objects; be conservative)
		return nil, errInvalidInput
	}
	flush()

	// tools.
	var tools []map[string]any
	if len(req.Tools) > 0 {
		json.Unmarshal(req.Tools, &tools)
	}
	for _, t := range tools {
		typ, _ := t["type"].(string)
		if typ != "function" {
			if _, hasName := t["name"]; !hasName {
				continue
			}
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		params, hasParams := t["parameters"]
		if !hasParams || params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		pb, _ := json.Marshal(params)
		if typ == "" {
			typ = "function"
		}
		out.Tools = append(out.Tools, ir.Tool{
			Type: typ,
			Function: &ir.FunctionDef{
				Name:        name,
				Description: desc,
				Parameters:  pb,
			},
		})
	}

	// tool_choice.
	if len(req.ToolChoice) > 0 {
		if s, ok := ir.ToolChoiceString(req.ToolChoice); ok {
			out.ToolChoice = json.RawMessage(jsonString(s))
		} else {
			var tc map[string]any
			if json.Unmarshal(req.ToolChoice, &tc) == nil {
				if name, ok := tc["name"].(string); ok && name != "" {
					out.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":` + jsonString(name) + `}}`)
				}
			}
		}
	}

	out.Stream = req.Stream
	out.MaxTokens = req.MaxOutputTokens
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	out.ParallelToolCalls = req.ParallelToolCalls
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		eff := req.Reasoning.Effort
		out.ReasoningEffort = &eff
	}
	return out, nil
}

var errInvalidInput = errors.New("input is required")
