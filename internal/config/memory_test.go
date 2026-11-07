package config

import (
	"strings"
	"testing"
)

// The background memory pass is the person's setting: their config sets
// it, -memory-background overrides it, the defaults fill the rest, and a
// project's file may not turn it on. (#160)
func TestTheBackgroundMemoryPassIsTheUsersAlone(t *testing.T) {
	if s, err := Resolve(nil, Flags{}, ""); err != nil || s.Memory != (Memory{Background: "off", BackgroundEvery: 1, BackgroundMaxTokens: 8000, BackgroundTrivialTokens: 500}) {
		t.Fatalf("the defaults: %+v, %v", s.Memory, err)
	}
	l, err := Parse([]byte(`{"memory":{"background":"auto","background_every":3,"background_max_tokens":4000}}`), "/u/config.json", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Resolve([]Layer{l}, Flags{}, "")
	if err != nil || s.Memory != (Memory{Background: "auto", BackgroundEvery: 3, BackgroundMaxTokens: 4000, BackgroundTrivialTokens: 500}) {
		t.Fatalf("the user's setting: %+v, %v", s.Memory, err)
	}
	propose := "propose"
	if s, _ := Resolve([]Layer{l}, Flags{MemoryBackground: &propose}, ""); s.Memory.Background != "propose" {
		t.Errorf("-memory-background propose resolved to %q", s.Memory.Background)
	}
	bad := "always"
	if _, err := Resolve(nil, Flags{MemoryBackground: &bad}, ""); err == nil || !strings.Contains(err.Error(), "want off, propose or auto") {
		t.Errorf("-memory-background always: %v", err)
	}
	if _, err := Parse([]byte(`{"memory":{"background":"auto"}}`), "/p/.dax/config.json", true); err == nil || !strings.Contains(err.Error(), "memory: a project file may only tighten") {
		t.Errorf("a project's memory block: %v", err)
	}
	for _, j := range []string{`{"memory":{"background":"sometimes"}}`, `{"memory":{"background_every":-1}}`, `{"memory":{"when":"idle"}}`} {
		if _, err := Parse([]byte(j), "/u/config.json", false); err == nil {
			t.Errorf("%s parsed", j)
		}
	}
}
