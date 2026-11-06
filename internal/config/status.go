package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/console"
)

// Status is the terminal client's status line, as the user's file
// gives it: where it sits, how it is drawn, its segments in order and
// their colors. Without it the client keeps its default line.
type Status struct {
	// Position is top (the default) or bottom.
	Position string `json:"position,omitempty"`
	// Style is plain (the default) or powerline.
	Style string `json:"style,omitempty"`
	// Segments are the names shown, in order: the console's built-ins
	// (console.BuiltinSegments) and dax's own (StatusHostSegments). Empty
	// is the console's default segments.
	Segments []string `json:"segments,omitempty"`
	// Colors are the segments' colors by name, each a color or, for a
	// numeric segment, thresholds: {"50": "yellow", "80": "red"}.
	Colors map[string]StatusColor `json:"colors,omitempty"`
}

// StatusHostSegments are the segments dax draws itself: who and where
// (user@host), the working directory, the git branch and whether the
// tree is dirty, and where the tools act.
var StatusHostSegments = []string{"user", "dir", "git", "executor"}

// numericSegments are the built-ins a threshold can color.
var numericSegments = []string{"context", "tokens", "cost", "time", "turn", "retry"}

// StatusColor is a segment's color, as the console takes one (an ANSI
// number or #rrggbb), or the colors a numeric segment takes past
// thresholds.
type StatusColor struct {
	Color      string
	Thresholds []StatusThreshold
	// raw are the thresholds as the file gave them, until validation
	// reads them and names the segment in any error.
	raw map[string]string
}

// StatusThreshold is the color a numeric segment takes at At or more.
type StatusThreshold struct {
	At    float64
	Color string
}

// statusColorNames are the colors a config may name, as ANSI numbers.
var statusColorNames = map[string]string{
	"black": "0", "red": "1", "green": "2", "yellow": "3", "blue": "4", "magenta": "5", "cyan": "6", "white": "7",
	"gray": "8", "grey": "8", "bright-black": "8", "bright-red": "9", "bright-green": "10", "bright-yellow": "11",
	"bright-blue": "12", "bright-magenta": "13", "bright-cyan": "14", "bright-white": "15",
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// statusColor is name as the console takes it, or false when it names
// no color: a name above, a number from 0 to 255, or #rrggbb.
func statusColor(name string) (string, bool) {
	if c, ok := statusColorNames[strings.ToLower(name)]; ok {
		return c, true
	}
	if n, err := strconv.Atoi(name); err == nil && n >= 0 && n <= 255 && strconv.Itoa(n) == name {
		return name, true
	}
	if hexColor.MatchString(name) {
		return name, true
	}
	return "", false
}

// UnmarshalJSON takes a color, or an object of thresholds; the values
// are checked by validation, which names the segment.
func (c *StatusColor) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = StatusColor{Color: s}
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("want a color or an object of thresholds: %w", err)
	}
	*c = StatusColor{raw: m}
	return nil
}

// check validates the block and puts its colors in the console's terms.
func (s *Status) check() error {
	switch s.Position {
	case "", string(console.StatusTop), string(console.StatusBottom):
	default:
		return fmt.Errorf("status.position %q: want top or bottom", s.Position)
	}
	switch s.Style {
	case "", string(console.StatusPlain), string(console.StatusPowerline):
	default:
		return fmt.Errorf("status.style %q: want plain or powerline", s.Style)
	}
	known := append(console.BuiltinSegments(), StatusHostSegments...)
	seen := map[string]bool{}
	for _, n := range s.Segments {
		if !slices.Contains(known, n) {
			return fmt.Errorf("status.segments: %q is not a segment; want one of %s", n, strings.Join(known, ", "))
		}
		if seen[n] {
			return fmt.Errorf("status.segments: %q twice", n)
		}
		seen[n] = true
	}
	for _, n := range sortedKeys(s.Colors) {
		if !slices.Contains(known, n) {
			return fmt.Errorf("status.colors: %q is not a segment; want one of %s", n, strings.Join(known, ", "))
		}
		c := s.Colors[n]
		if c.Color != "" {
			v, ok := statusColor(c.Color)
			if !ok {
				return fmt.Errorf("status.colors.%s: %q is not a color; want a name (%s), a number from 0 to 255 or #rrggbb", n, c.Color, strings.Join(sortedKeys(statusColorNames), ", "))
			}
			c.Color = v
		}
		if len(c.raw) > 0 && !slices.Contains(numericSegments, n) {
			return fmt.Errorf("status.colors.%s: thresholds are for context, tokens, cost, time, turn and retry; give a color", n)
		}
		for _, at := range sortedKeys(c.raw) {
			f, err := strconv.ParseFloat(at, 64)
			if err != nil || f < 0 {
				return fmt.Errorf("status.colors.%s: %q is not a number 0 or more", n, at)
			}
			v, ok := statusColor(c.raw[at])
			if !ok {
				return fmt.Errorf("status.colors.%s: %q is not a color; want a name (%s), a number from 0 to 255 or #rrggbb", n, c.raw[at], strings.Join(sortedKeys(statusColorNames), ", "))
			}
			c.Thresholds = append(c.Thresholds, StatusThreshold{At: f, Color: v})
		}
		c.raw = nil
		sort.SliceStable(c.Thresholds, func(i, j int) bool { return c.Thresholds[i].At < c.Thresholds[j].At })
		s.Colors[n] = c
	}
	return nil
}

// Line is the block as the console takes it, with the host's segments
// and what the context and model segments read left to the caller.
func (s *Status) Line() console.StatusLine {
	l := console.StatusLine{Position: console.StatusPosition(s.Position), Style: console.StatusStyle(s.Style), Segments: slices.Clone(s.Segments)}
	if len(s.Colors) > 0 {
		l.Colors = map[string]console.SegmentColor{}
		for n, c := range s.Colors {
			sc := console.SegmentColor{Color: c.Color}
			for _, t := range c.Thresholds {
				sc.Thresholds = append(sc.Thresholds, console.Threshold{At: t.At, Color: t.Color})
			}
			l.Colors[n] = sc
		}
	}
	return l
}

// Shows reports whether the line shows the segment name.
func (s *Status) Shows(name string) bool { return s != nil && slices.Contains(s.Segments, name) }
