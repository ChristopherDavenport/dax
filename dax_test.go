package dax

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// deployTool is an extension's tool: it says where it would deploy.
func deployTool(extension.ToolEnv) []agenttool.Tool {
	return []agenttool.Tool{agenttool.NewFunc("deploy", "Deploy the project.",
		json.RawMessage(`{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`),
		func(_ context.Context, c agenttool.Call) (agenttool.Result, error) {
			var a struct{ Env string }
			if err := json.Unmarshal(c.Args, &a); err != nil {
				return agenttool.Result{}, err
			}
			return agenttool.Text("deployed to " + a.Env), nil
		})}
}

// deployRenderer draws a deploy call as its environment and the
// output's last word.
type deployRenderer struct{}

func (deployRenderer) Head(c view.Call) (toolview.Line, bool) {
	var a struct{ Env string }
	if json.Unmarshal([]byte(c.Args), &a) != nil || a.Env == "" {
		return nil, false
	}
	return toolview.Line{toolview.S(toolview.Dim, "rolling out to "+a.Env)}, true
}

func (deployRenderer) Body(c view.Call, _ bool) ([]toolview.Line, bool) {
	if c.Output == "" {
		return nil, false
	}
	return []toolview.Line{{toolview.S(toolview.Dim, "landed: "+c.Output)}}, true
}

// deployExtension is a program's extension: the deploy tool, its rules
// and its renderer.
func deployExtension() extension.Extension {
	return extension.Extension{
		Name:      "acme-deploy",
		Tools:     deployTool,
		Policy:    policy.Rules{Allow: []string{"deploy"}},
		Renderers: func(string) toolview.Renderers { return toolview.Renderers{"deploy": deployRenderer{}} },
	}
}

func TestTheProgramsExtensionsFollowDaxsLessThoseLeftOut(t *testing.T) {
	defaults := []extension.Extension{{Name: coding.Name}, {Name: agents.Name}, {Name: memory.Name}}
	for _, tc := range []struct {
		name string
		opts []Option
		want string
	}{
		{"dax's alone", nil, "dax-coding dax-agents dax-memory"},
		{"the program's after dax's", []Option{WithExtension(deployExtension())}, "dax-coding dax-agents dax-memory acme-deploy"},
		{"one of dax's left out", []Option{WithoutExtension(agents.Name), WithExtension(deployExtension())}, "dax-coding dax-memory acme-deploy"},
		{"leaving out one the settings left out", []Option{WithoutExtension(skills.Name)}, "dax-coding dax-agents dax-memory"},
		{"even dax-coding", []Option{WithoutExtension(coding.Name)}, "dax-agents dax-memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p program
			for _, o := range tc.opts {
				o(&p)
			}
			exts, err := p.extensions(defaults)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range exts {
				names = append(names, e.Name)
			}
			if got := strings.Join(names, " "); got != tc.want {
				t.Errorf("extensions %q, want %q", got, tc.want)
			}
		})
	}
	var p program
	WithoutExtension("dax-codnig")(&p)
	if _, err := p.extensions(defaults); err == nil || !strings.Contains(err.Error(), "dax-codnig") {
		t.Errorf("leaving out an extension dax does not have: err = %v", err)
	}
}

// The policy line counts each extension's rules under its source, and
// with "builtin": false only their asks and denies.
func TestThePolicyLineNamesEachExtensionsRules(t *testing.T) {
	exts := []extension.Extension{
		{Name: "acme-deploy", Policy: policy.Rules{Allow: []string{"deploy(staging)"}, Ask: []string{"deploy(prod)"}}},
		{Name: "acme-docs"},
		{Name: "acme-ops", Policy: policy.Rules{Allow: []string{"rollback"}}},
	}
	if got := policySummary(policy.Settings{Builtin: true, Fallback: "ask"}, exts); !strings.HasPrefix(got, "2 rule(s) from extension:acme-deploy, 1 rule(s) from extension:acme-ops; ") {
		t.Errorf("policy line %q", got)
	}
	if got := policySummary(policy.Settings{Fallback: "ask"}, exts); !strings.HasPrefix(got, "1 rule(s) from extension:acme-deploy, no shipped allow rules; ") {
		t.Errorf("policy line without the shipped allow rules %q", got)
	}
}

// An extension's tool runs in the terminal client like dax's own, drawn
// by its renderer, and the client is the program's by name.
func TestTheTUIRunsAndDrawsAnExtensionsTool(t *testing.T) {
	m := &steps{parent: [][2]string{{"deploy", `{"env":"staging"}`}}}
	ext := deployExtension()
	r := startRig(t, m, policy.Rules{},
		func(o *agent.Options) { o.Extensions = append(o.Extensions, ext) },
		func(f *tuiFront) {
			f.info.Name = "acme"
			f.info.Renderers["deploy"] = ext.Renderers(f.info.Dir)["deploy"]
		}, true)
	r.type_("go\r")
	r.waitOutput("rolling out to staging")
	r.waitOutput("landed: deployed to staging")
	r.quit()
	if out := r.pre.String(); !strings.Contains(out, "To resume this session: acme -resume ") {
		t.Errorf("the resume command is not the program's:\n%s", out)
	}
}

func TestWithNameNamesTheProgram(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opts          []Option
		want, version string
		named         bool
	}{
		{name: "dax by default", want: "dax", version: buildVersion(agent.Version)},
		{name: "a name and a version", opts: []Option{WithName("acme", "v1.2.3")}, want: "acme", version: "v1.2.3", named: true},
		{name: "a name and the module's version", opts: []Option{WithName("acme", "")}, want: "acme", version: buildVersion("devel"), named: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := program{name: "dax", version: buildVersion(agent.Version)}
			for _, o := range tc.opts {
				o(&p)
			}
			if p.name != tc.want || p.version != tc.version || p.named != tc.named {
				t.Errorf("program %+v, want %s %s named=%v", p, tc.want, tc.version, tc.named)
			}
		})
	}
}

func TestMainReportsAnErrorUnderTheProgramsName(t *testing.T) {
	if got := Main(context.Background(), []string{"-gc", "nonsense"}, WithName("acme", "")); got != 1 {
		t.Errorf("Main = %d, want 1", got)
	}
	if got := Main(context.Background(), []string{"-h"}); got != 0 {
		t.Errorf("Main -h = %d, want 0", got)
	}
}
