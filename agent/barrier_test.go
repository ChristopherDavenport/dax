package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
)

// batched makes every call of calls in one response, the first of a
// run, then answers "done".
type batched struct{ calls [][2]string }

func (m *batched) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	answered := false
	for _, it := range req.Input {
		if _, ok := it.(*openresponses.FunctionCallOutput); ok {
			answered = true
		}
	}
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if !answered {
		for _, c := range m.calls {
			call, err := em.FunctionCall("", c[0])
			if err != nil {
				return err
			}
			if err := call.Arguments(c[1]); err != nil {
				return err
			}
			if err := call.Close(); err != nil {
				return err
			}
		}
		return em.Complete()
	}
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("done"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

// flight records, as each call starts, which calls were running and
// which had ended. A call given partners waits, a bounded while, for
// them to start too, so two calls that may overlap are seen to.
type flight struct {
	mu      sync.Mutex
	changed chan struct{}
	running map[string]bool
	ended   map[string]bool
	seen    map[string]start
}

// start is what a call found running and ended as it started, and
// whether its partners started while it ran.
type start struct {
	running, ended []string
	overlapped     bool
}

func newFlight() *flight {
	return &flight{changed: make(chan struct{}), running: map[string]bool{}, ended: map[string]bool{}, seen: map[string]start{}}
}

func (f *flight) notify() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// run is the call key, waiting for partners, around do.
func (f *flight) run(key string, partners []string, do func()) {
	f.mu.Lock()
	st := start{running: keys(f.running), ended: keys(f.ended), overlapped: true}
	f.running[key] = true
	f.notify()
	f.mu.Unlock()
	deadline := time.After(3 * time.Second)
	for _, p := range partners {
		for {
			f.mu.Lock()
			there := f.running[p] || f.ended[p]
			ch := f.changed
			f.mu.Unlock()
			if there {
				break
			}
			select {
			case <-ch:
				continue
			case <-deadline:
				st.overlapped = false
			}
			break
		}
	}
	do()
	f.mu.Lock()
	delete(f.running, key)
	f.ended[key] = true
	f.seen[key] = st
	f.notify()
	f.mu.Unlock()
}

func (f *flight) of(key string) start {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[key]
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// checkBarrierOrder holds f to the order of a response a b C c d, C
// the command: a and b overlap, C starts once both have ended and none
// after it has started, and c and d start once C has ended, and
// overlap.
func checkBarrierOrder(t *testing.T, f *flight, a, b, cmd, c, d string) {
	t.Helper()
	for _, k := range []string{a, b, c, d} {
		if !f.of(k).overlapped {
			t.Errorf("%s ran alone, want it beside its neighbour (%+v)", k, f.of(k))
		}
	}
	s := f.of(cmd)
	if len(s.running) != 0 || !slices.Equal(s.ended, []string{a, b}) {
		t.Errorf("%s started with %v running and %v ended, want nothing running and %s and %s ended", cmd, s.running, s.ended, a, b)
	}
	for _, k := range []string{c, d} {
		if !slices.Contains(f.of(k).ended, cmd) || slices.Contains(f.of(k).running, cmd) {
			t.Errorf("%s started before %s ended (%+v)", k, cmd, f.of(k))
		}
	}
}

// The reads of a response run together and bash alone in its place:
// those before it overlap each other and end before it starts, and
// those after it start once it has ended, and overlap. Over the
// executor's link that is two facts requests and three steps of
// calls, where one command used to run every call one at a time.
func TestBashRunsAloneInItsPlaceOverTheExecutor(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		write(t, filepath.Join(box.dir, n+".txt"), n+"\n")
	}
	f := newFlight()
	partner := map[string]string{"read a.txt": "read b.txt", "read b.txt": "read a.txt", "read c.txt": "read d.txt", "read d.txt": "read c.txt"}
	box.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			raw, _ := json.Marshal(req.GetParams())
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Path    string `json:"path"`
					Command string `json:"command"`
				} `json:"arguments"`
			}
			json.Unmarshal(raw, &p)
			key := p.Name + " " + p.Arguments.Path + p.Arguments.Command
			var partners []string
			if q, ok := partner[key]; ok {
				partners = []string{q}
			}
			var res mcp.Result
			var err error
			f.run(key, partners, func() { res, err = next(ctx, method, req) })
			return res, err
		}
	})
	ex, _ := box.dial(t)
	model := &batched{calls: [][2]string{
		{"read", `{"path":"a.txt"}`}, {"read", `{"path":"b.txt"}`},
		{"bash", `{"command":"ls"}`},
		{"read", `{"path":"c.txt"}`}, {"read", `{"path":"d.txt"}`},
	}}
	s, err := New(ctx, remoteOptions(t, model, ex))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "read them all", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if bash, ok := s.Kit.LookupTool("bash"); !ok || !agenttool.IsBarrier(bash) || agenttool.IsSequential(bash) {
		t.Errorf("the session's bash is not a barrier: ok %v", ok)
	}
	checkBarrierOrder(t, f, "read a.txt", "read b.txt", "bash ls", "read c.txt", "read d.txt")
	if n := box.factsRequests(); n != 2 {
		t.Errorf("%d facts requests, want 2 for the response", n)
	}
}

// bash still never overlaps a write or an edit of the same response:
// the write ends before it starts, and the edit starts after it ends.
func TestBashNeverOverlapsAnEditOverTheExecutor(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	write(t, filepath.Join(box.dir, "e.txt"), "old\n")
	f := newFlight()
	box.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			raw, _ := json.Marshal(req.GetParams())
			var p struct {
				Name string `json:"name"`
			}
			json.Unmarshal(raw, &p)
			var res mcp.Result
			var err error
			f.run(p.Name, nil, func() { res, err = next(ctx, method, req) })
			return res, err
		}
	})
	ex, _ := box.dial(t)
	model := &batched{calls: [][2]string{
		{"write", `{"path":"w.txt","content":"new\n"}`},
		{"bash", `{"command":"ls"}`},
		{"edit", `{"path":"e.txt","old_string":"old","new_string":"new"}`},
	}}
	s, err := New(ctx, remoteOptions(t, model, ex))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "write, list, edit", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if b := f.of("bash"); len(b.running) != 0 || !slices.Equal(b.ended, []string{"write"}) {
		t.Errorf("bash started with %v running and %v ended, want the write ended and nothing running", b.running, b.ended)
	}
	if e := f.of("edit"); !slices.Contains(e.ended, "bash") || len(e.running) != 0 {
		t.Errorf("the edit started with %v running and %v ended, want bash ended", e.running, e.ended)
	}
}

// probeExt is a stand-in extension in this process: probe calls that may
// overlap, a barrier and a sequential tool, each run through f.
func probeExt(f *flight, partner map[string]string) extension.Extension {
	run := func(name string) func(context.Context, agenttool.Call) (agenttool.Result, error) {
		return func(_ context.Context, c agenttool.Call) (agenttool.Result, error) {
			var a struct {
				ID string `json:"id"`
			}
			json.Unmarshal(c.Args, &a)
			key := name + " " + a.ID
			var partners []string
			if q, ok := partner[key]; ok {
				partners = []string{q}
			}
			f.run(key, partners, func() {})
			return agenttool.Text(key), nil
		}
	}
	schema := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`)
	return extension.Extension{
		Name: "probe",
		Tools: func(extension.ToolEnv) []agenttool.Tool {
			return []agenttool.Tool{
				agenttool.NewFunc("look", "Look.", schema, run("look")),
				agenttool.NewFunc("fence", "Fence.", schema, run("fence"), agenttool.WithBarrier()),
				agenttool.NewFunc("serial", "Serial.", schema, run("serial"), agenttool.WithSequential()),
			}
		},
	}
}

// In this process a tool's own barrier holds through the executor's
// adapter and the kit as over the link.
func TestABarrierToolRunsAloneInItsPlaceInProcess(t *testing.T) {
	ctx := context.Background()
	f := newFlight()
	model := &batched{calls: [][2]string{
		{"look", `{"id":"a"}`}, {"look", `{"id":"b"}`},
		{"fence", `{"id":"x"}`},
		{"look", `{"id":"c"}`}, {"look", `{"id":"d"}`},
	}}
	o := options(t, model)
	o.Extensions = append(o.Extensions, probeExt(f, map[string]string{"look a": "look b", "look b": "look a", "look c": "look d", "look d": "look c"}))
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "look", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	checkBarrierOrder(t, f, "look a", "look b", "fence x", "look c", "look d")
}

// A sequential tool still runs its whole batch one call at a time:
// no call starts while another runs.
func TestASequentialToolStillSerialisesItsBatch(t *testing.T) {
	ctx := context.Background()
	f := newFlight()
	model := &batched{calls: [][2]string{
		{"look", `{"id":"a"}`}, {"serial", `{"id":"s"}`}, {"look", `{"id":"b"}`}, {"fence", `{"id":"x"}`}, {"look", `{"id":"c"}`},
	}}
	o := options(t, model)
	o.Extensions = append(o.Extensions, probeExt(f, nil))
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "look", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	order := []string{"look a", "serial s", "look b", "fence x", "look c"}
	for i, k := range order {
		st := f.of(k)
		if len(st.running) != 0 || !slices.Equal(st.ended, sortedCopy(order[:i])) {
			t.Errorf("%s started with %v running and %v ended, want nothing running and %v ended", k, st.running, st.ended, order[:i])
		}
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
