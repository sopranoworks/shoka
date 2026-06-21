package llm

import (
	"context"
	"encoding/json"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// openaiClient adapts the official openai-go SDK to the neutral Client seam,
// behind the same CreateMessage round-trip the tool-call loop drives. Like the
// Anthropic client, only the base URL varies (set for ollama's
// OpenAI-compatible /v1/chat/completions); the key comes from OPENAI_API_KEY in
// the environment, never from config.
type openaiClient struct {
	client    openai.Client
	model     string
	maxTokens int64
}

// newOpenAIClient builds a Client over the OpenAI SDK. A non-empty BaseURL
// points it at an OpenAI-compatible endpoint (e.g. ollama at
// http://localhost:11434/v1); an empty BaseURL uses OpenAI's production default.
// No key option is set — the SDK reads OPENAI_API_KEY from the environment.
func newOpenAIClient(cfg LLMConfig) Client {
	var opts []option.RequestOption
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &openaiClient{
		client:    openai.NewClient(opts...),
		model:     cfg.Model,
		maxTokens: defaultMaxTokens,
	}
}

// CreateMessage performs one round-trip via Chat.Completions.New, converting
// neutral params in and the SDK reply out. It does NOT set tool_choice — ollama
// does not support it; the loop lets the model choose to call a tool or answer.
func (o *openaiClient) CreateMessage(ctx context.Context, params CreateMessageParams) (Message, error) {
	body := openai.ChatCompletionNewParams{
		Model:               o.model,
		MaxCompletionTokens: openai.Int(o.maxTokens),
		Messages:            toOpenAIMessages(params.System, params.Messages),
	}
	if len(params.Tools) > 0 {
		body.Tools = toOpenAITools(params.Tools)
	}

	resp, err := o.client.Chat.Completions.New(ctx, body)
	if err != nil {
		return Message{}, err
	}
	return fromOpenAIMessage(resp), nil
}

// toOpenAIMessages converts neutral messages to OpenAI chat messages. A neutral
// user turn may carry text and/or tool_result blocks (the latter become OpenAI
// tool messages); an assistant turn carries text and/or tool_use blocks (the
// latter become assistant tool_calls).
func toOpenAIMessages(system string, msgs []Message) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	if system != "" {
		out = append(out, openai.SystemMessage(system))
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			var text string
			var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					text += b.Text
				case BlockToolUse:
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: b.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      b.Name,
								Arguments: string(b.Input),
							},
						},
					})
				}
			}
			var asst openai.ChatCompletionAssistantMessageParam
			if text != "" {
				asst.Content.OfString = openai.String(text)
			}
			asst.ToolCalls = toolCalls
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		default: // RoleUser
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					out = append(out, openai.UserMessage(b.Text))
				case BlockToolResult:
					out = append(out, openai.ToolMessage(b.Content, b.ToolUseID))
				}
			}
		}
	}
	return out
}

// toOpenAITools converts neutral tool definitions to OpenAI function tools.
func toOpenAITools(tools []ToolDef) []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		fn := shared.FunctionDefinitionParam{
			Name: t.Name,
			Parameters: shared.FunctionParameters{
				"type":       "object",
				"properties": t.Properties,
				"required":   t.Required,
			},
		}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		out = append(out, openai.ChatCompletionFunctionTool(fn))
	}
	return out
}

// fromOpenAIMessage converts an OpenAI reply to a neutral assistant Message,
// keeping the text and the function tool_calls the loop acts on.
func fromOpenAIMessage(resp *openai.ChatCompletion) Message {
	out := Message{Role: RoleAssistant}
	if len(resp.Choices) == 0 {
		return out
	}
	msg := resp.Choices[0].Message
	if msg.Content != "" {
		out.Content = append(out.Content, Block{Type: BlockText, Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			continue
		}
		out.Content = append(out.Content, Block{
			Type:  BlockToolUse,
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out
}
