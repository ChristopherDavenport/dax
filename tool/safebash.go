package tool

import (
	"strings"
)

// safeWords reads a command line that is one simple command inside
// the safe subset and returns its words, or reports that it is not:
// no operators, no redirects, no globs. See parsePlan for the subset.
func safeWords(cmd string) ([]string, bool) {
	p, ok := parsePlan(strings.Trim(cmd, " \t"))
	if !ok || len(p.pipelines) != 1 || len(p.pipelines[0]) != 1 {
		return nil, false
	}
	st := p.pipelines[0][0]
	if len(st.redirects) > 0 {
		return nil, false
	}
	for _, w := range st.words {
		if w.glob {
			return nil, false
		}
	}
	return st.texts(), true
}

// plain reports a quoted string of printable ASCII, tab included.
func plain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// inWorkspace reports whether a path argument, absolute or relative
// to the workspace's root, stays inside it. A path with a ..
// component is refused outright: the kernel resolves link/.. through
// the link, where a cleaned name would not, so no reading of the name
// is safe. Without .., the path is resolved link by link, through the
// workspace, as far as it exists, and every link on the way must lead
// back inside. ~ is never a path here: bash would have expanded it,
// but a quoted one is a file named ~ and either way it is not the
// workspace. See view.inside, which takes a directory after a cd.
func inWorkspace(arg string, f *Files) bool { return f.view().inside(arg, f.dir) }

// gitEnv is the environment the bash tool adds to every command so that
// the repository's own configuration cannot run a program under git
// status, diff, log or show: no fsmonitor hook, no pager, no ssh
// command, no hooks directory, no prompt on the terminal. An external
// diff cannot be switched off by configuration (an empty value is run
// as a command), so the bash tool adds --no-ext-diff and --no-textconv
// to a read-only git diff, log or show itself; see ReadOnlyGit.
func gitEnv() []string {
	return []string{
		"GIT_CONFIG_COUNT=8",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.pager", "GIT_CONFIG_VALUE_1=cat",
		"GIT_CONFIG_KEY_2=core.sshCommand", "GIT_CONFIG_VALUE_2=ssh",
		"GIT_CONFIG_KEY_3=core.hooksPath", "GIT_CONFIG_VALUE_3=/dev/null",
		"GIT_CONFIG_KEY_4=gpg.program", "GIT_CONFIG_VALUE_4=/bin/false",
		"GIT_CONFIG_KEY_5=gpg.openpgp.program", "GIT_CONFIG_VALUE_5=/bin/false",
		"GIT_CONFIG_KEY_6=gpg.x509.program", "GIT_CONFIG_VALUE_6=/bin/false",
		"GIT_CONFIG_KEY_7=gpg.ssh.program", "GIT_CONFIG_VALUE_7=/bin/false",
		"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "PAGER=cat", "GIT_OPTIONAL_LOCKS=0",
	}
}

// autoEnv is what an auto-allowed command is added to the environment:
// gitEnv, and GOTOOLCHAIN=local so that go version and go env cannot
// download and run the toolchain a go.mod names.
func autoEnv() []string { return append(gitEnv(), "GOTOOLCHAIN=local") }
