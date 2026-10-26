package agent

// The trust a project's MCP servers need before they start: a
// repository's config may name servers, which run commands or reach
// URLs the repository chose, so they start only once the user has
// trusted that project's exact set (-trust-mcp), recorded here.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/internal/private"
)

// DefaultMCPTrustFile is where the projects whose MCP servers the user
// trusted are kept: mcp-trust.json in dir, the user's dax directory.
func DefaultMCPTrustFile(dir string) string { return filepath.Join(dir, "mcp-trust.json") }

// mcpTrustFile is the trust file's contents: one entry per project, the
// set the user last trusted for it.
type mcpTrustFile struct {
	Projects []trustedProject `json:"projects"`
}

// trustedProject is a project, by its workspace (its identity, as -list
// has it), and the hash of the server set trusted for it.
type trustedProject struct {
	Workspace recordedWorkspace `json:"workspace"`
	Servers   string            `json:"servers_sha256"`
}

// serverSetHash is the SHA-256 of servers' canonical JSON: each server's
// name, command, URL and placement, in name order. A change to any of
// them is another set. here says the workspace cannot start a process,
// so a command placed in it would run on this machine: that is a
// placement of its own, so a set trusted while it ran in the workspace
// is not trusted to run here. (A workspace that can start one leaves
// the hash as it was before here was kept.)
func serverSetHash(servers []MCPServer, here bool) string {
	type canon struct {
		Name    string `json:"name"`
		Command string `json:"command,omitempty"`
		URL     string `json:"url,omitempty"`
		Host    bool   `json:"host,omitempty"`
		Here    bool   `json:"here,omitempty"`
	}
	set := make([]canon, 0, len(servers))
	for _, m := range servers {
		set = append(set, canon{m.Name, m.Command, m.URL, m.Host, here && m.URL == "" && !m.Host})
	}
	slices.SortFunc(set, func(a, b canon) int { return strings.Compare(a.Name, b.Name) })
	b, _ := json.Marshal(set)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// trusted is servers less the project's, unless the user trusts the
// project's set for the project in d, placed as here says
// (serverSetHash): given trust, the set is recorded as the project's in
// file; otherwise it must be the set file holds for the project. What
// is left out is said once, with how to trust it.
func trusted(servers []MCPServer, d workspace.Descriptor, here bool, file string, trust bool) []MCPServer {
	var mine, project []MCPServer
	for _, m := range servers {
		if m.Project {
			project = append(project, m)
		} else {
			mine = append(mine, m)
		}
	}
	if len(project) == 0 {
		return servers
	}
	key := recordedWorkspace{Kind: d.Kind, Ref: d.Ref, Root: d.Root}
	h := serverSetHash(project, here)
	if trust {
		if err := recordTrust(file, key, h); err != nil {
			fmt.Fprintf(warnings{}, "dax: the project's MCP servers are trusted for this session, but the trust could not be kept: %v\n", err)
		}
		return servers
	}
	t, err := readTrust(file)
	if err != nil {
		fmt.Fprintf(warnings{}, "dax: %v\n", err)
	}
	if slices.Contains(t.Projects, trustedProject{Workspace: key, Servers: h}) {
		return servers
	}
	names := make([]string, len(project))
	for i, m := range project {
		names[i] = m.Name
	}
	fmt.Fprintf(warnings{}, "dax: not starting the MCP servers this project's config names (%s), since you have not trusted this set for %s; read them in its .dax/config.json and run with -trust-mcp to start them\n", strings.Join(names, ", "), where(d))
	return mine
}

// readTrust is file's contents; a missing file trusts nothing, and so
// does one another account could have written (checkPrivate).
func readTrust(file string) (mcpTrustFile, error) {
	var t mcpTrustFile
	if err := checkPrivate(file); err != nil {
		return t, fmt.Errorf("the MCP trust file %s: %w; trusting nothing in it", file, err)
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, fmt.Errorf("the MCP trust file: %w", err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return mcpTrustFile{}, fmt.Errorf("the MCP trust file %s: %w; trusting nothing in it", file, err)
	}
	return t, nil
}

// recordTrust keeps h as the set trusted for the project key in file,
// in place of any set before, in a private directory, the file
// replaced whole.
func recordTrust(file string, key recordedWorkspace, h string) error {
	if err := private.SecureDir(filepath.Dir(file)); err != nil {
		return err
	}
	// A file that cannot be trusted is replaced by one that holds this
	// trust alone: what it held was untrusted anyway.
	t, err := readTrust(file)
	if err != nil {
		t = mcpTrustFile{}
	}
	t.Projects = slices.DeleteFunc(t.Projects, func(p trustedProject) bool { return p.Workspace == key })
	t.Projects = append(t.Projects, trustedProject{Workspace: key, Servers: h})
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".mcp-trust-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// checkPrivate refuses a trust file another account could have written:
// one whose directory or itself is writable by group or others, or that
// is not this user's. A missing file is no refusal: it trusts nothing.
func checkPrivate(file string) error {
	for _, p := range []string{filepath.Dir(file), file} {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by other users (mode %04o)", p, fi.Mode().Perm())
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return fmt.Errorf("%s belongs to another user", p)
		}
	}
	return nil
}
