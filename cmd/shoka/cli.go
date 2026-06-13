package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/shoka/mcp-server/internal/config"
	"github.com/shoka/mcp-server/internal/storage/wal"
)

// runCLI dispatches the `shoka project` and `shoka wal` subcommand groups.
func runCLI(args []string) error {
	switch args[0] {
	case "project":
		return runProjectCmd(args[1:])
	case "wal":
		return runWALCmd(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// --- wal subcommands (read <base_dir>/.shoka/wal/ directly; no server needed) ---

func runWALCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shoka wal <list|dump|extract> [flags]")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("wal "+sub, flag.ContinueOnError)
	baseDir := fs.String("base-dir", "", "storage base directory (overrides --config)")
	configPath := fs.String("config", "shoka.yaml", "config file to read base_dir from when --base-dir is unset")
	out := fs.String("o", "", "output path (wal extract)")

	// list has no positional; dump/extract take <seq> first, then flags.
	var seqStr string
	if sub == "list" {
		if err := fs.Parse(rest); err != nil {
			return err
		}
	} else {
		if len(rest) == 0 {
			return fmt.Errorf("usage: shoka wal %s <seq> [flags]", sub)
		}
		seqStr = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
	}

	dir, err := resolveBaseDir(*baseDir, *configPath)
	if err != nil {
		return err
	}
	log, err := wal.Open(dir)
	if err != nil {
		return fmt.Errorf("open WAL: %w", err)
	}
	defer log.Close()

	switch sub {
	case "list":
		heads, err := log.ListPending()
		if err != nil {
			return err
		}
		if len(heads) == 0 {
			fmt.Println("WAL is empty (no pending commits).")
			return nil
		}
		fmt.Printf("%-8s  %-24s  %-7s  %-8s  %s\n", "SEQ", "TS", "OP", "SIZE", "PROJECT/PATH")
		for _, h := range heads {
			fmt.Printf("%-8d  %-24s  %-7s  %-8d  %s/%s/%s\n",
				h.Seq, h.Ts.UTC().Format("2006-01-02T15:04:05Z"), h.Op, h.Size, h.Namespace, h.Project, h.Path)
		}
		return nil

	case "dump":
		seq, err := parseSeq(seqStr)
		if err != nil {
			return err
		}
		e, err := log.ReadByID(seq)
		if err != nil {
			return err
		}
		fmt.Printf("seq:       %d\n", e.Seq)
		fmt.Printf("ts:        %s\n", e.Ts.UTC().Format("2006-01-02T15:04:05.000000000Z"))
		fmt.Printf("namespace: %s\n", e.Namespace)
		fmt.Printf("project:   %s\n", e.Project)
		fmt.Printf("path:      %s\n", e.Path)
		fmt.Printf("op:        %s\n", e.Op)
		fmt.Printf("version:   %s\n", e.Version)
		fmt.Printf("size:      %d\n", e.Size)
		fmt.Printf("--- content ---\n%s\n", string(e.Content))
		return nil

	case "extract":
		seq, err := parseSeq(seqStr)
		if err != nil {
			return err
		}
		if *out == "" {
			return fmt.Errorf("wal extract requires -o <path>")
		}
		e, err := log.ReadByID(seq)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, e.Content, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %d bytes from WAL entry %d to %s\n", len(e.Content), seq, *out)
		return nil

	default:
		return fmt.Errorf("unknown wal subcommand %q (want list|dump|extract)", sub)
	}
}

func parseSeq(s string) (uint64, error) {
	seq, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid seq %q: %w", s, err)
	}
	return seq, nil
}

func resolveBaseDir(baseDir, configPath string) (string, error) {
	if baseDir != "" {
		return baseDir, nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", fmt.Errorf("could not determine base_dir (pass --base-dir or a valid --config): %w", err)
	}
	return cfg.Storage.BaseDir, nil
}

// --- project subcommands (call the running server's admin API) ---

func runProjectCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shoka project <status|rescan|recover> <namespace>/<project> [flags]")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("project "+sub, flag.ContinueOnError)
	configPath := fs.String("config", "shoka.yaml", "config file (for the server URL and auth token)")
	acceptWorkingTree := fs.Bool("accept-working-tree", false, "recover: adopt the working tree as truth")
	acceptHead := fs.Bool("accept-head", false, "recover: discard working-tree changes back to git HEAD")

	// <namespace>/<project> comes first, then any flags.
	if len(rest) == 0 {
		return fmt.Errorf("usage: shoka project %s <namespace>/<project> [flags]", sub)
	}
	ns, project, err := splitNSProject(rest[0])
	if err != nil {
		return err
	}
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	q := url.Values{"namespace": {ns}, "project": {project}}

	switch sub {
	case "status":
		return adminCall(cfg, http.MethodGet, "/api/project/status", q)
	case "rescan":
		return adminCall(cfg, http.MethodPost, "/api/project/rescan", q)
	case "recover":
		switch {
		case *acceptWorkingTree && *acceptHead:
			return fmt.Errorf("pass only one of --accept-working-tree / --accept-head")
		case *acceptWorkingTree:
			q.Set("mode", "accept-working-tree")
		case *acceptHead:
			q.Set("mode", "accept-head")
		default:
			return fmt.Errorf("recover requires --accept-working-tree or --accept-head")
		}
		return adminCall(cfg, http.MethodPost, "/api/project/recover", q)
	default:
		return fmt.Errorf("unknown project subcommand %q (want status|rescan|recover)", sub)
	}
}

func splitNSProject(arg string) (string, string, error) {
	parts := strings.SplitN(arg, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <namespace>/<project>, got %q", arg)
	}
	return parts[0], parts[1], nil
}

func adminCall(cfg *config.Config, method, path string, q url.Values) error {
	u := webBaseURL(cfg) + path + "?" + q.Encode()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return err
	}
	if cfg.Server.Auth.Enabled && len(cfg.Server.Auth.Tokens) > 0 {
		req.Header.Set("Authorization", "Bearer "+cfg.Server.Auth.Tokens[0])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed (is the server running?): %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(body)))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}

func webBaseURL(cfg *config.Config) string {
	listen := cfg.Server.HTTP.Listen
	host := listen
	if strings.HasPrefix(listen, ":") {
		host = "localhost" + listen
	}
	scheme := "http"
	if cfg.Server.HTTP.TLS.Enabled {
		scheme = "https"
	}
	return scheme + "://" + host
}
