package dax

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/provider"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// home isolates the user's config and data directories.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	return h
}

func str(s string) *string { return &s }

// settingsOf is the settings as run reads them before any executor is
// connected: the user's file at userPath ("" for the default), then the
// project's through project unless an executor is set.
func settingsOf(project workspace.Workspace, userPath string, flags config.Flags) (config.Settings, error) {
	user, err := loadUser(userPath)
	if err != nil {
		return config.Settings{}, err
	}
	return loadSettings(project, user, flags)
}

// local is this machine's directory as a workspace, closed with the
// test.
func local(t *testing.T, dir string) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

// boxed stands in for a container: its root is /workspace, its files a
// directory here.
type boxed struct{ *workspace.Local }

func (b boxed) Root() string { return "/workspace" }
func (b boxed) Descriptor() workspace.Descriptor {
	return workspace.Descriptor{Kind: workspace.KindContainer, Ref: "sha256:abc", Root: "/workspace"}
}

// The project's config is the workspace's file, read through it: a
// workspace elsewhere gives its own file, named by its own root, and a
// link out of the workspace is refused, not followed; leaving the file
// out would drop rules that only tighten.
func TestTheProjectConfigIsReadThroughTheWorkspace(t *testing.T) {
	home(t)
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, files, outside string)
		box     bool
		deny    string // the project rule read; empty: none
		wantErr string // in the error; empty: none
	}{
		{"a local project's file", func(t *testing.T, files, _ string) {
			write(t, filepath.Join(files, ".dax", "config.json"), `{"policy":{"deny":["bash(git push:*)"]}}`)
		}, false, "bash(git push:*)", ""},
		{"a container's file", func(t *testing.T, files, _ string) {
			write(t, filepath.Join(files, ".dax", "config.json"), `{"policy":{"deny":["bash(rm:*)"]}}`)
		}, true, "bash(rm:*)", ""},
		{"a container's file names its root in errors", func(t *testing.T, files, _ string) {
			write(t, filepath.Join(files, ".dax", "config.json"), `{"model":"from-project"}`)
		}, true, "", "/workspace/.dax/config.json: model: a project file may only tighten"},
		{"no file", func(*testing.T, string, string) {}, true, "", ""},
		{"a link out of the workspace", func(t *testing.T, files, outside string) {
			write(t, filepath.Join(outside, "config.json"), `{"policy":{"deny":["bash(curl:*)"]}}`)
			must(t, os.MkdirAll(filepath.Join(files, ".dax"), 0o755))
			must(t, os.Symlink(filepath.Join(outside, "config.json"), filepath.Join(files, ".dax", "config.json")))
		}, false, "", "outside the workspace"},
		{"a link inside it", func(t *testing.T, files, _ string) {
			write(t, filepath.Join(files, "conf", "dax.json"), `{"policy":{"deny":["bash(make:*)"]}}`)
			must(t, os.MkdirAll(filepath.Join(files, ".dax"), 0o755))
			must(t, os.Symlink(filepath.Join("..", "conf", "dax.json"), filepath.Join(files, ".dax", "config.json")))
		}, false, "bash(make:*)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, outside := t.TempDir(), t.TempDir()
			tc.setup(t, files, outside)
			var ws workspace.Workspace = local(t, files)
			if tc.box {
				ws = boxed{ws.(*workspace.Local)}
			}
			s, err := settingsOf(ws, "", config.Flags{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(s.Policy.Project.Deny, ","); got != tc.deny {
				t.Errorf("project deny = %q, want %q", got, tc.deny)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSettingsPrecedenceFromRealFiles(t *testing.T) {
	h := home(t)
	proj := t.TempDir()
	write(t, filepath.Join(h, ".config", "dax", "config.json"), `{"provider":"anthropic","model":"from-user","think":false}`)

	s, err := settingsOf(local(t, proj), "", config.Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "anthropic" || s.Model != "from-user" || s.Think {
		t.Fatalf("user file: %+v", s)
	}

	// A project file tightens the policy and cannot set the model.
	write(t, filepath.Join(proj, ".dax", "config.json"), `{"model":"from-project"}`)
	if _, err = settingsOf(local(t, proj), "", config.Flags{}); err == nil || !strings.Contains(err.Error(), "model: a project file may only tighten") {
		t.Fatalf("project model: %v", err)
	}
	write(t, filepath.Join(proj, ".dax", "config.json"), `{"policy":{"deny":["bash(git push:*)"]}}`)
	if s, err = settingsOf(local(t, proj), "", config.Flags{}); err != nil || s.Model != "from-user" || len(s.Policy.Project.Deny) != 1 {
		t.Fatalf("project over user: %+v, %v", s, err)
	}

	s, err = settingsOf(local(t, proj), "", config.Flags{Model: str("from-flag"), Provider: str("openai")})
	if err != nil || s.Model != "from-flag" || s.Provider != "openai" {
		t.Fatalf("flags over files: %+v, %v", s, err)
	}
	if s.MemoryDir != filepath.Join(h, ".dax", "memory") {
		t.Errorf("default memory dir = %q", s.MemoryDir)
	}

	// -config names another user file, which must exist.
	other := filepath.Join(h, "other.json")
	if _, err := settingsOf(local(t, proj), other, config.Flags{}); err == nil {
		t.Error("a missing -config file should be an error")
	}
	write(t, other, `{"provider":"gemini"}`)
	if s, err = settingsOf(local(t, proj), other, config.Flags{}); err != nil || s.Provider != "gemini" {
		t.Fatalf("-config: %+v, %v", s, err)
	}
}

func TestABrokenConfigFileIsAClearError(t *testing.T) {
	h := home(t)
	path := filepath.Join(h, ".config", "dax", "config.json")
	write(t, path, `{"provider":"ollama","modle":"x"}`)
	_, err := settingsOf(local(t, t.TempDir()), "", config.Flags{})
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("err = %v, want the file and the field named", err)
	}
}

func TestPricingParsesAndPrices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	write(t, path, `{"m":{"input":2,"cached":0.5,"output":8}}`)
	cost, err := pricing(path)
	if err != nil {
		t.Fatal(err)
	}
	u := openresponses.Usage{
		InputTokens:        3,
		OutputTokens:       2,
		InputTokensDetails: openresponses.InputTokensDetails{CachedTokens: 1},
	}
	got, ok := cost("m", u)
	if !ok || got != 0.0000205 {
		t.Fatalf("cost = %v, %v; want $0.0000205, true", got, ok)
	}
	if _, ok := cost("unknown", u); ok {
		t.Error("an unpriced model was shown as priced")
	}
	if _, err := pricing(""); err != nil {
		t.Errorf("empty pricing file: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	write(t, bad, `[]`)
	if _, err := pricing(bad); err == nil || !strings.Contains(err.Error(), "pricing_file") {
		t.Fatalf("bad pricing file: %v", err)
	}
}

func TestSelectedProviderWithoutItsKeyFailsBeforeAnyRequest(t *testing.T) {
	home(t)
	t.Setenv("OPENAI_API_KEY", "")
	s, err := settingsOf(local(t, t.TempDir()), "", config.Flags{Provider: str("openai")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.New(context.Background(), provider.Spec{Provider: s.Provider, Model: s.Model, BaseURL: s.BaseURL})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestFronts(t *testing.T) {
	none := frontEnv{recording: true}
	both := frontEnv{stdinTTY: true, stdoutTTY: true, recording: true}
	kind := func(f front) string {
		switch f.(type) {
		case *replFront:
			return "repl"
		case *printFront:
			return "print"
		case *tuiFront:
			return "tui"
		}
		return "?"
	}
	for _, tc := range []struct {
		name, prompt string
		env          frontEnv
		want         string
		wantErr      string
	}{
		{"repl", "", none, "repl", ""},
		{"repl", "", both, "repl", ""},
		{"tui", "", both, "tui", ""},
		{"", "", both, "tui", ""},
		{"", "", none, "repl", ""},
		{"", "", frontEnv{stdinTTY: true, recording: true}, "repl", ""},
		{"", "", frontEnv{stdoutTTY: true, recording: true}, "repl", ""},
		// -p wins over everything.
		{"repl", "hello", none, "print", ""},
		{"tui", "hello", both, "print", ""},
		{"", "hello", both, "print", ""},
		// No session store: the default is the REPL, and asking for the
		// terminal client by name is refused plainly.
		{"", "", frontEnv{stdinTTY: true, stdoutTTY: true}, "repl", ""},
		{"tui", "", frontEnv{stdinTTY: true, stdoutTTY: true}, "", `the terminal client needs a session store; drop -sessions "" or use -front repl`},
		{"repl", "", frontEnv{stdinTTY: true, stdoutTTY: true}, "repl", ""},
		// The terminal client needs a terminal at both ends.
		{"tui", "", none, "", "needs a terminal"},
		{"tui", "", frontEnv{stdinTTY: true, recording: true}, "", "needs a terminal"},
		{"tui", "", frontEnv{stdoutTTY: true, recording: true}, "", "needs a terminal"},
		{"gui", "", both, "", "want tui or repl"},
	} {
		f, err := selectFront(tc.name, tc.prompt, frontInfo{Prompt: tc.prompt}, tc.env)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("selectFront(%q, %q, %+v) err = %v, want containing %q", tc.name, tc.prompt, tc.env, err, tc.wantErr)
			}
			continue
		}
		if err != nil || kind(f) != tc.want {
			t.Errorf("selectFront(%q, %q, %+v) = %T, %v; want %s", tc.name, tc.prompt, tc.env, f, err, tc.want)
		}
	}
	// The pause waits on the input for a prompt written to the output, so
	// it exists only when both are terminals.
	f, _ := selectFront("tui", "", frontInfo{}, both)
	if !f.(*tuiFront).pause {
		t.Error("the terminal client does not pause with both ends a terminal")
	}
}

// A character device is not a terminal: /dev/null has the mode and
// not the line discipline.
func TestIsTerminalIsNotJustACharacterDevice(t *testing.T) {
	null, err := os.Open("/dev/null")
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer null.Close()
	if fi, _ := null.Stat(); fi.Mode()&os.ModeCharDevice == 0 {
		t.Skip("/dev/null is not a character device here")
	}
	if isTerminal(null) {
		t.Error("/dev/null is a terminal")
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if isTerminal(r) || isTerminal(w) {
		t.Error("a pipe is a terminal")
	}
	if got := processEnv(true); got.stdinTTY && got.stdoutTTY && !isTerminal(os.Stdin) {
		t.Error("processEnv disagrees with isTerminal")
	}
}

// With an executor the project is where it runs, so this directory's
// .dax/config.json is not read, until it is read through the executor;
// without one it is.
func TestTheProjectConfigIsNotReadHereWithAnExecutor(t *testing.T) {
	h := home(t)
	proj := t.TempDir()
	write(t, filepath.Join(proj, ".dax", "config.json"), `{"policy":{"deny":["bash(make:*)"]}}`)
	s, err := settingsOf(local(t, proj), "", config.Flags{Executor: str("ssh host dax execute")})
	if err != nil || s.Executor != "ssh host dax execute" || len(s.Policy.Project.Deny) != 0 {
		t.Fatalf("with -executor: %q %v, %v", s.Executor, s.Policy.Project.Deny, err)
	}
	write(t, filepath.Join(h, ".config", "dax", "config.json"), `{"executor":{"command":"docker exec -i box dax execute"}}`)
	if s, err = settingsOf(local(t, proj), "", config.Flags{}); err != nil || s.Executor == "" || len(s.Policy.Project.Deny) != 0 {
		t.Fatalf("with the config's executor: %q %v, %v", s.Executor, s.Policy.Project.Deny, err)
	}
	if s, err = settingsOf(local(t, proj), "", config.Flags{Executor: str("")}); err != nil || s.Executor != "" || len(s.Policy.Project.Deny) != 1 {
		t.Fatalf("with -executor '': %q %v, %v", s.Executor, s.Policy.Project.Deny, err)
	}
}

// With an executor the project's .dax/config.json is the executor's,
// read through it once it is connected: its deny is in force, this
// directory's file is not read, and a file that sets what a project may
// not (the executor among them), that is a link out of the executor's
// workspace or over the bound fails the start, as does an executor
// that is gone, since leaving the file out would drop rules that only
// tighten.
func TestTheProjectConfigIsReadThroughTheExecutor(t *testing.T) {
	home(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	box, here, outside := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(here, ".dax", "config.json"), `{"policy":{"deny":["bash(rm:*)"]}}`)
	write(t, filepath.Join(outside, "config.json"), `{"policy":{"deny":["bash(curl:*)"]}}`)
	t.Setenv(executeRoot, box)
	ex, err := agent.DialExecutor(ctx, os.Args[0], agent.ExecutorOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	user, err := loadUser("")
	if err != nil {
		t.Fatal(err)
	}
	flags := config.Flags{Executor: str(os.Args[0])}
	if s, err := loadSettings(local(t, here), user, flags); err != nil || len(s.Policy.Project.Deny) != 0 {
		t.Fatalf("before the executor: %v %v", s.Policy.Project.Deny, err)
	}
	file := filepath.Join(box, ".dax", "config.json")
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T)
		deny    string
		wantErr string
	}{
		{"no file", func(*testing.T) {}, "", ""},
		{"a deny", func(t *testing.T) { write(t, file, `{"policy":{"deny":["bash(make:*)"]}}`) }, "bash(make:*)", ""},
		{"a link inside", func(t *testing.T) {
			write(t, filepath.Join(box, "conf.json"), `{"policy":{"deny":["bash(go:*)"]}}`)
			must(t, os.Symlink("../conf.json", file))
		}, "bash(go:*)", ""},
		{"it may not name an executor", func(t *testing.T) {
			write(t, file, `{"executor":{"command":"ssh elsewhere dax execute"}}`)
		}, "", box + "/.dax/config.json: executor: a project file may only tighten"},
		{"it may not name an MCP server", func(t *testing.T) {
			write(t, file, `{"mcp_servers":{"x":{"command":"touch started"}}}`)
		}, "", "mcp_servers: a project file may only tighten"},
		{"it may not allow", func(t *testing.T) { write(t, file, `{"policy":{"allow":["bash"]}}`) }, "", "may not allow anything"},
		{"a link out of the executor's workspace", func(t *testing.T) {
			must(t, os.Symlink(filepath.Join(outside, "config.json"), file))
		}, "", "outside the workspace"},
		{"over the bound", func(t *testing.T) {
			write(t, file, `{"policy":{"deny":["bash(make:*)"]}}`+strings.Repeat(" ", config.MaxProjectBytes))
		}, "", "over the 1048576-byte limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.RemoveAll(filepath.Join(box, ".dax"))
			must(t, os.MkdirAll(filepath.Join(box, ".dax"), 0o755))
			tc.setup(t)
			s, err := executorSettings(ex.Workspace(), user, flags)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(s.Policy.Project.Deny, ","); got != tc.deny {
				t.Errorf("project deny = %q, want %q", got, tc.deny)
			}
			if s.Executor != os.Args[0] {
				t.Errorf("executor %q", s.Executor)
			}
		})
	}
	ex.Close()
	if _, err := executorSettings(ex.Workspace(), user, flags); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("an executor that is gone: %v", err)
	}
}

// -executor refuses what it cannot do yet before it starts anything: an
// address in place of a command. An MCP server beside it is no longer
// refused: it starts in the executor, so the start gets as far as
// dialing the executor.
func TestExecutorRefusalsOnTheCommandLine(t *testing.T) {
	home(t)
	t.Chdir(t.TempDir())
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		args  []string
		wants string
	}{
		{[]string{"-executor", "https://box.example:7000"}, "an executor over https is not supported yet"},
		{[]string{"-executor", "unix:/run/dax.sock"}, "an executor over unix is not supported yet"},
		{[]string{"-executor", "/nonexistent/dax execute", "-mcp", "/nonexistent/server"}, "-executor: executor:"},
	} {
		err := run(context.Background(), append(tc.args, "-sessions", "", "-p", "hi"), p)
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.wants)
		}
	}
}

// The banner names the executor and where it acts.
func TestTheBannerNamesTheExecutor(t *testing.T) {
	lines := banner(frontInfo{Name: "dax", Provider: "ollama", Model: "m", Dir: "/work", Executor: "dax v1 · container box · /work"}, agent.Info{})
	if !slices.Contains(lines, "executor: dax v1 · container box · /work") || !strings.HasSuffix(lines[0], "· /work") {
		t.Errorf("banner %q", lines)
	}
	if executorLine(nil) != "" {
		t.Error("a line for no executor")
	}
}
