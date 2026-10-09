package dax_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// changelogTool reads the project's CHANGELOG.md through the session's
// workspace, so it reads the same file whether the workspace is this
// machine, a container or a remote runtime, and reaches nothing outside.
func changelogTool(env extension.ToolEnv) []agenttool.Tool {
	return []agenttool.Tool{agenttool.NewFunc("changelog", "Read the project's changelog.", nil,
		func(context.Context, agenttool.Call) (agenttool.Result, error) {
			data, err := env.Files.ReadFile("CHANGELOG.md", env.MaxReadBytes)
			if err != nil {
				return agenttool.Result{}, err
			}
			return agenttool.Text(string(data)), nil
		})}
}

// deployTool is a tool that changes something outside the project.
func deployTool(env extension.ToolEnv) []agenttool.Tool {
	return []agenttool.Tool{agenttool.NewFunc("deploy", "Deploy the project to an environment.",
		json.RawMessage(`{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`),
		func(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
			// The deploy runs where the session's tools act.
			out, err := env.Workspace.Exec(ctx, workspace.Command{Args: []string{"make", "deploy"}})
			if err != nil {
				return agenttool.Result{}, err
			}
			return agenttool.Text(fmt.Sprintf("deployed (exit %d)", out.ExitCode)), nil
		})}
}

// deployRenderer draws a deploy call in the terminal client.
type deployRenderer struct{}

func (deployRenderer) Head(c view.Call) (toolview.Line, bool) {
	var a struct{ Env string }
	if json.Unmarshal([]byte(c.Args), &a) != nil || a.Env == "" {
		return nil, false
	}
	return toolview.Line{toolview.S(toolview.Dim, "to "+a.Env)}, true
}

func (deployRenderer) Body(view.Call, bool) ([]toolview.Line, bool) { return nil, false }

// A program built on dax: dax's flags, config, providers, session store,
// fronts and extensions, with one extension of its own holding two tools. changelog only looks, so the
// explore sub-agent has it too, and it runs unasked; deploy asks before
// every call, and the user's config can say otherwise with a rule such
// as deploy(staging), which the env matcher reads.
func Example() {
	os.Exit(dax.Main(context.Background(), os.Args[1:],
		dax.WithName("acme", ""),
		dax.WithExtension(extension.Extension{
			Name: "acme-release",
			Tools: func(e extension.ToolEnv) []agenttool.Tool {
				return append(changelogTool(e), deployTool(e)...)
			},
			ReadOnly:     []string{"changelog"},
			Matchers:     extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": {Match: agentpolicy.GlobMatcher("env")}}),
			Instructions: "Read the changelog before you describe a release. Never deploy to prod unless the user asks.",
			Policy:       policy.Rules{Allow: []string{"changelog"}},
			Renderers:    func(string) toolview.Renderers { return toolview.Renderers{"deploy": deployRenderer{}} },
		})))
}
