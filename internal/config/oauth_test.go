package config

import (
	"slices"
	"strings"
	"testing"
)

// A url server's oauth block is the user's alone, checked, and its
// client secret's variable is a credential no other child gets (#175).
func TestAURLServersOAuthBlock(t *testing.T) {
	l, err := Parse([]byte(`{"mcp_servers":{
		"linear":{"url":"https://mcp.linear.app/mcp","oauth":{}},
		"corp":{"url":"https://mcp.corp.example/mcp","oauth":{"client_id":"dax","client_secret_env":"CORP_MCP_SECRET","callback_port":8765}}}}`), "/u/config.json", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Resolve([]Layer{l}, Flags{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var corp *MCPOAuth
	for _, m := range s.MCP {
		if m.Name == "corp" {
			corp = m.OAuth
		}
		if m.Name == "linear" && m.OAuth == nil {
			t.Error("linear's oauth block was dropped")
		}
	}
	if corp == nil || corp.ClientID != "dax" || corp.CallbackPort != 8765 {
		t.Fatalf("corp's oauth %+v", corp)
	}
	if !slices.Contains(s.Credentials(), "CORP_MCP_SECRET") || !slices.Contains(l.Credentials(), "CORP_MCP_SECRET") {
		t.Errorf("credentials %v, layer's %v: want the client secret's variable", s.Credentials(), l.Credentials())
	}
	for _, tc := range []struct{ name, json, want string }{
		{"project", `{"mcp_servers":{"x":{"url":"https://x.example/mcp","oauth":{}}}}`, "oauth: a project file may not"},
		{"command", `{"mcp_servers":{"x":{"command":"srv","oauth":{}}}}`, "oauth is for a url"},
		{"both", `{"mcp_servers":{"x":{"url":"https://x.example/mcp","token_env":"X_TOKEN","oauth":{}}}}`, "give token_env or oauth, not both"},
		{"secret without client", `{"mcp_servers":{"x":{"url":"https://x.example/mcp","oauth":{"client_secret_env":"S"}}}}`, "client_secret_env is for a client_id"},
		{"client without port", `{"mcp_servers":{"x":{"url":"https://x.example/mcp","oauth":{"client_id":"dax"}}}}`, "needs callback_port"},
		{"bad port", `{"mcp_servers":{"x":{"url":"https://x.example/mcp","oauth":{"callback_port":70000}}}}`, "callback_port 70000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json), "/p/.dax/config.json", tc.name == "project")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}
