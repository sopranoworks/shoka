// Command shoka-cli is the Shoka maintenance CLI for humans and agents (B-46b
// foundation). It is a SEPARATE binary from the server (cmd/server) — deliberately
// so: a same-named/different-behaviour binary risks accidentally starting the
// server, so the maintenance tool is kept distinct. Building or running shoka-cli
// never starts a server.
//
// shoka-cli is a THIN MCP client: it connects to a Shoka MCP endpoint with a
// Bearer token from the local client config and calls tools. It carries NO
// Shoka-specific judgement — all ingest/format/catalog logic lives in the
// server-side tools; the client only invokes them.
//
// Subcommands (this foundation):
//
//	shoka-cli auth      Store the display-once access token into the client config.
//	shoka-cli projects  Connect with the stored token and list projects (a
//	                    read-only credential check / smoke test).
//
// `shoka file add` and `shoka skill install` are later steps and are NOT here. The
// subcommand surface is small, so it stays on the repo's stdlib `flag` convention
// (cmd/server uses it too) with hand-rolled dispatch — no subcommand-library
// dependency until the tree is deep enough to actually need one.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shoka-cli: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "auth":
		return cmdAuth(args[1:])
	case "projects":
		return cmdProjects(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `shoka-cli — Shoka maintenance CLI (thin MCP client)

Usage:
  shoka-cli auth      [--env NAME] [--endpoint URL] [--token-file PATH] \
                      [--default-namespace NS] [--default-project PROJ]
  shoka-cli projects  [--env NAME]

The access token is read from --token-file or stdin (never from the command line,
which would leak it into shell history) and stored at
  <user-config-dir>/shoka/<env>/config.yaml  (file 0600, dir 0700).
`)
}
