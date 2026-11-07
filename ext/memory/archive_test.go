package memory_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/memory"
)

// kept is scripted that keeps every request it is sent.
type kept struct {
	scripted
	mu   sync.Mutex
	reqs []openresponses.Request
}

func (m *kept) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

// The user's and the project's archives take passages through
// memory_insert, which asks as every memory write does; memory_search
// finds them; the block never shows them, and the usage paragraph names
// them as searchable and not shown (#161).
func TestTheArchiveIsSearchedAndNeverShown(t *testing.T) {
	ctx := context.Background()
	o, _ := session(t, nil)
	project := memory.ArchiveScope(memory.ProjectScope(o.Dir))
	passage := "The retry backoff settled on 250ms doubling to 8s."
	model := &kept{scripted: scripted{calls: [][2]string{
		{"memory_insert", `{"scope":"` + string(project) + `","content":"` + passage + `"}`},
		{"memory_search", `{"query":"backoff doubling"}`},
	}}}
	o.Streamer = model
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var asked []string
	if _, err := prompt(ctx, s, "keep this", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != agentmemory.InsertTool {
		t.Errorf("asked about %v, want memory_insert alone", asked)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.reqs) < 3 {
		t.Fatalf("%d requests", len(model.reqs))
	}
	last := model.reqs[len(model.reqs)-1]
	var outs []string
	for _, it := range last.Input {
		if out, ok := it.(*openresponses.FunctionCallOutput); ok {
			outs = append(outs, out.Output.Text)
		}
	}
	if len(outs) != 2 || !strings.Contains(outs[1], "250ms doubling") {
		t.Errorf("memory_search did not find the passage: %q", outs)
	}
	for i, req := range model.reqs {
		if strings.Contains(req.Instructions, "250ms doubling") {
			t.Errorf("request %d's instructions show the archived passage", i)
		}
		for _, sc := range []agentmemory.Scope{"user-archive", project} {
			if !strings.Contains(req.Instructions, `"`+string(sc)+`"`) || !strings.Contains(req.Instructions, "the block never shows them") {
				t.Errorf("request %d's usage paragraph does not name %s as an archive:\n%s", i, sc, req.Instructions)
			}
		}
	}
}

// An archive's name is a scope however long the project's directory
// name is.
func TestAnArchiveIsAScope(t *testing.T) {
	for _, dir := range []string{"/", "/home/u/p", "/x/" + strings.Repeat("very-long-directory-name-", 8)} {
		sc := memory.ArchiveScope(memory.ProjectScope(dir))
		if !agentmemory.ValidScope(sc) {
			t.Errorf("ArchiveScope(ProjectScope(%q)) = %q, not a scope", dir, sc)
		}
	}
	if sc := memory.ArchiveScope("user"); sc != "user-archive" {
		t.Errorf("the user's archive is %q", sc)
	}
}
