package box

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mcpUnit is an MCP unit that passes every check, to vary.
func mcpUnit() Unit {
	return Unit{Image: "node:24", Command: []string{"server"}}
}

func TestHostConfigCheck(t *testing.T) {
	withUnit := func(name string, u Unit) HostConfig {
		return HostConfig{Units: map[string]Unit{"workspace": {}, name: u}, Profiles: map[string]Profile{"default": {Workspace: "workspace", MCP: []string{name}}}}
	}
	withWorkspace := func(u Unit) HostConfig {
		return HostConfig{Units: map[string]Unit{"workspace": u}}
	}
	project := func(mode string) *Files { return &Files{Project: mode} }
	for _, tc := range []struct {
		name string
		h    HostConfig
		err  string
	}{
		{name: "empty is the built-in", h: HostConfig{}},
		{name: "a Kata workspace", h: withWorkspace(Unit{Runtime: "kata"})},
		{name: "a bad runtime", h: HostConfig{Runtime: "runsc --debug"}, err: "runtime"},
		{name: "a bad unit runtime", h: withWorkspace(Unit{Runtime: "-x"}), err: "units.workspace.runtime"},
		{name: "a bad image", h: withWorkspace(Unit{Image: "-v /:/h"}), err: "units.workspace.image"},
		{name: "an image outside the patterns", h: HostConfig{Images: []string{"golang:*"}, Units: map[string]Unit{"workspace": {Image: "ubuntu:24.04"}}}, err: "not among the images"},
		{name: "the built-in image outside the patterns", h: HostConfig{Images: []string{"node:*"}}, err: "units.workspace.image"},
		{name: "a bad pattern", h: HostConfig{Images: []string{"golang:["}}, err: "images"},
		{name: "past the memory limit", h: HostConfig{Limits: Limits{Memory: "2g"}}, err: "more than the limit"},
		{name: "a unit past the cpu limit", h: HostConfig{Limits: Limits{CPUs: 1}, Units: map[string]Unit{"workspace": {CPUs: 1}, "big": {Image: "node:24", Command: []string{"x"}, CPUs: 4}}, Profiles: map[string]Profile{"default": {Workspace: "workspace"}}}, err: "units.big: cpus"},
		{name: "a bad limit", h: HostConfig{Limits: Limits{Memory: "lots"}}, err: "limits.memory"},
		{name: "an internet that says two things", h: withWorkspace(Unit{Internet: "on", Allow: []string{"a.test"}, Exfiltration: ExfiltrationAccepted}), err: "with units.workspace.allow"},
		{name: "allow with no hosts", h: withWorkspace(Unit{Internet: "allow", Exfiltration: ExfiltrationAccepted}), err: "names no hosts"},
		{name: "an address in allow", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Allow: []string{"10.0.0.1"}}), err: "allow"},
		{name: "no such default profile", h: HostConfig{DefaultProfile: "go"}, err: "default_profile"},
		{name: "several profiles and no default", h: HostConfig{Profiles: map[string]Profile{"a": {Workspace: "workspace"}, "b": {Workspace: "workspace"}}}, err: "default_profile: give one"},
		{name: "several profiles, one named default", h: HostConfig{Profiles: map[string]Profile{"default": {Workspace: "workspace"}, "b": {Workspace: "workspace"}}}},
		{name: "a bad profile name", h: HostConfig{Profiles: map[string]Profile{"go fast": {Workspace: "workspace"}}}, err: "profiles"},
		{name: "a bad unit name", h: HostConfig{Units: map[string]Unit{"Work_space": {}}}, err: `units "Work_space"`},
		{name: "units and no profile for them", h: HostConfig{Units: map[string]Unit{"box": {}}}, err: `profiles.default.workspace "workspace": no such unit`},
		{name: "a profile without a workspace", h: HostConfig{Profiles: map[string]Profile{"default": {}}}, err: "profiles.default.workspace: name"},
		{name: "a profile whose workspace is an MCP unit", h: HostConfig{Units: map[string]Unit{"s": mcpUnit()}, Profiles: map[string]Profile{"default": {Workspace: "s"}}}, err: "kind mcp, not workspace"},
		{name: "a profile whose mcp is the workspace", h: HostConfig{Profiles: map[string]Profile{"default": {Workspace: "workspace", MCP: []string{"workspace"}}}}, err: "kind workspace, not mcp"},
		{name: "a profile naming a unit twice", h: HostConfig{Units: map[string]Unit{"workspace": {}, "s": mcpUnit()}, Profiles: map[string]Profile{"default": {Workspace: "workspace", MCP: []string{"s", "s"}}}}, err: "named twice"},
		{name: "a profile naming no such unit", h: HostConfig{Profiles: map[string]Profile{"default": {Workspace: "workspace", MCP: []string{"github"}}}}, err: `mcp "github": no such unit`},
		{name: "an MCP unit", h: withUnit("github", Unit{Build: &Build{Base: "node:24-bookworm", Run: []string{"npm i -g x"}}, Command: []string{"x"}, Secrets: map[string]string{"GITHUB_TOKEN": "env:GITHUB_TOKEN"}, Allow: []string{"api.github.com"}})},
		{name: "an MCP unit without a command", h: withUnit("x", Unit{Kind: KindMCP, Image: "node:24"}), err: "units.x.command"},
		{name: "an MCP unit with image and build", h: withUnit("x", Unit{Image: "node:24", Build: &Build{Base: "node:24", Run: []string{"a"}}, Command: []string{"x"}}), err: "not both"},
		{name: "an MCP unit with neither", h: withUnit("x", Unit{Command: []string{"x"}}), err: "give image or build"},
		{name: "a build without run", h: withUnit("x", Unit{Build: &Build{Base: "node:24"}, Command: []string{"x"}}), err: "build.run: required"},
		{name: "a build line of two", h: withUnit("x", Unit{Build: &Build{Base: "node:24", Run: []string{"a\nb"}}, Command: []string{"x"}}), err: "one shell line"},
		{name: "a build base outside the patterns", h: HostConfig{Images: []string{"golang:*"}, Units: map[string]Unit{"workspace": {}, "x": {Build: &Build{Base: "node:24", Run: []string{"a"}}, Command: []string{"x"}}}, Profiles: map[string]Profile{"default": {Workspace: "workspace", MCP: []string{"x"}}}}, err: "units.x.build.base"},
		{name: "an empty command word", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x", ""}}), err: "empty argument"},
		{name: "a workspace with a command", h: withWorkspace(Unit{Kind: KindWorkspace, Command: []string{"x"}}), err: "workspace unit runs dax's own tools"},
		{name: "a workspace with secrets", h: withWorkspace(Unit{Secrets: map[string]string{"T": "env:T"}}), err: "workspace unit holds no secrets"},
		{name: "a workspace read-only", h: withWorkspace(Unit{Files: project(FilesRO)}), err: "a workspace unit has the project rw"},
		{name: "a workspace with no files", h: withWorkspace(Unit{Files: &Files{}}), err: "a workspace unit has the project rw"},
		{name: "a workspace with state", h: withWorkspace(Unit{Files: &Files{Project: FilesRW, State: FilesRW}})},
		{name: "bad files", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Files: project("all")}), err: "units.x.files.project"},
		{name: "bad state", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Files: &Files{State: "ro"}}), err: "units.x.files.state"},
		{name: "a bad kind", h: withUnit("x", Unit{Kind: "agent", Image: "node:24", Command: []string{"x"}}), err: "units.x.kind"},
		// Rule 1.
		{name: "secrets with internet on", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Internet: InternetOn, Secrets: map[string]string{"T": "env:T"}}), err: "(rule 1)"},
		{name: "secrets with an allowlist", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Allow: []string{"api.test"}, Secrets: map[string]string{"T": "env:T"}})},
		{name: "secrets with no internet", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"T": "env:T"}})},
		// Rule 2.
		{name: "the project read-only and an allowlist", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Files: project(FilesRO), Allow: []string{"a.test"}}), err: "(rule 2)"},
		{name: "the project and internet on", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Files: project(FilesRW), Internet: InternetOn}), err: "(rule 2)"},
		{name: "the project and the internet, accepted", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Files: project(FilesRO), Allow: []string{"a.test"}, Exfiltration: ExfiltrationAccepted})},
		{name: "the workspace and the internet", h: withWorkspace(Unit{Allow: []string{"proxy.golang.org"}}), err: "units.workspace: the project (rw) and internet allow"},
		{name: "the workspace and the internet, accepted", h: withWorkspace(Unit{Allow: []string{"proxy.golang.org"}, Exfiltration: ExfiltrationAccepted})},
		{name: "state alone and the internet", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Files: &Files{State: FilesRW}, Allow: []string{"a.test"}})},
		{name: "a bad exfiltration", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Exfiltration: "yes"}), err: "units.x.exfiltration"},
		// Rule 3.
		{name: "a secret written in", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"TOKEN": "ghp_abc"}}), err: "never written in the config"},
		{name: "a secret of another kind", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"TOKEN": "vault:x"}}), err: "env:VAR"},
		{name: "a bad secret name", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"A B": "env:A"}}), err: "variable's name (rule 3)"},
		{name: "an env secret of no name", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"T": "env:A B"}}), err: "env: and a variable's name"},
		{name: "a relative file secret", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"T": "file:token.txt"}}), err: "absolute path"},
		{name: "a command secret", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"T": "command:pass show gh"}})},
		{name: "a file secret", h: withUnit("x", Unit{Image: "node:24", Command: []string{"x"}, Secrets: map[string]string{"T": "file:/run/secrets/t"}})},
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

// brief is a Settings without its declaration's hash, to compare.
func brief(s Settings) Settings { s.Decl = ""; return s }

func TestHostConfigResolve(t *testing.T) {
	h := HostConfig{
		Runtime:        "runsc",
		DefaultProfile: "go",
		Units: map[string]Unit{
			"go":   {Image: "golang:1.26-bookworm", Allow: []string{"proxy.golang.org", "*.githubusercontent.com"}, Exfiltration: ExfiltrationAccepted},
			"open": {Internet: InternetOn, Memory: "8g", CPUs: 4, Exfiltration: ExfiltrationAccepted},
			"vm":   {Runtime: "io.containerd.kata.v2"},
			"rag":  {Build: &Build{Base: "python:3.12-slim", Run: []string{"pip install rag==1"}}, Command: []string{"rag"}, Files: &Files{Project: FilesRO, State: FilesRW}},
			"gh":   {Image: "node:24", Command: []string{"gh-mcp"}, Allow: []string{"api.github.com"}, Secrets: map[string]string{"GITHUB_TOKEN": "env:GH"}},
		},
		Profiles: map[string]Profile{
			"go":   {Workspace: "go", MCP: []string{"rag", "gh"}},
			"open": {Workspace: "open"},
			"vm":   {Workspace: "vm"},
		},
	}
	project := Files{Project: FilesRW}
	goAllow := Settings{Profile: "go", Unit: "go", Kind: KindWorkspace, Image: "golang:1.26-bookworm", Runtime: "runsc", Memory: "4g", CPUs: 2, Pids: 512, Files: project, Rules: Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org", "*.githubusercontent.com"}}}
	narrowed := goAllow
	narrowed.Rules.Allow = []string{"raw.githubusercontent.com"}
	off := goAllow
	off.Rules = Rules{Internet: InternetOff}
	open := Settings{Profile: "open", Unit: "open", Kind: KindWorkspace, Image: DefaultImage, Runtime: "runsc", Memory: "8g", CPUs: 4, Pids: 512, Files: project, Rules: Rules{Internet: InternetOn}}
	small := open
	small.Memory, small.CPUs, small.Pids = "512m", 1, 64
	openAllow := open
	openAllow.Rules = Rules{Internet: InternetAllow, Allow: []string{"example.com"}}
	vm := Settings{Profile: "vm", Unit: "vm", Kind: KindWorkspace, Image: DefaultImage, Runtime: "io.containerd.kata.v2", Memory: "4g", CPUs: 2, Pids: 512, Files: project, Rules: Rules{Internet: InternetOff}}
	ragBuild := Build{Base: "python:3.12-slim", Run: []string{"pip install rag==1"}}
	rag := Settings{Profile: "go", Unit: "rag", Kind: KindMCP, Image: ragBuild.Tag("rag"), Build: &ragBuild, Runtime: "runsc", Memory: DefaultUnitMemory, CPUs: DefaultUnitCPUs, Pids: DefaultUnitPids, Files: Files{Project: FilesRO, State: FilesRW}, Rules: Rules{Internet: InternetOff}, Command: []string{"rag"}}
	gh := Settings{Profile: "go", Unit: "gh", Kind: KindMCP, Image: "node:24", Runtime: "runsc", Memory: DefaultUnitMemory, CPUs: DefaultUnitCPUs, Pids: DefaultUnitPids, Rules: Rules{Internet: InternetAllow, Allow: []string{"api.github.com"}}, Secrets: map[string]string{"GITHUB_TOKEN": "env:GH"}, Command: []string{"gh-mcp"}}
	for _, tc := range []struct {
		name  string
		r     Request
		want  Settings
		units []Settings
		err   string
	}{
		{name: "the default profile, with its units", want: goAllow, units: []Settings{rag, gh}},
		{name: "some of the profile's units", r: Request{MCP: []string{"gh"}}, want: goAllow, units: []Settings{gh}},
		{name: "a unit the profile has not", r: Request{Profile: "open", MCP: []string{"gh"}}, err: `mcp "gh": not one of profile open's MCP units (none)`},
		{name: "a unit no one has", r: Request{MCP: []string{"jira"}}, err: `not one of profile go's MCP units (rag, gh)`},
		{name: "a profile by name", r: Request{Profile: "open"}, want: open},
		{name: "a unit's own runtime", r: Request{Profile: "vm"}, want: vm},
		{name: "no such profile", r: Request{Profile: "big"}, err: `profile "big": this box host has go, open, vm`},
		{name: "smaller limits", r: Request{Profile: "open", Tighten: Tighten{Memory: "512m", CPUs: 1, Pids: 64}}, want: small},
		{name: "more memory", r: Request{Tighten: Tighten{Memory: "8g"}}, err: "tighten.memory 8g is more than profile go's 4g"},
		{name: "more cpus", r: Request{Tighten: Tighten{CPUs: 3}}, err: "tighten.cpus"},
		{name: "more pids", r: Request{Tighten: Tighten{Pids: 1024}}, err: "tighten.pids"},
		{name: "off", r: Request{Tighten: Tighten{Internet: InternetOff}}, want: off, units: []Settings{rag, gh}},
		{name: "allow within the unit's", r: Request{Tighten: Tighten{Allow: []string{"Raw.GitHubUserContent.com"}}}, want: narrowed, units: []Settings{rag, gh}},
		{name: "allow outside the unit's", r: Request{Tighten: Tighten{Allow: []string{"example.com"}}}, err: "tighten.allow example.com is not among profile go's hosts"},
		{name: "allow keeps the unit's hosts", r: Request{Tighten: Tighten{Internet: InternetAllow}}, want: goAllow, units: []Settings{rag, gh}},
		{name: "on over allow", r: Request{Tighten: Tighten{Internet: InternetOn}}, err: "tighten.internet on is more than profile go's allow"},
		{name: "allow with hosts under on", r: Request{Profile: "open", Tighten: Tighten{Allow: []string{"example.com"}}}, want: openAllow},
		{name: "allow with no hosts under on", r: Request{Profile: "open", Tighten: Tighten{Internet: InternetAllow}}, err: "name the hosts"},
		{name: "allow under off", r: Request{Profile: "vm", Tighten: Tighten{Allow: []string{"example.com"}}}, err: "more than profile vm's off"},
		{name: "a bad tightening", r: Request{Tighten: Tighten{Memory: "lots"}}, err: "tighten.memory"},
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
			if !reflect.DeepEqual(brief(got.Workspace), tc.want) {
				t.Fatalf("Resolve's workspace = %+v, want %+v", got.Workspace, tc.want)
			}
			var units []Settings
			for _, u := range got.Units {
				if u.Decl == "" {
					t.Errorf("unit %s has no declaration's hash", u.Unit)
				}
				units = append(units, brief(u))
			}
			if !reflect.DeepEqual(units, tc.units) {
				t.Fatalf("Resolve's units = %+v, want %+v", units, tc.units)
			}
		})
	}
	// With no config, the built-in workspace unit alone: #98's defaults.
	got, err := (HostConfig{}).Resolve(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Settings{Profile: DefaultProfile, Unit: DefaultUnit, Kind: KindWorkspace, Image: DefaultImage, Runtime: DefaultRuntime, Memory: DefaultMemory, CPUs: DefaultCPUs, Pids: DefaultPids, Files: Files{Project: FilesRW}, Rules: Rules{Internet: InternetOff}}); !reflect.DeepEqual(brief(got.Workspace), want) || len(got.Units) != 0 {
		t.Fatalf("built-in = %+v, want %+v and no units", got, want)
	}
}

// The declaration's hash moves with what a container is made from, its
// secrets by reference, and not with its internet, which a session
// changes without making the container again.
func TestSettingsDecl(t *testing.T) {
	base := Unit{Image: "node:24", Command: []string{"s"}, Secrets: map[string]string{"T": "env:T"}}
	h := HostConfig{}
	decl := func(u Unit) string { return h.filled("s", u).Decl }
	same := decl(base)
	internet := base
	internet.Allow = []string{"a.test"}
	if decl(internet) != same {
		t.Error("the internet moved the declaration's hash")
	}
	for name, u := range map[string]Unit{
		"image":   {Image: "node:22", Command: base.Command, Secrets: base.Secrets},
		"command": {Image: base.Image, Command: []string{"other"}, Secrets: base.Secrets},
		"secret":  {Image: base.Image, Command: base.Command, Secrets: map[string]string{"T": "file:/t"}},
		"files":   {Image: base.Image, Command: base.Command, Secrets: base.Secrets, Files: &Files{Project: FilesRO}},
		"memory":  {Image: base.Image, Command: base.Command, Secrets: base.Secrets, Memory: "2g"},
	} {
		if decl(u) == same {
			t.Errorf("a changed %s left the declaration's hash", name)
		}
	}
}

func TestBuild(t *testing.T) {
	b := Build{Base: "node:24-bookworm-slim", Run: []string{"npm i -g a@1", "npm cache clean --force"}}
	if got, want := b.Dockerfile(), "FROM node:24-bookworm-slim\nRUN npm i -g a@1\nRUN npm cache clean --force\n"; got != want {
		t.Errorf("Dockerfile = %q, want %q", got, want)
	}
	tag := b.Tag("files")
	if !strings.HasPrefix(tag, "dax-unit/files:") || len(tag) != len("dax-unit/files:")+12 {
		t.Errorf("Tag = %q", tag)
	}
	if b.Tag("files") != tag {
		t.Error("the tag is not stable")
	}
	other := b
	other.Run = []string{"npm i -g a@2"}
	if other.Tag("files") == tag {
		t.Error("another declaration has the same tag")
	}
}

func TestFilesJSON(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Files
		err  string
	}{
		{in: `"none"`, want: Files{}},
		{in: `{"project":"ro"}`, want: Files{Project: FilesRO}},
		{in: `{"project":"rw","state":"rw"}`, want: Files{Project: FilesRW, State: FilesRW}},
		{in: `"all"`, err: `want "none"`},
		{in: `{"home":"rw"}`, err: "unknown field"},
	} {
		var f Files
		err := json.Unmarshal([]byte(tc.in), &f)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || f != tc.want {
			t.Errorf("%s = %+v, %v; want %+v", tc.in, f, err, tc.want)
		}
	}
	if got := (Files{}).String(); got != "none" {
		t.Errorf("none = %q", got)
	}
	if got := (Files{Project: FilesRO, State: FilesRW}).String(); got != "project ro, state rw" {
		t.Errorf("String = %q", got)
	}
}

// What the session is told, and records, names a secret and never
// holds its reference or value.
func TestBoxInfo(t *testing.T) {
	h := HostConfig{
		Units: map[string]Unit{
			"workspace": {},
			"gh":        {Image: "node:24", Command: []string{"gh"}, Allow: []string{"api.github.com"}, Secrets: map[string]string{"GITHUB_TOKEN": "env:GH_SECRET_SOURCE"}},
		},
		Profiles: map[string]Profile{"default": {Workspace: "workspace", MCP: []string{"gh"}}},
	}
	r, err := h.Resolve(Request{})
	if err != nil {
		t.Fatal(err)
	}
	info := r.BoxInfo()
	want := BoxInfo{Profile: "default", Units: []Envelope{
		{Name: "workspace", Kind: KindWorkspace, Image: DefaultImage, Runtime: DefaultRuntime, Files: "project rw", Internet: InternetOff},
		{Name: "gh", Kind: KindMCP, Image: "node:24", Runtime: DefaultRuntime, Files: "none", Internet: InternetAllow, Allow: []string{"api.github.com"}, Secrets: []string{"GITHUB_TOKEN"}},
	}}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("BoxInfo = %+v, want %+v", info, want)
	}
	data, _ := json.Marshal(info)
	if strings.Contains(string(data), "GH_SECRET_SOURCE") {
		t.Fatalf("the envelope holds a secret's reference: %s", data)
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
		{`{"units":{"workspace":{"image":"golang:1.26"}}}`, ""},
		{`{"units":{"workspace":{},"files":{"build":{"base":"node:24","run":["npm i -g x"]},"command":["x"],"files":{"project":"ro"}}},"profiles":{"default":{"workspace":"workspace","mcp":["files"]}}}`, ""},
		{`{"units":{"workspace":{},"files":{"image":"node:24","command":["x"],"files":"none"}},"profiles":{"default":{"workspace":"workspace","mcp":["files"]}}}`, ""},
		{`{"units":{"workspace":{"files":"all"}}}`, `want "none"`},
		{`{"profile":{}}`, "unknown field"},
		{`{"units":{"workspace":{"network":"host"}}}`, "unknown field"},
		{`{"profiles":{"go":{"image":"golang:1.26"}}}`, "unknown field"},
		{`{"mcp":{}}`, "unknown field"},
		{`{"runtime":"run sc"}`, "runtime"},
		{`{"checkouts":{"allow":["https://github.com/me/*","git@gitlab.com:team/*"],"dir":"/srv/checkouts"}}`, ""},
		{`{"checkouts":{"allow":["https://github.com/["]}}`, "checkouts.allow"},
		{`{"checkouts":{"allow":[""]}}`, "checkouts.allow: an empty pattern"},
		{`{"checkouts":{"dir":"checkouts"}}`, "checkouts.dir"},
		{`{"checkouts":{"allow":[],"clone":"deep"}}`, "unknown field"},
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
