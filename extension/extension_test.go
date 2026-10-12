package extension

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

func TestAQuestionForText(t *testing.T) {
	q := TextQuestion("which colour?")
	if !IsTextQuestion(q) {
		t.Fatal("a TextQuestion is not one")
	}
	for _, tc := range []struct {
		name   string
		schema string
		want   bool
	}{
		{"the same form, reordered and spaced", `{ "required": ["answer"], "properties": {"answer": {"type": "string"}}, "x-dax": "text-question", "type": "object" }`, true},
		{"an MCP server's one text field", `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`, false},
		{"an enum", `{"type":"object","x-dax":"text-question","properties":{"answer":{"type":"string","enum":["a","b"]}},"required":["answer"]}`, false},
		{"a number", `{"type":"object","properties":{"n":{"type":"number"}}}`, false},
		{"no form", ``, false},
	} {
		if got := IsTextQuestion(agenttool.Elicitation{Message: "m", Schema: json.RawMessage(tc.schema)}); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	foreign := agenttool.Elicitation{Message: "m", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)}
	if text, ok := TextAnswer(foreign, agenttool.Answer{Action: agenttool.ActionAccept}); ok || text != "" {
		t.Errorf("another's form got %q from a bare yes", text)
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
