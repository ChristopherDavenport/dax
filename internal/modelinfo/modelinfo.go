// Package modelinfo asks a provider what a model supports, ahead of the
// first request, and fits each request's reasoning effort to it.
//
// No table of models is kept here: a table lags every release. Each
// vendor that publishes model metadata is asked at run time (Anthropic's
// and Gemini's model endpoints, OpenRouter's catalogue, Ollama's
// /api/show). A vendor that publishes none (OpenAI, a generic Open
// Responses server) describes nothing, and its requests go out as they
// were asked, which is what dex did before this package.
//
// This lives in dex while the shape is being tried; the intent is to
// move Info, Describer and the Streamer to openresponses once it holds.
package modelinfo

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/openresponses"
)

// Support is a capability that a vendor may or may not report.
type Support int

// Support values. Unknown is the zero value: the vendor did not say.
const (
	Unknown Support = iota
	No
	Yes
)

// Info is what a vendor says about one model. Every field's zero value
// means the vendor did not say.
type Info struct {
	// Model is the name that was described.
	Model string
	// Source names who answered, "openrouter" or "anthropic" say; empty
	// when nobody did.
	Source string
	// Reasons says whether the model reasons at all.
	Reasons Support
	// Efforts are the reasoning efforts the model accepts, in [Order];
	// "none" is among them when reasoning can be turned off. Nil is
	// unknown, and a request's effort is then sent as asked.
	Efforts []openresponses.ReasoningEffort
	// DefaultEffort is the effort the model runs at when none is sent.
	DefaultEffort openresponses.ReasoningEffort
	// ContextWindow and MaxOutput are token limits; zero is unknown.
	ContextWindow, MaxOutput int64
}

// Known reports whether any vendor answered for the model.
func (i Info) Known() bool { return i.Source != "" }

// Order ranks the efforts from none to the most. A vendor's effort that
// is not here is left out of Info.Efforts.
var Order = []openresponses.ReasoningEffort{
	openresponses.ReasoningEffortNone,
	openresponses.ReasoningEffortMinimal,
	openresponses.ReasoningEffortLow,
	openresponses.ReasoningEffortMedium,
	openresponses.ReasoningEffortHigh,
	openresponses.ReasoningEffortXHigh,
	"max",
}

func rank(e openresponses.ReasoningEffort) int { return slices.Index(Order, e) }

// sortEfforts keeps the efforts Order knows, once each, in its order.
func sortEfforts(in []openresponses.ReasoningEffort) []openresponses.ReasoningEffort {
	out := make([]openresponses.ReasoningEffort, 0, len(in))
	for _, e := range Order {
		if slices.Contains(in, e) {
			out = append(out, e)
		}
	}
	return out
}

// Fit is the effort to send for a requested one: the request itself
// when the model accepts it or nothing is known, else the accepted
// effort nearest to it, the lower of two equally near. Reasoning asked
// for is never fitted to "none" while the model takes any other effort,
// however near none is. So "none" on a model that always reasons
// becomes its least effort, "low" on one that takes none, high and
// xhigh becomes high, and "low" on one that cannot reason becomes
// "none".
func (i Info) Fit(requested openresponses.ReasoningEffort) openresponses.ReasoningEffort {
	if requested == "" || i.Efforts == nil || slices.Contains(i.Efforts, requested) {
		return requested
	}
	r := rank(requested)
	if r < 0 {
		return requested
	}
	on := slices.ContainsFunc(i.Efforts, func(e openresponses.ReasoningEffort) bool { return e != none && rank(e) >= 0 })
	best, bestDist := requested, len(Order)
	for _, e := range i.Efforts {
		if requested != none && e == none && on {
			continue
		}
		d := rank(e) - r
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = e, d
		}
	}
	return best
}

// String is one line for the banner: what reasoning the model takes and
// its limits, or "" when nothing is known.
func (i Info) String() string {
	if !i.Known() {
		return ""
	}
	var parts []string
	switch {
	case i.Reasons == No:
		parts = append(parts, "no reasoning")
	case len(i.Efforts) > 0:
		on := slices.DeleteFunc(slices.Clone(i.Efforts), func(e openresponses.ReasoningEffort) bool { return e == openresponses.ReasoningEffortNone })
		s := "reasoning "
		switch len(on) {
		case 0:
			s += "off only"
		case 1:
			s += string(on[0])
		default:
			s += string(on[0]) + "–" + string(on[len(on)-1])
		}
		if !slices.Contains(i.Efforts, openresponses.ReasoningEffortNone) && len(on) > 0 {
			s += ", always on"
		}
		parts = append(parts, s)
	case i.Reasons == Yes:
		parts = append(parts, "reasons")
	}
	if i.DefaultEffort != "" {
		parts = append(parts, "default "+string(i.DefaultEffort))
	}
	if i.ContextWindow > 0 {
		parts = append(parts, tokens(i.ContextWindow)+" context")
	}
	if i.MaxOutput > 0 {
		parts = append(parts, tokens(i.MaxOutput)+" out")
	}
	return strings.Join(parts, " · ") + " (" + i.Source + ")"
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

// Describer asks a vendor about a model. An error means the vendor
// could not be asked or does not know the model; the caller then sends
// requests as they were asked.
type Describer interface {
	Describe(ctx context.Context, model string) (Info, error)
}
