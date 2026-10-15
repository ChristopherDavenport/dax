// Package memory is dax-memory, the extension that gives the model a
// memory between sessions: a store with the user's scope and one for
// each project, the workspace the tools act in, the memory tools, and
// the memory block in the system prompt. It is built only from what package extension offers
// any extension, and dax's private-directory check.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/private"
	"github.com/ChristopherDavenport/dax/policy"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-memory.
const Name = "dax-memory"

// New is dax-memory over the store in dir, which is made private (0700)
// when the session starts, with a warning if it was not. The model
// reads and writes a user scope and the scope of the project the tools
// act in (WorkspaceScope). It owns the memory tools and ships one rule: a search
// runs unasked, while a save, a patch and a forget ask.
func New(dir string) extension.Extension {
	return extension.Extension{
		Name:   Name,
		Owns:   []string{agentmemory.SaveTool, agentmemory.PatchTool, agentmemory.ForgetTool, agentmemory.SearchTool},
		Policy: policy.Rules{Allow: []string{agentmemory.SearchTool}},
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			if err := private.SecureDir(dir); err != nil {
				return nil, fmt.Errorf("memory: %w", err)
			}
			mem, err := filestore.Open(dir)
			if err != nil {
				return nil, fmt.Errorf("memory: %w", err)
			}
			project := ProjectScope(env.Dir())
			if ws := env.ToolEnv().Workspace; ws != nil {
				project = WorkspaceScope(ws.Descriptor(), env.Dir())
			}
			return []agentkit.Option{agentkit.WithMemory(mem, "user", project)}, nil
		},
	}
}

// ProjectScope is the memory scope of a working directory. One store
// holds every project's memory beside the user's, so each project gets
// a scope of its own, named for its directory and a hash of its path.
func ProjectScope(dir string) agentmemory.Scope {
	sum := sha256.Sum256([]byte(dir))
	return scope(filepath.Base(dir), sum[:])
}

// WorkspaceScope is the memory scope of the project a session's tools
// act in, d, for a session started in dir on this machine. It is
// exported so that a program built on dax that reads the store finds
// a session's project where dax-memory keeps it.
//
// This machine's workspace (kind local and no ref, which is what
// workspace.Local says) is ProjectScope(dir), the scope a project had
// before the workspace decided, so the memory kept then is still the
// project's. Any other, a container or a host an executor runs on, is
// named for the base of its root with a hash of its kind, ref and root:
// two sandboxes started from one directory here are two projects, and
// one sandbox is one project from wherever dax was started.
func WorkspaceScope(d workspace.Descriptor, dir string) agentmemory.Scope {
	if d.Kind == workspace.KindLocal && d.Ref == "" {
		return ProjectScope(dir)
	}
	// A JSON array, so no kind, ref or root can be read as another's.
	key, _ := json.Marshal([]string{d.Kind, d.Ref, d.Root})
	sum := sha256.Sum256(key)
	return scope(path.Base(filepath.ToSlash(d.Root)), sum[:])
}

// scope is a project's scope: "project-", base in lower-case kebab
// form, and the first four bytes of sum in hex.
func scope(base string, sum []byte) agentmemory.Scope {
	var b strings.Builder
	b.WriteString("project-")
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if !strings.HasSuffix(b.String(), "-") {
		b.WriteByte('-')
	}
	b.WriteString(hex.EncodeToString(sum[:4]))
	return agentmemory.Scope(b.String())
}
