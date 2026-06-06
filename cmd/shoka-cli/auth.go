package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shoka/mcp-server/internal/clientconfig"
)

// cmdAuth stores the display-once access token (minted by the server's admin-gated
// token-to-self action) into the per-environment client config. The token is read
// from --token-file or stdin — NEVER from the command line, which would leak the
// secret into shell history. An existing config's fields are preserved unless the
// matching flag overrides them, so `shoka-cli auth --env prod` can refresh just the
// token while keeping the endpoint and defaults.
func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	env := fs.String("env", clientconfig.DefaultEnvironment, "environment name (selects the instance)")
	endpoint := fs.String("endpoint", "", "MCP endpoint URL (kept from existing config if omitted)")
	tokenFile := fs.String("token-file", "", "read the token from this file instead of stdin")
	defNS := fs.String("default-namespace", "", "optional default namespace for ergonomics")
	defProj := fs.String("default-project", "", "optional default project for ergonomics")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Start from the existing config (if any) so we update rather than clobber.
	cfg, err := clientconfig.Load(*env)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		cfg = &clientconfig.Config{}
	}

	if *endpoint != "" {
		cfg.Endpoint = strings.TrimSpace(*endpoint)
	}
	if *defNS != "" {
		cfg.DefaultNamespace = strings.TrimSpace(*defNS)
	}
	if *defProj != "" {
		cfg.DefaultProject = strings.TrimSpace(*defProj)
	}

	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("no token provided (paste it on stdin or use --token-file)")
	}
	cfg.Token = token

	if cfg.Endpoint == "" {
		return fmt.Errorf("no endpoint configured for environment %q; pass --endpoint URL", *env)
	}

	if err := clientconfig.Save(*env, cfg); err != nil {
		return err
	}
	path, _ := clientconfig.Path(*env)
	// Confirm WITHOUT echoing the token.
	fmt.Printf("Stored token for environment %q at %s\n", *env, path)
	return nil
}

// readToken reads the token from a file (when path != "") or from stdin, trimming
// surrounding whitespace/newlines. It deliberately accepts no token argument so the
// secret never appears in argv.
func readToken(path string) (string, error) {
	var (
		data []byte
		err  error
	)
	if path != "" {
		data, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
	} else {
		if isInteractiveStdin() {
			fmt.Fprintln(os.Stderr, "Paste the token, then press Enter and Ctrl-D:")
		}
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
	}
	return strings.TrimSpace(string(data)), nil
}

// isInteractiveStdin reports whether stdin is a terminal, so the prompt is shown
// only to a human (and never pollutes a piped/agent invocation).
func isInteractiveStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
