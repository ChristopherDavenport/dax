package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"
)

// The library that reads AGENTS.md files follows symbolic links, and
// what it reads goes into the system prompt, which goes to the
// provider. A repository can ship AGENTS.md -> ~/.ssh/id_rsa. So the
// files that come with the repository are screened here, before the
// library sees them: one that is a link resolving outside the
// workspace is left out and reported as omitted. The user's own
// ~/.dax/AGENTS.md is the user's and is not screened. dax-skills
// screens a repository's skills the same way.

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
	return agentkit.Omission{Part: part, Source: "dax", What: path, Reason: reason}
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
