package box

// This file is the box host's config: the units `dax box serve` makes
// on its machine, each an envelope of files, connectivity and secrets,
// the profiles a session asks for, and what a session may ask of them,
// which only ever narrows.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The built-in unit and profile: a box host with no config makes the
// workspace unit alone, as this box.
const (
	DefaultProfile = "default"
	DefaultUnit    = "workspace"
	DefaultImage   = "golang:1.26-bookworm"
	DefaultRuntime = "runsc"
	DefaultMemory  = "4g"
	DefaultCPUs    = 2
	DefaultPids    = 512
)

// An MCP unit's limits when it names none: a server, not a build.
const (
	DefaultUnitMemory = "1g"
	DefaultUnitCPUs   = 1
	DefaultUnitPids   = 256
)

// The kinds of unit.
const (
	// KindWorkspace is the unit dax's own tools run in: the project box,
	// its `dax execute`.
	KindWorkspace = "workspace"
	// KindMCP is a unit that runs an MCP server.
	KindMCP = "mcp"
)

// The modes of a unit's files.
const (
	FilesRO = "ro"
	FilesRW = "rw"
)

// ExfiltrationAccepted is the one value of Unit.Exfiltration: the
// administrator's word that a unit with the project and the internet
// may send the one to the other (rule 2).
const ExfiltrationAccepted = "accepted"

// UnitCommand is the reserved first word of a dax/process.start that
// asks the box host for a unit's server: ["dax-unit", NAME]. It is not
// a program; behind no box host it is not found.
const UnitCommand = "dax-unit"

// HostConfigPath is the box host config's default place: box-host.json
// in dax's config directory on the box host's machine.
func HostConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "dax", "box-host.json"), nil
}

// HostConfig is the box host's config, its machine's administrator's:
// what no session can widen.
type HostConfig struct {
	// Runtime is the OCI runtime a unit that names none runs under; any
	// runtime docker has registered (runsc, kata, io.containerd.kata.v2).
	Runtime string `json:"runtime,omitempty"`
	// Limits are ceilings no unit and no tightening goes past.
	Limits Limits `json:"limits,omitzero"`
	// Images are glob patterns (path.Match) every image a unit names, and
	// every build's base, must match; none means any.
	Images []string `json:"images,omitempty"`
	// DefaultProfile is the profile a session that names none gets.
	DefaultProfile string `json:"default_profile,omitempty"`
	// Units are the tool groups the box host makes, by name: each an
	// envelope of files, connectivity and secrets.
	Units map[string]Unit `json:"units,omitempty"`
	// Profiles are what a session asks for: a workspace unit and the MCP
	// units beside it.
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

// Limits bound a unit. A zero field is no bound.
type Limits struct {
	Memory string  `json:"memory,omitempty"`
	CPUs   float64 `json:"cpus,omitempty"`
	Pids   int64   `json:"pids,omitempty"`
}

// Unit is a tool group's envelope, one schema for every kind: what it
// runs, what files it has, what it can reach and what secrets it holds.
// A field left out takes the built-in default for its kind.
type Unit struct {
	// Kind is KindWorkspace or KindMCP; empty is KindMCP when Command is
	// set and KindWorkspace otherwise.
	Kind string `json:"kind,omitempty"`
	// Image the unit runs, or Build to make one; a workspace unit with
	// neither runs DefaultImage.
	Image string `json:"image,omitempty"`
	Build *Build `json:"build,omitempty"`
	// Runtime, and the limits, within the config's.
	Runtime string  `json:"runtime,omitempty"`
	Memory  string  `json:"memory,omitempty"`
	CPUs    float64 `json:"cpus,omitempty"`
	Pids    int64   `json:"pids,omitempty"`
	// Files is what the unit mounts: none (an MCP unit's default), or the
	// project and its own state (a workspace unit's default, and need:
	// the project read-write).
	Files *Files `json:"files,omitempty"`
	// Internet and Allow are the unit's own, through its own sidecar.
	Internet string   `json:"internet,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	// Secrets are the variables an MCP unit's server is given, each a
	// reference the box host resolves when it makes the unit's
	// container: env:VAR, command:LINE or file:/abs/path.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Command is an MCP unit's server, its argv in the unit's container.
	Command []string `json:"command,omitempty"`
	// Exfiltration is ExfiltrationAccepted on a unit with the project
	// and any internet (rule 2).
	Exfiltration string `json:"exfiltration,omitempty"`
}

// Files is what a unit mounts: the project at its own path, read-only
// or read-write, and a state directory the box host keeps for the unit
// at StatePath. Zero is none. In the config it is "none" or an object.
type Files struct {
	Project string `json:"project,omitempty"`
	State   string `json:"state,omitempty"`
}

// StatePath is where a unit's state directory is mounted, and the
// value of DAX_UNIT_STATE in it.
const StatePath = "/state"

// UnmarshalJSON takes "none" or {"project", "state"}, nothing else.
func (f *Files) UnmarshalJSON(b []byte) error {
	var word string
	if json.Unmarshal(b, &word) == nil {
		if word != "none" {
			return fmt.Errorf("files %q: want \"none\" or an object with project and state", word)
		}
		*f = Files{}
		return nil
	}
	type plain Files
	var p plain
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	*f = Files(p)
	return nil
}

// String is f as a status line names it: none, or project ro, state rw.
func (f Files) String() string {
	var parts []string
	if f.Project != "" {
		parts = append(parts, "project "+f.Project)
	}
	if f.State != "" {
		parts = append(parts, "state "+f.State)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// Build makes an image: Base with each of Run run in it, a RUN apiece.
type Build struct {
	Base string   `json:"base"`
	Run  []string `json:"run"`
}

// Tag is the image b makes for unit: dax-unit/<unit>:<the first twelve
// hex digits of the SHA-256 of the declaration>, so a changed
// declaration is another image and an unchanged one is built once.
func (b Build) Tag(unit string) string {
	data, _ := json.Marshal(b)
	sum := sha256.Sum256(data)
	return "dax-unit/" + unit + ":" + hex.EncodeToString(sum[:])[:12]
}

// Dockerfile is b as docker builds it.
func (b Build) Dockerfile() string {
	var sb strings.Builder
	sb.WriteString("FROM " + b.Base + "\n")
	for _, line := range b.Run {
		sb.WriteString("RUN " + line + "\n")
	}
	return sb.String()
}

// Profile is what a session asks for by name: exactly one workspace
// unit, and the MCP units beside it.
type Profile struct {
	Workspace string   `json:"workspace"`
	MCP       []string `json:"mcp,omitempty"`
}

// Request is what a session asks of the box host, under the
// executor's capability: a profile, which of its MCP units, and
// tightenings of its workspace unit.
type Request struct {
	Profile string   `json:"profile,omitempty"`
	MCP     []string `json:"mcp,omitempty"`
	Tighten Tighten  `json:"tighten,omitzero"`
}

// Tighten narrows a profile's workspace unit: smaller limits, less
// internet, fewer hosts. Each field left out keeps the unit's.
type Tighten struct {
	Memory   string   `json:"memory,omitempty"`
	CPUs     float64  `json:"cpus,omitempty"`
	Pids     int64    `json:"pids,omitempty"`
	Internet string   `json:"internet,omitempty"`
	Allow    []string `json:"allow,omitempty"`
}

// Settings are a unit as the box host makes it: resolved and, for the
// workspace, tightened, every field set.
type Settings struct {
	// Profile is the profile it was resolved from; Unit its name.
	Profile, Unit string
	Kind          string
	// Image is the image it runs: the unit's, or Build's tag.
	Image           string
	Build           *Build
	Runtime, Memory string
	CPUs            float64
	Pids            int64
	Files           Files
	Rules           Rules
	Secrets         map[string]string
	Command         []string
	// Decl is a hash of the unit's declaration, so a container made from
	// another is refused.
	Decl string
}

// Resolved is a session's box as the box host makes it: the workspace
// unit, and the session's MCP units in the profile's order.
type Resolved struct {
	Profile   string
	Workspace Settings
	Units     []Settings
}

// Envelope is a unit as the session and its record see it: what it
// runs, what files it has and what it can reach, and the names of its
// secrets, never their values.
type Envelope struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Image    string   `json:"image"`
	Runtime  string   `json:"runtime"`
	Files    string   `json:"files"`
	Internet string   `json:"internet"`
	Allow    []string `json:"allow,omitempty"`
	Secrets  []string `json:"secrets,omitempty"`
}

// BoxInfo is what the box host tells the session it resolved, in its
// answer to the handshake, under the executor's capability as "box".
type BoxInfo struct {
	Profile string     `json:"profile"`
	Units   []Envelope `json:"units"`
}

// BoxInfo is r as the session is told it.
func (r Resolved) BoxInfo() BoxInfo {
	info := BoxInfo{Profile: r.Profile}
	for _, s := range append([]Settings{r.Workspace}, r.Units...) {
		info.Units = append(info.Units, s.Envelope())
	}
	return info
}

// Envelope is s as the session sees it.
func (s Settings) Envelope() Envelope {
	e := Envelope{Name: s.Unit, Kind: s.Kind, Image: s.Image, Runtime: s.Runtime, Files: s.Files.String(), Internet: s.Rules.Internet, Allow: s.Rules.Allow}
	e.Secrets = sortedNames(s.Secrets)
	if len(e.Secrets) == 0 {
		e.Secrets = nil
	}
	return e
}

// LoadHostConfig reads the box host config at path. A file that is not
// there is the built-in config unless explicit, when it is an error;
// a field the config does not know, or a value it refuses, is an error.
func LoadHostConfig(path string, explicit bool) (HostConfig, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		return HostConfig{}, nil
	}
	if err != nil {
		return HostConfig{}, fmt.Errorf("box host config: %w", err)
	}
	var h HostConfig
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&h); err != nil {
		return HostConfig{}, fmt.Errorf("box host config %s: %w", path, err)
	}
	if err := h.Check(); err != nil {
		return HostConfig{}, fmt.Errorf("box host config %s: %w", path, err)
	}
	return h, nil
}

// runtime is the config's default runtime.
func (h HostConfig) runtime() string {
	if h.Runtime != "" {
		return h.Runtime
	}
	return DefaultRuntime
}

// units are the config's, or the built-in workspace unit when it names
// none.
func (h HostConfig) units() map[string]Unit {
	if len(h.Units) == 0 {
		return map[string]Unit{DefaultUnit: {}}
	}
	return h.Units
}

// profiles are the config's, or the built-in one, of the unit named
// DefaultUnit alone, when it names none.
func (h HostConfig) profiles() map[string]Profile {
	if len(h.Profiles) == 0 {
		return map[string]Profile{DefaultProfile: {Workspace: DefaultUnit}}
	}
	return h.Profiles
}

// defaultProfile is the profile a session that names none gets.
func (h HostConfig) defaultProfile() string {
	if h.DefaultProfile != "" {
		return h.DefaultProfile
	}
	ps := h.profiles()
	if len(ps) == 1 {
		for name := range ps {
			return name
		}
	}
	return DefaultProfile
}

// kind is u's kind, its default filled in.
func (u Unit) kind() string {
	switch {
	case u.Kind != "":
		return u.Kind
	case len(u.Command) > 0:
		return KindMCP
	}
	return KindWorkspace
}

// filled is unit name as the box host makes it, the defaults for its
// kind in.
func (h HostConfig) filled(name string, u Unit) Settings {
	kind := u.kind()
	s := Settings{Unit: name, Kind: kind, Image: DefaultImage, Runtime: h.runtime(), Memory: DefaultMemory, CPUs: DefaultCPUs, Pids: DefaultPids, Rules: Rules{Internet: InternetOff}}
	if kind == KindMCP {
		s.Image, s.Memory, s.CPUs, s.Pids = "", DefaultUnitMemory, DefaultUnitCPUs, DefaultUnitPids
	} else {
		s.Files = Files{Project: FilesRW}
	}
	if u.Image != "" {
		s.Image = u.Image
	}
	if u.Build != nil {
		b := *u.Build
		s.Build = &b
		s.Image = b.Tag(name)
	}
	if u.Runtime != "" {
		s.Runtime = u.Runtime
	}
	if u.Memory != "" {
		s.Memory = u.Memory
	}
	if u.CPUs != 0 {
		s.CPUs = u.CPUs
	}
	if u.Pids != 0 {
		s.Pids = u.Pids
	}
	if u.Files != nil {
		s.Files = *u.Files
	}
	switch {
	case u.Internet != "":
		s.Rules.Internet = u.Internet
	case len(u.Allow) > 0:
		s.Rules.Internet = InternetAllow
	}
	if s.Rules.Internet == InternetAllow {
		s.Rules.Allow = lower(u.Allow)
	}
	if len(u.Secrets) > 0 {
		s.Secrets = maps.Clone(u.Secrets)
	}
	s.Command = slices.Clone(u.Command)
	// The declaration, defaults in, without the internet, which a session
	// changes without making the container again; secrets by reference.
	decl, _ := json.Marshal(struct {
		Kind, Image, Runtime, Memory string
		CPUs                         float64
		Pids                         int64
		Files                        Files
		Secrets                      map[string]string
		Command                      []string
	}{s.Kind, s.Image, s.Runtime, s.Memory, s.CPUs, s.Pids, s.Files, s.Secrets, s.Command})
	sum := sha256.Sum256(decl)
	s.Decl = hex.EncodeToString(sum[:])[:16]
	return s
}

// unitName is what a unit may be called: its container's name, its
// network's and its MCP server's are made from it, so lower-case letters,
// digits and inner hyphens, at most 32.
var unitName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Check refuses a config the box host cannot run safely: a value docker
// would misread, a unit past the limits or outside the images, an
// internet that says two things, and a unit that breaks one of the
// three rules (checkUnit).
func (h HostConfig) Check() error {
	if h.Runtime != "" {
		if err := CheckName("runtime", h.Runtime); err != nil {
			return err
		}
	}
	if err := checkLimits("limits", h.Limits); err != nil {
		return err
	}
	for _, p := range h.Images {
		if p == "" {
			return errors.New("images: an empty pattern")
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("images %q: %w", p, err)
		}
	}
	// The built-in unit too, when the config has none: the images and
	// the limits bind it as they bind any.
	us := h.units()
	for _, name := range sortedNames(us) {
		if !unitName.MatchString(name) {
			return fmt.Errorf("units %q: want lower-case letters, digits and inner hyphens, at most 32", name)
		}
		if err := h.checkUnit(name, us[name]); err != nil {
			return err
		}
	}
	ps := h.profiles()
	for _, name := range sortedNames(ps) {
		if err := CheckName("profiles", name); err != nil {
			return err
		}
		if err := h.checkProfile(name, ps[name]); err != nil {
			return err
		}
	}
	if h.DefaultProfile != "" {
		if _, ok := ps[h.DefaultProfile]; !ok {
			return fmt.Errorf("default_profile %q: no such profile", h.DefaultProfile)
		}
	} else if len(ps) > 1 {
		if _, ok := ps[DefaultProfile]; !ok {
			return fmt.Errorf("default_profile: give one, since the profiles are several and none is %q", DefaultProfile)
		}
	}
	return nil
}

func (h HostConfig) checkProfile(name string, p Profile) error {
	field := "profiles." + name
	us := h.units()
	w, ok := us[p.Workspace]
	switch {
	case p.Workspace == "":
		return fmt.Errorf("%s.workspace: name the workspace unit", field)
	case !ok:
		return fmt.Errorf("%s.workspace %q: no such unit", field, p.Workspace)
	case w.kind() != KindWorkspace:
		return fmt.Errorf("%s.workspace %q: a unit of kind %s, not workspace", field, p.Workspace, w.kind())
	}
	seen := map[string]bool{}
	for _, m := range p.MCP {
		u, ok := us[m]
		switch {
		case !ok:
			return fmt.Errorf("%s.mcp %q: no such unit", field, m)
		case u.kind() != KindMCP:
			return fmt.Errorf("%s.mcp %q: a unit of kind %s, not mcp", field, m, u.kind())
		case seen[m]:
			return fmt.Errorf("%s.mcp %q: named twice", field, m)
		}
		seen[m] = true
	}
	return nil
}

// checkUnit refuses a unit docker would misread, one past the limits or
// outside the images, and one that breaks a rule:
//
//  1. a unit with secrets has internet allow or off, never on, so a
//     token reaches only the hosts it is for;
//  2. a unit with the project and any internet says
//     "exfiltration": "accepted", since it is the one way the project's
//     contents leave without the session seeing them;
//  3. a secret is a variable's name and a reference, env:VAR,
//     command:LINE or file:/abs/path, never a value.
func (h HostConfig) checkUnit(name string, u Unit) error {
	field := "units." + name
	kind := u.kind()
	switch kind {
	case KindWorkspace, KindMCP:
	default:
		return fmt.Errorf("%s.kind %q: want workspace or mcp", field, u.Kind)
	}
	switch {
	case u.Image != "" && u.Build != nil:
		return fmt.Errorf("%s: give image or build, not both", field)
	case kind == KindMCP && u.Image == "" && u.Build == nil:
		return fmt.Errorf("%s: give image or build", field)
	case kind == KindMCP && len(u.Command) == 0:
		return fmt.Errorf("%s.command: an MCP unit's server, required", field)
	case kind == KindWorkspace && len(u.Command) > 0:
		return fmt.Errorf("%s.command: a workspace unit runs dax's own tools; command is an MCP unit's", field)
	case kind == KindWorkspace && len(u.Secrets) > 0:
		return fmt.Errorf("%s.secrets: a workspace unit holds no secrets, since its bash would read them; give them to an MCP unit", field)
	}
	for _, a := range u.Command {
		if a == "" || strings.ContainsRune(a, 0) {
			return fmt.Errorf("%s.command: an empty argument", field)
		}
	}
	s := h.filled(name, u)
	if u.Image != "" {
		if err := h.checkImage(field+".image", u.Image); err != nil {
			return err
		}
	} else if u.Build == nil {
		if err := h.checkImage(field+".image", s.Image); err != nil {
			return err
		}
	}
	if b := u.Build; b != nil {
		if err := h.checkImage(field+".build.base", b.Base); err != nil {
			return err
		}
		if len(b.Run) == 0 {
			return fmt.Errorf("%s.build.run: required", field)
		}
		for _, line := range b.Run {
			if strings.TrimSpace(line) == "" || strings.ContainsAny(line, "\n\r\x00") {
				return fmt.Errorf("%s.build.run %q: want one shell line apiece", field, line)
			}
		}
	}
	if u.Runtime != "" {
		if err := CheckName(field+".runtime", u.Runtime); err != nil {
			return err
		}
	}
	if err := checkLimits(field, Limits{Memory: u.Memory, CPUs: u.CPUs, Pids: u.Pids}); err != nil {
		return err
	}
	if err := h.withinLimits(field, s); err != nil {
		return err
	}
	if f := u.Files; f != nil {
		switch f.Project {
		case "", FilesRO, FilesRW:
		default:
			return fmt.Errorf("%s.files.project %q: want ro or rw", field, f.Project)
		}
		switch f.State {
		case "", FilesRW:
		default:
			return fmt.Errorf("%s.files.state %q: want rw", field, f.State)
		}
	}
	if kind == KindWorkspace && s.Files.Project != FilesRW {
		return fmt.Errorf("%s.files: a workspace unit has the project rw, where dax's tools act", field)
	}
	if err := CheckInternet(field, u.Internet, u.Allow); err != nil {
		return err
	}
	for _, v := range sortedNames(u.Secrets) {
		if !envName.MatchString(v) {
			return fmt.Errorf("%s.secrets %q: want a variable's name (rule 3)", field, v)
		}
		if err := checkSecretRef(u.Secrets[v]); err != nil {
			return fmt.Errorf("%s.secrets.%s: %w (rule 3)", field, v, err)
		}
	}
	if len(u.Secrets) > 0 && s.Rules.Internet == InternetOn {
		return fmt.Errorf("%s: secrets with internet on; give allow and the hosts the secrets are for, so a secret cannot be sent anywhere else (rule 1)", field)
	}
	switch u.Exfiltration {
	case "", ExfiltrationAccepted:
	default:
		return fmt.Errorf("%s.exfiltration %q: want %q", field, u.Exfiltration, ExfiltrationAccepted)
	}
	if s.Files.Project != "" && s.Rules.Internet != InternetOff && u.Exfiltration != ExfiltrationAccepted {
		return fmt.Errorf("%s: the project (%s) and internet %s together are a way for the project's contents to leave that the session does not see; say \"exfiltration\": \"accepted\" if that is meant, or take one away (rule 2)", field, s.Files.Project, s.Rules.Internet)
	}
	return nil
}

// checkSecretRef refuses a secret that is not a reference the box host
// resolves: env:VAR, command:LINE or file:/abs/path.
func checkSecretRef(ref string) error {
	kind, rest, ok := strings.Cut(ref, ":")
	switch {
	case !ok || (kind != "env" && kind != "command" && kind != "file"):
		return fmt.Errorf("%q: want env:VAR, command:LINE or file:/abs/path; a secret is never written in the config", ref)
	case kind == "env" && !envName.MatchString(rest):
		return fmt.Errorf("%q: want env: and a variable's name", ref)
	case kind == "command" && (strings.TrimSpace(rest) == "" || strings.ContainsAny(rest, "\n\r\x00")):
		return fmt.Errorf("%q: want command: and one shell line", ref)
	case kind == "file" && (!filepath.IsAbs(rest) || strings.ContainsRune(rest, 0)):
		return fmt.Errorf("%q: want file: and an absolute path", ref)
	}
	return nil
}

// checkImage refuses an image docker would misread, or one the
// config's patterns do not take.
func (h HostConfig) checkImage(field, image string) error {
	if err := CheckImage(field, image); err != nil {
		return err
	}
	if len(h.Images) == 0 {
		return nil
	}
	for _, p := range h.Images {
		if ok, _ := path.Match(p, image); ok {
			return nil
		}
	}
	return fmt.Errorf("%s %q: not among the images this box host takes (%s)", field, image, strings.Join(h.Images, ", "))
}

// withinLimits refuses s past the config's limits.
func (h HostConfig) withinLimits(field string, s Settings) error {
	l := h.Limits
	if l.Memory != "" {
		have, _ := parseSize(s.Memory)
		ceiling, _ := parseSize(l.Memory)
		if have > ceiling {
			return fmt.Errorf("%s: memory %s is more than the limit, %s", field, s.Memory, l.Memory)
		}
	}
	if l.CPUs != 0 && s.CPUs > l.CPUs {
		return fmt.Errorf("%s: cpus %g is more than the limit, %g", field, s.CPUs, l.CPUs)
	}
	if l.Pids != 0 && s.Pids > l.Pids {
		return fmt.Errorf("%s: pids %d is more than the limit, %d", field, s.Pids, l.Pids)
	}
	return nil
}

// Resolve is the box r asks for: its profile's workspace unit, the
// defaults filled in and narrowed by its tightenings, and the MCP units
// it asks for, all of the profile's when it names none. A profile the
// config does not have, a unit the profile does not, and a tightening
// that widens are refused with the reason.
func (h HostConfig) Resolve(r Request) (Resolved, error) {
	name := r.Profile
	if name == "" {
		name = h.defaultProfile()
	}
	p, ok := h.profiles()[name]
	if !ok {
		return Resolved{}, fmt.Errorf("profile %q: this box host has %s", name, strings.Join(sortedNames(h.profiles()), ", "))
	}
	us := h.units()
	s := h.filled(p.Workspace, us[p.Workspace])
	s.Profile = name
	t := r.Tighten
	if err := CheckTighten("tighten", t); err != nil {
		return Resolved{}, err
	}
	if t.Memory != "" {
		want, _ := parseSize(t.Memory)
		have, _ := parseSize(s.Memory)
		if want > have {
			return Resolved{}, fmt.Errorf("tighten.memory %s is more than profile %s's %s; a session only narrows", t.Memory, name, s.Memory)
		}
		s.Memory = t.Memory
	}
	if t.CPUs != 0 {
		if t.CPUs > s.CPUs {
			return Resolved{}, fmt.Errorf("tighten.cpus %g is more than profile %s's %g; a session only narrows", t.CPUs, name, s.CPUs)
		}
		s.CPUs = t.CPUs
	}
	if t.Pids != 0 {
		if t.Pids > s.Pids {
			return Resolved{}, fmt.Errorf("tighten.pids %d is more than profile %s's %d; a session only narrows", t.Pids, name, s.Pids)
		}
		s.Pids = t.Pids
	}
	rules, err := narrow(name, s.Rules, t)
	if err != nil {
		return Resolved{}, err
	}
	s.Rules = rules
	out := Resolved{Profile: name, Workspace: s}
	want := p.MCP
	if len(r.MCP) > 0 {
		for _, m := range r.MCP {
			if !slices.Contains(p.MCP, m) {
				have := "none"
				if len(p.MCP) > 0 {
					have = strings.Join(p.MCP, ", ")
				}
				return Resolved{}, fmt.Errorf("mcp %q: not one of profile %s's MCP units (%s)", m, name, have)
			}
		}
		want = slices.DeleteFunc(slices.Clone(p.MCP), func(m string) bool { return !slices.Contains(r.MCP, m) })
	}
	for _, m := range want {
		u := h.filled(m, us[m])
		u.Profile = name
		out.Units = append(out.Units, u)
	}
	return out, nil
}

// rank orders the modes from less internet to more.
var rank = map[string]int{InternetOff: 0, InternetAllow: 1, InternetOn: 2}

// narrow is have, the profile's internet, as t narrows it.
func narrow(profile string, have Rules, t Tighten) (Rules, error) {
	want := t.Internet
	if want == "" && len(t.Allow) > 0 {
		want = InternetAllow
	}
	if want == "" {
		return have, nil
	}
	if rank[want] > rank[have.Internet] {
		return Rules{}, fmt.Errorf("tighten.internet %s is more than profile %s's %s; a session only narrows", want, profile, have.Internet)
	}
	switch want {
	case InternetOff, InternetOn:
		return Rules{Internet: want}, nil
	}
	hosts := lower(t.Allow)
	if len(hosts) == 0 {
		if have.Internet != InternetAllow {
			return Rules{}, fmt.Errorf("tighten.internet allow: name the hosts, since profile %s's internet is %s", profile, have.Internet)
		}
		return have, nil
	}
	if have.Internet == InternetAllow {
		for _, host := range hosts {
			if !slices.ContainsFunc(have.Allow, func(p string) bool { return covers(p, host) }) {
				return Rules{}, fmt.Errorf("tighten.allow %s is not among profile %s's hosts (%s); a session only narrows", host, profile, strings.Join(have.Allow, ", "))
			}
		}
	}
	return Rules{Internet: InternetAllow, Allow: hosts}, nil
}

// covers reports whether allow entry p takes every host entry h does:
// the same entry, or a *.domain above it.
func covers(p, h string) bool {
	if p == h {
		return true
	}
	d, ok := strings.CutPrefix(p, "*.")
	return ok && strings.HasSuffix(h, "."+d)
}

// The checks the box host's config and a session's settings share.
var (
	imageRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
	nameRef  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sizeRef  = regexp.MustCompile(`^([0-9]+)([bkmgBKMG]?)$`)
	hostRef  = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// CheckImage refuses an image reference docker would misread as an
// argument.
func CheckImage(field, image string) error {
	if !imageRef.MatchString(image) {
		return fmt.Errorf("%s %q: want an image reference (%s)", field, image, DefaultImage)
	}
	return nil
}

// CheckName refuses a name (a runtime's, a profile's, a network's)
// docker or a flag would misread. A runtime is any name docker has
// registered: runsc, io.containerd.kata.v2.
func CheckName(field, name string) error {
	if !nameRef.MatchString(name) {
		return fmt.Errorf("%s %q: want a name of letters, digits, '.', '_' and '-'", field, name)
	}
	return nil
}

// CheckMemory refuses a size docker does not take.
func CheckMemory(field, size string) error {
	if _, err := parseSize(size); err != nil {
		return fmt.Errorf("%s %q: want a size as docker takes it (4g, 512m)", field, size)
	}
	return nil
}

// CheckInternet refuses an internet that says two things, and an allow
// list that is not host names.
func CheckInternet(field, internet string, allow []string) error {
	switch {
	case internet != "" && internet != InternetOff && internet != InternetOn && internet != InternetAllow:
		return fmt.Errorf("%s.internet %q: want off, on or allow", field, internet)
	case internet == InternetAllow && len(allow) == 0:
		return fmt.Errorf("%s.internet allow: %s.allow names no hosts", field, field)
	case (internet == InternetOff || internet == InternetOn) && len(allow) > 0:
		return fmt.Errorf("%s.internet %s with %s.allow: give allow, or leave the hosts out", field, internet, field)
	}
	return CheckAllow(field+".allow", allow)
}

// CheckAllow refuses an allow entry that is not a host name or *. and
// a domain: no address, port or scheme.
func CheckAllow(field string, allow []string) error {
	for _, h := range allow {
		l := strings.ToLower(h)
		if !hostRef.MatchString(l) || net.ParseIP(strings.TrimPrefix(l, "*.")) != nil || strings.Trim(strings.TrimPrefix(l, "*."), "0123456789.") == "" {
			return fmt.Errorf("%s %q: want a host name (proxy.golang.org) or *. and a domain (*.githubusercontent.com); no address, port or scheme", field, h)
		}
	}
	return nil
}

// CheckTighten refuses a tightening whose values are not ones a box
// takes; whether it narrows is Resolve's.
func CheckTighten(field string, t Tighten) error {
	if t.Memory != "" {
		if err := CheckMemory(field+".memory", t.Memory); err != nil {
			return err
		}
	}
	switch {
	case t.CPUs < 0:
		return fmt.Errorf("%s.cpus: want a positive number", field)
	case t.Pids < 0:
		return fmt.Errorf("%s.pids: want a positive number", field)
	case (t.Internet == InternetOff || t.Internet == InternetOn) && len(t.Allow) > 0:
		return fmt.Errorf("%s.internet %s with %s.allow: give allow, or leave the hosts out", field, t.Internet, field)
	case t.Internet != "" && t.Internet != InternetOff && t.Internet != InternetOn && t.Internet != InternetAllow:
		return fmt.Errorf("%s.internet %q: want off, on or allow", field, t.Internet)
	}
	return CheckAllow(field+".allow", t.Allow)
}

func checkLimits(field string, l Limits) error {
	if l.Memory != "" {
		if err := CheckMemory(field+".memory", l.Memory); err != nil {
			return err
		}
	}
	if l.CPUs < 0 {
		return fmt.Errorf("%s.cpus: want a positive number", field)
	}
	if l.Pids < 0 {
		return fmt.Errorf("%s.pids: want a positive number", field)
	}
	return nil
}

// parseSize is a size as docker takes it, in bytes.
func parseSize(s string) (int64, error) {
	m := sizeRef.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	shift := map[string]uint{"": 0, "b": 0, "k": 10, "m": 20, "g": 30}[strings.ToLower(m[2])]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("size %q", s)
	}
	return n << shift, nil
}

func lower(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		out = append(out, strings.ToLower(h))
	}
	return out
}

func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
