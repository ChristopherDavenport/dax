package agents_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/extension"
)

// The sub-agents read the session's memory: the block as the kit renders
// it for the main agent, recorded in the child's session as memory parts
// carrying their trust, and memory_search over the same scopes; neither
// child is offered a memory writer (#158).
func TestTheSubagentsReadTheSessionsMemory(t *testing.T) {
	ctx := context.Background()
	model := &childModels{parent: scripted{calls: [][2]string{
		{"explore", `{"input":"how do we wrap errors?"}`},
		{"task", `{"input":"wrap the errors"}`},
	}}}
	o := options(t, model, nil)
	memDir := filepath.Join(o.UserDir, "memory")
	store, err := filestore.Open(memDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, agentmemory.Entry{Scope: "user", Name: "error-style", Content: "Wrap errors with %w and a lower-case message.",
		Meta: map[string]string{agentmemory.MetaDescription: "how errors are wrapped"}}); err != nil {
		t.Fatal(err)
	}
	o.Extensions = []extension.Extension{coding.New(0), agents.New(agents.Options{}), memory.New(memDir)}
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	parent := s.ID()
	if _, err := prompt(ctx, s, "go", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.explore) == 0 || len(model.task) == 0 {
		t.Fatalf("explore asked %d times, task %d", len(model.explore), len(model.task))
	}
	for name, req := range map[string]string{"explore": model.explore[0].Instructions, "task": model.task[0].Instructions} {
		if !strings.Contains(req, "Wrap errors with %w") {
			t.Errorf("%s was not shown the memory saved before it started:\n%s", name, req)
		}
	}
	for name, tools := range map[string]string{"explore": requestTools(model.explore[0]), "task": requestTools(model.task[0])} {
		if !strings.Contains(","+tools+",", ",memory_search,") {
			t.Errorf("%s's tools %s lack memory_search", name, tools)
		}
		for _, w := range agentmemory.WriteTools() {
			if strings.Contains(","+tools+",", ","+w+",") {
				t.Errorf("%s is offered %s: %s", name, w, tools)
			}
		}
	}
	// Each child's session records the entry as a memory part with its
	// trust, not as text of the product prompt.
	sums, err := agent.List(ctx, o.Root, o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, sum := range sums {
		if sum.Header.ParentSession != parent {
			continue
		}
		children++
		path, err := agent.Project(ctx, o.Root, sum.Header.ID, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		text := string(data)
		id := agentmemory.PartID("user", "error-style")
		i := strings.Index(text, `"id":"`+id+`"`)
		if i < 0 {
			t.Errorf("child session %s records no part %s:\n%s", sum.Header.ID, id, text)
			continue
		}
		end := strings.Index(text[i:], "}}")
		if end < 0 || !strings.Contains(text[i:i+end], `"untrusted"`) {
			t.Errorf("child session %s records the memory part without its trust: %s", sum.Header.ID, text[i:])
		}
	}
	if children != 2 {
		t.Errorf("%d child sessions, want explore's and task's", children)
	}
}
