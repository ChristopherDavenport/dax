package config

import (
	"strings"
	"testing"
)

// The permission mode: the user's default_mode, a project's only when
// stricter, the -mode flag over both, -no-policy its old name.
func TestTheModesPrecedence(t *testing.T) {
	user := func(m string) Layer {
		l, err := Parse([]byte(`{"policy":{"default_mode":"`+m+`"}}`), "/u/config.json", false)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	project := func(m string) Layer {
		l, err := Parse([]byte(`{"policy":{"default_mode":"`+m+`"}}`), "/p/.dax/config.json", true)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	mode := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		layers []Layer
		flags  Flags
		want   string
	}{
		{"none", nil, Flags{}, "default"},
		{"the user's", []Layer{user("acceptEdits")}, Flags{}, "acceptEdits"},
		{"a name in any case", []Layer{user("BYPASS")}, Flags{}, "bypassPermissions"},
		{"a stricter project's", []Layer{user("acceptEdits"), project("dontAsk")}, Flags{}, "dontAsk"},
		{"a project's default over the user's bypass", []Layer{user("bypassPermissions"), project("default")}, Flags{}, "default"},
		{"a project's default is not stricter than dontAsk", []Layer{user("dontAsk"), project("default")}, Flags{}, "dontAsk"},
		{"the flag over both", []Layer{user("acceptEdits"), project("dontAsk")}, Flags{Mode: mode("bypassPermissions")}, "bypassPermissions"},
		{"-no-policy", nil, Flags{NoPolicy: true}, "bypassPermissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Resolve(tc.layers, tc.flags, "")
			if err != nil {
				t.Fatal(err)
			}
			if s.Policy.Mode != tc.want {
				t.Errorf("mode %q, want %q", s.Policy.Mode, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		json    string
		project bool
		want    string
	}{
		{`{"policy":{"default_mode":"planning"}}`, false, `policy.default_mode: mode "planning": want one of default, acceptEdits, plan, dontAsk, bypassPermissions`},
		{`{"policy":{"default_mode":"acceptEdits"}}`, true, `policy.default_mode "acceptEdits": a project file may only pick a stricter mode`},
		{`{"policy":{"default_mode":"bypassPermissions"}}`, true, "a project file may only pick a stricter mode"},
	} {
		if _, err := Parse([]byte(tc.json), "/x/config.json", tc.project); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.json, err, tc.want)
		}
	}
	// A project may start its sessions in plan mode, which only
	// tightens (#147); and plan_model and act_model are the user's.
	if l, err := Parse([]byte(`{"policy":{"default_mode":"plan"}}`), "/p/.dax/config.json", true); err != nil {
		t.Errorf("a project's default_mode plan: %v", err)
	} else if s, _ := Resolve([]Layer{user("default"), l}, Flags{}, ""); s.Policy.Mode != "plan" {
		t.Errorf("a project's plan resolved to %q", s.Policy.Mode)
	}
	if _, err := Parse([]byte(`{"plan_model":"big"}`), "/p/.dax/config.json", true); err == nil || !strings.Contains(err.Error(), "plan_model: a project file may only tighten") {
		t.Errorf("a project's plan_model: %v", err)
	}
	if l, err := Parse([]byte(`{"plan_model":"big","act_model":"fast"}`), "/u/config.json", false); err != nil {
		t.Fatal(err)
	} else if s, _ := Resolve([]Layer{l}, Flags{}, ""); s.PlanModel != "big" || s.ActModel != "fast" {
		t.Errorf("plan_model %q act_model %q", s.PlanModel, s.ActModel)
	}
	// The policy's sources name the files they came from.
	s, err := Resolve([]Layer{user("default"), project("dontAsk")}, Flags{}, "")
	if err != nil || s.Policy.UserPath != "/u/config.json" || s.Policy.ProjectPath != "/p/.dax/config.json" {
		t.Errorf("paths %q %q, %v", s.Policy.UserPath, s.Policy.ProjectPath, err)
	}
	if _, err := Resolve(nil, Flags{Mode: mode("dontask")}, ""); err != nil {
		t.Errorf("-mode dontask: %v", err)
	}
	if _, err := Resolve(nil, Flags{Mode: mode("x")}, ""); err == nil || !strings.Contains(err.Error(), "-mode") {
		t.Errorf("-mode x: %v", err)
	}
	if _, err := Resolve(nil, Flags{Mode: mode("default"), NoPolicy: true}, ""); err == nil {
		t.Error("-no-policy with -mode was taken")
	}
}
