package box

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
)

// A unit's server that writes its own secret: the session reads the
// placeholder on standard output and error, never the value, and the
// box host is told each secret once.
func TestStartUnitProcRedacts(t *testing.T) {
	const v = `tok"en/1é`
	script := filepath.Join(t.TempDir(), "server")
	if err := os.WriteFile(script, []byte(`cat <<'EOF'
{"result":{"env":{"DAX_TEST_TOKEN":"tok\"en/1é"}}}
{"result":{"ascii":"tok\"en\/1\u00e9","raw":"tok"en/1é"}}
EOF
echo 'err tok"en/1é' >&2
`), 0o755); err != nil {
		t.Fatal(err)
	}
	env, _ := json.Marshal([]string{"PATH=/bin", "DAX_TEST_TOKEN=" + v, "UNNAMED=tok"})
	f := &fake{answers: map[string][]answer{"inspect": {{out: string(env)}}}}
	c := &Client{Runner: f, DockerPath: standInDocker(t)}
	spec := Spec{Unit: "env", Secrets: map[string]string{"DAX_TEST_TOKEN": "env:DAX_TEST_TOKEN"}}
	var mu sync.Mutex
	told := map[string]int{}
	procs := newHostProcs(func(ctx context.Context, unit string, stderr bool) (Proc, error) {
		return c.StartUnitProc(ctx, spec, "unit", []string{"sh", script}, stderr, func(secret string, n int) {
			mu.Lock()
			told[secret] += n
			mu.Unlock()
		})
	})
	defer procs.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := procs.serve(ctx, wire.MethodServersStart, json.RawMessage(`{"name":"env","stderr":true}`))
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]any)["id"].(string)
	read := func(fd int) string {
		var all []byte
		for {
			params, _ := json.Marshal(map[string]any{"id": id, "fd": fd})
			r, err := procs.serve(ctx, methodRead, params)
			if err != nil {
				t.Fatal(err)
			}
			m := r.(map[string]any)
			if d, ok := m["data"].([]byte); ok {
				all = append(all, d...)
			}
			if m["eof"] == true {
				return string(all)
			}
		}
	}
	out, errOut := read(1), read(2)
	wantOut := `{"result":{"env":{"DAX_TEST_TOKEN":"[REDACTED DAX_TEST_TOKEN]"}}}` + "\n" +
		`{"result":{"ascii":"[REDACTED DAX_TEST_TOKEN]","raw":"[REDACTED DAX_TEST_TOKEN]"}}` + "\n"
	if out != wantOut {
		t.Errorf("stdout:\n got %q\nwant %q", out, wantOut)
	}
	if errOut != "err [REDACTED DAX_TEST_TOKEN]\n" {
		t.Errorf("stderr: %q", errOut)
	}
	if strings.Contains(out+errOut, v) || strings.Contains(out+errOut, `tok\"en`) {
		t.Errorf("the value reached the session: %q %q", out, errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 || told["DAX_TEST_TOKEN"] != 4 {
		t.Errorf("told %v, want DAX_TEST_TOKEN 4 times", told)
	}
	// What is read is the container's environment, by the secrets' names.
	if got := strings.Join(f.calls[0], " "); got != "inspect -f {{json .Config.Env}} unit" {
		t.Errorf("docker %s", got)
	}
}

// A container whose secrets cannot be read is not started, and the
// error holds nothing docker printed on standard output.
func TestStartUnitProcUnreadable(t *testing.T) {
	f := &fake{answers: map[string][]answer{"inspect": {{out: "not json tok"}}}}
	c := &Client{Runner: f, DockerPath: standInDocker(t)}
	spec := Spec{Unit: "env", Secrets: map[string]string{"T": "env:T"}}
	_, err := c.StartUnitProc(context.Background(), spec, "unit", []string{"true"}, false, nil)
	if err == nil || strings.Contains(err.Error(), "tok") || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("err = %v", err)
	}
	// A unit with no secrets reads nothing and is not wrapped.
	c = &Client{Runner: &fake{}, DockerPath: standInDocker(t)}
	p, err := c.StartUnitProc(context.Background(), Spec{Unit: "plain"}, "unit", []string{"true"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if _, wrapped := p.(*redactedProc); wrapped {
		t.Error("a unit without secrets was wrapped")
	}
}
