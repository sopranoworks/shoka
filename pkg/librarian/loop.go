package librarian

import (
	"context"
	"fmt"
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
func runLoop(ctx context.Context, client llm.Client, system, question string, tools []tool, maxSteps int) (string, []ToolCall, error) {
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
		maxSteps = 8
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
			return "", calls, fmt.Errorf("llm round-trip (step %d): %w", step, err)
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
			}
		}
		if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" {
			lastText = t
		}

		// No tool calls => the model has answered.
		if len(toolUses) == 0 {
			return lastText, calls, nil
		}

		// Dispatch every tool_use and feed the results back.
		resultBlocks := make([]llm.Block, 0, len(toolUses))
		for _, tu := range toolUses {
			t, ok := byName[tu.Name]
			if !ok {
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
			calls = append(calls, ToolCall{
				Tool:    tu.Name,
				Path:    res.path,
				Refused: res.isError,
				Detail:  detailIf(res.isError, res.content),
			})
			resultBlocks = append(resultBlocks, llm.Block{
				Type:      llm.BlockToolResult,
				ToolUseID: tu.ID,
				Content:   res.content,
				IsError:   res.isError,
			})
		}
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: resultBlocks})
	}

	// Step budget exhausted: return whatever text we last saw (may be empty).
	return lastText, calls, nil
}

func detailIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}
