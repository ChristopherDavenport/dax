package modelinfo

import (
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

type E = openresponses.ReasoningEffort

func TestFit(t *testing.T) {
	always := []E{"low", "medium", "high", "xhigh", "max"}
	tests := []struct {
		name    string
		efforts []E
		want    E
		got     E
	}{
		{"unknown sends as asked", nil, "low", "low"},
		{"unknown sends none as asked", nil, "none", "none"},
		{"accepted", []E{"none", "low", "high"}, "low", "low"},
		{"always reasons: none becomes the least", always, "none", "low"},
		{"always reasons from minimal", []E{"minimal", "low", "medium", "high"}, "none", "minimal"},
		{"cannot reason: low becomes none", []E{"none"}, "low", "none"},
		{"nearest above", []E{"none", "medium", "high"}, "low", "medium"},
		{"nearest below", []E{"none", "low", "medium"}, "xhigh", "medium"},
		{"a tie goes lower", []E{"minimal", "medium"}, "low", "minimal"},
		{"an effort Order does not know is sent as asked", []E{"none"}, "turbo", "turbo"},
		{"no effort asked", []E{"none"}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Info{Efforts: tc.efforts}).Fit(tc.want); got != tc.got {
				t.Errorf("Fit(%q) over %v = %q, want %q", tc.want, tc.efforts, got, tc.got)
			}
		})
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		info Info
		want string
	}{
		{Info{}, ""},
		{Info{Model: "m"}, ""},
		{Info{Source: "openrouter", Reasons: Yes, Efforts: []E{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "high", ContextWindow: 1_000_000, MaxOutput: 128_000},
			"reasoning low–max, always on · default high · 1M context · 128k out (openrouter)"},
		{Info{Source: "anthropic", Reasons: Yes, Efforts: []E{"none", "low", "high"}, ContextWindow: 200_000}, "reasoning low–high · 200k context (anthropic)"},
		{Info{Source: "ollama", Reasons: No, Efforts: []E{"none"}, ContextWindow: 262_144}, "no reasoning · 262k context (ollama)"},
		{Info{Source: "gemini", Reasons: Yes}, "reasons (gemini)"},
		{Info{Source: "x", Reasons: Yes, Efforts: []E{"high"}}, "reasoning high, always on (x)"},
	}
	for _, tc := range tests {
		if got := tc.info.String(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.info, got, tc.want)
		}
	}
}
