package dax

// This file is `execute`: the extensions' tools served over MCP on
// standard input and output, run inside the sandbox where they are to
// act, for a session elsewhere to drive.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/tool"
)

// executeKinds are the workspace kinds -kind takes.
var executeKinds = []string{workspace.KindLocal, workspace.KindContainer, workspace.KindRemote}

// runExecute serves the tools of every extension that has Tools,
// dax-coding and the program's less those it left out, over MCP on
// standard input and output, until the client hangs up or a signal
// comes. The pipe is the credential: whoever started the process (a
// docker exec, an ssh, a kubectl exec) is the client, so there is no
// listener and no token.
//
// No config file is read. The sandbox controls the files a config would
// be read from, and the policy is the controlling session's, so the
// flags are all there is. The workspace's processes inherit this
// process's environment with credentials removed, as a session's do;
// the model's key is never here to remove.
//
// Standard output carries MCP and nothing else: for as long as the
// server runs, os.Stdout is standard error, so a stray print cannot
// corrupt the stream, and the tools' processes get no standard input
// and have their output captured.
func runExecute(ctx context.Context, args []string, p program) error {
	fs := flag.NewFlagSet(p.name+" execute", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s execute [flags]\n\nserves the tools over MCP on standard input and output, for a session started with its executor here; reads no config file.\n", p.name)
		fs.PrintDefaults()
	}
	root := fs.String("root", "", "directory the tools act in; default the current directory")
	maxRead := fs.Int64("max-read-bytes", 0, "most bytes of a file the read tool scans, edit rewrites and the bash analyzer reads; 0 is the default")
	passEnv := fs.String("pass-env", "", "comma-separated variables that look like credentials to pass to the tools' processes anyway")
	kind := fs.String("kind", workspace.KindRemote, "the workspace's kind as the session records it: "+strings.Join(executeKinds, ", "))
	ref := fs.String("ref", "", "the workspace's ref as the session records it: an image, a host, an instance; default this host's name")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("execute: unexpected argument %q", fs.Arg(0))
	}
	if !slices.Contains(executeKinds, *kind) {
		return fmt.Errorf("execute: -kind %q: want one of %s", *kind, strings.Join(executeKinds, ", "))
	}
	if *maxRead < 0 {
		return errors.New("execute: -max-read-bytes: want a positive number of bytes")
	}
	var pass []string
	for v := range strings.SplitSeq(*passEnv, ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if strings.ContainsAny(v, "= \t") {
			return fmt.Errorf("execute: -pass-env: %q is not a variable name", v)
		}
		pass = append(pass, v)
	}
	dir := *root
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = wd
	}
	descRef := *ref
	if descRef == "" {
		if h, err := os.Hostname(); err == nil {
			descRef = h
		}
	}

	exts, err := p.selected(choice{MaxReadBytes: *maxRead})
	if err != nil {
		return err
	}
	exts = slices.DeleteFunc(exts, func(e extension.Extension) bool { return e.Tools == nil })
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(pass))
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	defer ws.Close()
	srv, closeTools, err := executor.NewServer(executor.ServeOptions{
		Name: p.name, Version: p.version,
		Descriptor: workspace.Descriptor{Kind: *kind, Ref: descRef, Root: ws.Root()},
	}, exts, extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws), MaxReadBytes: *maxRead})
	if err != nil {
		return err
	}
	defer closeTools()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	mcpOut := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = mcpOut }()
	err = srv.Run(ctx, &sdk.IOTransport{Reader: os.Stdin, Writer: nopCloser{mcpOut}})
	if ctx.Err() != nil {
		// A signal, or the caller's end: the client is gone or going.
		return nil
	}
	return err
}

// nopCloser is standard output as the transport's writer: closing the
// transport leaves it open, as the SDK's stdio transport does.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
