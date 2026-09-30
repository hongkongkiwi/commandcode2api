// Envelope shape tests: the /alpha/generate body must match the CLI wire
// format field-for-field (buildCcRequest contracts).
package cc

import (
	"encoding/json"
	"testing"

	"github.com/hongkongkiwi/commandcode2api/internal/ir"
)

func build(t *testing.T, req *ir.ChatRequest, sessionID string) map[string]any {
	t.Helper()
	env := BuildEnvelope(req, sessionID, BuildOpts{
		Device:                 NewDeviceProfile(""),
		CLIMode:                "agent",
		EmptySystemPlaceholder: true,
	})
	raw, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The 9-key CLI envelope, in order.
func TestEnvelopeKeyOrderAndShape(t *testing.T) {
	req := &ir.ChatRequest{Model: "test/model", Messages: []ir.Message{{Role: "user", Content: ir.StringContent("hi")}}}
	raw, err := MarshalEnvelope(BuildEnvelope(req, "0195c4d2-1111-7222-8333-444455556666", BuildOpts{
		Device: NewDeviceProfile(""), CLIMode: "agent", EmptySystemPlaceholder: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"config":{"workingDir":` // wire order starts with config
	if string(raw[:len(want)]) != want {
		t.Fatalf("envelope must start with config key, got %.40s", raw)
	}
	// ordered keys present: memory, taste, skills, permissionMode, threadId, mode, params
	m := build(t, req, "0195c4d2-1111-7222-8333-444455556666")
	for _, k := range []string{"memory", "taste", "skills", "permissionMode", "threadId", "mode", "params"} {
		if _, ok := m[k]; !ok {
			t.Errorf("envelope missing key %q", k)
		}
	}
	if m["threadId"] != "0195c4d2-1111-7222-8333-444455556666" {
		t.Errorf("threadId = %v", m["threadId"])
	}
}

func TestThreadIdOmittedForNonUUID(t *testing.T) {
	m := build(t, &ir.ChatRequest{Model: "m", Messages: []ir.Message{{Role: "user", Content: ir.StringContent("hi")}}}, "my-session-123")
	if _, ok := m["threadId"]; ok {
		t.Fatal("threadId must be omitted for non-UUID session (CLI toWireThreadId)")
	}
}

func TestSystemBlocksAndCacheBreakpoint(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{
			{Role: "system", Content: ir.Content{Parts: []ir.Part{
				{Type: "text", Text: "part one"},
				{Type: "text", Text: "part two", CacheControl: json.RawMessage(`{"type":"ephemeral"}`)},
			}}},
			{Role: "user", Content: ir.StringContent("hi")},
		},
		PromptCacheKey: "cache-key-1",
	}
	m := build(t, req, "")
	params := m["params"].(map[string]any)
	sys := params["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("system blocks = %d, want 2", len(sys))
	}
	first := sys[0].(map[string]any)
	if first["text"] != "part one\n" {
		t.Fatalf("non-last system block must get trailing \\n, got %q", first["text"])
	}
	second := sys[1].(map[string]any)
	if second["cache_control"] == nil {
		t.Fatal("client cache_control must be preserved on system blocks")
	}
}

func TestPromptCacheKeyPlacesBreakpointOnLastSystemBlock(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{
			{Role: "system", Content: ir.StringContent("sys")},
			{Role: "user", Content: ir.StringContent("hi")},
		},
		PromptCacheKey: "cache-key-1",
	}
	m := build(t, req, "")
	params := m["params"].(map[string]any)
	sys := params["system"].([]any)
	last := sys[len(sys)-1].(map[string]any)
	if last["cache_control"] == nil {
		t.Fatal("prompt_cache_key must place an ephemeral breakpoint on the last system block")
	}
}

func TestEmptySystemPlaceholder(t *testing.T) {
	req := &ir.ChatRequest{Model: "m", Messages: []ir.Message{{Role: "user", Content: ir.StringContent("hi")}}}
	m := build(t, req, "")
	sys := m["params"].(map[string]any)["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != " " {
		t.Fatalf("placeholder system = %v, want single space (issue #17)", sys)
	}
}

func TestToolNameAliasesAndShape(t *testing.T) {
	req := &ir.ChatRequest{
		Model:    "m",
		Messages: []ir.Message{{Role: "user", Content: ir.StringContent("hi")}},
		Tools: []ir.Tool{
			{Type: "function", Function: &ir.FunctionDef{Name: "bash_output", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "function", Function: &ir.FunctionDef{Name: "custom_tool", Description: "", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
		},
	}
	m := build(t, req, "")
	tools := m["params"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] != "shell_output" {
		t.Fatalf("bash_output must alias to shell_output, got %v", first["name"])
	}
	if _, hasType := first["type"]; hasType {
		t.Fatal("CC tools must NOT have a type field (only name/description/input_schema)")
	}
	second := tools[1].(map[string]any)
	if second["name"] != "custom_tool" {
		t.Fatalf("custom_tool passed through, got %v", second["name"])
	}
}

func TestToolChoiceMapping(t *testing.T) {
	base := func(tc json.RawMessage) map[string]any {
		req := &ir.ChatRequest{
			Model:      "m",
			Messages:   []ir.Message{{Role: "user", Content: ir.StringContent("hi")}},
			ToolChoice: tc,
		}
		return build(t, req, "")
	}
	cases := map[string]map[string]any{
		`"auto"`:     {"type": "auto"},
		`"none"`:     {"type": "none"},
		`"required"`: {"type": "any"},
	}
	for in, want := range cases {
		got := base(json.RawMessage(in))["params"].(map[string]any)["tool_choice"].(map[string]any)
		if got["type"] != want["type"] {
			t.Errorf("tool_choice %s → %v, want type %v", in, got, want["type"])
		}
	}
	named := base(json.RawMessage(`{"type":"function","function":{"name":"lookup"}}`))["params"].(map[string]any)["tool_choice"].(map[string]any)
	if named["type"] != "tool" || named["name"] != "lookup" {
		t.Fatalf("named tool_choice → {type:tool,name}, got %v", named)
	}
}

func TestAssistantReplayOrder(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{
			{Role: "user", Content: ir.StringContent("hi")},
			{Role: "assistant", ReasoningContent: "thinking...", Content: ir.StringContent("answer"),
				ToolCalls: []ir.ToolCall{{ID: "call_1", Type: "function", Function: ir.FunctionCall{
					Name: "tool", Arguments: json.RawMessage(`"\"{}\""`)}},
				},
			},
		},
	}
	m := build(t, req, "")
	msgs := m["params"].(map[string]any)["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	parts := assistant["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("assistant parts = %d, want [reasoning, text, tool-call]", len(parts))
	}
	if parts[0].(map[string]any)["type"] != "reasoning" {
		t.Fatal("reasoning must come FIRST in replayed assistant content (CC rejects otherwise)")
	}
	if parts[1].(map[string]any)["type"] != "text" || parts[2].(map[string]any)["type"] != "tool-call" {
		t.Fatal("order must be [reasoning, text, tool-call]")
	}
}

func TestUserImageConversion(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{{Role: "user", Content: ir.Content{Parts: []ir.Part{
			{Type: "image_url", ImageURL: &ir.ImageURL{URL: "data:image/jpeg;base64,AbCd"}},
		}}}},
	}
	m := build(t, req, "")
	msgs := m["params"].(map[string]any)["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
	img := parts[0].(map[string]any)
	if img["type"] != "image" || img["image"] != "data:image/jpeg;base64,AbCd" || img["mimeType"] != "image/jpeg" {
		t.Fatalf("image part = %v, want CC format {type:image,image,mimeType}", img)
	}
}

func TestMaxTokensClamp(t *testing.T) {
	req := &ir.ChatRequest{Model: "m", Messages: []ir.Message{{Role: "user", Content: ir.StringContent("hi")}}}
	big := 999999
	req.MaxTokens = &big
	m := build(t, req, "")
	if got := m["params"].(map[string]any)["max_tokens"].(float64); got != 200000 {
		t.Fatalf("max_tokens = %v, want clamped 200000", got)
	}
}

func TestToolResultTextJoin(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{
			{Role: "assistant", ToolCalls: []ir.ToolCall{{ID: "c1", Type: "function", Function: ir.FunctionCall{Name: "t", Arguments: json.RawMessage(`"{}"`)}}}},
			{Role: "tool", ToolCallID: "c1", Name: "t", Content: ir.Content{Parts: []ir.Part{
				{Type: "text", Text: "line1"}, {Type: "text", Text: "line2"},
			}}},
		},
	}
	m := build(t, req, "")
	msgs := m["params"].(map[string]any)["messages"].([]any)
	tool := msgs[1].(map[string]any)
	content := tool["content"].([]any)[0].(map[string]any)
	if content["type"] != "tool-result" || content["toolCallId"] != "c1" || content["toolName"] != "t" {
		t.Fatalf("tool message = %v", content)
	}
	out := content["output"].(map[string]any)
	if out["value"] != "line1\nline2" {
		t.Fatalf("tool output value = %v, want newline join of text blocks", out["value"])
	}
}
