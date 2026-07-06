package llm

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultMaxTokens caps a single model reply. The Anthropic API requires
// max_tokens; the librarian's answers and tool-call arguments are small, so a
// modest default suffices for both live and ollama.
const defaultMaxTokens = 2048

// anthropicClient adapts the official anthropic-sdk-go to the neutral Client
// seam. The SAME type serves live Anthropic and local ollama — the difference
// is entirely in the base URL / key / model passed to NewAnthropicClient.
type anthropicClient struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
}

// newAnthropicClient builds a Client over the Anthropic SDK. A non-empty
// BaseURL points the SDK at an Anthropic-compatible endpoint (e.g. ollama's
// /v1/messages at http://localhost:11434); an empty BaseURL uses the real
// Anthropic API. No key option is set — the SDK reads ANTHROPIC_API_KEY from
// the environment (Shoka never handles the key).
func newAnthropicClient(cfg LLMConfig) Client {
	var opts []option.RequestOption
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &anthropicClient{
		client:    anthropic.NewClient(opts...),
		model:     anthropic.Model(cfg.Model),
		maxTokens: defaultMaxTokens,
	}
}

// CreateMessage performs one round-trip via client.Messages.New, converting
// neutral params in and the SDK reply out. It does NOT set tool_choice —
// ollama does not support it (design report §3.2); the loop lets the model
// choose to call a tool or answer.
func (a *anthropicClient) CreateMessage(ctx context.Context, params CreateMessageParams) (Message, error) {
	body := anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  toSDKMessages(params.Messages),
	}
	if params.System != "" {
		body.System = []anthropic.TextBlockParam{{Text: params.System}}
	}
	if len(params.Tools) > 0 {
		body.Tools = toSDKTools(params.Tools)
	}

	resp, err := a.client.Messages.New(ctx, body)
	if err != nil {
		return Message{}, err
	}
	return fromSDKMessage(resp), nil
}

// toSDKMessages converts neutral messages to SDK MessageParams.
func toSDKMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Content))
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case BlockToolUse:
				// json.RawMessage marshals to its raw bytes, so the original
				// argument JSON round-trips unchanged.
				blocks = append(blocks, anthropic.NewToolUseBlock(b.ID, b.Input, b.Name))
			case BlockToolResult:
				blocks = append(blocks, anthropic.NewToolResultBlock(b.ToolUseID, b.Content, b.IsError))
			}
		}
		if m.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		} else {
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out
}

// toSDKTools converts neutral tool definitions to SDK tool params.
func toSDKTools(tools []ToolDef) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{
			Properties: t.Properties,
			Required:   t.Required,
		}
		tu := anthropic.ToolUnionParamOfTool(schema, t.Name)
		if t.Description != "" {
			tu.OfTool.Description = anthropic.String(t.Description)
		}
		out = append(out, tu)
	}
	return out
}

// fromSDKMessage converts an SDK reply to a neutral assistant Message, keeping
// only the text and tool_use blocks the loop acts on.
func fromSDKMessage(resp *anthropic.Message) Message {
	out := Message{Role: RoleAssistant, RawResponse: rawSnapshot(resp)}
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			out.Content = append(out.Content, Block{Type: BlockText, Text: blk.Text})
		case "tool_use":
			out.Content = append(out.Content, Block{
				Type:  BlockToolUse,
				ID:    blk.ID,
				Name:  blk.Name,
				Input: blk.Input,
			})
		}
	}
	return out
}
