package config

import (
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"
)

// scoped is a served workspace whose client reports scopes; nothing
// else of it is called.
type scoped struct {
	remote.Workspace
	scopes []remote.Scope
}

func (s scoped) Scopes() []remote.Scope { return s.scopes }

// wrapped is a served workspace behind a wrapper that embeds it, as
// dax's identify does.
type wrapped struct{ remote.Workspace }

// ServersOnly and ServersAlone read the grant the client reports and
// nothing else: servers alone is exactly [servers]; servers-only is
// servers without write or exec, read or not; a workspace that is not
// served has no grant and is neither.
func TestServersGrantIsTheReportedScopes(t *testing.T) {
	const (
		r, w, x, s = remote.ScopeRead, remote.ScopeWrite, remote.ScopeExec, remote.ScopeServers
	)
	local, err := workspace.NewLocal(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	for _, tc := range []struct {
		name        string
		ws          workspace.Workspace
		only, alone bool
	}{
		{"servers", scoped{scopes: []remote.Scope{s}}, true, true},
		{"servers behind a wrapper", wrapped{scoped{scopes: []remote.Scope{s}}}, true, true},
		{"servers and read", scoped{scopes: []remote.Scope{r, s}}, true, false},
		{"read and servers, wrapped", wrapped{scoped{scopes: []remote.Scope{s, r}}}, true, false},
		{"servers and write", scoped{scopes: []remote.Scope{s, w}}, false, false},
		{"servers and exec", scoped{scopes: []remote.Scope{x, s}}, false, false},
		{"everything", scoped{scopes: []remote.Scope{r, w, x, s}}, false, false},
		{"no servers", scoped{scopes: []remote.Scope{r}}, false, false},
		{"servers twice", scoped{scopes: []remote.Scope{s, s}}, true, false},
		{"none", scoped{}, false, false},
		{"not served", local, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServersOnly(tc.ws); got != tc.only {
				t.Errorf("ServersOnly = %v, want %v", got, tc.only)
			}
			if got := ServersAlone(tc.ws); got != tc.alone {
				t.Errorf("ServersAlone = %v, want %v", got, tc.alone)
			}
		})
	}
}
