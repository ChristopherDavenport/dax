package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
)

// conversation_search finds what an earlier session of this project
// said, unasked: the project's top-level sessions, newest first, not
// another project's and not the calling session's own; its output is
// labelled untrusted (#157).
func TestConversationSearchReadsThisProjectsSessions(t *testing.T) {
	ctx := context.Background()
	o, _ := session(t, nil)
	o.Root = filepath.Join(filepath.Dir(o.UserDir), "sessions")
	other := filepath.Join(filepath.Dir(o.Dir), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	// Three sessions before: two here, one in another project, each
	// saying a word of its own beside "backoff".
	for _, tc := range []struct{ dir, text string }{
		{o.Dir, "the backoff is 250ms here"},
		{other, "the backoff is 9s over there"},
		{o.Dir, "the backoff doubles each try"},
	} {
		p := o
		p.Dir = tc.dir
		p.Streamer = &scripted{}
		s, err := agent.New(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prompt(ctx, s, tc.text, nil); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	model := &kept{scripted: scripted{calls: [][2]string{{agentkit.ConversationSearchTool, `{"query":"backoff"}`}}}}
	o.Streamer = model
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var asked []string
	if _, err := prompt(ctx, s, "what backoff did we settle on?", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Errorf("asked about %v; a search of this project runs unasked", asked)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	var out string
	for _, it := range model.reqs[len(model.reqs)-1].Input {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			out = o.Output.Text
		}
	}
	for _, want := range []string{"250ms here", "doubles each try"} {
		if !strings.Contains(out, want) {
			t.Errorf("the search did not find %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"9s over there", "settle on"} {
		if strings.Contains(out, not) {
			t.Errorf("the search found %q, from another project or this session:\n%s", not, out)
		}
	}
	tl, ok := s.Kit.LookupTool(agentkit.ConversationSearchTool)
	if !ok {
		t.Fatal("no conversation_search")
	}
	if f, ok := agenttool.FlowOf(tl); !ok || len(f.Reads) != 1 || f.Reads[0] != agenttool.LabelUntrusted {
		t.Errorf("conversation_search's flow %+v, want reads untrusted", f)
	}
}
