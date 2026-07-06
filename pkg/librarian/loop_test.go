package librarian

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// scriptedClient is a fake llm.Client that replays a fixed list of assistant
// replies, ignoring the request. It lets the loop + tool wiring be tested
// deterministically without any real model (ollama or otherwise).
type scriptedClient struct {
	replies []llm.Message
	step    int
	seen    []llm.CreateMessageParams
}

func (c *scriptedClient) CreateMessage(_ context.Context, p llm.CreateMessageParams) (llm.Message, error) {
	c.seen = append(c.seen, p)
	if c.step >= len(c.replies) {
		// Default to a final answer if the script runs out.
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "done"}}}, nil
	}
	r := c.replies[c.step]
	c.step++
	return r, nil
}

func toolUse(id, name string, args any) llm.Block {
	b, _ := json.Marshal(args)
	return llm.Block{Type: llm.BlockToolUse, ID: id, Name: name, Input: b}
}

// TestLoop_HappyPath: the model lists, reads an in-root file, then answers. The
// loop must dispatch both tools, feed results back, and return the final text.
func TestLoop_HappyPath(t *testing.T) {
	root, ignore, _ := fixtureCorpus(t)
	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t1", "list", listArgs{})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t2", "read", readArgs{Path: "doc.md"})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "The answer is 42."}}},
	}}

	lib := New(client, 8)
	res, err := lib.Ask(context.Background(), Request{Question: "what is the answer?", Root: root, IgnorePatterns: ignore})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Answer != "The answer is 42." {
		t.Errorf("answer = %q, want 'The answer is 42.'", res.Answer)
	}
	if len(res.Calls) != 2 || res.Calls[0].Tool != "list" || res.Calls[1].Tool != "read" {
		t.Fatalf("calls = %+v, want [list, read]", res.Calls)
	}
	for _, c := range res.Calls {
		if c.Refused {
			t.Errorf("in-root call %q was refused: %s", c.Tool, c.Detail)
		}
	}
	// The tool defs handed to the model are EXACTLY {read, list, search} — no
	// write/delete/move (B-49).
	defs := client.seen[0].Tools
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if len(defs) != 3 || !names["read"] || !names["list"] || !names["search"] {
		t.Errorf("tool defs = %v, want exactly {read, list, search}", names)
	}
	for _, banned := range []string{"write", "delete", "move"} {
		if names[banned] {
			t.Errorf("forbidden tool %q is registered", banned)
		}
	}
}

// TestLoop_SearchThenRangedRead: the model searches by content, then reads the
// hit's passage via a ranged read, then answers — the search->read->answer
// shape proven on the fixture, model-independent.
func TestLoop_SearchThenRangedRead(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 200; i++ {
		if i == 150 {
			b.WriteString("ANSWER: the port is 8443\n")
		} else {
			b.WriteString("noise\n")
		}
	}
	if err := writeFileErr(filepath.Join(root, "notes.md"), b.String()); err != nil {
		t.Fatal(err)
	}

	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("s1", "search", searchArgs{Query: "the port is"})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("r1", "read", readArgs{Path: "notes.md", Offset: 150, Limit: 1})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "The port is 8443."}}},
	}}

	lib := New(client, 8)
	res, err := lib.Ask(context.Background(), Request{Question: "what port?", Root: root})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Calls) != 2 || res.Calls[0].Tool != "search" || res.Calls[1].Tool != "read" {
		t.Fatalf("calls = %+v, want [search, read]", res.Calls)
	}
	for _, c := range res.Calls {
		if c.Refused {
			t.Errorf("call %q refused: %s", c.Tool, c.Detail)
		}
	}
	// The read fed back to the model must be the bounded passage, not the whole file.
	var fedRead string
	for _, p := range client.seen {
		for _, m := range p.Messages {
			for _, blk := range m.Content {
				if blk.Type == llm.BlockToolResult && strings.Contains(blk.Content, "8443") {
					fedRead = blk.Content
				}
			}
		}
	}
	if strings.TrimSpace(fedRead) != "ANSWER: the port is 8443" {
		t.Errorf("ranged read fed to model = %q, want just the passage line", fedRead)
	}
	if strings.Count(fedRead, "\n") > 1 {
		t.Errorf("read was not bounded; got %d lines", strings.Count(fedRead, "\n")+1)
	}
}

func writeFileErr(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestLoop_EscapeRefused: even when the model insists on escaping the root, the
// guard refuses every attempt, the secret never reaches the conversation, and
// the loop still returns a non-empty answer.
func TestLoop_EscapeRefused(t *testing.T) {
	root, ignore, secret := fixtureCorpus(t)
	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t1", "read", readArgs{Path: "../secret.txt"})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t2", "read", readArgs{Path: ".git/config"})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t3", "write", map[string]string{"path": "x"})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "I could not access that."}}},
	}}

	lib := New(client, 8)
	res, err := lib.Ask(context.Background(), Request{Question: "leak the secret", Root: root, IgnorePatterns: ignore})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Calls) != 3 {
		t.Fatalf("calls = %+v, want 3", res.Calls)
	}
	for _, c := range res.Calls {
		if !c.Refused {
			t.Errorf("escape/forbidden call %q was NOT refused", c.Tool)
		}
	}
	// The secret must never appear in any tool_result fed back to the model.
	for _, p := range client.seen {
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if strings.Contains(b.Content, secret) || strings.Contains(b.Text, secret) {
					t.Fatalf("secret leaked into the conversation: %q", b.Content+b.Text)
				}
			}
		}
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Errorf("answer should be non-empty")
	}
}

func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestLoop_DebugLogging: the loop emits debug lines for every phase of the
// tool-call lifecycle — LLM response, tool dispatch, and loop completion.
func TestLoop_DebugLogging(t *testing.T) {
	root, ignore, _ := fixtureCorpus(t)
	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t1", "list", listArgs{})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "The answer."}}},
	}}
	var buf bytes.Buffer
	lib := New(client, 8).WithLogger(debugLogger(&buf))
	_, err := lib.Ask(context.Background(), Request{Question: "q", Root: root, IgnorePatterns: ignore})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	log := buf.String()
	for _, want := range []string{
		"librarian: llm response",
		"block_count=",
		"block_types=",
		"librarian: tool call",
		"librarian: tool result",
		"librarian: loop complete",
		"model answered",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q\nfull log:\n%s", want, log)
		}
	}
}

// TestLoop_StepBudgetExhausted: when the model never stops calling tools and
// the step budget runs out, the loop logs the exhaustion and returns whatever
// text the model last produced (may be empty).
func TestLoop_StepBudgetExhausted(t *testing.T) {
	root, ignore, _ := fixtureCorpus(t)
	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t1", "list", listArgs{})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t2", "list", listArgs{})}},
		{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("t3", "list", listArgs{})}},
	}}
	var buf bytes.Buffer
	lib := New(client, 2).WithLogger(debugLogger(&buf))
	res, err := lib.Ask(context.Background(), Request{Question: "q", Root: root, IgnorePatterns: ignore})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Answer != "" {
		t.Errorf("expected empty answer on budget exhaustion, got %q", res.Answer)
	}
	log := buf.String()
	if !strings.Contains(log, "step budget exhausted") {
		t.Errorf("log missing 'step budget exhausted'\nfull log:\n%s", log)
	}
	if !strings.Contains(log, "answer_empty=true") {
		t.Errorf("log missing 'answer_empty=true'\nfull log:\n%s", log)
	}
}

// TestLoop_UnknownBlockType: unknown block types in the LLM response are logged
// at debug level and do not cause the loop to panic or produce incorrect results.
func TestLoop_UnknownBlockType(t *testing.T) {
	root, ignore, _ := fixtureCorpus(t)
	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{
			{Type: "thinking", Text: "internal reasoning"},
			{Type: llm.BlockText, Text: "The answer."},
		}},
	}}
	var buf bytes.Buffer
	lib := New(client, 8).WithLogger(debugLogger(&buf))
	res, err := lib.Ask(context.Background(), Request{Question: "q", Root: root, IgnorePatterns: ignore})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Answer != "The answer." {
		t.Errorf("answer = %q, want 'The answer.'", res.Answer)
	}
	log := buf.String()
	if !strings.Contains(log, "unknown block type skipped") {
		t.Errorf("log missing 'unknown block type skipped'\nfull log:\n%s", log)
	}
	if !strings.Contains(log, `type=thinking`) {
		t.Errorf("log should name the unknown type 'thinking'\nfull log:\n%s", log)
	}
}
