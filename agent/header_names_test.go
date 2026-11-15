package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// The header names what identifies a run, and where its settings came
// from, dax:<name>, which a reader comparing configurations does not
// read as a setting; the settings that shape its requests stay
// dax.<name> (agenteval#69, agentsession#299).
func TestTheHeaderNamesARunsIdentityWithAColon(t *testing.T) {
	o := options(t, &echo.Adapter{})
	o.Compact, o.CompactFrom, o.Window, o.WindowFrom = 1000, "flag", 65536, "ollama /api/ps"
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, _ := headerOf(t, o.Root, s)
	for _, name := range []string{"dax:workspace", "dax:context_from", "dax.context"} {
		if _, ok := h.Extra[name]; !ok {
			t.Errorf("the header lacks %s: %v", name, h.Extra)
		}
	}
	if _, ok := h.Extra["dax.workspace"]; ok {
		t.Error("the header still names the workspace dax.workspace")
	}
	if c := string(h.Extra["dax.context"]); strings.Contains(c, "_from") {
		t.Errorf("dax.context = %s, want where its values came from out of it", c)
	}
	var from struct {
		Budget string `json:"budget_from"`
		Window string `json:"window_from"`
	}
	if err := json.Unmarshal(h.Extra["dax:context_from"], &from); err != nil || from.Budget != "flag" || from.Window != "ollama /api/ps" {
		t.Errorf("dax:context_from = %s (%v), want the budget's and the window's sources", h.Extra["dax:context_from"], err)
	}
}

// A session recorded before the rename, its members dax.<name>, is
// still read: its workspace, its servers workspaces, its origin, and
// the executor -list finds it by.
func TestAnOldHeadersDottedMembersAreRead(t *testing.T) {
	ctx := context.Background()
	box := workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: "/work"}
	ws, _ := json.Marshal(recordedWorkspace{Kind: box.Kind, Ref: box.Ref, Root: box.Root})
	id := executorCommandID([]string{"ssh", "box", "dax execute"})
	mark, _ := json.Marshal(executorMark{SHA256: id})
	h := agentsession.Header{CWD: "/work", Extra: map[string]json.RawMessage{
		"dax.workspace":          ws,
		"dax.executor":           mark,
		"dax.servers_workspace":  json.RawMessage(`{"kind":"remote","ref":"http://x","servers":["p"]}`),
		"dax.servers_workspaces": json.RawMessage(`{"gh":{"kind":"remote","ref":"http://y","servers":["q"]}}`),
		"dax.origin":             json.RawMessage(`"a2a"`),
	}}
	if d, ok := recordedIn(h); !ok || d != box {
		t.Errorf("the workspace read %+v, %v; want %+v", d, ok, box)
	}
	if l := ServersWorkspacesOf(h); len(l) != 2 || l[0].Name != "gh" || l[1].Workspace.Ref != "http://x" {
		t.Errorf("the servers workspaces read %+v", l)
	}
	if o := sessionOrigin(h); o != originPeer {
		t.Errorf("the origin read %q, want %q", o, originPeer)
	}
	root := filepath.Join(t.TempDir(), "sessions")
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	sid := s.Header().ID
	st.Close()
	d, sums, err := listExecutor(ctx, root, id)
	if err != nil || d != box || len(sums) != 1 || sums[0].Header.ID != sid {
		t.Errorf("listed by its executor: %+v, %d sessions, %v; want %s in %+v", d, len(sums), err, sid, box)
	}
}
