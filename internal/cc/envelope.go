package cc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/ir"
)

// Wire types for POST /alpha/generate. Field order matches the reference
// builder's insertion order exactly (config, memory, taste, skills,
// permissionMode, threadId?, mode, promptCache?, params), since the wire
// shape is what the upstream sees.

type Envelope struct {
	Config         WireConfig      `json:"config"`
	Memory         any             `json:"memory"` // always null
	Taste          any             `json:"taste"`  // always null
	Skills         any             `json:"skills"` // always null (not "")
	PermissionMode string          `json:"permissionMode"`
	ThreadID       string          `json:"threadId,omitempty"` // only when session id is a valid UUID
	Mode           string          `json:"mode"`
	PromptCache    json.RawMessage `json:"promptCache,omitempty"`
	Params         WireParams      `json:"params"`
}

type WireConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type WireParams struct {
	Model             string            `json:"model"`
	Messages          []WireMessage     `json:"messages"`
	MaxTokens         int               `json:"max_tokens"`
	Stream            bool              `json:"stream"` // CC API is always streamed
	System            []WireSystemBlock `json:"system,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	ReasoningEffort   *string           `json:"reasoning_effort,omitempty"`
	Tools             []WireTool        `json:"tools"` // always sent; [] when empty (wire-observable)
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
}

type WireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type WireSystemBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type WireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toolNameAliases mirrors the CLI's resolveToolNameAlias / ow table.
var toolNameAliases = map[string]string{
	"bash_output":         "shell_output",
	"task_output":         "shell_output",
	"tool_search":         "search_tools",
	"read_multiple_files": "read_file",
}

func ToWireToolName(name string) string {
	if alias, ok := toolNameAliases[name]; ok {
		return alias
	}
	return name
}

// toWireToolOutputValue mirrors the CLI's toWireToolOutput: text blocks only,
// joined with '\n'; null → ”.
func ToWireToolOutputValue(content ir.Content) string {
	if content.Str != nil {
		return *content.Str
	}
	if content.Parts != nil {
		var b strings.Builder
		first := true
		for _, p := range content.Parts {
			if p.Type != "text" {
				continue
			}
			if !first {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
			first = false
		}
		return b.String()
	}
	return ""
}

var dataURLMimeRe = regexp.MustCompile(`^data:([^;,]+)`)
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsValidUUID reports whether s is a UUID (the CLI's toWireThreadId gate).
func IsValidUUID(s string) bool { return uuidRe.MatchString(strings.ToLower(s)) }

// BuildOpts carries everything the builder needs beyond the request itself.
type BuildOpts struct {
	Device                 DeviceProfile
	CLIMode                string
	EmptySystemPlaceholder bool
}

// BuildEnvelope is the port of buildCcRequest + forwardToCC's threadId step.
// sessionID may be "" (threadId omitted entirely, per the CLI's toWireThreadId).
func BuildEnvelope(req *ir.ChatRequest, sessionID string, opts BuildOpts) *Envelope {
	// --- system blocks (string | block array, cache_control preserved) ---
	var systemBlocks []WireSystemBlock
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		if m.Content.Str != nil {
			if *m.Content.Str != "" {
				systemBlocks = append(systemBlocks, WireSystemBlock{Type: "text", Text: *m.Content.Str})
			}
		} else if m.Content.Parts != nil {
			for _, p := range m.Content.Parts {
				text := p.Text
				block := WireSystemBlock{Type: "text", Text: text, CacheControl: p.CacheControl}
				if text == "" && len(p.CacheControl) == 0 {
					continue
				}
				systemBlocks = append(systemBlocks, block)
			}
		}
	}
	// Non-last blocks get a trailing \n (CLI's composeSystemPrompt shape).
	for i := 0; i < len(systemBlocks)-1; i++ {
		systemBlocks[i].Text += "\n"
	}

	// --- tool_call_id → tool_name reverse lookup ---
	toolNameMap := map[string]string{}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					toolNameMap[tc.ID] = tc.Function.Name
				}
			}
		}
	}

	// --- messages → CC wire format ---
	wireMessages := make([]WireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			continue
		}
		wireMessages = append(wireMessages, convertMessage(m, toolNameMap))
	}

	// --- cache breakpoint: system is the prefix, so breakpoint lands there ---
	hasCacheMarker := false
	for _, b := range systemBlocks {
		if len(b.CacheControl) > 0 {
			hasCacheMarker = true
			break
		}
	}
	if !hasCacheMarker {
		for _, wm := range wireMessages {
			var parts []ir.Part
			if json.Unmarshal(wm.Content, &parts) == nil {
				for _, p := range parts {
					if len(p.CacheControl) > 0 {
						hasCacheMarker = true
						break
					}
				}
			}
			if hasCacheMarker {
				break
			}
		}
	}
	if req.PromptCacheKey != "" && !hasCacheMarker && len(systemBlocks) > 0 {
		systemBlocks[len(systemBlocks)-1].CacheControl = json.RawMessage(`{"type":"ephemeral"}`)
	}

	// --- envelope ---
	maxTokens := 64000
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	if maxTokens > 200000 {
		maxTokens = 200000
	}

	env := &Envelope{
		Config: WireConfig{
			WorkingDir:    opts.Device.ProjectDir,
			Date:          time.Now().UTC().Format("2006-01-02"),
			Environment:   opts.Device.Platform,
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		PermissionMode: "standard",
		Mode:           opts.CLIMode,
		Params: WireParams{
			Model:     req.Model,
			Messages:  wireMessages,
			MaxTokens: maxTokens,
			Stream:    true,
			Tools:     []WireTool{},
		},
	}
	if env.Params.Model == "" {
		env.Params.Model = "deepseek/deepseek-v4-flash"
	}
	if sessionID != "" && IsValidUUID(sessionID) {
		env.ThreadID = sessionID
	}

	if len(systemBlocks) > 0 {
		env.Params.System = systemBlocks
	} else if opts.EmptySystemPlaceholder {
		// A single space prevents CC from injecting its ~7.5K-token default
		// prompt (issue #17; prompt_tokens 7653 → 85 on live traffic).
		env.Params.System = []WireSystemBlock{{Type: "text", Text: " "}}
	}
	env.Params.Temperature = req.Temperature
	env.Params.ReasoningEffort = req.ReasoningEffort

	for _, t := range req.Tools {
		env.Params.Tools = append(env.Params.Tools, WireTool{
			Name:        ToWireToolName(t.WireName()),
			Description: t.WireDescription(),
			InputSchema: t.WireParameters(),
		})
	}

	if len(req.ToolChoice) > 0 {
		env.Params.ToolChoice = convertToolChoice(req.ToolChoice)
	}
	env.Params.ParallelToolCalls = req.ParallelToolCalls

	return env
}

func convertMessage(m ir.Message, toolNameMap map[string]string) WireMessage {
	switch m.Role {
	case "user":
		if m.Content.Str != nil {
			return wm("user", mustJSON([]ir.Part{{Type: "text", Text: *m.Content.Str}}))
		}
		if m.Content.Parts != nil {
			parts := make([]ir.Part, 0, len(m.Content.Parts))
			for _, p := range m.Content.Parts {
				if p.Type == "image_url" && p.ImageURL != nil {
					url := p.ImageURL.URL
					// CC CLI real format: {type:"image", image:"data:<mime>;base64,...", mimeType?}
					part := ir.Part{Type: "image"}
					var b strings.Builder
					b.WriteString(`{"type":"image","image":`)
					writeJSONString(&b, url)
					if mime := dataURLMimeRe.FindStringSubmatch(url); mime != nil {
						b.WriteString(`,"mimeType":`)
						writeJSONString(&b, mime[1])
					}
					b.WriteString("}")
					part.Raw = json.RawMessage(b.String())
					parts = append(parts, part)
					continue
				}
				parts = append(parts, p) // passthrough (Raw preserved)
			}
			return wm("user", mustJSON(parts))
		}
		// null / other shapes → text fallback
		return wm("user", mustJSON([]ir.Part{{Type: "text", Text: ""}}))

	case "assistant":
		var parts []json.RawMessage
		// Thinking MUST replay, and in this order: [reasoning, text, tool-call].
		if m.ReasoningContent != "" {
			parts = append(parts, json.RawMessage(fmt.Sprintf(`{"type":"reasoning","text":%s}`, mustJSON(m.ReasoningContent))))
		}
		if m.Content.Str != nil && *m.Content.Str != "" {
			parts = append(parts, mustJSON(ir.Part{Type: "text", Text: *m.Content.Str}))
		} else if m.Content.Parts != nil {
			for _, p := range m.Content.Parts {
				if p.Type == "text" {
					parts = append(parts, mustJSON(p))
				} else if p.Type == "reasoning" && m.ReasoningContent == "" {
					parts = append(parts, mustJSON(p))
				}
			}
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, json.RawMessage(fmt.Sprintf(
				`{"type":"tool-call","toolCallId":%s,"toolName":%s,"input":%s}`,
				mustJSON(tc.ID), mustJSON(tc.Function.Name), tc.Function.ObjectArguments())))
		}
		if parts == nil {
			parts = []json.RawMessage{}
		}
		return wmRaw("assistant", joinRawArray(parts))

	case "tool":
		name := toolNameMap[m.ToolCallID]
		if name == "" {
			name = m.Name
		}
		out := fmt.Sprintf(`{"type":"tool-result","toolCallId":%s,"toolName":%s,"output":{"type":"text","value":%s}}`,
			mustJSON(m.ToolCallID), mustJSON(name), mustJSON(ToWireToolOutputValue(m.Content)))
		return wmRaw("tool", json.RawMessage("["+out+"]"))

	default:
		// Unknown role → normalized user message (CC validation rejects others)
		text := m.Content.String()
		return wm("user", mustJSON([]ir.Part{{Type: "text", Text: text}}))
	}
}

// convertToolChoice: OpenAI format → CC (Anthropic-style) format.
func convertToolChoice(raw json.RawMessage) json.RawMessage {
	if s, ok := ir.ToolChoiceString(raw); ok {
		switch s {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		default:
			return json.RawMessage(`{"type":"auto"}`)
		}
	}
	if name, ok := ir.ToolChoiceFunction(raw); ok {
		return json.RawMessage(fmt.Sprintf(`{"type":"tool","name":%s}`, mustJSON(name)))
	}
	return raw // object passthrough
}

func wm(role string, content any) WireMessage {
	return WireMessage{Role: role, Content: mustJSON(content)}
}

func wmRaw(role string, content json.RawMessage) WireMessage {
	return WireMessage{Role: role, Content: content}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func joinRawArray(items []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(it)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func writeJSONString(b *strings.Builder, s string) {
	enc, err := json.Marshal(s)
	if err != nil {
		b.WriteString(`""`)
		return
	}
	b.Write(enc)
}
