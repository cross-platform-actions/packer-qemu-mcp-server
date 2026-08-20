// Command packer-qemu-mcp-server lets an agent watch and steer an interactive
// operating system installation driven by Packer's QEMU builder, instead of
// waiting blind for an SSH connection that may never arrive.
//
// It runs as an MCP server over stdio. It also re-runs itself, once per build,
// as that build's supervisor:
//
//	packer-qemu-mcp-server                  serve over stdio
//	packer-qemu-mcp-server supervise --dir  hold one build's terminal
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/builds"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/mcpserver"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/supervisor"
)

// version is stamped in at release time from the tag. A binary built any
// other way says so rather than claiming a release it is not.
var version = "dev"

// Environment variables, both optional.
const (
	rootVar   = "PK_BUILDS_DIR"
	packerVar = "PACKER_BIN"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run writes what was asked for to out and what was not to err: asking for the
// help is answered on stdout and succeeds, being unable to answer prints the
// same text on stderr and fails.
func run(args []string, out, err io.Writer) error {
	switch name := command(args); name {
	case builds.SuperviseCommand:
		return supervise(args[1:])
	case "serve":
		return serve()
	case "version", "-v", "--version":
		_, _ = fmt.Fprintln(out, version)
		return nil
	case "help", "-h", "--help":
		usage(out)
		return nil
	default:
		usage(err)
		return fmt.Errorf("unknown command: %s", name)
	}
}

func command(args []string) string {
	if len(args) == 0 {
		return "serve"
	}
	return args[0]
}

// serve offers the build tools to an MCP client over stdio. It owns no build:
// everything it reports is read from the build directories, so restarting it
// costs nothing and losing it costs nothing either.
func serve() error {
	service := builds.New(buildstate.New(root()), packerBinary())
	return mcpserver.New(service, version).Run(context.Background(), &mcp.StdioTransport{})
}

// supervise holds one build's terminal until the build ends. It is started by
// serve, detached, and is not meant to be run by hand.
func supervise(args []string) error {
	flags := flag.NewFlagSet(builds.SuperviseCommand, flag.ContinueOnError)
	dir := flags.String("dir", "", "the build directory to supervise")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("supervise needs a --dir")
	}
	return supervisor.Run(buildstate.At(*dir))
}

func root() string {
	if path := os.Getenv(rootVar); path != "" {
		return path
	}
	return buildstate.DefaultRoot
}

func packerBinary() string {
	if path := os.Getenv(packerVar); path != "" {
		return path
	}
	return "packer"
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `packer-qemu-mcp-server %s

  packer-qemu-mcp-server [serve]           serve the build tools over MCP on stdio
  packer-qemu-mcp-server supervise --dir   hold one build's terminal (started for you)
  packer-qemu-mcp-server version           print the version (-v, --version)
  packer-qemu-mcp-server help              print this (-h, --help)

Environment variables, both optional:

  %-14s where builds keep their state (default %s)
  %-14s the packer binary to run (default packer)
`, version, rootVar, buildstate.DefaultRoot, packerVar)
}
