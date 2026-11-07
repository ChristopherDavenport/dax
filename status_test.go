package dax

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// fakeStatusSession is what the status line reads of a session.
type fakeStatusSession struct {
	git           tool.Git
	model         string
	mode, pending string
}

func (f *fakeStatusSession) Git(context.Context) (tool.Git, error) { return f.git, nil }
func (f *fakeStatusSession) Model() string                         { return f.model }
func (f *fakeStatusSession) Mode() (string, string)                { return f.mode, f.pending }
func (f *fakeStatusSession) Subscribe(func(context.Context, agentturn.Event) error) func() {
	return func() {}
}

// The config's block is the console's line: its position, style,
// segments and colors, dax's host segments, the model dax runs next
// and the window it believes that model has.
func TestTheStatusBlockMakesUpTheLine(t *testing.T) {
	home, _ := os.UserHomeDir()
	st := &config.Status{Position: "bottom", Style: "powerline", Segments: []string{"user", "dir", "git", "executor", "model", "context"},
		Colors: map[string]config.StatusColor{"context": {Thresholds: []config.StatusThreshold{{At: 50, Color: "3"}}}}}
	info := frontInfo{Model: "big", Dir: filepath.Join(home, "src", "p"), Executor: "dax v1 · container box · /work", Status: st, Windows: map[string]int64{"big": 200000}}
	h := newStatus(info, &fakeStatusSession{git: tool.Git{Repo: true, Branch: "main", Dirty: true}, model: "big"})
	h.refresh(context.Background())
	l := h.line()
	if l.Position != console.StatusBottom || l.Style != console.StatusPowerline || strings.Join(l.Segments, ",") != "user,dir,git,executor,model,context" {
		t.Fatalf("line %+v", l)
	}
	if c := l.Colors["context"]; len(c.Thresholds) != 1 || c.Thresholds[0] != (console.Threshold{At: 50, Color: "3"}) {
		t.Errorf("colors %+v", l.Colors)
	}
	for name, want := range map[string]console.Segment{
		// Tools that act elsewhere: the root is that machine's path.
		"dir":      {Text: filepath.Join(home, "src", "p"), Color: "4"},
		"git":      {Text: "main ±", Color: "3"},
		"executor": {Text: "dax v1 · container box · /work"},
	} {
		if got := l.Host[name](); got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	if u := l.Host["user"](); !strings.Contains(u.Text, "@") || u.Color != "0" {
		t.Errorf("user: %+v", u)
	}
	if l.Model() != "big" || l.Window("big") != 200000 || l.Window("other") != 0 {
		t.Errorf("model %q window %d", l.Model(), l.Window("big"))
	}
	// Clean is green; the executor here says so, and the home here is ~.
	h = newStatus(frontInfo{Status: st, Dir: filepath.Join(home, "src", "p")}, &fakeStatusSession{git: tool.Git{Repo: true, Branch: "dev"}})
	h.refresh(context.Background())
	l = h.line()
	if got := l.Host["git"](); got != (console.Segment{Text: "dev", Color: "2"}) {
		t.Errorf("clean git: %+v", got)
	}
	if got := l.Host["executor"](); got.Text != "here" {
		t.Errorf("executor here: %+v", got)
	}
	if got := l.Host["dir"](); got.Text != filepath.Join("~", "src", "p") {
		t.Errorf("dir here: %+v", got)
	}
	// A color the config gives the segment wins over dax's own.
	h = newStatus(frontInfo{Status: &config.Status{Segments: []string{"git"}, Colors: map[string]config.StatusColor{"git": {Color: "6"}}}}, &fakeStatusSession{git: tool.Git{Repo: true, Branch: "dev"}})
	h.refresh(context.Background())
	if got := h.line().Host["git"](); got != (console.Segment{Text: "dev"}) {
		t.Errorf("configured git color: %+v", got)
	}
	// A repository not read says so; none hides the segment.
	h = newStatus(frontInfo{Status: st}, &fakeStatusSession{git: tool.Git{Repo: true, Withheld: "filter.x.clean"}})
	h.refresh(context.Background())
	if got := h.line().Host["git"](); !strings.Contains(got.Text, "not read") {
		t.Errorf("withheld git: %+v", got)
	}
	h = newStatus(frontInfo{Status: st}, &fakeStatusSession{})
	h.refresh(context.Background())
	if got := h.line().Host["git"](); got.Text != "" {
		t.Errorf("no repository: %+v", got)
	}
}

// In the terminal client the git segment shows the branch, and ± once a
// tool run leaves the tree dirty; the executor's line is in the bar, not
// before the screen.
func TestTheTUIStatusLineShowsGitAndTheExecutor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	m := &steps{parent: [][2]string{{"write", `{"path":"new.txt","content":"x\n"}`}}}
	r := startRig(t, m, policy.Rules{Allow: []string{"write"}}, nil, func(f *tuiFront) {
		f.info.Executor = "dax v1 · container box · /work"
		f.info.Status = &config.Status{Position: "bottom", Segments: []string{"git", "executor"}}
		dir := f.info.Dir
		for _, args := range [][]string{{"init", "-q", "-b", "trunk"}, {"add", "-A"}, {"commit", "-q", "-m", "one"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}, true)
	r.waitOutput("trunk")
	r.waitOutput("dax v1 · container box · /work")
	mark := r.mark()
	r.type_("go\r")
	r.waitOutput("all done")
	r.waitOutputSince(mark, "±")
	r.quit()
	if pre := r.pre.String(); strings.Contains(pre, "executor:") {
		t.Errorf("the executor's line was printed before the screen with its segment shown: %q", pre)
	}
}
