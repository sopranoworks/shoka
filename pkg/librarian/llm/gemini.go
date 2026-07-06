package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/genai"
)

// geminiClient adapts the official google.golang.org/genai SDK to the neutral
// Client seam, behind the same one-round-trip CreateMessage the loop drives.
// Like the anthropic/openai clients, only the base URL varies (HTTPOptions.BaseURL,
// for a proxy/gateway); the key is read by the SDK from the environment —
// GEMINI_API_KEY or GOOGLE_API_KEY (GOOGLE_API_KEY wins if both are set) — never
// from config. The native genai function-calling surface (functionDeclarations /
// functionCall / functionResponse) is used, not Gemini's OpenAI-compat endpoint:
// that compat path would just be openai-go pointed at a Gemini base URL (already
// the openai provider) and would never exercise a Gemini SDK.
//
// The genai SDK validates auth at construction (unlike the anthropic/openai SDKs,
// whose constructors never fail), so the underlying client is built LAZILY on
// first use. That keeps newGeminiClient non-failing — matching the other
// constructors and the registration invariant (a missing key surfaces via the
// health-check / first call; it does not prevent the tool from registering).
type geminiClient struct {
	cfg       LLMConfig
	maxTokens int32

	once    sync.Once
	client  *genai.Client
	initErr error
}

// newGeminiClient builds a Client over the genai SDK. A non-empty BaseURL points
// the SDK at a custom endpoint (proxy/gateway); an empty BaseURL uses Gemini's
// production default. No key is set in config — the SDK reads it from the
// environment (Shoka never handles the key).
func newGeminiClient(cfg LLMConfig) Client {
	return &geminiClient{cfg: cfg, maxTokens: defaultMaxTokens}
}

// ensure lazily constructs the genai client. genai.NewClient verifies that some
// auth (env key) or a custom base URL is present, so construction can fail — we
// defer it here rather than in the constructor so a missing key never prevents
// the tool from registering (it is reported by the health-check instead).
func (g *geminiClient) ensure(ctx context.Context) (*genai.Client, error) {
	g.once.Do(func() {
		cc := &genai.ClientConfig{Backend: genai.BackendGeminiAPI}
		if g.cfg.BaseURL != "" {
			cc.HTTPOptions = genai.HTTPOptions{BaseURL: g.cfg.BaseURL}
		}
		g.client, g.initErr = genai.NewClient(ctx, cc)
	})
	return g.client, g.initErr
}

// CreateMessage performs one round-trip via Models.GenerateContent, converting
// neutral params in and the SDK reply out. It does NOT constrain tool choice; the
// loop lets the model choose to call a tool or answer.
func (g *geminiClient) CreateMessage(ctx context.Context, params CreateMessageParams) (Message, error) {
	client, err := g.ensure(ctx)
	if err != nil {
		return Message{}, err
	}

	cfg := &genai.GenerateContentConfig{MaxOutputTokens: g.maxTokens}
	if params.System != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: params.System}}}
	}
	if len(params.Tools) > 0 {
		cfg.Tools = toGeminiTools(params.Tools)
	}

	resp, err := client.Models.GenerateContent(ctx, g.cfg.Model, toGeminiContents(params.Messages), cfg)
	if err != nil {
		return Message{}, err
	}
	return fromGeminiResponse(resp), nil
}

// geminiPendingCall pairs a tool_use ID with its function name, so a later
// tool_result (which carries only the ID) can be given the name Gemini's
// FunctionResponse requires.
type geminiPendingCall struct{ id, name string }

// toGeminiContents converts neutral messages to genai Contents. The assistant
// role maps to Gemini's "model"; user and tool-result turns map to "user".
//
// Gemini's FunctionResponse requires the function NAME, but the neutral
// tool_result block carries only the originating tool_use ID. The id→name pairing
// lives in the assistant tool_use blocks earlier in the same message list, so we
// resolve it here. Gemini's Developer API may emit empty function-call IDs (it
// correlates by name + order), so we match by ID when one is present and fall
// back to call order otherwise.
func toGeminiContents(msgs []Message) []*genai.Content {
	var pending []geminiPendingCall
	out := make([]*genai.Content, 0, len(msgs))

	for _, m := range msgs {
		role := genai.RoleUser
		if m.Role == RoleAssistant {
			role = genai.RoleModel
		}
		parts := make([]*genai.Part, 0, len(m.Content))
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				parts = append(parts, &genai.Part{Text: b.Text})
			case BlockToolUse:
				pending = append(pending, geminiPendingCall{id: b.ID, name: b.Name})
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
					ID:   b.ID,
					Name: b.Name,
					Args: jsonToMap(b.Input),
				}})
			case BlockToolResult:
				name, id := resolvePendingCall(&pending, b.ToolUseID)
				parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
					ID:       id,
					Name:     name,
					Response: toolResponse(b.Content, b.IsError),
				}})
			}
		}
		if len(parts) > 0 {
			out = append(out, &genai.Content{Role: role, Parts: parts})
		}
	}
	return out
}

// resolvePendingCall returns the function name (and matching ID) for a
// tool_result, consuming the pending call it corresponds to. It prefers an exact
// ID match; absent that (Gemini may issue empty IDs), it consumes the oldest
// pending call, which is the order the API expects responses in.
func resolvePendingCall(pending *[]geminiPendingCall, toolUseID string) (name, id string) {
	q := *pending
	if toolUseID != "" {
		for i, p := range q {
			if p.id == toolUseID {
				*pending = append(q[:i:i], q[i+1:]...)
				return p.name, p.id
			}
		}
	}
	if len(q) > 0 {
		p := q[0]
		*pending = q[1:]
		return p.name, p.id
	}
	return "", toolUseID
}

// toGeminiTools converts neutral tool definitions to a single genai Tool holding
// all function declarations. The neutral tool's object schema is passed straight
// through as ParametersJsonSchema (a raw JSON-schema object), mirroring how the
// OpenAI client passes its FunctionParameters map.
func toGeminiTools(tools []ToolDef) []*genai.Tool {
	decls := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		schema := map[string]any{
			"type":       "object",
			"properties": t.Properties,
		}
		if len(t.Required) > 0 {
			schema["required"] = t.Required
		}
		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJsonSchema: schema,
		})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// fromGeminiResponse converts a genai reply to a neutral assistant Message,
// keeping the text and the function calls the loop acts on. Thought (reasoning)
// parts are skipped — they are not the answer.
func fromGeminiResponse(resp *genai.GenerateContentResponse) Message {
	out := Message{Role: RoleAssistant, RawResponse: rawSnapshot(resp)}
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return out
	}
	for _, p := range resp.Candidates[0].Content.Parts {
		switch {
		case p == nil || p.Thought:
			continue
		case p.FunctionCall != nil:
			out.Content = append(out.Content, Block{
				Type:  BlockToolUse,
				ID:    p.FunctionCall.ID,
				Name:  p.FunctionCall.Name,
				Input: mapToJSON(p.FunctionCall.Args),
			})
		case p.Text != "":
			out.Content = append(out.Content, Block{Type: BlockText, Text: p.Text})
		}
	}
	return out
}

// jsonToMap parses raw tool-call argument JSON into the map Gemini's FunctionCall
// expects. Empty or invalid input yields a nil map (no arguments).
func jsonToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// mapToJSON marshals Gemini's function-call arguments back to the raw JSON the
// neutral tool_use block carries. An empty or unmarshalable map yields "{}".
func mapToJSON(args map[string]any) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// toolResponse wraps a tool's string result in the JSON object Gemini's
// FunctionResponse requires — under "error" for a failure, "output" otherwise
// (the keys Gemini documents for function output vs error detail).
func toolResponse(content string, isError bool) map[string]any {
	key := "output"
	if isError {
		key = "error"
	}
	return map[string]any{key: content}
}

// geminiEmbedder adapts the genai SDK's Models.EmbedContent method.
type geminiEmbedder struct {
	cfg  LLMConfig
	once sync.Once
	client  *genai.Client
	initErr error
}

func newGeminiEmbedder(cfg LLMConfig) Embedder {
	return &geminiEmbedder{cfg: cfg}
}

func (g *geminiEmbedder) ensure(ctx context.Context) (*genai.Client, error) {
	g.once.Do(func() {
		cc := &genai.ClientConfig{Backend: genai.BackendGeminiAPI}
		if g.cfg.BaseURL != "" {
			cc.HTTPOptions = genai.HTTPOptions{BaseURL: g.cfg.BaseURL}
		}
		g.client, g.initErr = genai.NewClient(ctx, cc)
	})
	return g.client, g.initErr
}

func (g *geminiEmbedder) Embed(ctx context.Context, text string) (*EmbeddingVector, error) {
	client, err := g.ensure(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := client.Models.EmbedContent(ctx, g.cfg.Model, []*genai.Content{
		{Parts: []*genai.Part{{Text: text}}},
	}, nil)
	if err != nil {
		return nil, err
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("gemini embedding response contained no embeddings")
	}
	vals32 := resp.Embeddings[0].Values
	vals := make([]float64, len(vals32))
	for i, v := range vals32 {
		vals[i] = float64(v)
	}
	return &EmbeddingVector{
		Model:      g.cfg.Model,
		Dimensions: len(vals),
		Values:     vals,
	}, nil
}
