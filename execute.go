package dax

// This file is `execute`: the extensions' tools served over MCP on
// standard input and output, or over HTTP with -listen, run inside the
// sandbox where they are to act, for a session elsewhere to drive.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

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
// With -listen it serves over MCP's streamable HTTP transport instead,
// each client a session of its own over the same workspace, until a
// signal comes. Nothing authenticates a client yet, so the address
// must be a loopback one, reached from elsewhere through a tunnel,
// unless -insecure-no-auth says to serve whoever reaches it.
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
		fmt.Fprintf(fs.Output(), "usage: %s execute [flags]\n\nserves the tools over MCP on standard input and output, or over HTTP with -listen, for a session started with its executor here; reads no config file.\n", p.name)
		fs.PrintDefaults()
	}
	root := fs.String("root", "", "directory the tools act in; default the current directory")
	maxRead := fs.Int64("max-read-bytes", 0, fmt.Sprintf("the most a session's max_read_bytes may be (the most bytes of a file the read tool scans, edit rewrites and the bash analyzer reads): a session gets its own setting up to this, and a dax older than the setting gets this; 0 is dax's default, %d", tool.DefaultMaxRead))
	passEnv := fs.String("pass-env", "", "comma-separated variables that look like credentials to pass to the tools' processes anyway")
	kind := fs.String("kind", workspace.KindRemote, "the workspace's kind as the session records it: "+strings.Join(executeKinds, ", "))
	ref := fs.String("ref", "", "the workspace's ref as the session records it: an image, a host, an instance; default this host's name")
	listen := fs.String("listen", "", "serve over HTTP at this address (127.0.0.1:7777) instead of on standard input and output; a loopback address unless -insecure-no-auth")
	insecure := fs.Bool("insecure-no-auth", false, "with -listen, take an address that is not loopback: nothing authenticates a client, so anyone who can reach it can run commands here")
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
	if *insecure && *listen == "" {
		return errors.New("execute: -insecure-no-auth is for -listen")
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

	var ln net.Listener
	if *listen != "" {
		var err error
		if ln, err = executeListen(*listen, *insecure); err != nil {
			return err
		}
		defer ln.Close()
	}

	// Each session's tools are built for it, at the read limit it asks
	// for up to -max-read-bytes: dax-coding's, and the ToolEnv's.
	exts := func(maxRead int64) ([]extension.Extension, error) {
		exts, err := p.selected(choice{MaxReadBytes: maxRead})
		if err != nil {
			return nil, err
		}
		return slices.DeleteFunc(exts, func(e extension.Extension) bool { return e.Tools == nil }), nil
	}
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(pass))
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	defer ws.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The processes started for the session end at a signal before
	// the session's close waits for the requests reading them.
	srv, err := executor.NewServer(executor.ServeOptions{
		Name: p.name, Version: p.version,
		Descriptor:   workspace.Descriptor{Kind: *kind, Ref: descRef, Root: ws.Root()},
		Done:         ctx.Done(),
		MaxReadBytes: *maxRead,
	}, exts, extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		return err
	}
	defer srv.Close()
	if ln != nil {
		return serveExecute(ctx, ln, srv, ws.Root(), *insecure)
	}

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

// executeListen listens on addr for `execute -listen`: a loopback
// address, or with insecure any. Nothing authenticates a client yet,
// so whoever reaches the address runs the tools.
func executeListen(addr string, insecure bool) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("execute: -listen %q: %w", addr, err)
	}
	if port == "" || port == "0" {
		port = "7777"
	}
	refuse := fmt.Errorf("execute: -listen %q: nothing authenticates a client yet, so dax execute listens on loopback only (127.0.0.1, ::1 or localhost); from another machine, tunnel to it: ssh -L %s:127.0.0.1:%s <this host>; -insecure-no-auth serves anyone who can reach the address", addr, port, port)
	if !insecure && !isLoopbackHost(host) {
		return nil, refuse
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("execute: -listen: %w", err)
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !insecure && (!ok || !a.IP.IsLoopback()) {
		ln.Close()
		return nil, refuse
	}
	return ln, nil
}

// serveExecute serves srv over HTTP on ln until ctx ends, and says on
// standard error where, which the user connects -executor to. srv's
// processes end with ctx (ServeOptions.Done) before the server shuts
// down.
func serveExecute(ctx context.Context, ln net.Listener, srv *executor.Server, root string, insecure bool) error {
	hs := &http.Server{Handler: executor.Handler(srv, ctx.Done()), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	url := "http://" + servedHost(ln.Addr())
	fmt.Fprintf(os.Stderr, "dax execute: serving %s at %s\n", root, url)
	if a, ok := ln.Addr().(*net.TCPAddr); insecure && (!ok || !a.IP.IsLoopback()) {
		fmt.Fprintf(os.Stderr, "dax execute: warning: no authentication: anyone who can reach %s can run commands in %s\n", url, root)
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-served:
	}
	sctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	hs.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("execute: %w", err)
	}
	return nil
}

// nopCloser is standard output as the transport's writer: closing the
// transport leaves it open, as the SDK's stdio transport does.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
