package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
)

// hostileRepo builds a directory the way a downloaded archive with a
// .git in it would be: its .git/config is the attacker's.
func hostileRepo(t *testing.T, config string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "f"), []byte("a\n"), 0o644)
	run("add", "f")
	run("-c", "user.name=n", "-c", "user.email=e@x", "commit", "-qm", "x")
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(config)
	f.Close()
	return dir
}

// R2-1 of the second review: a clean filter named in .git/config ran
// under an auto-allowed git status; gpg.program under %GG.
func TestGitCommandsAskWhenTheRepositoryNamesAProgram(t *testing.T) {
	hostile := map[string]string{
		"filter.x.clean":              "[filter \"x\"]\n\tclean = touch PROBE; cat\n",
		"filter.x.smudge":             "[filter \"x\"]\n\tsmudge = cat\n",
		"filter.x.process":            "[filter \"x\"]\n\tprocess = evil\n",
		"diff.x.textconv":             "[diff \"x\"]\n\ttextconv = evil\n",
		"diff.x.command":              "[diff \"x\"]\n\tcommand = evil\n",
		"diff.external":               "[diff]\n\texternal = evil\n",
		"core.askPass":                "[core]\n\taskPass = evil\n",
		"core.editor":                 "[core]\n\teditor = evil\n",
		"core.gitProxy":               "[core]\n\tgitProxy = evil\n",
		"core.attributesFile":         "[core]\n\tattributesFile = /tmp/x\n",
		"remote.o.uploadpack":         "[remote \"o\"]\n\tuploadpack = evil\n",
		"remote.o.receivepack":        "[remote \"o\"]\n\treceivepack = evil\n",
		"credential.helper":           "[credential]\n\thelper = !evil\n",
		"credential.https://x.helper": "[credential \"https://x\"]\n\thelper = !evil\n",
		"sequence.editor":             "[sequence]\n\teditor = evil\n",
		"merge.x.driver":              "[merge \"x\"]\n\tdriver = evil\n",
		"uploadpack.packObjectsHook":  "[uploadpack]\n\tpackObjectsHook = evil\n",
		"protocol.ext.allow":          "[protocol \"ext\"]\n\tallow = always\n",
		"pager.log":                   "[pager]\n\tlog = evil\n",
		"through an include":          "[include]\n\tpath = extra\n",
	}
	for name, cfg := range hostile {
		t.Run(name, func(t *testing.T) {
			dir := hostileRepo(t, cfg)
			os.WriteFile(filepath.Join(dir, ".git", "extra"), []byte("[filter \"y\"]\n\tclean = evil\n"), 0o644)
			for _, cmd := range []string{"git status", "git status -s", "git diff", "git log --oneline -n5", "git show HEAD", "git diff HEAD -- f"} {
				got, reason := decideIn(t, dir, defaults, "bash", bash(cmd))
				if got != agentturn.Defer {
					t.Errorf("%q = %v (%s), want Defer", cmd, got, reason)
				}
				if name != "through an include" && !strings.Contains(strings.ToLower(reason), strings.ToLower(name)) && !strings.HasPrefix(name, "credential") {
					t.Errorf("%q: the question does not name %s: %s", cmd, name, reason)
				}
			}
		})
	}
	// A clean repository, and the same commands with only keys dex
	// switches off or that are harmless, run.
	for name, cfg := range map[string]string{
		"nothing":                "",
		"neutralised keys":       "[core]\n\tfsmonitor = evil\n\tpager = evil\n\tsshCommand = evil\n\thooksPath = evil\n[gpg]\n\tprogram = evil\n[gpg \"ssh\"]\n\tprogram = evil\n",
		"a boolean pager":        "[pager]\n\tlog = false\n",
		"protocol allow never":   "[protocol \"ext\"]\n\tallow = never\n",
		"user-ish harmless keys": "[user]\n\tname = n\n[alias]\n\tst = status\n[remote \"o\"]\n\turl = https://example.com/x.git\n",
	} {
		t.Run("allows "+name, func(t *testing.T) {
			dir := hostileRepo(t, cfg)
			for _, cmd := range []string{"git status", "git diff", "git log --oneline -n5", "git show HEAD"} {
				if got, reason := decideIn(t, dir, defaults, "bash", bash(cmd)); got != agentturn.Allow {
					t.Errorf("%q = %v (%s), want Allow", cmd, got, reason)
				}
			}
		})
	}
}

func TestPercentGInAFormatAsks(t *testing.T) {
	for _, cmd := range []string{
		"git log --pretty=format:%GG -1", "git log --pretty=format:%GS -1", "git log --format=%GK", "git show --format=%G? HEAD",
		"git log --pretty=%H%GG", "git log --format='%h %GS'",
	} {
		if got, reason := decide(t, defaults, "bash", bash(cmd)); got != agentturn.Defer {
			t.Errorf("%q = %v (%s), want Defer", cmd, got, reason)
		}
	}
	if got, _ := decide(t, defaults, "bash", bash("git log --format=%h:%an -1")); got != agentturn.Allow {
		t.Errorf("a harmless format = %v", got)
	}
}
