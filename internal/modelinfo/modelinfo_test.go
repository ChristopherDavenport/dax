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
		// deepseek-v4-pro on OpenRouter: none, high and xhigh. none and
		// high are as near to low, and reasoning was asked for.
		{"reasoning asked is not turned off", []E{"none", "high", "xhigh"}, "low", "high"},
		{"minimal is not turned off either", []E{"none", "high"}, "minimal", "high"},
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

func TestCompactBudget(t *testing.T) {
	tests := []struct {
		info Info
		want int
	}{
		{Info{}, 0},
		{Info{MaxOutput: 8192}, 0},
		{Info{ContextWindow: 128_000}, 96_000},
		{Info{ContextWindow: 1_048_576, MaxOutput: 943_718}, 786_432},
		{Info{ContextWindow: 200_000, MaxOutput: 64_000}, 150_000},
	}
	for _, tc := range tests {
		if got := tc.info.CompactBudget(); got != tc.want {
			t.Errorf("%+v: %d, want %d", tc.info, got, tc.want)
		}
	}
}
