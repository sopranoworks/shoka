package librarian

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// ToolCall records one tool invocation the loop dispatched, for the trace the
// caller (and the structural tests) inspect.
type ToolCall struct {
	Tool    string // "read" / "list" / an unknown name the model attempted
	Path    string // the path/dir argument the call targeted
	Refused bool   // the guard (or an error) rejected the call
	Detail  string // error detail when Refused
}

// runLoop drives the agentic tool-call loop: send the running conversation +
// the tool defs; if the reply has tool_use blocks, dispatch each through its
// guard-wrapped tool and feed the tool_result blocks back; stop at a final text
// answer or when maxSteps round-trips are spent.
func runLoop(ctx context.Context, client llm.Client, system, question string, tools []tool, maxSteps int, log *slog.Logger) (string, []ToolCall, error) {
	byName := make(map[string]tool, len(tools))
	defs := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		byName[t.def.Name] = t
		defs = append(defs, t.def)
	}

	messages := []llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.Block{{Type: llm.BlockText, Text: question}},
	}}

	if maxSteps <= 0 {
		maxSteps = 12
	}

	var calls []ToolCall
	lastText := ""

	for step := 0; step < maxSteps; step++ {
		reply, err := client.CreateMessage(ctx, llm.CreateMessageParams{
			System:   system,
			Messages: messages,
			Tools:    defs,
		})
		if err != nil {
			log.Debug("librarian: llm call failed",
				slog.Int("step", step),
				slog.String("error", err.Error()))
			return "", calls, fmt.Errorf("llm round-trip (step %d): %w", step, err)
		}

		// Log raw SDK response (truncated) so dropped/misparsed blocks are visible.
		log.Debug("librarian: llm raw response",
			slog.Int("step", step),
			slog.Int("block_count", len(reply.Content)),
			slog.String("raw", reply.RawResponse))

		// Log each block individually.
		for i, b := range reply.Content {
			attrs := []slog.Attr{
				slog.Int("step", step),
				slog.Int("block", i),
				slog.String("type", b.Type),
			}
			switch b.Type {
			case llm.BlockText:
				attrs = append(attrs,
					slog.Int("len", len(b.Text)),
					slog.String("preview", truncate(b.Text, 200)))
			case llm.BlockToolUse:
				attrs = append(attrs,
					slog.String("id", b.ID),
					slog.String("name", b.Name),
					slog.Int("input_len", len(b.Input)),
					slog.String("input_preview", truncate(string(b.Input), 200)))
			case llm.BlockToolResult:
				attrs = append(attrs,
					slog.String("tool_use_id", b.ToolUseID),
					slog.Bool("is_error", b.IsError),
					slog.Int("len", len(b.Content)),
					slog.String("preview", truncate(b.Content, 200)))
			default:
				attrs = append(attrs,
					slog.Int("text_len", len(b.Text)),
					slog.Int("input_len", len(b.Input)),
					slog.String("preview", truncate(b.Text, 200)))
			}
			log.LogAttrs(ctx, slog.LevelDebug, "librarian: llm response block", attrs...)
		}

		messages = append(messages, reply)

		var toolUses []llm.Block
		var text []string
		for _, b := range reply.Content {
			switch b.Type {
			case llm.BlockText:
				if strings.TrimSpace(b.Text) != "" {
					text = append(text, b.Text)
				}
			case llm.BlockToolUse:
				toolUses = append(toolUses, b)
			default:
				log.Debug("librarian: unknown block type skipped",
					slog.Int("step", step),
					slog.String("type", b.Type))
			}
		}
		if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" {
			lastText = t
		}

		// No tool calls => the model has answered (or tried to).
		if len(toolUses) == 0 {
			if lastText == "" {
				log.Debug("librarian: model answered with empty text, forcing synthesis",
					slog.Int("step", step))
				forced, ferr := forceFinalAnswer(ctx, client, system, messages, defs, log)
				if ferr != nil {
					return "", calls, fmt.Errorf("force-final-answer: %w", ferr)
				}
				return forced, calls, nil
			}
			lastText = stripControlTokens(lastText)
			log.Debug("librarian: loop complete (model answered)",
				slog.Int("step", step),
				slog.Int("total_calls", len(calls)),
				slog.String("answer_preview", truncate(lastText, 200)))
			return lastText, calls, nil
		}

		log.Debug("librarian: dispatching tool calls",
			slog.Int("step", step),
			slog.Int("tool_call_count", len(toolUses)))

		// Dispatch every tool_use and feed the results back.
		resultBlocks := make([]llm.Block, 0, len(toolUses))
		for _, tu := range toolUses {
			log.Debug("librarian: tool call",
				slog.Int("step", step),
				slog.String("id", tu.ID),
				slog.String("tool", tu.Name),
				slog.String("input", truncate(string(tu.Input), 200)))

			t, ok := byName[tu.Name]
			if !ok {
				log.Debug("librarian: unknown tool requested",
					slog.Int("step", step),
					slog.String("tool", tu.Name))
				calls = append(calls, ToolCall{Tool: tu.Name, Refused: true, Detail: "unknown tool"})
				resultBlocks = append(resultBlocks, llm.Block{
					Type:      llm.BlockToolResult,
					ToolUseID: tu.ID,
					Content:   fmt.Sprintf("unknown tool %q", tu.Name),
					IsError:   true,
				})
				continue
			}
			res := t.dispatch(ctx, tu.Input)

			log.Debug("librarian: tool result",
				slog.Int("step", step),
				slog.String("tool", tu.Name),
				slog.Bool("is_error", res.isError),
				slog.String("path", res.path),
				slog.String("content_preview", truncate(res.content, 200)))

			calls = append(calls, ToolCall{
				Tool:    tu.Name,
				Path:    res.path,
				Refused: res.isError,
				Detail:  detailIf(res.isError, res.content),
			})
			for _, p := range res.autoReadPaths {
				calls = append(calls, ToolCall{Tool: "read", Path: p})
			}
			resultBlocks = append(resultBlocks, llm.Block{
				Type:      llm.BlockToolResult,
				ToolUseID: tu.ID,
				Content:   res.content,
				IsError:   res.isError,
			})
		}
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: resultBlocks})
	}

	// Step budget exhausted.
	log.Debug("librarian: step budget exhausted",
		slog.Int("max_steps", maxSteps),
		slog.Int("total_calls", len(calls)),
		slog.Bool("answer_empty", lastText == ""))

	if lastText == "" {
		lastText, err := forceFinalAnswer(ctx, client, system, messages, defs, log)
		if err != nil {
			return "", calls, fmt.Errorf("force-final-answer: %w", err)
		}
		return lastText, calls, nil
	}
	return stripControlTokens(lastText), calls, nil
}

// forceFinalAnswer tries two strategies to get a text answer:
//  1. Keep tools + append a user nudge message ("answer now, no more tools").
//  2. If the model still returns tool_calls with no text, retry WITHOUT tools.
//
// Any raw chat-template control tokens (<|...|>) are stripped from the result.
func forceFinalAnswer(ctx context.Context, client llm.Client, system string, messages []llm.Message, tools []llm.ToolDef, log *slog.Logger) (string, error) {
	log.Debug("librarian: forcing final answer")

	nudge := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.Block{{
			Type: llm.BlockText,
			Text: "Based on what you have read, answer my original question now. Do not call any more tools.",
		}},
	}
	finalMessages := make([]llm.Message, len(messages)+1)
	copy(finalMessages, messages)
	finalMessages[len(messages)] = nudge

	// Attempt 1: with tools + nudge.
	reply, err := client.CreateMessage(ctx, llm.CreateMessageParams{
		System:   system,
		Messages: finalMessages,
		Tools:    tools,
	})
	if err != nil {
		return "", err
	}
	answer := extractText(reply)
	log.Debug("librarian: force-final-answer attempt 1 (tools+nudge)",
		slog.Bool("answer_empty", answer == ""),
		slog.String("raw", reply.RawResponse))

	// Attempt 2: if the model returned tool_calls with no text, retry without tools.
	if answer == "" {
		log.Debug("librarian: force-final-answer attempt 2 (no tools)")
		reply2, err2 := client.CreateMessage(ctx, llm.CreateMessageParams{
			System:   system,
			Messages: finalMessages,
		})
		if err2 != nil {
			return "", err2
		}
		answer = extractText(reply2)
		log.Debug("librarian: force-final-answer attempt 2 result",
			slog.Bool("answer_empty", answer == ""),
			slog.String("raw", reply2.RawResponse))
	}

	answer = stripControlTokens(answer)

	log.Debug("librarian: loop complete (force-final-answer)",
		slog.Bool("answer_empty", answer == ""),
		slog.String("answer_preview", truncate(answer, 200)))

	return answer, nil
}

func extractText(msg llm.Message) string {
	var parts []string
	for _, b := range msg.Content {
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

var controlTokenRe = regexp.MustCompile(`<\|[^|]*\|>`)

func stripControlTokens(s string) string {
	return strings.TrimSpace(controlTokenRe.ReplaceAllString(s, ""))
}

func detailIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func blockTypeSummary(blocks []llm.Block) string {
	if len(blocks) == 0 {
		return "(empty)"
	}
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i] = b.Type
	}
	return strings.Join(types, ",")
}

func textPreview(blocks []llm.Block, maxLen int) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "(no text)"
	}
	return truncate(strings.Join(parts, " "), maxLen)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
