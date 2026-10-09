// Package skills is dax-skills, the extension that offers Agent Skills:
// the project's .dax/skills, the user's ~/.dax/skills and the config's
// skills_dirs, through the skill tool, with the catalogue in the system
// prompt. Under -trust-skills a skill the user installed may widen the
// policy with its allowed-tools until the next user message; a
// repository's never does. It is built only from what package
// extension offers any extension.
package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentskill"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
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
			// shadows the user's.
			user := filepath.Join(env.UserDir(), "skills")
			dirs := []string{user}
			if ok, refused := projectSkillsOK(env.Dir()); ok {
				dirs = []string{filepath.Join(env.Dir(), ".dax", "skills"), user}
			} else {
				for _, om := range refused {
					env.Omit(om)
				}
			}
			opts := []agentkit.Option{agentkit.WithOptionalSkills(dirs...)}
			if len(o.Dirs) > 0 {
				opts = append(opts, agentkit.WithSkills(o.Dirs...))
			}
			if o.Trust {
				roots := append([]string{user}, o.Dirs...)
				opts = append(opts,
					agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
						// Only a skill from a directory the user named is
						// trusted: ~/.dax/skills and the config's
						// skills_dirs. A repository's skill is text from
						// the repository; its allowed-tools are withheld
						// like any untrusted rule.
						return agentpolicy.Source{Name: "skill:" + sk.ListedName(), Path: sk.Location, Trusted: under(roots, sk.Location)}
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

// within reports whether p, once its links are resolved, is dir or
// below it. dir is already resolved.
func within(dir, p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	return inside(dir, real)
}

// The skill library follows symbolic links, and a skill's catalogue
// entry goes into the system prompt and its body to the model. A
// repository can ship .dax/skills -> ~/. So a project's skills directory
// is screened before the library sees it, as the session screens
// AGENTS.md.

// projectSkillsOK reports whether the project's skills directory can be
// offered: it, and every link below it, stays inside the workspace. A
// directory with one link that leaves is left out whole, since the
// kit takes directories, and reported.
func projectSkillsOK(dir string) (ok bool, omitted []agentkit.Omission) {
	skills := filepath.Join(dir, ".dax", "skills")
	if _, err := os.Lstat(skills); err != nil {
		return true, nil // absent: nothing to offer, nothing to refuse
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	if !within(real, skills) {
		return false, []agentkit.Omission{omission(skills, "symbolic link outside the workspace")}
	}
	var bad string
	filepath.WalkDir(skills, func(p string, d fs.DirEntry, err error) error {
		if err != nil || bad != "" {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 && !within(real, p) {
			bad = p
		}
		return nil
	})
	if bad != "" {
		return false, []agentkit.Omission{omission(skills, fmt.Sprintf("holds a symbolic link outside the workspace, %s", bad))}
	}
	return true, nil
}

func omission(path, reason string) agentkit.Omission {
	return agentkit.Omission{Part: "skills", Source: Name, What: path, Reason: reason}
}
