package extension

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

func TestAQuestionForText(t *testing.T) {
	q := TextQuestion("which colour?")
	if f, ok := AsksForText(q); !ok || f != "answer" {
		t.Fatalf("TextQuestion asks for %q, %v", f, ok)
	}
	for _, tc := range []struct {
		name   string
		schema string
		field  string
	}{
		{"an MCP server's one text field", `{"type":"object","properties":{"branch":{"type":"string"}}}`, "branch"},
		{"a number", `{"type":"object","properties":{"n":{"type":"number"}}}`, ""},
		{"two fields", `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`, ""},
		{"no form", ``, ""},
	} {
		f, ok := AsksForText(agenttool.Elicitation{Message: "m", Schema: json.RawMessage(tc.schema)})
		if f != tc.field || ok != (tc.field != "") {
			t.Errorf("%s: %q, %v", tc.name, f, ok)
		}
	}
	for _, tc := range []struct {
		name string
		a    agenttool.Answer
		text string
		ok   bool
	}{
		{"the form", agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"answer":" blue "}`), Note: "n"}, "blue", true},
		{"a note", agenttool.Answer{Action: agenttool.ActionAccept, Note: "green"}, "green", true},
		{"a bare yes", agenttool.Answer{Action: agenttool.ActionAccept}, "yes", true},
		{"a no", agenttool.Answer{Action: agenttool.ActionDecline, Note: "blue"}, "", false},
		{"nobody", agenttool.Answer{Action: agenttool.ActionCancel}, "", false},
	} {
		text, ok := TextAnswer(q, tc.a)
		if text != tc.text || ok != tc.ok {
			t.Errorf("%s: %q, %v", tc.name, text, ok)
		}
	}
}

func TestSecretsOf(t *testing.T) {
	got := SecretsOf([]Extension{{Secrets: []string{"A", ""}}, {}, {Secrets: []string{"B", "A"}}})
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("SecretsOf = %q", got)
	}
}
