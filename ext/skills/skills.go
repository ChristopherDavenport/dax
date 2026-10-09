// Package skills is dax-skills, the extension that offers Agent Skills:
// the project's .dax/skills, the user's ~/.dax/skills and the config's
// skills_dirs, through the skill tool, with the catalogue in the system
// prompt. Under -trust-skills a skill the user installed may widen the
// policy with its allowed-tools until the next user message; a
// repository's never does. It is built only from what package
// extension offers any extension.
package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentskill"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/workspace"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-skills.
const Name = "dax-skills"

// Options configure dax-skills.
type Options struct {
	// Dirs are the config's skills_dirs. One that does not exist is an
	// error, unlike the project's and the user's directories.
	Dirs []string
	// Trust lets a skill the user installed (under ~/.dax/skills or
	// Dirs) widen the policy with its allowed-tools while the model
	// follows it, until the next user message (-trust-skills).
	Trust bool
}

// New is dax-skills. It owns the skill tool and ships one rule:
// starting a skill runs unasked, since reading one does nothing of
// itself.
func New(o Options) extension.Extension {
	return extension.Extension{
		Name:   Name,
		Owns:   []string{agentskill.ToolName},
		Policy: policy.Rules{Allow: []string{agentskill.ToolName}},
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			// Neither default directory is one the user configured, so
			// either may be absent; the project's comes first and
			// shadows the user's, and the config's come last. Every
			// one is a source, since the kit discovers directories it
			// names before sources and the project's is read through
			// the workspace, which may not be this machine.
			ws := env.ToolEnv().Workspace
			project, ok, refused := projectSkills(ws)
			for _, om := range refused {
				env.Omit(om)
			}
			var sources []agentskill.Source
			if ok {
				sources = append(sources, project)
			}
			user := filepath.Join(env.UserDir(), "skills")
			switch src, err := agentskill.Dir(user); {
			case err == nil:
				sources = append(sources, src)
			case !errors.Is(err, fs.ErrNotExist):
				return nil, fmt.Errorf("skills %s: %w", user, err)
			}
			for _, d := range o.Dirs {
				src, err := agentskill.Dir(d)
				if err != nil {
					return nil, fmt.Errorf("skills %s: %w", d, err)
				}
				sources = append(sources, src)
			}
			opts := []agentkit.Option{agentkit.WithSkillSources(sources...)}
			if o.Trust {
				roots := append([]string{user}, o.Dirs...)
				opts = append(opts,
					agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
						// Only a skill from a directory the user named is
						// trusted: ~/.dax/skills and the config's
						// skills_dirs. A repository's skill is text from
						// the repository; its allowed-tools are withheld
						// like any untrusted rule.
						// The project's is refused by its location before
						// any path on this machine is consulted, since
						// the workspace's paths need not be this
						// machine's.
						trusted := !(ok && inside(project.Location, sk.Location)) && under(roots, sk.Location)
						return agentpolicy.Source{Name: "skill:" + sk.ListedName(), Path: sk.Location, Trusted: trusted}
					}),
					agentkit.WithSkillGrantScope(),
					agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) {
						env.Log("[skill %s: granted %v, refused %d, err %v]", g.Skill, g.Granted, len(g.Refused), g.Err)
					}))
			}
			return opts, nil
		},
	}
}

// under reports whether location, its links resolved, is inside one of
// roots.
func under(roots []string, location string) bool {
	loc := filepath.Clean(location)
	if real, err := filepath.EvalSymlinks(loc); err == nil {
		loc = real
	}
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		if inside(r, loc) {
			return true
		}
	}
	return false
}

// inside reports whether p is dir or below it, both resolved.
func inside(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// The skill library follows the links its file system follows, and a
// skill's catalogue entry goes into the system prompt and its body to
// the model. A repository can ship .dax/skills -> ~/. So the project's
// skills are read through the session's workspace, whose file system
// refuses a name that leaves it, and screened through it first, so a
// directory that would be refused is left out and reported rather than
// failing the session or offering part of itself. The screening reads
// the workspace, never this machine: the project may be in a container.

// skillsDir is the project's skills directory, as a name in the
// workspace.
const skillsDir = ".dax/skills"

// projectSkills is the project's skills directory as a source over the
// workspace's file system, and whether it can be offered: it, and
// every link below it, leads somewhere inside the workspace. A
// directory with one link that does not is left out whole, and
// reported.
func projectSkills(ws workspace.Workspace) (src agentskill.Source, ok bool, omitted []agentkit.Omission) {
	fsys := ws.FS()
	shown := path.Join(ws.Root(), skillsDir)
	// Lstat is Stat on a file system that cannot read links, and a
	// .dax that is itself a link out fails it: either is refused.
	_, err := fs.Lstat(fsys, skillsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return src, false, nil // absent: nothing to offer, nothing to refuse
	}
	if _, serr := fs.Stat(fsys, skillsDir); err != nil || serr != nil {
		return src, false, []agentkit.Omission{omission(shown, "symbolic link outside the workspace")}
	}
	var bad string
	fs.WalkDir(fsys, skillsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || bad != "" {
			return nil
		}
		// A link is followed through the workspace, which refuses one
		// that leaves it; a link to nothing is refused as well.
		if d.Type()&fs.ModeSymlink != 0 {
			if _, err := fs.Stat(fsys, p); err != nil {
				bad = path.Join(ws.Root(), p)
			}
		}
		return nil
	})
	if bad != "" {
		return src, false, []agentkit.Omission{omission(shown, fmt.Sprintf("holds a symbolic link outside the workspace, %s", bad))}
	}
	sub, err := fs.Sub(fsys, skillsDir)
	if err != nil {
		return src, false, []agentkit.Omission{omission(shown, err.Error())}
	}
	return agentskill.Source{FS: sub, Location: shown}, true, nil
}

func omission(path, reason string) agentkit.Omission {
	return agentkit.Omission{Part: "skills", Source: Name, What: path, Reason: reason}
}
