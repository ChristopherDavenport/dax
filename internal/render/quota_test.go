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
			got, ok := Explain(tc.err)
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
	p := &Printer{W: &b}
	p.Handle(context.Background(), &agentturn.RunEnd{Reason: agentturn.ReasonError, Err: httpError(402, openRouterLimit)})
	if out := b.String(); strings.Contains(out, "previous_errors") || !strings.Contains(out, "OpenRouter key's spend limit") {
		t.Errorf("the printer says:\n%s", out)
	}
}
