package offer

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

const secret = "the-offered-value-7e1a"

// lockedBuffer is a buffer a process's writes and the test's reads
// share.
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

// local is a workspace over a directory of its own whose processes get
// PATH alone.
func local(t *testing.T) *workspace.Local {
	t.Helper()
	l, err := workspace.NewLocal(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// lookup is an environment holding env alone.
func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// run starts the server called name and returns its standard output
// and standard error, once it has exited, within ten seconds.
func run(t *testing.T, o *Offering, name string) (stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errBuf lockedBuffer
	p, err := o.StartServer(ctx, name, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Stdin().Close()
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	return string(out), errBuf.String()
}

// A secret holder is given its variables and nothing else is: its
// output, standard output and standard error, is redacted of them,
// standard error after it is cleaned, so a control character inside a
// value does not carry it through; a server without secrets is
// started as it is.
func TestOfferRedactsWhatTheServerWrites(t *testing.T) {
	o, err := Offer(local(t), []Server{
		{Name: "held", Command: `sh -c 'printf "out %s\n" "$S"; printf "err %s\n" "$S" >&2; printf "split the-o\a\033ffered-value-7e1a\n" >&2; printf "tail %s" "$S" >&2'`, SecretEnv: []string{"S"}},
		{Name: "plain", Command: `sh -c 'printf "plain [%s]\n" "$S"; printf "plain err\n" >&2'`},
	}, lookup(map[string]string{"S": secret}))
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := run(t, o, "held")
	if out != "out [REDACTED S]\n" {
		t.Errorf("standard output %q", out)
	}
	if want := "err [REDACTED S]\nsplit [REDACTED S]\ntail [REDACTED S]"; errOut != want {
		t.Errorf("standard error %q, want %q", errOut, want)
	}
	out, errOut = run(t, o, "plain")
	if out != "plain []\n" || errOut != "plain err\n" {
		t.Errorf("a server without secrets: %q %q", out, errOut)
	}
	list, err := o.Servers(context.Background())
	if err != nil || len(list) != 2 || list[0].Name != "held" || list[1].Name != "plain" {
		t.Errorf("servers %+v, %v", list, err)
	}
}

// Offer refuses a secret it cannot redact or was not given, naming the
// variable and never the value, and a command it cannot split.
func TestOfferRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server Server
		env    map[string]string
		want   string
	}{
		{"unset", Server{Name: "p", Command: "x", SecretEnv: []string{"S"}}, nil, "server p: secret_env S is not set"},
		{"empty", Server{Name: "p", Command: "x", SecretEnv: []string{"S"}}, map[string]string{"S": ""}, "secret_env S is not set"},
		{"short", Server{Name: "p", Command: "x", SecretEnv: []string{"S"}}, map[string]string{"S": "1234567"}, "secret_env S is shorter than 8 bytes"},
		{"an open quote", Server{Name: "p", Command: `x "y`}, nil, "server p: command"},
		{"no command", Server{Name: "p", Command: "  "}, nil, "server p"},
		{"a bad name", Server{Name: "P_1", Command: "x"}, nil, "server name"},
	} {
		_, err := Offer(local(t), []Server{tc.server}, lookup(tc.env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "1234567") {
			t.Errorf("%s: the error holds the value: %v", tc.name, err)
		}
	}
}
