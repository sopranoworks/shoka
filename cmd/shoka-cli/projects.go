package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/clientconfig"
	"github.com/sopranoworks/shoka/internal/mcpclient"
)

// cmdProjects connects with the stored token and calls the read-only list_projects
// tool, printing whatever the server returns. It is the credential smoke test: it
// proves the end-to-end path (token-to-self mint -> shoka-cli auth stored it ->
// connect with Bearer -> a real MCP tool call succeeds). It is a thin wrapper —
// build args (none), call the tool, render the result; no Shoka logic client-side.
func cmdProjects(args []string) error {
	fs := flag.NewFlagSet("projects", flag.ContinueOnError)
	env := fs.String("env", clientconfig.DefaultEnvironment, "environment name (selects the instance)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := clientconfig.Load(*env)
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return fmt.Errorf("no token stored for environment %q; run `shoka-cli auth` first", *env)
	}

	ctx := context.Background()
	sess, err := mcpclient.Connect(ctx, cfg.Endpoint, cfg.Token)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.CallTool(ctx, "list_projects", nil)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("list_projects returned an error: %s", renderContent(res))
	}
	out := renderContent(res)
	if out == "" {
		out = "(no output)"
	}
	fmt.Println(out)
	return nil
}

// renderContent flattens a tool result's text content for display.
func renderContent(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
