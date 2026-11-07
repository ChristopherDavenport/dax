package render

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// openRouterLimit is OpenRouter's 402 for a key past its daily limit,
// its key ID replaced (dax#15).
const openRouterLimit = `{"error":{"message":"Prompt tokens limit exceeded: 162733 > 121537. To increase, visit https://openrouter.ai/workspaces/default/keys/KEY and adjust the key's daily limit","code":402,"metadata":{"limit_source":"openrouter_key_limit","remedy_hint":"Raise or remove this API key's usage limit at https://openrouter.ai/settings/keys, or use a key without a limit.","provider_name":null,"previous_errors":[{"code":402,"message":"This request requires more credits"}]}}}`

func httpError(status int, body string) error {
	return openresponses.ErrorFromResponse(&http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))})
}

// A request the key's or account's credit cannot cover is explained in
// words, with the provider's message and remedy, not as its raw JSON;
// any other error is not explained (dax#15, openresponses v0.0.19).
func TestExplainSaysAQuotaRefusalInWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string // in the explanation; empty for none
	}{
		{"OpenRouter's key limit", fmt.Errorf("agentturn: model: %w", httpError(402, openRouterLimit)), []string{
			"the OpenRouter key's spend limit cannot cover this request",
			"not retried",
			"Prompt tokens limit exceeded: 162733 > 121537",
			"Raise or remove this API key's usage limit at https://openrouter.ai/settings/keys",
			"/compact",
		}},
		{"OpenAI's insufficient_quota", httpError(429, `{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}`), []string{
			"the key's or account's credit or spend limit cannot cover this request",
			"You exceeded your current quota",
		}},
		{"a rate limit", httpError(429, `{"error":{"type":"too_many_requests","message":"slow down"}}`), nil},
		{"none", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Explain(tc.err, true)
			if ok != (len(tc.want) > 0) {
				t.Fatalf("Explain = %q, %v", got, ok)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("the explanation lacks %q:\n%s", w, got)
				}
			}
			if strings.Contains(got, `{"error"`) || strings.Contains(got, "previous_errors") {
				t.Errorf("the explanation carries the raw JSON:\n%s", got)
			}
		})
	}
}

// The print fronts say a quota refusal in words where they printed the
// error.
func TestThePrinterExplainsAQuotaRefusal(t *testing.T) {
	var b bytes.Buffer
	p := &Printer{W: &b, Compact: true}
	p.Handle(context.Background(), &agentturn.RunEnd{Reason: agentturn.ReasonError, Err: httpError(402, openRouterLimit)})
	if out := b.String(); strings.Contains(out, "previous_errors") || !strings.Contains(out, "OpenRouter key's spend limit") {
		t.Errorf("the printer says:\n%s", out)
	}
}

// A body that is not the spec's envelope and says no message of its own
// leaves the adapter's message standing; a front with no /compact does
// not suggest it (the -p printer, dax's exit line).
func TestExplainKeepsTheAdaptersMessageAndNamesOnlyAnOfferedCommand(t *testing.T) {
	err := &openresponses.Error{StatusCode: 402, Code: openresponses.CodeInsufficientQuota, Message: "the adapter says the key has no credit", Body: []byte(`{"error":{"code":402}}`)}
	got, ok := Explain(err, true)
	if !ok || !strings.Contains(got, "the adapter says the key has no credit") {
		t.Errorf("Explain = %q, %v; want the adapter's message", got, ok)
	}
	if !strings.Contains(got, "/compact") {
		t.Errorf("a front with /compact is not told of it: %q", got)
	}
	// A message that is itself a body with no message of its own is not
	// shown raw.
	raw := &openresponses.Error{StatusCode: 402, Code: openresponses.CodeInsufficientQuota, Message: `{"error":{"code":402}}`}
	if got, _ := Explain(raw, true); strings.Contains(got, `{"error"`) {
		t.Errorf("the explanation carries the raw JSON: %q", got)
	}
	if got, _ := Explain(err, false); strings.Contains(got, "/compact") {
		t.Errorf("a front without /compact is told of it: %q", got)
	}
	var b bytes.Buffer
	(&Printer{W: &b}).Handle(context.Background(), &agentturn.RunEnd{Reason: agentturn.ReasonError, Err: err})
	if strings.Contains(b.String(), "/compact") {
		t.Errorf("the -p printer names /compact:\n%s", b.String())
	}
	b.Reset()
	(&Printer{W: &b, Compact: true}).Handle(context.Background(), &agentturn.RunEnd{Reason: agentturn.ReasonError, Err: err})
	if !strings.Contains(b.String(), "/compact") {
		t.Errorf("the REPL's printer does not name /compact:\n%s", b.String())
	}
}
