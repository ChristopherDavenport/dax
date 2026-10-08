// Package memory is dax-memory, the extension that gives the model a
// memory between sessions: a store with the user's scope and one for
// each project directory, the memory tools, and the memory block in the
// system prompt. It is built only from what package extension offers
// any extension, and dax's private-directory check.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/private"
	"github.com/ChristopherDavenport/dax/policy"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-memory.
const Name = "dax-memory"

// New is dax-memory over the store in dir, which is made private (0700)
// when the session starts, with a warning if it was not. The model
// reads and writes a user scope and the scope of the working directory
// (ProjectScope). It owns the memory tools and ships one rule: a search
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
			return []agentkit.Option{agentkit.WithMemory(mem, "user", ProjectScope(env.Dir()))}, nil
		},
	}
}

// ProjectScope is the memory scope of a working directory. One store
// holds every project's memory beside the user's, so each project gets
// a scope of its own, named for its directory and a hash of its path.
func ProjectScope(dir string) agentmemory.Scope {
	var b strings.Builder
	b.WriteString("project-")
	for _, r := range strings.ToLower(filepath.Base(dir)) {
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
	sum := sha256.Sum256([]byte(dir))
	b.WriteString(hex.EncodeToString(sum[:4]))
	return agentmemory.Scope(b.String())
}
