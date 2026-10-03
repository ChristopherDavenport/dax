package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"
)

// The libraries that read AGENTS.md files and skill directories follow
// symbolic links, and what they read goes into the system prompt, which
// goes to the provider. A repository can ship AGENTS.md -> ~/.ssh/id_rsa
// or .dex/skills -> ~/. So the files that come with the repository are
// screened here, before the libraries see them: one that is a link
// resolving outside the workspace is left out and reported as omitted.
// The user's own files (~/.dex/AGENTS.md, ~/.dex/skills and the
// skills_dirs of the user's config) are the user's and are not screened.

// within reports whether p, once its links are resolved, is dir or
// below it. dir is already resolved.
func within(dir, p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func omission(part, path, reason string) agentkit.Omission {
	return agentkit.Omission{Part: part, Source: "dex", What: path, Reason: reason}
}

// agentsFiles lists the AGENTS.md files that apply at dir, farthest
// first, the way agentsmd walks: dir and each ancestor. A file that is
// a link leaving the workspace is left out and reported.
func agentsFiles(dir string) (files []string, omitted []agentkit.Omission) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	var chain []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		chain = append([]string{filepath.Join(d, agentsmd.DefaultNames[0])}, chain...)
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, f := range chain {
		fi, err := os.Lstat(f)
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 && !within(real, f) {
			target, _ := filepath.EvalSymlinks(f)
			if target == "" {
				target = "a missing file"
			}
			omitted = append(omitted, omission(agentsmd.PartID, f, "symbolic link to "+target+", outside the workspace"))
			continue
		}
		files = append(files, f)
	}
	return files, omitted
}

// projectSkillsOK reports whether the project's skills directory can be
// offered: it, and every link below it, stays inside the workspace. A
// directory with one link that leaves is left out whole, since the
// kit takes directories, and reported.
func projectSkillsOK(dir string) (ok bool, omitted []agentkit.Omission) {
	skills := filepath.Join(dir, ".dex", "skills")
	if _, err := os.Lstat(skills); err != nil {
		return true, nil // absent: nothing to offer, nothing to refuse
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	if !within(real, skills) {
		return false, []agentkit.Omission{omission("skills", skills, "symbolic link outside the workspace")}
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
		return false, []agentkit.Omission{omission("skills", skills, fmt.Sprintf("holds a symbolic link outside the workspace, %s", bad))}
	}
	return true, nil
}
