package librarian

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// toolResult is the outcome of one tool dispatch. Errors are encoded as
// isError + content (which becomes a tool_result error fed back to the model),
// never as a Go error that would abort the loop — a refused access is data, not
// a crash. path records the path/dir/query the call targeted, for the trace.
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

// readArgs / listArgs / searchArgs are the tool input shapes.
type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type listArgs struct {
	Dir string `json:"dir"`
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// buildTools returns the read-only, constraint-wrapped tool set for one guard +
// corpus. It is EXACTLY {read, list, search}; there is deliberately no
// write/delete/move (B-49 — an out-of-bounds operation is not a registered
// hand). Every dispatch runs through the guard BEFORE the corpus is touched,
// and every search hit is guard-filtered before the model sees it.
func buildTools(g *Guard, c Corpus) []tool {
	return []tool{
		{
			def: llm.ToolDef{
				Name: "read",
				Description: "Read a UTF-8 text file from the corpus. Supports ranged reads: " +
					"'offset' skips that many leading lines (0-based) and 'limit' caps the " +
					"number of lines returned (omit or 0 for the whole file). Use the 'offset' " +
					"from a search hit to read just the passage of a large file. Paths are " +
					"relative to the corpus root; reads outside the root, of ignored files, " +
					"or through symlinks are refused.",
				Properties: map[string]any{
					"path":   map[string]any{"type": "string", "description": "File path relative to the corpus root."},
					"offset": map[string]any{"type": "integer", "description": "Number of leading lines to skip (0-based). Optional."},
					"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to return. Optional; 0 means all."},
				},
				Required: []string{"path"},
			},
			dispatch: readDispatch(g, c),
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
			dispatch: listDispatch(g, c),
		},
		{
			def: llm.ToolDef{
				Name: "search",
				Description: "Search the corpus for a case-insensitive substring and return matching " +
					"files with a context snippet and the 0-based line of the match ('offset'). " +
					"Pass that 'offset' to the read tool to read just the matching passage of a " +
					"large file. Out-of-root, ignored, and symlink hits are never returned.",
				Properties: map[string]any{
					"query": map[string]any{"type": "string", "description": "Substring to search for (case-insensitive)."},
					"limit": map[string]any{"type": "integer", "description": "Maximum number of hits to return. Optional."},
				},
				Required: []string{"query"},
			},
			dispatch: searchDispatch(g, c),
		},
	}
}

func readDispatch(g *Guard, c Corpus) toolFunc {
	return func(ctx context.Context, input json.RawMessage) toolResult {
		var args readArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return toolResult{content: fmt.Sprintf("invalid arguments: %v", err), isError: true}
		}
		res := toolResult{path: args.Path}

		if _, _, err := g.Resolve(args.Path, false); err != nil {
			res.content, res.isError = fmt.Sprintf("refused: %v", err), true
			return res
		}
		data, err := c.Read(ctx, args.Path, args.Offset, args.Limit)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("cannot read %q: %v", args.Path, err), true
			return res
		}
		res.content = string(data)
		return res
	}
}

func listDispatch(g *Guard, c Corpus) toolFunc {
	return func(ctx context.Context, input json.RawMessage) toolResult {
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

		_, rel, err := g.Resolve(dir, true)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("refused: %v", err), true
			return res
		}
		entries, err := c.List(ctx, dir)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("cannot list %q: %v", dir, err), true
			return res
		}

		var out []string
		for _, e := range entries {
			childRel := e.Name
			if rel != "." && rel != "" {
				childRel = rel + "/" + e.Name
			}
			// Drop ignored entries and symlinks — the guard refuses both.
			if _, _, gerr := g.Resolve(childRel, e.IsDir); gerr != nil {
				continue
			}
			name := e.Name
			if e.IsDir {
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

func searchDispatch(g *Guard, c Corpus) toolFunc {
	return func(ctx context.Context, input json.RawMessage) toolResult {
		var args searchArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return toolResult{content: fmt.Sprintf("invalid arguments: %v", err), isError: true}
		}
		res := toolResult{path: args.Query}
		if strings.TrimSpace(args.Query) == "" {
			res.content, res.isError = "query is required", true
			return res
		}

		hits, err := c.Search(ctx, args.Query, args.Limit)
		if err != nil {
			res.content, res.isError = fmt.Sprintf("search failed: %v", err), true
			return res
		}

		var lines []string
		for _, h := range hits {
			// Guard-filter: drop any hit whose path is out-of-root, ignored, or a
			// symlink BEFORE it (or its snippet) ever reaches the model.
			if _, _, gerr := g.Resolve(h.Path, false); gerr != nil {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s (offset %d): %s", h.Path, h.Offset, h.Snippet))
		}
		if len(lines) == 0 {
			res.content = "(no matches)"
		} else {
			res.content = strings.Join(lines, "\n")
		}
		return res
	}
}
