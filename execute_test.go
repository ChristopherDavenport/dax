package dax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/facts/factspolicy"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/tool"
)

// executeRoot is set in the environment of the test binary run as
// `dax execute`.
const executeRoot = "DAX_TEST_EXECUTE_ROOT"

// TestMain lets the test binary be `dax execute` over the directory in
// DAX_TEST_EXECUTE_ROOT, with an extension whose tool prints to
// standard output as a careless tool might.
func TestMain(m *testing.M) {
	if dir := os.Getenv(executeRoot); dir != "" {
		os.Exit(Main(context.Background(), []string{"execute", "-root", dir, "-kind", "container", "-ref", "box"}, WithExtension(stray)))
	}
	os.Exit(m.Run())
}

// stray is an extension whose tool prints to os.Stdout.
var stray = extension.Extension{Name: "stray", Tools: func(extension.ToolEnv) []agenttool.Tool {
	return []agenttool.Tool{agenttool.NewFunc("shout", "Print to standard output.", nil, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		fmt.Println("stray output")
		return agenttool.Text("shouted"), nil
	})}
}}

// lockedBuffer is a buffer two goroutines may use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// `dax execute` in another process: its standard output carries MCP and
// nothing else, a stray print included; its tools' processes read no
// standard input; it says where it runs as the flags told it; and a
// stamp this process minted, of a bash plan or of a file tool's facts,
// is refused there, because the key that signs stamps is the
// executor's own. The executor's own stamp runs.
func TestExecuteServesTheToolsOverStdio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o644)

	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), executeRoot+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr, seen lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := struct {
		io.Reader
		io.Closer
	}{io.TeeReader(stdout, &seen), stdout}
	r, err := mcpclient.Connect(ctx, &sdk.IOTransport{Reader: reader, Writer: stdin}, mcpclient.WithClaims(), mcpclient.WithNotificationGrace(0))
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
	}
	closed := false
	defer func() {
		if !closed {
			r.Close()
			cmd.Wait()
		}
	}()
	byName := map[string]agenttool.Tool{}
	for _, tl := range r.Tools() {
		byName[tl.Name()] = tl
	}
	run := func(name, args string) (string, error) {
		t.Helper()
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("no tool %s", name)
		}
		res, err := tl.Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(args)})
		return res.Output.String(), err
	}

	raw, _ := json.Marshal(r.Session().InitializeResult().Capabilities.Experimental[executor.CapabilityKey])
	var c struct {
		Descriptor struct{ Kind, Ref, Root string }
		Extensions []string
	}
	json.Unmarshal(raw, &c)
	if c.Descriptor.Kind != workspace.KindContainer || c.Descriptor.Ref != "box" || c.Descriptor.Root != dir ||
		strings.Join(c.Extensions, " ") != "dax-coding stray" {
		t.Errorf("capability %s", raw)
	}

	// Standard input is not the tools' to read: cat gets end of file.
	if out, err := run("bash", `{"command":"cat"}`); err != nil || !strings.Contains(out, "[exit 0]") {
		t.Errorf("cat = %q, %v", out, err)
	}
	if out, err := run("shout", `{}`); err != nil || out != "shouted" {
		t.Errorf("shout = %q, %v", out, err)
	}

	// A stamp minted here is no stamp there.
	ws, err := workspace.NewLocal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	files := tool.NewFiles(ws)
	mine, changed, err := tool.StampArgs(ctx, &tool.Analyzer{Files: files}, json.RawMessage(`{"command":"cat notes.txt"}`))
	if err != nil || !changed {
		t.Fatalf("StampArgs = %s, %v, %v", mine, changed, err)
	}
	if out, err := run("bash", string(mine)); err == nil || !strings.Contains(err.Error()+out, "ask again") {
		t.Errorf("bash with this process's stamp = %q, %v; want it refused", out, err)
	}
	read := tool.Read(files)
	d, err := factspolicy.Hook([]agenttool.Tool{read})(ctx, agentturn.ToolCallInfo{Call: &openresponses.FunctionCall{Name: "read", Arguments: `{"path":"notes.txt"}`}, Args: json.RawMessage(`{"path":"notes.txt"}`)})
	if err != nil || d == nil || !strings.Contains(string(d.Args), "dax_stamp") {
		t.Fatalf("read not stamped here: %+v, %v", d, err)
	}
	if out, err := run("read", string(d.Args)); err == nil || !strings.Contains(err.Error()+out, "ask again") {
		t.Errorf("read with this process's stamp = %q, %v; want it refused", out, err)
	}

	// The executor's own stamp, from its facts claim, runs.
	f, _, err := agenttool.FactsOf(ctx, byName["bash"], json.RawMessage(`{"command":"cat notes.txt"}`))
	if err != nil || f.Rewrite == nil {
		t.Fatalf("facts = %+v, %v", f, err)
	}
	if out, err := run("bash", string(f.Rewrite)); err != nil || !strings.Contains(out, "notes") {
		t.Errorf("bash with the executor's stamp = %q, %v", out, err)
	}

	r.Close()
	if err := cmd.Wait(); err != nil {
		t.Errorf("execute exited: %v\nstderr: %s", err, stderr.String())
	}
	closed = true
	sc := bufio.NewScanner(strings.NewReader(seen.String()))
	sc.Buffer(nil, 1<<20)
	lines := 0
	for sc.Scan() {
		var msg struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Errorf("standard output carried %q", sc.Text())
		}
		lines++
	}
	if lines == 0 {
		t.Error("standard output carried nothing")
	}
	if !strings.Contains(stderr.String(), "stray output") {
		t.Errorf("the stray print is not on standard error: %q", stderr.String())
	}
}

// execute takes its own flags and refuses what it cannot serve.
func TestExecuteRefusesBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"execute", "-kind", "vm"},
		{"execute", "-max-read-bytes", "-1"},
		{"execute", "-pass-env", "A=B"},
		{"execute", "extra"},
		{"execute", "-root", filepath.Join(t.TempDir(), "missing")},
	} {
		if err := run(context.Background(), args, program{name: "dax"}); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

// A signal to `dax execute` ends the processes it started for the
// session, a read waiting on one included, and then the executor: the
// session's close does not wait on the read.
func TestExecuteEndsItsProcessesAtASignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), executeRoot+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r, err := executor.Connect(ctx, &sdk.IOTransport{Reader: stdout, Writer: stdin}, executor.ConnectOptions{Name: "dax", Version: "test"})
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
	}
	defer r.Close()
	p, err := r.Start(ctx, workspace.Command{Args: []string{"sh", "-c", "echo $$; exec sleep 1000"}})
	if err != nil {
		t.Fatal(err)
	}
	out := bufio.NewReader(p.Stdout())
	line, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(line)
	go io.Copy(io.Discard, out) // a read waiting for output
	time.Sleep(50 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("execute exited: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("execute did not exit at the signal\nstderr: %s", stderr.String())
	}
	if exec.Command("kill", "-0", pid).Run() == nil {
		t.Errorf("process %s outlived the executor", pid)
	}
}
