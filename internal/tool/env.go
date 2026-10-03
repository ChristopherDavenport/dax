package tool

import (
	"os"
	"strings"
)

// secretSuffixes name the environment variables that hold credentials.
// A variable ending in one is not passed to a child process unless the
// user's config names it.
var secretSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_SECRET_ACCESS_KEY", "_ACCESS_KEY_ID"}

// secretNames are credentials whose names do not end that way, and the
// provider keys by name.
var secretNames = set("OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
	"DEX_API_KEY", "AWS_SESSION_TOKEN", "NPM_TOKEN", "GITHUB_TOKEN", "GH_TOKEN")

// Secret reports whether the name looks like a credential.
func Secret(name string) bool {
	n := strings.ToUpper(name)
	if secretNames[n] {
		return true
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// ChildEnv is base without its credentials: every variable Secret
// names, except those in pass. Bash commands and MCP servers get this
// environment, so a test, a build script or a server the model started
// cannot read the provider's key from it.
func ChildEnv(base, pass []string) []string {
	keep := set(pass...)
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if Secret(name) && !keep[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// DefaultEnv is the current environment through ChildEnv.
func DefaultEnv(pass []string) []string { return ChildEnv(os.Environ(), pass) }
