package librarian

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// toolResult is the outcome of one tool dispatch. Errors are encoded as
// isError + content (which becomes a tool_result error fed back to the model),
// never as a Go error that would abort the loop — a refused access is data, not
// a crash. path records the path/dir the call targeted, for the trace.
type toolResult struct {
	content string
	isError bool
	path    string
}

// toolFunc dispatches one tool call from its raw JSON arguments.
type toolFunc func(ctx context.Context, input json.RawMessage) toolResult

// tool pairs an LLM-facing definition with its Go dispatch.
type tool struct {
	def      llm.ToolDef
	dispatch toolFunc
}

// readArgs / listArgs are the tool input shapes.
type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type listArgs struct {
	Dir string `json:"dir"`
}

// buildTools returns the read-only, constraint-wrapped tool set for one guard.
// It is EXACTLY {read, list}; there is deliberately no write/delete/move/search
// (B-49 — an out-of-bounds operation is not a registered hand). Every dispatch
// runs through the guard BEFORE touching the filesystem.
func buildTools(g *Guard) []tool {
	return []tool{
		{
			def: llm.ToolDef{
				Name: "read",
				Description: "Read a UTF-8 text file from the corpus. Supports ranged reads: " +
					"'offset' skips that many leading lines (0-based) and 'limit' caps the " +
					"number of lines returned (omit or 0 for the whole file). Paths are " +
					"relative to the corpus root; reads outside the root, of ignored files, " +
					"or through symlinks are refused.",
				Properties: map[string]any{
					"path":   map[string]any{"type": "string", "description": "File path relative to the corpus root."},
					"offset": map[string]any{"type": "integer", "description": "Number of leading lines to skip (0-based). Optional."},
					"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to return. Optional; 0 means all."},
				},
				Required: []string{"path"},
			},
			dispatch: readDispatch(g),
		},
		{
			def: llm.ToolDef{
				Name: "list",
				Description: "List the entries of a directory in the corpus. 'dir' is relative to " +
					"the corpus root (omit or '.' for the root). Directories end with '/'. " +
					"Ignored entries and symlinks are omitted; listing outside the root is refused.",
				Properties: map[string]any{
					"dir": map[string]any{"type": "string", "description": "Directory path relative to the corpus root. Optional; defaults to the root."},
				},
				Required: []string{},
			},
			dispatch: listDispatch(g),
		},
	}
}

func readDispatch(g *Guard) toolFunc {
	return func(_ context.Context, input json.RawMessage) toolResult {
		var args readArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return toolResult{content: fmt.Sprintf("invalid arguments: %v", err), isError: true}
		}
		res := toolResult{path: args.Path}

		full, _, err := g.Resolve(args.Path, false)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("refused: %v", err), true
			return res
		}
		info, err := os.Stat(full)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("cannot read %q: %v", args.Path, err), true
			return res
		}
		if info.IsDir() {
			res.content, res.isError = fmt.Sprintf("%q is a directory; use list", args.Path), true
			return res
		}
		data, err := os.ReadFile(full)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("cannot read %q: %v", args.Path, err), true
			return res
		}
		res.content = applyLineRange(string(data), args.Offset, args.Limit)
		return res
	}
}

// applyLineRange returns the [offset, offset+limit) slice of lines. A negative
// offset is clamped to 0; limit <= 0 means "to the end". The shape is built for
// the future ~368k single-file backlog (Stage 4): read spans, not the whole.
func applyLineRange(content string, offset, limit int) string {
	if offset <= 0 && limit <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], "\n")
}

func listDispatch(g *Guard) toolFunc {
	return func(_ context.Context, input json.RawMessage) toolResult {
		var args listArgs
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return toolResult{content: fmt.Sprintf("invalid arguments: %v", err), isError: true}
			}
		}
		dir := args.Dir
		if dir == "" {
			dir = "."
		}
		res := toolResult{path: dir}

		full, rel, err := g.Resolve(dir, true)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("refused: %v", err), true
			return res
		}
		entries, err := os.ReadDir(full)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("cannot list %q: %v", dir, err), true
			return res
		}

		var out []string
		for _, e := range entries {
			// DirEntry.Type comes from Lstat, so a symlink is reported as such
			// and SKIPPED — never followed (the B-73 correction).
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			childRel := e.Name()
			if rel != "." && rel != "" {
				childRel = rel + "/" + e.Name()
			}
			if g.Ignored(childRel, e.IsDir()) {
				continue
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			out = append(out, name)
		}
		sort.Strings(out)
		if len(out) == 0 {
			res.content = "(empty)"
		} else {
			res.content = strings.Join(out, "\n")
		}
		return res
	}
}
