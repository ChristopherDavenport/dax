package coding

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/facts/factspolicy"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/policy"
)

// boundMatchers are sessionMatchers as a session builds them now: over
// the executor's adapters of dax-coding's tools (executor.InProcess,
// executor.Bind), whose claims the executor answers.
func boundMatchers(t testing.TB, dir string) map[string]agentpolicy.ToolMatcher {
	t.Helper()
	ws, files := local(t, dir)
	e := New(0)
	x, err := executor.InProcess([]extension.Extension{e}, extension.ToolEnv{Workspace: ws, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	set, err := executor.Bind(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	own := slices.Collect(maps.Keys(e.Aliases))
	var tools []agenttool.Tool
	for _, b := range set.Tools() {
		tools = append(tools, b.Adapter)
		own = append(own, b.Name())
	}
	ms, err := factspolicy.Matchers(tools, own, nil, matchers())
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// boundEngine is engine over boundMatchers.
func boundEngine(t *testing.T, dir string, s Settings) *agentpolicy.Engine {
	t.Helper()
	s.Shipped = append(slices.Clone(s.Shipped), shipped(t, dir))
	p, err := policy.Build(s)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := agentpolicy.Build(p, boundMatchers(t, dir), agentpolicy.WithAliases(aliases))
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// The review's exploits ask, and what the analyzer finds read-only
// runs, when dax-coding's claims are answered through the executor, as
// they did when the policy read the tools directly; every verdict,
// reason and subject is the one the tools themselves give.
func TestTheReviewsExploitsAskThroughTheExecutor(t *testing.T) {
	user := defaults
	user.User = Rules{Allow: []string{"bash(git status:*)", "bash(git log:*)", "bash(git diff:*)", "bash(ls:*)"}}
	asks := []string{
		"git status #'\n touch /x\n#'",
		"git status #\"\n touch /x\n#\"",
		`git status $'\'' ; touch /x #'`,
		`git status -- $'\'' >/x #'`,
		"git status >&/home/u/.bashrc",
		"git status >& /home/u/.bashrc",
		"git status >&$HOME/x",
		"git status >&2foo",
		"ls >&/home/u/.bashrc",
		"git log --output=/home/u/.bashrc",
		"git diff --output=/home/u/.bashrc",
		"git show --output=/x HEAD",
		"git diff --no-index /dev/null /home/u/.ssh/id_rsa",
		"ls /home/u/.ssh",
		"ls /etc",
		"ls ~/.ssh",
		"git -c core.pager='sh -c x' log",
		"git log --ext-diff -p",
		"git diff --textconv",
	}
	runs := []string{"git status", "git log --oneline -n5", "git diff --stat HEAD~1", "ls -la internal", "pwd"}
	type call struct{ tool, args string }
	var calls []call
	for _, c := range append(slices.Clone(asks), runs...) {
		calls = append(calls, call{"bash", bash(c)})
	}
	for _, tl := range []string{"read", "write", "edit", "grep", "glob", "ls"} {
		for _, p := range []string{".env", "docs/../.git/hooks/x", "/etc/passwd", "secrets/k", "src/ok.go", testDir + "/.env"} {
			calls = append(calls, call{tl, pathArgs("path", p)})
		}
	}
	calls = append(calls, call{"read", `{"path":"src/ok.go","Path":".env"}`}, call{"bash", `{"command":"pwd","Command":"touch x"}`})

	for name, s := range map[string]Settings{"builtin": defaults, "with user allow rules": user} {
		direct, bound := engine(t, testDir, s), boundEngine(t, testDir, s)
		for _, c := range calls {
			want, got := would(t, direct, c.tool, c.args), would(t, bound, c.tool, c.args)
			if got.Action != want.Action || got.Reason != want.Reason || got.Subject != want.Subject {
				t.Errorf("%s: %s %s: through the executor %v (%s) [%s], directly %v (%s) [%s]",
					name, c.tool, c.args, got.Action, got.Reason, got.Subject, want.Action, want.Reason, want.Subject)
			}
		}
		for _, c := range asks {
			if got := would(t, bound, "bash", bash(c)); got.Action != agentturn.Defer {
				t.Errorf("%s: %q = %v (%s), want Defer", name, c, got.Action, got.Reason)
			}
		}
	}
	for _, c := range runs {
		if got := would(t, boundEngine(t, testDir, defaults), "bash", bash(c)); got.Action != agentturn.Allow {
			t.Errorf("%q = %v (%s), want Allow", c, got.Action, got.Reason)
		}
	}
}
