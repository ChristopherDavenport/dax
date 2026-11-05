package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// The session's env entry records the workspace's host and instance
// beside its kind and ref (agentworkspace RFC 0001 §12, agentsession's
// workspace.host and workspace.instance): a local workspace names this
// machine, so a session resumed on another reads as another file
// system (#155).
func TestTheEnvEntryNamesTheWorkspacesHost(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	host := s.x.Descriptor().Host
	if host == "" {
		t.Fatal("the local workspace names no host")
	}
	if _, err := promptOn(ctx, s, "hi", nil); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(string(projected(t, o, s)), "\n") {
		var e struct {
			Type      string `json:"type"`
			Workspace struct {
				Kind, Host string
			} `json:"workspace"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Type == "env" {
			found = true
			if e.Workspace.Host != host {
				t.Errorf("env workspace %+v, want host %q", e.Workspace, host)
			}
		}
	}
	if !found {
		t.Error("no env entry recorded")
	}
}
