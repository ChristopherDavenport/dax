package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
)

// headerOf is the session's header as the store lists it, and the
// header file as written.
func headerOf(t *testing.T, root string, s *Session) (agentsession.Header, string) {
	t.Helper()
	sums, err := List(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(sums, func(sum agentsession.Summary) bool { return sum.Header.ID == s.ID() })
	if i < 0 {
		t.Fatalf("%s is not listed", s.ID())
	}
	raw, err := os.ReadFile(filepath.Join(s.Path(), "header"))
	if err != nil {
		t.Fatal(err)
	}
	return sums[i].Header, string(raw)
}

// Every new session's header records its workspace, and one whose
// tools run through an executor the address that reached it, hashed:
// the command's argv or the URL, never written out.
func TestTheHeaderRecordsTheWorkspaceAndTheExecutor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type made struct {
		o       Options
		addr    string   // the executor's identity; "" for none
		secrets []string // what the header must not hold
	}
	for _, tc := range []struct {
		name string
		make func(t *testing.T) made
		want func(o Options) recordedWorkspace
	}{
		{"this machine", func(t *testing.T) made {
			return made{o: options(t, &scripted{})}
		}, func(o Options) recordedWorkspace { return recordedWorkspace{Kind: workspace.KindLocal, Root: o.Dir} }},
		{"a command", func(t *testing.T) made {
			box := t.TempDir()
			t.Setenv(executorRoot, box)
			argv := []string{os.Args[0], "--token=hunter2"}
			ex, err := DialExecutor(ctx, argv, ExecutorOptions{Name: "dax", Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ex.Close() })
			o := options(t, &scripted{})
			o.Extensions, o.Executor = []extension.Extension{coding.New(0)}, ex
			return made{o: o, addr: executorCommandID(argv), secrets: []string{"hunter2", os.Args[0]}}
		}, func(o Options) recordedWorkspace {
			return recordedWorkspace{Kind: workspace.KindContainer, Ref: "box", Root: o.Executor.Workspace().Root()}
		}},
		{"a URL", func(t *testing.T) made {
			b := newRemoteBox(t)
			done := make(chan struct{})
			ts := httptest.NewServer(executor.Handler(b.srv, done))
			t.Cleanup(func() {
				close(done)
				ts.CloseClientConnections()
				ts.Close()
			})
			endpoint := ts.URL + "/"
			ex, err := DialExecutorURL(ctx, endpoint, ExecutorOptions{Name: "dax", Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ex.Close() })
			return made{o: remoteOptions(t, &scripted{}, ex), addr: executorURLID(ts.URL), secrets: []string{strings.TrimPrefix(ts.URL, "http://")}}
		}, func(Options) recordedWorkspace {
			return recordedWorkspace{Kind: workspace.KindContainer, Ref: "box", Root: "/work"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.make(t)
			s, err := New(ctx, m.o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h, raw := headerOf(t, m.o.Root, s)
			var ws recordedWorkspace
			if err := json.Unmarshal(h.Extra[headerWorkspace], &ws); err != nil || ws != tc.want(m.o) {
				t.Errorf("%s = %s, want %+v (%v)", headerWorkspace, h.Extra[headerWorkspace], tc.want(m.o), err)
			}
			if h.CWD != ws.Root {
				t.Errorf("cwd %q, workspace root %q", h.CWD, ws.Root)
			}
			got, ok := h.Extra[headerExecutor]
			switch {
			case m.addr == "" && ok:
				t.Errorf("a session here records an executor: %s", got)
			case m.addr != "":
				var mark executorMark
				if err := json.Unmarshal(got, &mark); err != nil || mark.SHA256 != m.addr || len(mark.SHA256) != 64 {
					t.Errorf("%s = %s, want the hash %s", headerExecutor, got, m.addr)
				}
				// The listing by that address finds it, in its workspace.
				d, sums, err := listExecutor(ctx, m.o.Root, m.addr)
				want := tc.want(m.o)
				if err != nil || d != (workspace.Descriptor{Kind: want.Kind, Ref: want.Ref, Root: want.Root}) || len(sums) != 1 || sums[0].Header.ID != s.ID() {
					t.Errorf("listed by its address: %+v, %d sessions, %v", d, len(sums), err)
				}
			}
			for _, secret := range m.secrets {
				if strings.Contains(raw, secret) {
					t.Errorf("the header holds %q: %s", secret, raw)
				}
			}
		})
	}
}

// An address is the same executor however it is spelled where the
// spelling cannot matter, and another in every other way.
func TestAnExecutorsAddressIsItsArgvOrItsURL(t *testing.T) {
	same := [][2]string{
		{executorURLID("http://127.0.0.1:7777"), executorURLID("HTTP://127.0.0.1:7777/")},
		{executorURLID("https://Box.example:7000/mcp"), executorURLID(" https://box.example:7000/mcp ")},
		{executorCommandID([]string{"ssh", "box", "dax execute"}), executorCommandID([]string{"ssh", "box", "dax execute"})},
	}
	for _, p := range same {
		if p[0] != p[1] {
			t.Errorf("%s != %s", p[0], p[1])
		}
	}
	differ := []string{
		executorURLID("http://127.0.0.1:7777"),
		executorURLID("http://127.0.0.1:7778"),
		executorURLID("https://127.0.0.1:7777"),
		executorURLID("http://127.0.0.1:7777/mcp"),
		executorCommandID([]string{"ssh", "box", "dax", "execute"}),
		executorCommandID([]string{"ssh", "box", "dax execute"}),
		executorCommandID([]string{"ssh box", "dax", "execute"}),
		// A URL given as a command is another address.
		executorCommandID([]string{"http://127.0.0.1:7777"}),
	}
	seen := map[string]bool{}
	for _, id := range differ {
		if seen[id] {
			t.Errorf("two addresses share %s", id)
		}
		seen[id] = true
	}
}

// A listing keeps the sessions in its workspace: a sub-agent's session
// records only its parent's cwd, so it is in its parent's workspace,
// and a session recorded before dax wrote one is this machine's at its
// cwd.
func TestAListingKeepsTheSessionsOfItsWorkspace(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	box := workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: "/work"}
	here := workspace.Descriptor{Kind: workspace.KindLocal, Root: "/work"}
	raw := func(d workspace.Descriptor) map[string]json.RawMessage {
		data, _ := json.Marshal(recordedWorkspace{Kind: d.Kind, Ref: d.Ref, Root: d.Root})
		return map[string]json.RawMessage{headerWorkspace: data}
	}
	create := func(h agentsession.Header) string {
		t.Helper()
		s, err := st.Create(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		return s.Header().ID
	}
	inBox := create(agentsession.Header{CWD: "/work", Extra: raw(box)})
	child := create(agentsession.Header{CWD: "/work", ParentSession: inBox, SpawnedBy: "call_1"})
	grandchild := create(agentsession.Header{CWD: "/work", ParentSession: child, SpawnedBy: "call_2"})
	local := create(agentsession.Header{CWD: "/work", Extra: raw(here)})
	localChild := create(agentsession.Header{CWD: "/work", ParentSession: local, SpawnedBy: "call_3"})
	old := create(agentsession.Header{CWD: "/work"})
	elsewhere := create(agentsession.Header{CWD: "/srv", Extra: raw(workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: "/srv"})})
	for _, tc := range []struct {
		in   workspace.Descriptor
		want []string
	}{
		{box, []string{inBox, child, grandchild}},
		{here, []string{local, localChild, old}},
	} {
		sums, err := listIn(ctx, st, agentsession.ListFilter{CWD: tc.in.Root}, &tc.in)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range sums {
			got = append(got, s.Header.ID)
		}
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("in %+v: %v, want %v", tc.in, got, tc.want)
		}
		if slices.Contains(got, elsewhere) {
			t.Errorf("in %+v: the session at /srv", tc.in)
		}
	}
}

// A session continued in another workspace than the one it was
// recorded in says so, naming both, even when their roots are one
// path; one continued where it was recorded does not, and one recorded
// before dax wrote the workspace is compared by its cwd, as before.
func TestResumingInAnotherWorkspaceSaysWhere(t *testing.T) {
	ctx := context.Background()
	inBox := func(t *testing.T, root string) Options {
		ex, _ := newRemoteBox(t).dial(t)
		o := remoteOptions(t, &scripted{}, ex)
		o.Root = root
		return o
	}
	here := func(t *testing.T, root string) Options {
		o := options(t, &scripted{})
		o.Root = root
		return o
	}
	for _, tc := range []struct {
		name       string
		record     func(t *testing.T, root string) (Options, string)
		resume     func(t *testing.T, root string) Options
		want       func(recorded, resumed Options) string // "" for no warning
		recordedIn string
	}{
		{"from a box to here", func(t *testing.T, root string) (Options, string) { return inBox(t, root), "" }, here,
			func(_, resumed Options) string {
				return "dax: session was recorded in container box:/work, continuing in " + resumed.Dir + "\n"
			}, ""},
		{"from here to a box", func(t *testing.T, root string) (Options, string) { return here(t, root), "" }, inBox,
			func(recorded, _ Options) string {
				return "dax: session was recorded in " + recorded.Dir + ", continuing in container box:/work\n"
			}, ""},
		{"from a box to the box", func(t *testing.T, root string) (Options, string) { return inBox(t, root), "" }, inBox,
			func(Options, Options) string { return "" }, ""},
		{"from here to here", func(t *testing.T, root string) (Options, string) { return here(t, root), "" }, func(t *testing.T, root string) Options { return Options{} },
			func(Options, Options) string { return "" }, ""},
		{"recorded before the workspace, at the box's root", nil, inBox,
			func(Options, Options) string { return "" }, "/work"},
		{"recorded before the workspace, elsewhere", nil, inBox,
			func(Options, Options) string {
				return "dax: session was recorded in /srv, continuing in container box:/work\n"
			}, "/srv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sessions")
			var recorded Options
			var id string
			if tc.record != nil {
				recorded, _ = tc.record(t, root)
				s, err := New(ctx, recorded)
				if err != nil {
					t.Fatal(err)
				}
				id = s.ID()
				s.Close()
			} else {
				st, err := openStore(root)
				if err != nil {
					t.Fatal(err)
				}
				s, err := st.Create(ctx, agentsession.Header{CWD: tc.recordedIn})
				if err != nil {
					t.Fatal(err)
				}
				id = s.Header().ID
				st.Close()
			}
			resumed := tc.resume(t, root)
			if resumed.Streamer == nil {
				// Here again, in the directory it was recorded in.
				resumed = recorded
			}
			var got syncBuffer
			restore := CaptureWarnings(&got)
			s, err := Resume(ctx, resumed, id)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			var warned []string
			for _, line := range strings.SplitAfter(got.String(), "\n") {
				if strings.Contains(line, "recorded in") {
					warned = append(warned, line)
				}
			}
			want := tc.want(recorded, resumed)
			switch {
			case want == "" && len(warned) > 0:
				t.Errorf("warned %q", warned)
			case want != "" && !slices.Equal(warned, []string{want}):
				t.Errorf("warned %q, want %q", warned, want)
			}
		})
	}
}
