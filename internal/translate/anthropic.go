package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/ir"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// Anthropic → OpenAI-IR conversion (port of convertAnthropicToOpenAI).

type AnthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     *int                 `json:"max_tokens"`
	Messages      []AnthropicMsg       `json:"messages"`
	System        json.RawMessage      `json:"system"` // string | [{type:text,text,cache_control}]
	Stream        bool                 `json:"stream"`
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	StopSequences []string             `json:"stop_sequences"`
	Tools         []AnthropicTool      `json:"tools"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice"`
	Thinking      *AnthropicThinking   `json:"thinking"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | blocks
}

type AnthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string | blocks
	// image
	Source *AnthropicImageSource `json:"source"`
	// passthrough
	CacheControl json.RawMessage `json:"cache_control"`
}

type AnthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name"`
}

type AnthropicThinking struct {
	Type         string `json:"type"` // enabled | disabled | adaptive | none
	BudgetTokens *int   `json:"budget_tokens"`
	Effort       string `json:"effort"`
}

// decodeBlocks accepts string or block-array content, returning blocks.
func decodeBlocks(raw json.RawMessage) []AnthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []AnthropicBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []AnthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

// ConvertAnthropicToIR ports convertAnthropicToOpenAI.
func ConvertAnthropicToIR(req *AnthropicRequest) *ir.ChatRequest {
	out := &ir.ChatRequest{Model: req.Model}
	if out.Model == "" {
		out.Model = "deepseek/deepseek-v4-flash"
	}

	// 1. system: top-level string or block array (cache_control preserved).
	var systemBlocks []ir.Part
	if len(req.System) > 0 {
		blocks := decodeBlocks(req.System)
		if len(blocks) == 1 && blocks[0].Type == "text" && len(blocks[0].CacheControl) == 0 {
			// plain string case
			out.Messages = append(out.Messages, ir.Message{Role: "system", Content: ir.StringContent(blocks[0].Text)})
		} else if len(blocks) > 0 {
			for _, b := range blocks {
				if b.Type != "text" {
					continue
				}
				systemBlocks = append(systemBlocks, ir.Part{Type: "text", Text: b.Text, CacheControl: b.CacheControl, Raw: rawBlock(b)})
			}
			out.Messages = append(out.Messages, ir.Message{Role: "system", Content: ir.Content{Parts: systemBlocks}})
		}
	}

	// 2. messages.
	toolNameFromID := map[string]string{}
	for _, msg := range req.Messages {
		blocks := decodeBlocks(msg.Content)
		if msg.Role == "assistant" {
			var textParts []ir.Part
			textHasCache := false
			textContent := ""
			thinkingContent := ""
			var toolCalls []ir.ToolCall
			for _, block := range blocks {
				switch block.Type {
				case "text":
					textContent += block.Text
					part := ir.Part{Type: "text", Text: block.Text, CacheControl: block.CacheControl, Raw: rawBlock(block)}
					if len(block.CacheControl) > 0 {
						textHasCache = true
					}
					textParts = append(textParts, part)
				case "thinking":
					thinkingContent += block.Thinking
				case "tool_use":
					toolNameFromID[block.ID] = block.Name
					toolCalls = append(toolCalls, ir.ToolCall{
						ID:   block.ID,
						Type: "function",
						Function: ir.FunctionCall{
							Name:      block.Name,
							Arguments: mustJSONBytes(block.Input),
						},
					})
				}
			}
			m := ir.Message{Role: "assistant"}
			if len(textParts) > 1 || textHasCache {
				m.Content = ir.Content{Parts: textParts}
			} else if textContent != "" {
				m.Content = ir.StringContent(textContent)
			} else {
				m.Content = ir.Content{IsNull: true}
			}
			if thinkingContent != "" {
				m.ReasoningContent = thinkingContent
			}
			m.ToolCalls = toolCalls
			out.Messages = append(out.Messages, m)
		} else if msg.Role == "user" {
			var parts []ir.Part
			partsHaveCache := false
			textContent := ""
			var toolResults []AnthropicBlock
			for _, block := range blocks {
				switch block.Type {
				case "text":
					textContent += block.Text
					part := ir.Part{Type: "text", Text: block.Text, CacheControl: block.CacheControl, Raw: rawBlock(block)}
					if len(block.CacheControl) > 0 {
						partsHaveCache = true
					}
					parts = append(parts, part)
				case "image":
					if block.Source == nil {
						continue
					}
					url := ""
					if block.Source.Type == "base64" && block.Source.Data != "" {
						mt := block.Source.MediaType
						if mt == "" {
							mt = "image/png"
						}
						url = "data:" + mt + ";base64," + block.Source.Data
					} else if block.Source.URL != "" {
						url = block.Source.URL
					}
					if url != "" {
						parts = append(parts, ir.Part{
							Type:     "image_url",
							ImageURL: &ir.ImageURL{URL: url},
							Raw:      json.RawMessage(`{"type":"image_url","image_url":{"url":` + jsonString(url) + `}}`),
						})
					}
				case "tool_result":
					toolResults = append(toolResults, block)
				}
			}
			// tool results enqueue first: OpenAI semantics require the tool
			// message right after the assistant tool_calls (issue #15: don't
			// fabricate empty names when history was trimmed).
			for _, tr := range toolResults {
				toolMsg := ir.Message{Role: "tool", ToolCallID: tr.ToolUseID}
				if name, ok := toolNameFromID[tr.ToolUseID]; ok {
					toolMsg.Name = name
				}
				toolMsg.Content = ir.StringContent(anthropicToolResultText(tr))
				out.Messages = append(out.Messages, toolMsg)
			}
			if len(parts) > 0 || textContent != "" {
				singleText := len(parts) <= 1 && (len(parts) == 0 || parts[0].Type == "text") && !partsHaveCache
				if singleText {
					out.Messages = append(out.Messages, ir.Message{Role: "user", Content: ir.StringContent(textContent)})
				} else {
					out.Messages = append(out.Messages, ir.Message{Role: "user", Content: ir.Content{Parts: parts}})
				}
			}
		}
	}

	// 3. base params.
	sixtyFourK := 64000
	out.MaxTokens = req.MaxTokens
	if out.MaxTokens == nil {
		out.MaxTokens = &sixtyFourK
	}
	out.Stream = req.Stream

	// 4. tools (Anthropic → OpenAI function form; WireParameters falls back to
	// Parameters, so set Parameters; but keep InputSchema? — set Parameters).
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ir.Tool{
			Type: "function",
			Function: &ir.FunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	// 5. tool_choice.
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto", "":
			out.ToolChoice = json.RawMessage(`"auto"`)
		case "any":
			out.ToolChoice = json.RawMessage(`"required"`)
		case "tool":
			out.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":` + jsonString(req.ToolChoice.Name) + `}}`)
		case "none":
			out.ToolChoice = json.RawMessage(`"none"`)
		}
	}

	// 6. optional params.
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	if len(req.StopSequences) > 0 {
		out.Stop = req.StopSequences
	}
	if req.Metadata != nil {
		out.User = req.Metadata.UserID
	}

	// 7. thinking → reasoning_effort (LiteLLM-standard mapping).
	if req.Thinking != nil {
		t := req.Thinking
		switch t.Type {
		case "disabled", "none":
			// no reasoning_effort
		case "adaptive":
			eff := t.Effort
			if eff == "" {
				eff = "medium"
			}
			out.ReasoningEffort = &eff
		default:
			if t.BudgetTokens != nil {
				eff := "low"
				switch {
				case *t.BudgetTokens >= 10000:
					eff = "high"
				case *t.BudgetTokens >= 5000:
					eff = "medium"
				}
				out.ReasoningEffort = &eff
			}
		}
	}

	return out
}

func anthropicToolResultText(block AnthropicBlock) string {
	if len(block.Content) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(block.Content))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(block.Content, &s) == nil {
			return s
		}
		return ""
	}
	var blocks []AnthropicBlock
	if json.Unmarshal(block.Content, &blocks) == nil {
		var b strings.Builder
		for i, cb := range blocks {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(cb.Text)
		}
		return b.String()
	}
	return trimmed
}

// rawBlock re-encodes a decoded block so unknown fields survive the trip
// through the IR into the CC envelope (the reference forwards parts as-is).
func rawBlock(b AnthropicBlock) json.RawMessage {
	var buf strings.Builder
	buf.WriteString(`{"type":`)
	buf.WriteString(jsonString(b.Type))
	buf.WriteString(`,"text":`)
	buf.WriteString(jsonString(b.Text))
	if len(b.CacheControl) > 0 {
		buf.WriteString(`,"cache_control":`)
		buf.Write(compactJSON(b.CacheControl))
	}
	buf.WriteString("}")
	return json.RawMessage(buf.String())
}

func mustJSONBytes(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	var check json.RawMessage
	if json.Unmarshal(raw, &check) != nil {
		return json.RawMessage("{}")
	}
	return raw
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func compactJSON(raw json.RawMessage) []byte {
	var dst bytes.Buffer
	if json.Compact(&dst, raw) == nil {
		return dst.Bytes()
	}
	return raw
}

// FakeThinkingSignature ports fakeThinkingSignature: Claude Code's shallow
// signature check only requires base64 starting with E/R and payload byte
// 0x12; the payload derives from the thinking text so blocks differ.
func FakeThinkingSignature(thinkingText string) string {
	seed := sha256.Sum256([]byte(thinkingText))
	if thinkingText == "" {
		seed = sha256.Sum256([]byte("dsh-proxy-thinking"))
	}
	raw := make([]byte, 0, 2+64)
	raw = append(raw, 0x12, byte(len(seed)))
	raw = append(raw, seed[:]...)
	// Reference truncates the digest to 64 bytes; SHA-256 is 32, so the whole
	// digest is used (Buffer.subarray(0,64) on a 32-byte buffer keeps 32).
	return base64Encode(raw)
}

// MapAnthropicStopReason ports mapAnthropicStopReason. pause_turn must pass
// through untouched — folding it to end_turn lies about completion.
func MapAnthropicStopReason(finishReason string) string {
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	case "pause_turn":
		return "pause_turn"
	case "refusal":
		return "refusal"
	default:
		return "end_turn"
	}
}

// --- Anthropic SSE translator (port of createAnthropicSseTranslator) ---

type AnthropicTranslator struct {
	MessageID string
	Model     string

	LastCcEvent       string
	UpstreamError     *cc.MappedError
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	NoCacheTokens     int64 // -1 = upstream didn't provide

	BytesReceived int64

	nextBlockIndex int
	blockStarted   bool
	blockType      string
	blockIndex     int
	thinkingText   string

	sawFinish  bool
	finishNorm string
	stopReason string
	hasError   bool
	startSent  bool

	out SSEBuf
}

func NewAnthropicTranslator(model, messageID string) *AnthropicTranslator {
	return &AnthropicTranslator{MessageID: messageID, Model: model, NoCacheTokens: -1}
}

// base64Encode wraps std base64 for the signature payload.
func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func (t *AnthropicTranslator) Take() SSEBuf {
	out := t.out
	t.out = t.out[:0]
	return out
}

func (t *AnthropicTranslator) ev(name string, obj any) {
	t.out = append(t.out, "event: "...)
	t.out = append(t.out, name...)
	t.out = append(t.out, '\n')
	t.out.Data(obj)
}

func (t *AnthropicTranslator) messageStart() {
	t.startSent = true
	t.ev("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      t.MessageID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"model":   t.Model,
			"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (t *AnthropicTranslator) closeBlock() {
	if !t.blockStarted {
		return
	}
	idx := t.blockIndex
	typ := t.blockType
	if typ == "thinking" {
		t.ev("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "signature_delta", "signature": FakeThinkingSignature(t.thinkingText)},
		})
		t.thinkingText = ""
	}
	t.blockStarted = false
	t.blockType = ""
	t.ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (t *AnthropicTranslator) startBlock(typ string, contentBlock map[string]any) {
	if t.blockStarted && t.blockType == typ {
		return
	}
	t.closeBlock()
	t.blockIndex = t.nextBlockIndex
	t.nextBlockIndex++
	t.blockType = typ
	t.blockStarted = true
	t.ev("content_block_start", map[string]any{
		"type": "content_block_start", "index": t.blockIndex, "content_block": contentBlock,
	})
}

// Start emits message_start (call once before pumping).
func (t *AnthropicTranslator) Start() { t.messageStart() }

// Started reports whether message_start has been emitted.
func (t *AnthropicTranslator) Started() bool { return t.startSent }

// ParseLine consumes one NDJSON line, emitting SSE events into the buffer.
func (t *AnthropicTranslator) ParseLine(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "[DONE]" {
		return
	}
	var event cc.CCEvent
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil || event.Type == "" {
		return
	}
	t.LastCcEvent = event.Type

	switch event.Type {
	case "start", "start-step", "text-start", "reasoning-start":
		// signal events

	case "reasoning-delta":
		if event.Text == "" {
			return
		}
		t.startBlock("thinking", map[string]any{"type": "thinking", "thinking": ""})
		t.thinkingText += event.Text
		t.ev("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": t.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": event.Text},
		})

	case "text-delta":
		text := event.TextOf()
		t.startBlock("text", map[string]any{"type": "text", "text": ""})
		t.ev("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": t.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
		t.OutputTokens++

	case "tool-call":
		t.closeBlock()
		id := event.ToolCallID
		if id == "" {
			id = "toolu_" + cc.NewUUID()[:12]
		}
		input := event.ArgsOf()
		tcIndex := t.nextBlockIndex
		t.nextBlockIndex++
		t.ev("content_block_start", map[string]any{
			"type": "content_block_start", "index": tcIndex,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": event.ToolName, "input": map[string]any{}},
		})
		t.ev("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": tcIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": input},
		})
		t.ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": tcIndex})
		t.OutputTokens += 20

	case "finish-step", "finish":
		t.sawFinish = true
		if fr := cc.EffectiveFinishReason(&event); fr != "" {
			t.finishNorm = cc.MapFinishReason(fr)
			t.stopReason = MapAnthropicStopReason(t.finishNorm)
		}
		u := event.TotalUsage
		if u == nil {
			u = event.Usage
		}
		if u != nil {
			cc.NormalizeUsage(u)
			t.InputTokens = u.InputTokens
			t.OutputTokens = u.OutputTokens
			t.CachedInputTokens = u.EffectiveCachedTokens()
			t.CacheWriteTokens = u.EffectiveCacheWriteTokens()
			if u.InputTokenDetails != nil && u.InputTokenDetails.NoCacheTokens != nil && *u.InputTokenDetails.NoCacheTokens > 0 {
				t.NoCacheTokens = *u.InputTokenDetails.NoCacheTokens
			}
		}
		// Local delta-based estimate retained when upstream omits usage —
		// zeroing would misjudge a content-ful response as zero-output.

	case "error":
		t.hasError = true
		t.UpstreamError = cc.MapCcEventError(&event)
		t.ev("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": t.UpstreamError.Body.Error.Type, "message": t.UpstreamError.Body.Error.Message,
			},
		})

	case "reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
		"tool-input-end", "tool-error", "text-end", "cache-write-tokens":
		// silent

	default:
		logx.Warn("Unknown CC event type", map[string]any{"type": event.Type})
	}
}

// Finalize closes the stream: pending block, message_delta + message_stop,
// or error events for incomplete / zero-output upstreams. Returns the final
// AnthropicUsageSummary for usage reporting; ok=false when an error event
// terminated the stream (no message_stop — never fake completion).
func (t *AnthropicTranslator) Finalize() (AnthropicUsageSummary, bool) {
	var zero AnthropicUsageSummary
	if t.hasError {
		return zero, false
	}
	t.closeBlock()

	if detail := cc.IncompleteDetail(t.sawFinish, t.finishNorm); detail != "" {
		logx.Warn("Upstream stream incomplete", map[string]any{"path": "/v1/messages", "reason": detail})
		errBody := cc.IncompleteUpstreamError(detail).Body
		t.ev("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errBody.Error.Type, "message": errBody.Error.Message},
		})
		return zero, false
	}
	if t.OutputTokens == 0 && !t.startSent {
		return zero, false
	}

	inputTokens := t.NoCacheTokens
	if inputTokens < 0 {
		n := t.InputTokens - t.CachedInputTokens - t.CacheWriteTokens
		if n < 0 {
			n = 0
		}
		inputTokens = n
	}
	usage := AnthropicUsageSummary{
		OutputTokens:             t.OutputTokens,
		CacheReadInputTokens:     t.CachedInputTokens,
		CacheCreationInputTokens: t.CacheWriteTokens,
		InputTokens:              inputTokens,
	}
	stop := t.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	t.ev("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop},
		"usage": map[string]any{
			"output_tokens":               usage.OutputTokens,
			"cache_read_input_tokens":     usage.CacheReadInputTokens,
			"cache_creation_input_tokens": usage.CacheCreationInputTokens,
			"input_tokens":                usage.InputTokens,
		},
	})
	t.ev("message_stop", map[string]any{"type": "message_stop"})
	return usage, true
}

// AnthropicUsageSummary is the final usage block of message_delta.
type AnthropicUsageSummary struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// BuildAnthropicResponse ports buildAnthropicResponse (non-streaming shape).
func BuildAnthropicResponse(model, fullText string, toolCalls []ChatTool, finishReason string, usage *cc.CCUsage, thinkingText string) map[string]any {
	content := []any{}
	if thinkingText != "" {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": thinkingText, "signature": FakeThinkingSignature(thinkingText),
		})
	}
	if fullText != "" {
		content = append(content, map[string]any{"type": "text", "text": fullText})
	}
	for _, tc := range toolCalls {
		var input any
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" || json.Unmarshal([]byte(args), &input) != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}

	cc.NormalizeUsage(usage)
	// When upstream reports no usage, estimate output tokens from content
	// length so clients don't display/bill zero.
	estOut := int64(1)
	if usage == nil || usage.OutputTokens == 0 {
		estOut = (int64(len(fullText)+len(thinkingText)) + 3) / 4
		if estOut < 1 {
			estOut = 1
		}
		estOut += int64(len(toolCalls)) * 20
	}
	outputTokens := estOut
	if usage != nil && usage.OutputTokens > 0 {
		outputTokens = usage.OutputTokens
	}
	cacheWrite := int64(0)
	cacheRead := int64(0)
	if usage != nil {
		cacheRead = usage.EffectiveCachedTokens()
		cacheWrite = usage.EffectiveCacheWriteTokens()
	}
	return map[string]any{
		"id":            "msg_" + cc.NewUUID()[:12],
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   MapAnthropicStopReason(finishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                cc.AnthropicInputTokens(usage, -1),
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": cacheWrite,
			"cache_read_input_tokens":     cacheRead,
		},
	}
}
