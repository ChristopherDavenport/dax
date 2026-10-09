package tool

import (
	"os"
	"regexp"
	"strings"
)

// secretSuffixes name the environment variables that hold credentials.
// A variable ending in one is not passed to a child process unless the
// user's config names it.
var secretSuffixes = []string{
	"_KEY", "_KEY_ID", "_PAT", "_PWD", "_JWT", "_CREDENTIALS", "_AUTH",
	"_TOKEN", "_SECRET", "_PASSWORD", "_API_KEY",
}

// secretNames are credentials the suffixes do not reach, and the bare
// names.
var secretNames = set("PASSWORD", "TOKEN", "API_KEY", "SECRET_KEY", "SECRET", "KEY", "AUTH",
	"DATABASE_URL", "SSH_AUTH_SOCK", "PGPASSWORD", "MYSQL_PWD",
	"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "DAX_API_KEY",
	"AWS_SESSION_TOKEN", "NPM_TOKEN", "GITHUB_TOKEN", "GH_TOKEN")

// secretWords scrub a name that holds one anywhere in it.
var secretWords = []string{"PASSWORD", "SECRET"}

// pathVars hold the path of a credential file, not a credential. They
// are passed through, though some end the way a credential does: a
// program that needs its file needs the variable, and the file is as
// readable to the model through bash as it was. They are not scrubbed;
// to keep one from a command, unset it before starting dax.
var pathVars = set("GOOGLE_APPLICATION_CREDENTIALS", "KUBECONFIG", "DOCKER_CONFIG", "NETRC",
	"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "CLOUDSDK_CONFIG",
	"SSH_ASKPASS", "GIT_ASKPASS", "XDG_RUNTIME_DIR", "PGPASSFILE", "PGSERVICEFILE")

// urlWithPassword is a URL carrying user:password@.
var urlWithPassword = regexp.MustCompile(`://[^/@\s:]*:[^/@\s]+@`)

// isSecret reports whether a variable looks like a credential, by its
// name and, for a URL, by its value.
func isSecret(name, value string) bool {
	n := strings.ToUpper(name)
	if pathVars[n] {
		return false
	}
	if secretNames[n] {
		return true
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	for _, w := range secretWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return urlWithPassword.MatchString(value)
}

// ChildEnv is base without its credentials: every variable isSecret
// names, and every one in secrets, except those in pass. Bash commands
// and MCP servers get this environment, so a test, a build script or a
// server cannot read the provider's key from it. secrets is for a key
// whose variable the user named and isSecret may not recognise.
func ChildEnv(base, pass []string, secrets ...string) []string {
	keep := set(pass...)
	extra := set(secrets...)
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, value, _ := strings.Cut(kv, "=")
		if (isSecret(name, value) || extra[name]) && !keep[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// DefaultEnv is the current environment through ChildEnv.
func DefaultEnv(pass []string, secrets ...string) []string {
	return ChildEnv(os.Environ(), pass, secrets...)
}
