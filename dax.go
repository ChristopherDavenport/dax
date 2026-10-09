// Package dax is a coding agent: a terminal client, a REPL, or one
// prompt with -p, over an Open Responses model, under a policy,
// recording every session. Command dax (./cmd/dax) is Main with no
// options.
//
// dax is a minimal core meant to be extended. The session (the model,
// the store, the policy, the workspace, AGENTS.md, MCP servers,
// compaction) knows no tool. Everything the model can do comes from an
// extension.Extension, and dax's own are extensions like any other:
// dax-coding (package ext/coding) has the file tools and bash,
// dax-agents the explore and task sub-agents, dax-skills skills,
// dax-memory memory. A program built on dax adds its own with
// WithExtension, on the same terms, and may leave one of dax's out with
// WithoutExtension; it keeps the flags, the config files, the
// providers, the session store and the fronts.
//
//	func main() {
//		os.Exit(dax.Main(context.Background(), os.Args[1:],
//			dax.WithName("acme", ""),
//			dax.WithExtension(extension.Extension{
//				Name:      "acme-deploy",
//				Tools:     func(e extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{deploy.New(e.Workspace)} },
//				Matchers:  extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": {Match: agentpolicy.GlobMatcher("env")}}),
//				Policy:    policy.Rules{Allow: []string{"deploy(staging)"}},
//				Renderers: func(string) toolview.Renderers { return toolview.Renderers{"deploy": deploy.Renderer{}} },
//			})))
//	}
//
// A program that wants a front of its own builds the session with
// agent.New and the same extensions.
//
// dax is pre-1.0: the exported API of this package, agent, extension,
// policy, tool, toolrender and the ext packages may change in a minor
// version, and the CHANGELOG says when.
package dax

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/extension"
)

// Option configures Main.
type Option func(*program)

// WithName names the program, for the usage line, the banner, the
// resume command, the client header and the session header, and for
// the agent in the system prompt. An empty version is the main
// module's, as `go install` stamps it, or devel for a build that has
// none; never dax's, which would put dax's version under another name. The config files and the session store are still dax's:
// ~/.config/dax, .dax and ~/.dax.
func WithName(name, version string) Option {
	return func(p *program) {
		p.name, p.named = name, true
		if version == "" {
			version = buildVersion("devel")
		}
		p.version = version
	}
}

// WithExtension adds e after dax's extensions, in the order given.
// Two extensions that claim one name, a tool's, an alias or their own,
// are an error at start.
func WithExtension(e extension.Extension) Option {
	return func(p *program) { p.ext = append(p.ext, e) }
}

// WithoutExtension leaves out the dax extension called name (dax-coding,
// dax-agents, dax-skills or dax-memory), whatever the settings say. A
// name that is none of them is an error at start.
func WithoutExtension(name string) Option {
	return func(p *program) { p.without = append(p.without, name) }
}

// program is what the options configure.
type program struct {
	name, version string
	// named is set by WithName; without it the session is dax's.
	named   bool
	ext     []extension.Extension
	without []string
}

// Main runs dax with args, the command line after the program's name,
// and returns the process's exit status: 0, or 1 after printing the
// error.
func Main(ctx context.Context, args []string, opts ...Option) int {
	p := program{name: "dax", version: buildVersion(agent.Version)}
	for _, o := range opts {
		o(&p)
	}
	if err := run(ctx, args, p); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", p.name, err)
		if h := hint(err); h != "" {
			fmt.Fprintf(os.Stderr, "%s: %s\n", p.name, h)
		}
		return 1
	}
	return 0
}

// daxExtensions are the names WithoutExtension takes.
var daxExtensions = []string{coding.Name, agents.Name, skills.Name, memory.Name}

// extensions are defaults, dax's as the settings chose them, less those
// the program left out, then the program's.
func (p program) extensions(defaults []extension.Extension) ([]extension.Extension, error) {
	for _, n := range p.without {
		if !slices.Contains(daxExtensions, n) {
			return nil, fmt.Errorf("WithoutExtension(%q): dax's extensions are %v", n, daxExtensions)
		}
	}
	var out []extension.Extension
	for _, e := range defaults {
		if !slices.Contains(p.without, e.Name) {
			out = append(out, e)
		}
	}
	return append(out, p.ext...), nil
}

// hasExtension reports whether exts holds the one called name.
func hasExtension(exts []extension.Extension, name string) bool {
	return slices.ContainsFunc(exts, func(e extension.Extension) bool { return e.Name == name })
}
