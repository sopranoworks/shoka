package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/clientconfig"
	"github.com/sopranoworks/shoka/internal/mcpclient"
)

// cmdFile dispatches the `file` subcommand group. Today it has one member, `add`.
func cmdFile(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("file: a subcommand is required (add)")
	}
	switch args[0] {
	case "add":
		return cmdFileAdd(args[1:])
	default:
		return fmt.Errorf("unknown file subcommand %q (expected: add)", args[0])
	}
}

// cmdFileAdd ingests a LOCAL file into Shoka, byte-faithful. It is a thin client:
// it reads the raw bytes, base64-encodes them, resolves <dest> in the Shoka
// address grammar (B-47) into the write_file tool's namespace/project_name/path
// fields, and calls write_file with content_encoding="base64". It carries NO
// Shoka judgement — the format allowlist and the base64 decode are enforced
// server-side; the only thing done here is the mechanical B-47 address split.
func cmdFileAdd(args []string) error {
	fs := flag.NewFlagSet("file add", flag.ContinueOnError)
	env := fs.String("env", clientconfig.DefaultEnvironment, "environment name (selects the instance)")
	ns := fs.String("namespace", "", "namespace for a relative destination (overrides the config default)")
	proj := fs.String("project", "", "project for a relative destination (overrides the config default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("usage: shoka-cli file add [--env NAME] [--namespace NS] [--project PROJ] <local-path> <dest>")
	}
	localPath, dest := rest[0], rest[1]

	cfg, err := clientconfig.Load(*env)
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return fmt.Errorf("no token stored for environment %q; run `shoka-cli auth` first", *env)
	}

	// Resolve the destination BEFORE reading the file, so an addressing error is
	// reported without side effects.
	rNS, rProj, rPath, err := resolveDest(dest, *ns, *proj, cfg.DefaultNamespace, cfg.DefaultProject)
	if err != nil {
		return err
	}

	// Read the raw bytes verbatim — no transformation, no LLM — and base64-encode
	// so genuinely non-UTF-8 bytes ride the JSON wire intact (the server decodes).
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read local file: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(data)

	ctx := context.Background()
	sess, err := mcpclient.Connect(ctx, cfg.Endpoint, cfg.Token)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.CallTool(ctx, "write_file", map[string]any{
		"namespace":        rNS,
		"project_name":     rProj,
		"path":             rPath,
		"content":          encoded,
		"content_encoding": "base64",
	})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("write_file rejected the ingest: %s", renderContent(res))
	}

	// Print the resolved destination back so the operator sees exactly where the
	// file landed (B-47: the location must be unambiguous). The etag is read from
	// the tool's own structured output — this is display only, not judgement.
	etag := etagFromResult(res)
	if etag != "" {
		fmt.Printf("added -> namespace=%s project=%s path=%s (etag %s)\n", rNS, rProj, rPath, etag)
	} else {
		fmt.Printf("added -> namespace=%s project=%s path=%s\n", rNS, rProj, rPath)
	}
	return nil
}

// resolveDest applies the B-47 address grammar to a destination, filling the
// write_file tool's namespace/project_name/path fields.
//
//   - An ABSOLUTE dest (leading "/") is /namespace/project/path: the first two
//     root-anchored segments are namespace and project, the remainder is the
//     in-project path. It carries ns/project itself, so the flags are not
//     consulted — a flag that CONFLICTS with the absolute dest is an error
//     (ambiguous addressing is never silently resolved; B-37/B-47).
//   - A RELATIVE dest (no leading "/") is an in-project path; ns/project come from
//     the flags, else the config defaults. If neither supplies them it is an
//     explicit error — never a silent "default" normalisation (B-37/B-47).
func resolveDest(dest, flagNS, flagProj, defNS, defProj string) (ns, proj, path string, err error) {
	if strings.HasPrefix(dest, "/") {
		segs := strings.SplitN(strings.TrimPrefix(dest, "/"), "/", 3)
		if len(segs) < 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
			return "", "", "", fmt.Errorf("absolute destination must be /namespace/project/path, got %q", dest)
		}
		ns, proj, path = segs[0], segs[1], segs[2]
		if flagNS != "" && flagNS != ns {
			return "", "", "", fmt.Errorf("conflicting namespace: absolute destination names %q but --namespace is %q", ns, flagNS)
		}
		if flagProj != "" && flagProj != proj {
			return "", "", "", fmt.Errorf("conflicting project: absolute destination names %q but --project is %q", proj, flagProj)
		}
		return ns, proj, path, nil
	}

	ns = firstNonEmpty(flagNS, defNS)
	proj = firstNonEmpty(flagProj, defProj)
	if ns == "" || proj == "" {
		return "", "", "", fmt.Errorf("relative destination %q needs a namespace and project: pass --namespace/--project or set default_namespace/default_project in the client config (no silent \"default\")", dest)
	}
	return ns, proj, dest, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// etagFromResult extracts the etag from a write_file result. The SDK mirrors the
// handler's structured output into the result's text content as JSON, so the
// etag is read from there. Best-effort: an empty string just omits the etag from
// the printed confirmation (display only — not judgement logic).
func etagFromResult(res *mcp.CallToolResult) string {
	var out struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal([]byte(renderContent(res)), &out); err != nil {
		return ""
	}
	return out.ETag
}
