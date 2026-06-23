package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// toGeminiContents must map roles (assistant⇒model, user⇒user), carry text and
// function calls, and — crucially — give each tool_result the function NAME
// Gemini requires, correlated from the earlier tool_use (the neutral result block
// carries only the ID).
func TestToGeminiContents(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "q"}}},
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockText, Text: "let me look"},
			{Type: BlockToolUse, ID: "c1", Name: "search", Input: json.RawMessage(`{"query":"x"}`)},
		}},
		{Role: RoleUser, Content: []Block{
			{Type: BlockToolResult, ToolUseID: "c1", Content: "match"},
		}},
	}
	contents := toGeminiContents(msgs)
	if len(contents) != 3 {
		t.Fatalf("got %d contents, want 3", len(contents))
	}
	if contents[0].Role != genai.RoleUser || contents[0].Parts[0].Text != "q" {
		t.Errorf("content[0] = %+v, want user text %q", contents[0], "q")
	}
	// Assistant ⇒ "model", with a text part and a function call carrying parsed args.
	if contents[1].Role != genai.RoleModel {
		t.Errorf("content[1].Role = %q, want %q", contents[1].Role, genai.RoleModel)
	}
	fc := contents[1].Parts[1].FunctionCall
	if fc == nil || fc.Name != "search" || fc.ID != "c1" {
		t.Fatalf("content[1] function call = %+v, want name=search id=c1", fc)
	}
	if fc.Args["query"] != "x" {
		t.Errorf("function call args = %v, want query=x", fc.Args)
	}
	// The tool result ⇒ a user FunctionResponse, NAME correlated from the call,
	// output wrapped under "output".
	if contents[2].Role != genai.RoleUser {
		t.Errorf("content[2].Role = %q, want user", contents[2].Role)
	}
	fr := contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "search" || fr.ID != "c1" {
		t.Fatalf("function response = %+v, want name=search id=c1", fr)
	}
	if fr.Response["output"] != "match" {
		t.Errorf("function response = %v, want output=match", fr.Response)
	}
}

// An error tool_result wraps under "error", and an empty function-call ID (which
// Gemini's Developer API may emit) still correlates the name by call order.
func TestToGeminiContents_ErrorAndEmptyID(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockToolUse, ID: "", Name: "list", Input: json.RawMessage(`{}`)},
		}},
		{Role: RoleUser, Content: []Block{
			{Type: BlockToolResult, ToolUseID: "", Content: "boom", IsError: true},
		}},
	}
	contents := toGeminiContents(msgs)
	fr := contents[1].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "list" {
		t.Fatalf("function response = %+v, want name=list (correlated by order)", fr)
	}
	if fr.Response["error"] != "boom" {
		t.Errorf("error result = %v, want error=boom", fr.Response)
	}
}

// fromGeminiResponse keeps the answer text and the function calls, skips thought
// parts, and survives an empty/blank response without panicking.
func TestFromGeminiResponse(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{Text: "internal reasoning", Thought: true}, // skipped
					{Text: "the answer"},
					{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "read", Args: map[string]any{"path": "a.md"}}},
				},
			},
		}},
	}
	msg := fromGeminiResponse(resp)
	if msg.Role != RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("got %d blocks, want 2 (thought skipped)", len(msg.Content))
	}
	if msg.Content[0].Type != BlockText || msg.Content[0].Text != "the answer" {
		t.Errorf("block[0] = %+v, want the answer text", msg.Content[0])
	}
	tu := msg.Content[1]
	if tu.Type != BlockToolUse || tu.Name != "read" || tu.ID != "c1" {
		t.Fatalf("block[1] = %+v, want tool_use read/c1", tu)
	}
	var args map[string]any
	if err := json.Unmarshal(tu.Input, &args); err != nil || args["path"] != "a.md" {
		t.Errorf("tool_use args = %s (err %v), want path=a.md", tu.Input, err)
	}
	// No candidates ⇒ an empty assistant message, no panic.
	if got := fromGeminiResponse(&genai.GenerateContentResponse{}); len(got.Content) != 0 {
		t.Errorf("empty response = %+v, want no content", got)
	}
}

// toGeminiTools wraps the neutral defs in a single Tool of function declarations,
// passing the object schema straight through as ParametersJsonSchema.
func TestToGeminiTools(t *testing.T) {
	tools := []ToolDef{{
		Name:        "read",
		Description: "read a file",
		Properties:  map[string]any{"path": map[string]any{"type": "string"}},
		Required:    []string{"path"},
	}}
	out := toGeminiTools(tools)
	if len(out) != 1 || len(out[0].FunctionDeclarations) != 1 {
		t.Fatalf("got %d tools, want 1 with 1 declaration", len(out))
	}
	d := out[0].FunctionDeclarations[0]
	if d.Name != "read" || d.Description != "read a file" {
		t.Errorf("declaration = %+v, want name=read", d)
	}
	schema, ok := d.ParametersJsonSchema.(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("ParametersJsonSchema = %v, want an object schema", d.ParametersJsonSchema)
	}
	if req, _ := schema["required"].([]string); len(req) != 1 || req[0] != "path" {
		t.Errorf("schema required = %v, want [path]", schema["required"])
	}
}

// classifyError maps the genai SDK's APIError to the same five kinds, with a
// non-empty, secret-free detail on every non-ready outcome.
func TestClassifyError_Gemini(t *testing.T) {
	cases := []struct {
		name string
		err  genai.APIError
		want HealthKind
	}{
		{"model not found", genai.APIError{Code: 404, Status: "NOT_FOUND", Message: "models/gemini-x is not found"}, HealthModelNotFound},
		{"permission denied", genai.APIError{Code: 403, Status: "PERMISSION_DENIED", Message: "denied"}, HealthAuthFailed},
		{"unauthenticated", genai.APIError{Code: 401, Status: "UNAUTHENTICATED"}, HealthAuthFailed},
		// A 400 INVALID_ARGUMENT "API key not valid" is the real invalid-key shape;
		// status code alone would bucket it misconfigured, so the message forces auth.
		{"invalid api key", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "API key not valid. Please pass a valid API key."}, HealthAuthFailed},
		{"other bad request", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "bad request"}, HealthMisconfigured},
		{"server error", genai.APIError{Code: 503, Status: "UNAVAILABLE"}, HealthUnreachable},
	}
	for _, c := range cases {
		got := classifyError(c.err)
		if got.Kind != c.want {
			t.Errorf("%s: kind = %v, want %v", c.name, got.Kind, c.want)
		}
		if got.Detail == "" {
			t.Errorf("%s: detail is empty, want a non-empty secret-free detail", c.name)
		}
	}
	// The 404 detail surfaces the status and message (the cause), never a key.
	if got := classifyError(genai.APIError{Code: 404, Status: "NOT_FOUND", Message: "models/gemini-x is not found"}); !strings.Contains(got.Detail, "404") || !strings.Contains(got.Detail, "gemini-x") {
		t.Errorf("404 detail = %q, want it to name the status and model", got.Detail)
	}
}

// A missing Gemini key (BOTH accepted env vars empty) is auth_failed with NO
// network call; the detail names both variables and never a value.
func TestCheckHealth_MissingKey_Gemini(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	got := CheckHealth(t.Context(), LLMConfig{Provider: ProviderGemini, Model: "gemini-x"})
	if got.Kind != HealthAuthFailed {
		t.Fatalf("missing key kind = %v, want auth_failed", got.Kind)
	}
	if got.Detail != "GEMINI_API_KEY (or GOOGLE_API_KEY) is empty or unset" {
		t.Errorf("detail = %q, want the dual-var message", got.Detail)
	}
}

// The key is "present" when EITHER variable is set — so a GOOGLE_API_KEY-only
// environment is NOT reported as a missing key (verified without a network call
// via the presence helper the gate uses).
func TestApiKeyPresence_Gemini(t *testing.T) {
	envs := apiKeyEnvVars(ProviderGemini)
	t.Run("both empty ⇒ missing", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "")
		t.Setenv("GOOGLE_API_KEY", "")
		if !allEnvEmpty(envs) {
			t.Error("both empty: want allEnvEmpty true")
		}
	})
	t.Run("GOOGLE_API_KEY only ⇒ present", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "")
		t.Setenv("GOOGLE_API_KEY", "x")
		if allEnvEmpty(envs) {
			t.Error("GOOGLE_API_KEY set: want allEnvEmpty false (key present)")
		}
	})
	t.Run("GEMINI_API_KEY only ⇒ present", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "x")
		t.Setenv("GOOGLE_API_KEY", "")
		if allEnvEmpty(envs) {
			t.Error("GEMINI_API_KEY set: want allEnvEmpty false (key present)")
		}
	})
}

func TestMissingKeyDetail(t *testing.T) {
	if got := missingKeyDetail([]string{"ANTHROPIC_API_KEY"}); got != "ANTHROPIC_API_KEY is empty or unset" {
		t.Errorf("single var = %q", got)
	}
	if got := missingKeyDetail([]string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}); got != "GEMINI_API_KEY (or GOOGLE_API_KEY) is empty or unset" {
		t.Errorf("dual var = %q", got)
	}
}
