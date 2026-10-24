package box

// This file is the box host's config: the boxes `dax box serve` makes
// on its machine, at most how large and with which internet, and what
// a session may ask of them, which only ever narrows.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The built-in profile: a box host with no config makes this box.
const (
	DefaultProfile = "default"
	DefaultImage   = "golang:1.26-bookworm"
	DefaultRuntime = "runsc"
	DefaultMemory  = "4g"
	DefaultCPUs    = 2
	DefaultPids    = 512
)

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
	// Runtime is the OCI runtime a profile that names none runs under;
	// any runtime docker has registered (runsc, io.containerd.kata.v2).
	Runtime string `json:"runtime,omitempty"`
	// Limits are ceilings no profile and no tightening goes past.
	Limits Limits `json:"limits,omitzero"`
	// Images are glob patterns (path.Match) every image a profile or an
	// MCP server names must match; none means any.
	Images []string `json:"images,omitempty"`
	// DefaultProfile is the profile a session that names none gets.
	DefaultProfile string `json:"default_profile,omitempty"`
	// Profiles are the boxes the box host makes, by name.
	Profiles map[string]Profile `json:"profiles,omitempty"`
	// MCP is the catalog of MCP servers, each run in a box of its own
	// (step 2 of docs/plans/proposals/box-host.md; read and checked,
	// not yet run).
	MCP map[string]MCPServer `json:"mcp,omitempty"`
}

// Limits bound a box. A zero field is no bound.
type Limits struct {
	Memory string  `json:"memory,omitempty"`
	CPUs   float64 `json:"cpus,omitempty"`
	Pids   int64   `json:"pids,omitempty"`
}

// Profile is a box the box host makes. A field left out takes the
// built-in default; Runtime, the config's.
type Profile struct {
	Image    string   `json:"image,omitempty"`
	Runtime  string   `json:"runtime,omitempty"`
	Memory   string   `json:"memory,omitempty"`
	CPUs     float64  `json:"cpus,omitempty"`
	Pids     int64    `json:"pids,omitempty"`
	Internet string   `json:"internet,omitempty"`
	Allow    []string `json:"allow,omitempty"`
}

// MCPServer is a catalog entry: an MCP server run in a box of its own,
// with its secrets resolved on the box host into that box alone.
type MCPServer struct {
	// Image the server's box runs, or Build to make one.
	Image string `json:"image,omitempty"`
	Build *Build `json:"build,omitempty"`
	// Command is the server's command line in its box.
	Command string `json:"command"`
	// Secrets are the variables the server is given, each a reference
	// the box host resolves: env:NAME, command:LINE or file:PATH.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Internet and Allow are the server's own, through its own proxy.
	Internet string   `json:"internet,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	// Files is its access to the project: none (the default), ro or rw.
	Files string `json:"files,omitempty"`
}

// Build makes an image: Base with Run run in it.
type Build struct {
	Base string `json:"base"`
	Run  string `json:"run"`
}

// Request is what a session asks of the box host, under the
// executor's capability: a profile, the catalog's MCP servers, and
// tightenings.
type Request struct {
	Profile string   `json:"profile,omitempty"`
	MCP     []string `json:"mcp,omitempty"`
	Tighten Tighten  `json:"tighten,omitzero"`
}

// Tighten narrows a profile: smaller limits, less internet, fewer
// hosts. Each field left out keeps the profile's.
type Tighten struct {
	Memory   string   `json:"memory,omitempty"`
	CPUs     float64  `json:"cpus,omitempty"`
	Pids     int64    `json:"pids,omitempty"`
	Internet string   `json:"internet,omitempty"`
	Allow    []string `json:"allow,omitempty"`
}

// Settings are a box as the box host makes it: a profile resolved and
// tightened, every field set.
type Settings struct {
	Profile                string
	Image, Runtime, Memory string
	CPUs                   float64
	Pids                   int64
	Rules                  Rules
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

// profiles are the config's, or the built-in one when it names none.
func (h HostConfig) profiles() map[string]Profile {
	if len(h.Profiles) == 0 {
		return map[string]Profile{DefaultProfile: {}}
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

// filled is p with the defaults in.
func (h HostConfig) filled(p Profile) Settings {
	s := Settings{Image: DefaultImage, Runtime: h.runtime(), Memory: DefaultMemory, CPUs: DefaultCPUs, Pids: DefaultPids, Rules: Rules{Internet: InternetOff}}
	if p.Image != "" {
		s.Image = p.Image
	}
	if p.Runtime != "" {
		s.Runtime = p.Runtime
	}
	if p.Memory != "" {
		s.Memory = p.Memory
	}
	if p.CPUs != 0 {
		s.CPUs = p.CPUs
	}
	if p.Pids != 0 {
		s.Pids = p.Pids
	}
	switch {
	case p.Internet != "":
		s.Rules.Internet = p.Internet
	case len(p.Allow) > 0:
		s.Rules.Internet = InternetAllow
	}
	if s.Rules.Internet == InternetAllow {
		s.Rules.Allow = lower(p.Allow)
	}
	return s
}

// Check refuses a config the box host cannot run safely: a value docker
// would misread, a profile past the limits or outside the images, an
// internet that says two things, a catalog entry without what it needs.
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
	// The built-in profile too, when the config has none: the images and
	// the limits bound it as they bind any.
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
		if _, ok := h.profiles()[h.DefaultProfile]; !ok {
			return fmt.Errorf("default_profile %q: no such profile", h.DefaultProfile)
		}
	} else if ps := h.profiles(); len(ps) > 1 {
		if _, ok := ps[DefaultProfile]; !ok {
			return fmt.Errorf("default_profile: give one, since the profiles are several and none is %q", DefaultProfile)
		}
	}
	for _, name := range sortedNames(h.MCP) {
		if err := CheckName("mcp", name); err != nil {
			return err
		}
		if err := h.checkMCP(name, h.MCP[name]); err != nil {
			return err
		}
	}
	return nil
}

func (h HostConfig) checkProfile(name string, p Profile) error {
	field := "profiles." + name
	if err := h.checkImage(field+".image", h.filled(p).Image); err != nil {
		return err
	}
	if p.Runtime != "" {
		if err := CheckName(field+".runtime", p.Runtime); err != nil {
			return err
		}
	}
	if err := checkLimits(field, Limits{Memory: p.Memory, CPUs: p.CPUs, Pids: p.Pids}); err != nil {
		return err
	}
	if err := CheckInternet(field, p.Internet, p.Allow); err != nil {
		return err
	}
	s := h.filled(p)
	return h.withinLimits(field, s)
}

func (h HostConfig) checkMCP(name string, m MCPServer) error {
	field := "mcp." + name
	switch {
	case strings.TrimSpace(m.Command) == "":
		return fmt.Errorf("%s.command: required", field)
	case m.Image != "" && m.Build != nil:
		return fmt.Errorf("%s: give image or build, not both", field)
	case m.Image == "" && m.Build == nil:
		return fmt.Errorf("%s: give image or build", field)
	}
	if m.Image != "" {
		if err := h.checkImage(field+".image", m.Image); err != nil {
			return err
		}
	}
	if b := m.Build; b != nil {
		if err := h.checkImage(field+".build.base", b.Base); err != nil {
			return err
		}
		if strings.TrimSpace(b.Run) == "" {
			return fmt.Errorf("%s.build.run: required", field)
		}
	}
	for _, v := range sortedNames(m.Secrets) {
		if !envName.MatchString(v) {
			return fmt.Errorf("%s.secrets %q: want a variable's name", field, v)
		}
		ref := m.Secrets[v]
		kind, rest, ok := strings.Cut(ref, ":")
		if !ok || strings.TrimSpace(rest) == "" || (kind != "env" && kind != "command" && kind != "file") {
			return fmt.Errorf("%s.secrets.%s %q: want env:NAME, command:LINE or file:PATH; a secret is never written in the config", field, v, ref)
		}
	}
	if err := CheckInternet(field, m.Internet, m.Allow); err != nil {
		return err
	}
	switch m.Files {
	case "", "none", "ro", "rw":
	default:
		return fmt.Errorf("%s.files %q: want none, ro or rw", field, m.Files)
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

// Resolve is the box r asks for: its profile, the defaults filled in,
// narrowed by its tightenings. A profile the config does not have, a
// tightening that widens, and an MCP server (not yet run in a box) are
// refused with the reason.
func (h HostConfig) Resolve(r Request) (Settings, error) {
	if len(r.MCP) > 0 {
		return Settings{}, fmt.Errorf("MCP servers in a box are not run yet (step 2 of dax's docs/plans/proposals/box-host.md); this session asks for %s", strings.Join(r.MCP, ", "))
	}
	name := r.Profile
	if name == "" {
		name = h.defaultProfile()
	}
	p, ok := h.profiles()[name]
	if !ok {
		return Settings{}, fmt.Errorf("profile %q: this box host has %s", name, strings.Join(sortedNames(h.profiles()), ", "))
	}
	s := h.filled(p)
	s.Profile = name
	t := r.Tighten
	if err := CheckTighten("tighten", t); err != nil {
		return Settings{}, err
	}
	if t.Memory != "" {
		want, _ := parseSize(t.Memory)
		have, _ := parseSize(s.Memory)
		if want > have {
			return Settings{}, fmt.Errorf("tighten.memory %s is more than profile %s's %s; a session only narrows", t.Memory, name, s.Memory)
		}
		s.Memory = t.Memory
	}
	if t.CPUs != 0 {
		if t.CPUs > s.CPUs {
			return Settings{}, fmt.Errorf("tighten.cpus %g is more than profile %s's %g; a session only narrows", t.CPUs, name, s.CPUs)
		}
		s.CPUs = t.CPUs
	}
	if t.Pids != 0 {
		if t.Pids > s.Pids {
			return Settings{}, fmt.Errorf("tighten.pids %d is more than profile %s's %d; a session only narrows", t.Pids, name, s.Pids)
		}
		s.Pids = t.Pids
	}
	rules, err := narrow(name, s.Rules, t)
	if err != nil {
		return Settings{}, err
	}
	s.Rules = rules
	return s, nil
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
