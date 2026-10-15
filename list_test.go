package dax

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// listRig records sessions into one store, on this machine and through
// the test binary run as `dax execute` (TestMain), and lists them as
// -list does.
type listRig struct {
	t    *testing.T
	root string
}

// record starts a session and closes it, which leaves its header in
// the store, and returns its ID. With argv it runs through an executor
// started by argv over box; the test binary takes no arguments as an
// executor, so argv's words past the first only make another address
// to the same box.
func (r listRig) record(dir string, argv []string, box string) string {
	t := r.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	o := agent.Options{Streamer: &echo.Adapter{}, Model: "echo", Dir: dir, Root: r.root, UserDir: t.TempDir()}
	if argv != nil {
		t.Setenv(executeRoot, box)
		ex, err := agent.DialExecutor(ctx, argv, agent.ExecutorOptions{Name: "dax", Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		defer ex.Close()
		o.Executor = ex
		// The session takes the extensions the executor serves.
		o.Extensions = []extension.Extension{coding.New(0), stray}
	}
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	s.Close()
	// Two sessions a moment apart have distinct created_at, which
	// orders them.
	time.Sleep(5 * time.Millisecond)
	return id
}

// list is what -list prints from dir with the user's file at cfg ("" for
// none) and the -executor flag, nil when not given: the IDs, and the
// lines.
func (r listRig) list(dir, cfg string, flag *string) ([]string, string) {
	t := r.t
	t.Helper()
	var out bytes.Buffer
	if err := runList(context.Background(), &out, r.root, dir, cfg, config.Flags{Executor: flag}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if f := strings.Fields(line); len(f) > 2 {
			ids = append(ids, f[2])
		}
	}
	slices.Sort(ids)
	return ids, out.String()
}

// -list lists the project here: with no executor, the sessions whose
// tools acted in this directory, and with one configured, by the
// -executor flag or the user's file in either of its forms, the
// project of the workspace the latest session reached at that address
// recorded, whichever address reached its other sessions. It dials
// nothing: an address no session used lists nothing.
func TestListFindsTheProjectWhereTheToolsActed(t *testing.T) {
	h := home(t)
	r := listRig{t: t, root: filepath.Join(t.TempDir(), "sessions")}
	here, box, box2 := t.TempDir(), t.TempDir(), t.TempDir()
	bin := os.Args[0]
	a, a2, d := []string{bin, "addr-a"}, []string{bin, "addr-a2"}, []string{bin, "addr-d"}

	local := r.record(here, nil, "")
	// Through an executor whose root is this directory's path: not a
	// session of this machine's directory.
	sameRoot := r.record(here, []string{bin, "addr-c"}, here)
	inBox := r.record(here, a, box)
	otherAddr := r.record(t.TempDir(), a2, box)
	// addr-d led to box, then to box2: box2 is its project now.
	dOld := r.record(here, d, box)
	dNew := r.record(here, d, box2)

	// Sessions recorded before dax wrote the workspace: one here, which
	// is listed here as before, and one whose cwd is the box's root,
	// which no executor finds.
	st, err := cas.Open(r.root)
	if err != nil {
		t.Fatal(err)
	}
	var old []string
	for _, cwd := range []string{here, box} {
		s, err := st.Create(context.Background(), agentsession.Header{CWD: cwd})
		if err != nil {
			t.Fatal(err)
		}
		old = append(old, s.Header().ID)
	}
	st.Close()
	oldHere, oldBox := old[0], old[1]

	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }
	boxSessions := sorted(inBox, otherAddr, dOld)
	quoted := func(argv []string) string {
		var b strings.Builder
		for i, w := range argv {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("'" + w + "'")
		}
		return b.String()
	}
	userFile := func(name string, executor any) string {
		data, err := json.Marshal(map[string]any{"executor": executor})
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(h, name)
		write(t, p, string(data))
		return p
	}
	mark := filepath.Join(t.TempDir(), "dialed")
	for _, tc := range []struct {
		name string
		cfg  string
		flag *string
		want []string
		at   string // what each line ends with
	}{
		{"no executor", "", nil, sorted(local, oldHere), ""},
		{"-executor", "", str(quoted(a)), boxSessions, "  container box:" + box},
		{"-executor at another address to the box", "", str(quoted(a2)), boxSessions, "  container box:" + box},
		{"the config's command as a string", userFile("string.json", map[string]any{"command": quoted(a)}), nil, boxSessions, "  container box:" + box},
		{"the config's command as an array", userFile("array.json", map[string]any{"command": a}), nil, boxSessions, "  container box:" + box},
		{"-executor \"\" clears the config's", userFile("cleared.json", map[string]any{"command": a}), str(""), sorted(local, oldHere), ""},
		{"the latest session at an address decides", "", str(quoted(d)), []string{dNew}, "  container box:" + box2},
		{"an executor whose root is this path", "", str(quoted([]string{bin, "addr-c"})), []string{sameRoot}, "  container box:" + here},
		{"a command no session used", "", str("sh -c 'touch " + mark + "'"), nil, ""},
		{"a URL no session used", "", str("http://127.0.0.1:1"), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.t = t
			got, out := r.list(here, tc.cfg, tc.flag)
			if !slices.Equal(got, tc.want) {
				t.Errorf("listed %v, want %v\n%s", got, tc.want, out)
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line != "" && !strings.HasSuffix(line, filepath.Join(r.root, "sessions", strings.Fields(line)[2])+tc.at) {
					t.Errorf("line %q, want it to end with the session's path and %q", line, tc.at)
				}
			}
			if slices.Contains(got, oldBox) {
				t.Errorf("a session recorded before the workspace was found: %s", oldBox)
			}
		})
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Errorf("-list ran the executor's command: %v", err)
	}
}
