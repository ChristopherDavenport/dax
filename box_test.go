package dax

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/cmdline"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// boxDocker answers docker by its first argument, each answer once and
// the last for ever, and records the calls.
type boxDocker struct {
	answers map[string][]string
	calls   [][]string
}

func (d *boxDocker) Docker(_ context.Context, args ...string) ([]byte, error) {
	d.calls = append(d.calls, args)
	q := d.answers[args[0]]
	if len(q) == 0 {
		return nil, nil
	}
	out := q[0]
	if len(q) > 1 {
		d.answers[args[0]] = q[1:]
	}
	return []byte(out), nil
}

func TestBoxCommand(t *testing.T) {
	home(t)
	dir := t.TempDir()
	t.Chdir(dir)
	here, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		name    string
		args    []string
		answers map[string][]string
		out     string // in what it prints
		err     string
		last    []string // the last docker call
	}{
		{
			name:    "ls",
			args:    []string{"ls"},
			answers: map[string][]string{"ps": {"dax-a-1\trunning\t/src/a\trunsc\n"}},
			out:     "dax-a-1  running  runsc    /src/a",
		},
		{
			name:    "down takes this directory's box",
			args:    []string{"down"},
			answers: map[string][]string{"ps": {"running\tx\ty\n"}},
			out:     "removed " + here.Name,
			last:    []string{"rm", "-f", here.Name},
		},
		{
			name:    "down by name",
			args:    []string{"down", "dax-other-1"},
			answers: map[string][]string{"ps": {"exited\tx\ty\n"}},
			last:    []string{"rm", "-f", "dax-other-1"},
		},
		{
			name: "down of no box",
			args: []string{"down", "nginx"},
			err:  "no box nginx",
		},
		{
			name:    "up prints the executor line",
			args:    []string{"up", "-runtime", "runsc-ptrace"},
			answers: map[string][]string{"ps": {"", "running\t\t\n"}},
			out:     "dax -executor ",
		},
		{name: "no verb", args: nil, err: "usage: dax box up"},
		{name: "another verb", args: []string{"rm"}, err: `box "rm": want up, ls or down`},
		{name: "ls takes no name", args: []string{"ls", "x"}, err: "unexpected argument"},
		{name: "down takes one name", args: []string{"down", "a", "b"}, err: `unexpected argument "b"`},
		{name: "a bad runtime", args: []string{"up", "-runtime", "run sc"}, err: "-box-runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &boxDocker{answers: tc.answers}
			var out bytes.Buffer
			err := runBoxWith(context.Background(), tc.args, p, &out, &box.Client{Runner: d})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.out) {
				t.Errorf("printed %q, want %q in it", out.String(), tc.out)
			}
			if tc.last != nil && !slices.Equal(d.calls[len(d.calls)-1], tc.last) {
				t.Errorf("last docker call %q, want %q", d.calls[len(d.calls)-1], tc.last)
			}
		})
	}
}

// What up prints is a -executor line that splits back into the box's
// command, which -box would run.
func TestBoxUpPrintsTheBoxsCommand(t *testing.T) {
	home(t)
	dir := filepath.Join(t.TempDir(), "my project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	d := &boxDocker{answers: map[string][]string{"ps": {"", "running\t\t\n"}}}
	var out bytes.Buffer
	if err := runBoxWith(context.Background(), []string{"up"}, program{name: "dax"}, &out, &box.Client{Runner: d}); err != nil {
		t.Fatal(err)
	}
	words, err := cmdline.Split(strings.TrimSpace(out.String()))
	if err != nil || len(words) != 3 || words[1] != "-executor" {
		t.Fatalf("printed %q: %q, %v", out.String(), words, err)
	}
	argv, err := cmdline.Split(words[2])
	if err != nil {
		t.Fatal(err)
	}
	b, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, b.Command()) {
		t.Errorf("the -executor line runs %q, the box's command is %q", argv, b.Command())
	}
	run := d.calls[1]
	if run[0] != "run" || !slices.Contains(run, "runsc") || !slices.Contains(run, config.DefaultBox().Image) {
		t.Errorf("up made the box with %q, want the defaults", run)
	}
}

// -list in a box lists the sessions run in it, from its address alone:
// a stand-in docker on PATH runs the test binary as the box's dax
// execute to record one, and -list then finds it without running
// docker.
func TestListFindsTheBoxsSessions(t *testing.T) {
	h := home(t)
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexec '"+os.Args[0]+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := listRig{t: t, root: filepath.Join(t.TempDir(), "sessions")}
	here := t.TempDir()
	b, err := projectBox(here)
	if err != nil {
		t.Fatal(err)
	}
	local := r.record(here, nil, "")
	inBox := r.record(here, b.Command(), t.TempDir())
	// docker is gone: -list must not need it.
	if err := os.Remove(docker); err != nil {
		t.Fatal(err)
	}

	list := func(cfg string, f config.Flags) []string {
		t.Helper()
		var out bytes.Buffer
		if err := runList(context.Background(), &out, r.root, here, cfg, f); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if f := strings.Fields(line); len(f) > 2 {
				ids = append(ids, f[2])
			}
		}
		return ids
	}
	on, off := true, false
	if got := list("", config.Flags{}); !slices.Equal(got, []string{local}) {
		t.Errorf("-list here = %q, want the local session %q", got, local)
	}
	if got := list("", config.Flags{Box: &on}); !slices.Equal(got, []string{inBox}) {
		t.Errorf("-list -box = %q, want the box's session %q", got, inBox)
	}
	cfg := filepath.Join(h, "box.json")
	if err := os.WriteFile(cfg, []byte(`{"box":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := list(cfg, config.Flags{}); !slices.Equal(got, []string{inBox}) {
		t.Errorf("-list with the config's box = %q, want %q", got, inBox)
	}
	if got := list(cfg, config.Flags{Box: &off}); !slices.Equal(got, []string{local}) {
		t.Errorf("-list -box=false = %q, want %q", got, local)
	}
}

func TestBoxRefusalsOnTheCommandLine(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	cfg := filepath.Join(h, "executor.json")
	if err := os.WriteFile(cfg, []byte(`{"executor":{"command":"ssh host dax execute"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		args  []string
		wants string
	}{
		{[]string{"-box", "-executor", "ssh host dax execute"}, "give -box=false to use this executor"},
		{[]string{"-box", "-config", cfg}, `give -executor "" to run them in a box`},
		{[]string{"-box-image", "debian"}, "-box-image goes with -box"},
		{[]string{"-box", "-box-image", "--privileged"}, "-box-image"},
		{[]string{"-box", "-list", "-executor", "ssh host dax execute"}, "give -box=false"},
	} {
		err := run(context.Background(), append(tc.args, "-sessions", "", "-p", "hi"), p)
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.wants)
		}
	}
}
