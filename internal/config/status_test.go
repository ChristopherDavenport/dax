package config

import (
	"reflect"
	"strings"
	"testing"
)

// The status block is read as the console takes it: names checked
// against the console's built-ins and dax's host segments, colors by
// name, number or #rrggbb, thresholds on the numeric segments only.
func TestTheStatusBlockIsReadAndChecked(t *testing.T) {
	l, err := Parse([]byte(`{"status":{"position":"bottom","style":"powerline",
		"segments":["user","dir","git","model","context","cost"],
		"colors":{"context":{"50":"yellow","80":"red"},"git":"green","dir":"#336699","user":"33"}}}`), "/u/config.json", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Resolve([]Layer{l}, Flags{}, "")
	if err != nil {
		t.Fatal(err)
	}
	st := s.Status
	if st == nil || st.Position != "bottom" || st.Style != "powerline" || !reflect.DeepEqual(st.Segments, []string{"user", "dir", "git", "model", "context", "cost"}) {
		t.Fatalf("status %+v", st)
	}
	want := map[string]StatusColor{
		"context": {Thresholds: []StatusThreshold{{At: 50, Color: "3"}, {At: 80, Color: "1"}}},
		"git":     {Color: "2"},
		"dir":     {Color: "#336699"},
		"user":    {Color: "33"},
	}
	if !reflect.DeepEqual(st.Colors, want) {
		t.Errorf("colors %+v, want %+v", st.Colors, want)
	}
	// No block keeps the console's default line.
	if s, _ := Resolve(nil, Flags{}, ""); s.Status != nil {
		t.Errorf("no status block resolved to %+v", s.Status)
	}
}

func TestABadStatusBlockFailsWithThePath(t *testing.T) {
	for _, tc := range []struct{ name, json, want string }{
		{"unknown segment", `{"status":{"segments":["user","weather"]}}`, `status.segments: "weather" is not a segment`},
		{"unknown color's segment", `{"status":{"colors":{"weather":"red"}}}`, `status.colors: "weather" is not a segment`},
		{"unknown color", `{"status":{"colors":{"git":"chartreuse"}}}`, `status.colors.git: "chartreuse" is not a color`},
		{"bad position", `{"status":{"position":"left"}}`, `status.position "left": want top or bottom`},
		{"bad style", `{"status":{"style":"fancy"}}`, `status.style "fancy": want plain or powerline`},
		{"threshold on a word", `{"status":{"colors":{"git":{"50":"red"}}}}`, `status.colors.git: thresholds are for context, tokens, cost, time, turn and retry`},
		{"threshold not a number", `{"status":{"colors":{"context":{"half":"red"}}}}`, `status.colors.context: "half" is not a number`},
		{"repeated segment", `{"status":{"segments":["git","git"]}}`, `status.segments: "git" twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json), "/u/config.json", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "/u/config.json") {
				t.Fatalf("err %v, want %q with the path", err, tc.want)
			}
		})
	}
	// The status line is the user's: a repository does not set it.
	if _, err := Parse([]byte(`{"status":{"position":"bottom"}}`), "/p/.dax/config.json", true); err == nil || !strings.Contains(err.Error(), "status: a project file may only tighten") {
		t.Fatalf("a project's status block: %v", err)
	}
}
