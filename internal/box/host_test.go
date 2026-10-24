package box

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHostConfigCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    HostConfig
		err  string
	}{
		{name: "empty is the built-in", h: HostConfig{}},
		{name: "a Kata profile", h: HostConfig{Profiles: map[string]Profile{"default": {}, "vm": {Runtime: "io.containerd.kata.v2"}}}},
		{name: "a bad runtime", h: HostConfig{Runtime: "runsc --debug"}, err: "runtime"},
		{name: "a bad profile runtime", h: HostConfig{Profiles: map[string]Profile{"default": {Runtime: "-x"}}}, err: "profiles.default.runtime"},
		{name: "a bad image", h: HostConfig{Profiles: map[string]Profile{"default": {Image: "-v /:/h"}}}, err: "profiles.default.image"},
		{name: "an image outside the patterns", h: HostConfig{Images: []string{"golang:*"}, Profiles: map[string]Profile{"default": {Image: "ubuntu:24.04"}}}, err: "not among the images"},
		{name: "the built-in image outside the patterns", h: HostConfig{Images: []string{"node:*"}}, err: "profiles.default.image"},
		{name: "a bad pattern", h: HostConfig{Images: []string{"golang:["}}, err: "images"},
		{name: "a profile past the memory limit", h: HostConfig{Limits: Limits{Memory: "2g"}}, err: "more than the limit"},
		{name: "a profile past the cpu limit", h: HostConfig{Limits: Limits{CPUs: 1}, Profiles: map[string]Profile{"default": {CPUs: 1}, "big": {CPUs: 4}}}, err: "profiles.big: cpus"},
		{name: "a bad limit", h: HostConfig{Limits: Limits{Memory: "lots"}}, err: "limits.memory"},
		{name: "an internet that says two things", h: HostConfig{Profiles: map[string]Profile{"default": {Internet: "on", Allow: []string{"a.test"}}}}, err: "with profiles.default.allow"},
		{name: "allow with no hosts", h: HostConfig{Profiles: map[string]Profile{"default": {Internet: "allow"}}}, err: "names no hosts"},
		{name: "an address in allow", h: HostConfig{Profiles: map[string]Profile{"default": {Allow: []string{"10.0.0.1"}}}}, err: "allow"},
		{name: "no such default profile", h: HostConfig{DefaultProfile: "go"}, err: "default_profile"},
		{name: "several profiles and no default", h: HostConfig{Profiles: map[string]Profile{"a": {}, "b": {}}}, err: "default_profile: give one"},
		{name: "several profiles, one named default", h: HostConfig{Profiles: map[string]Profile{"default": {}, "b": {}}}},
		{name: "a bad profile name", h: HostConfig{Profiles: map[string]Profile{"go fast": {}}}, err: "profiles"},
		{name: "an MCP server", h: HostConfig{MCP: map[string]MCPServer{"github": {Build: &Build{Base: "node:24-bookworm", Run: "npm i -g x"}, Command: "x", Secrets: map[string]string{"GITHUB_TOKEN": "env:GITHUB_TOKEN"}, Allow: []string{"api.github.com"}, Files: "none"}}}},
		{name: "an MCP server without a command", h: HostConfig{MCP: map[string]MCPServer{"x": {Image: "node:24"}}}, err: "mcp.x.command"},
		{name: "an MCP server with image and build", h: HostConfig{MCP: map[string]MCPServer{"x": {Image: "node:24", Build: &Build{Base: "node:24", Run: "a"}, Command: "x"}}}, err: "not both"},
		{name: "an MCP server with neither", h: HostConfig{MCP: map[string]MCPServer{"x": {Command: "x"}}}, err: "give image or build"},
		{name: "a build without run", h: HostConfig{MCP: map[string]MCPServer{"x": {Build: &Build{Base: "node:24"}, Command: "x"}}}, err: "build.run"},
		{name: "a build base outside the patterns", h: HostConfig{Images: []string{"golang:*"}, MCP: map[string]MCPServer{"x": {Build: &Build{Base: "node:24", Run: "a"}, Command: "x"}}}, err: "mcp.x.build.base"},
		{name: "a secret written in", h: HostConfig{MCP: map[string]MCPServer{"x": {Image: "node:24", Command: "x", Secrets: map[string]string{"TOKEN": "ghp_abc"}}}}, err: "never written in the config"},
		{name: "a secret of another kind", h: HostConfig{MCP: map[string]MCPServer{"x": {Image: "node:24", Command: "x", Secrets: map[string]string{"TOKEN": "vault:x"}}}}, err: "env:NAME"},
		{name: "a bad secret name", h: HostConfig{MCP: map[string]MCPServer{"x": {Image: "node:24", Command: "x", Secrets: map[string]string{"A B": "env:A"}}}}, err: "variable's name"},
		{name: "bad files", h: HostConfig{MCP: map[string]MCPServer{"x": {Image: "node:24", Command: "x", Files: "all"}}}, err: "mcp.x.files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.Check()
			if tc.err == "" {
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Check error = %v, want one naming %q", err, tc.err)
			}
		})
	}
}

func TestHostConfigResolve(t *testing.T) {
	h := HostConfig{
		Runtime:        "runsc",
		DefaultProfile: "go",
		Profiles: map[string]Profile{
			"go":   {Image: "golang:1.26-bookworm", Allow: []string{"proxy.golang.org", "*.githubusercontent.com"}},
			"open": {Internet: InternetOn, Memory: "8g", CPUs: 4},
			"vm":   {Runtime: "io.containerd.kata.v2"},
		},
	}
	goAllow := Settings{Profile: "go", Image: "golang:1.26-bookworm", Runtime: "runsc", Memory: "4g", CPUs: 2, Pids: 512, Rules: Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org", "*.githubusercontent.com"}}}
	narrowed := goAllow
	narrowed.Rules.Allow = []string{"raw.githubusercontent.com"}
	off := goAllow
	off.Rules = Rules{Internet: InternetOff}
	open := Settings{Profile: "open", Image: DefaultImage, Runtime: "runsc", Memory: "8g", CPUs: 4, Pids: 512, Rules: Rules{Internet: InternetOn}}
	small := open
	small.Memory, small.CPUs, small.Pids = "512m", 1, 64
	openAllow := open
	openAllow.Rules = Rules{Internet: InternetAllow, Allow: []string{"example.com"}}
	vm := Settings{Profile: "vm", Image: DefaultImage, Runtime: "io.containerd.kata.v2", Memory: "4g", CPUs: 2, Pids: 512, Rules: Rules{Internet: InternetOff}}
	for _, tc := range []struct {
		name string
		r    Request
		want Settings
		err  string
	}{
		{name: "the default profile", want: goAllow},
		{name: "a profile by name", r: Request{Profile: "open"}, want: open},
		{name: "a profile's own runtime", r: Request{Profile: "vm"}, want: vm},
		{name: "no such profile", r: Request{Profile: "big"}, err: `profile "big": this box host has go, open, vm`},
		{name: "smaller limits", r: Request{Profile: "open", Tighten: Tighten{Memory: "512m", CPUs: 1, Pids: 64}}, want: small},
		{name: "more memory", r: Request{Tighten: Tighten{Memory: "8g"}}, err: "tighten.memory 8g is more than profile go's 4g"},
		{name: "more cpus", r: Request{Tighten: Tighten{CPUs: 3}}, err: "tighten.cpus"},
		{name: "more pids", r: Request{Tighten: Tighten{Pids: 1024}}, err: "tighten.pids"},
		{name: "off", r: Request{Tighten: Tighten{Internet: InternetOff}}, want: off},
		{name: "allow within the profile's", r: Request{Tighten: Tighten{Allow: []string{"Raw.GitHubUserContent.com"}}}, want: narrowed},
		{name: "allow outside the profile's", r: Request{Tighten: Tighten{Allow: []string{"example.com"}}}, err: "tighten.allow example.com is not among profile go's hosts"},
		{name: "allow keeps the profile's hosts", r: Request{Tighten: Tighten{Internet: InternetAllow}}, want: goAllow},
		{name: "on over allow", r: Request{Tighten: Tighten{Internet: InternetOn}}, err: "tighten.internet on is more than profile go's allow"},
		{name: "allow with hosts under on", r: Request{Profile: "open", Tighten: Tighten{Allow: []string{"example.com"}}}, want: openAllow},
		{name: "allow with no hosts under on", r: Request{Profile: "open", Tighten: Tighten{Internet: InternetAllow}}, err: "name the hosts"},
		{name: "allow under off", r: Request{Profile: "vm", Tighten: Tighten{Allow: []string{"example.com"}}}, err: "more than profile vm's off"},
		{name: "a bad tightening", r: Request{Tighten: Tighten{Memory: "lots"}}, err: "tighten.memory"},
		{name: "an MCP server, not yet", r: Request{MCP: []string{"github"}}, err: "not run yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.Resolve(tc.r)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Resolve error = %v, want one naming %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
	// With no config, the built-in profile: #98's defaults.
	got, err := (HostConfig{}).Resolve(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Settings{Profile: DefaultProfile, Image: DefaultImage, Runtime: DefaultRuntime, Memory: DefaultMemory, CPUs: DefaultCPUs, Pids: DefaultPids, Rules: Rules{Internet: InternetOff}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("built-in = %+v, want %+v", got, want)
	}
}

func TestLoadHostConfig(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.json")
	if h, err := LoadHostConfig(missing, false); err != nil || !reflect.DeepEqual(h, HostConfig{}) {
		t.Errorf("a missing default file = %+v, %v; want the built-in", h, err)
	}
	if _, err := LoadHostConfig(missing, true); err == nil {
		t.Error("a missing named file was taken")
	}
	for _, tc := range []struct{ body, err string }{
		{`{"profiles":{"go":{"image":"golang:1.26"}}}`, ""},
		{`{"profile":{}}`, "unknown field"},
		{`{"profiles":{"go":{"network":"host"}}}`, "unknown field"},
		{`{"runtime":"run sc"}`, "runtime"},
		{`not json`, "box host config"},
	} {
		path := filepath.Join(dir, "box-host.json")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadHostConfig(path, true)
		if tc.err == "" && err != nil {
			t.Errorf("%s: %v", tc.body, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: err = %v, want %q", tc.body, err, tc.err)
		}
	}
}

func TestCovers(t *testing.T) {
	for _, tc := range []struct {
		p, h string
		want bool
	}{
		{"a.test", "a.test", true},
		{"a.test", "b.a.test", false},
		{"*.a.test", "b.a.test", true},
		{"*.a.test", "*.b.a.test", true},
		{"*.a.test", "a.test", false},
		{"*.a.test", "*.a.test", true},
		{"*.b.a.test", "*.a.test", false},
		{"*.a.test", "ba.test", false},
	} {
		if got := covers(tc.p, tc.h); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.p, tc.h, got, tc.want)
		}
	}
}
