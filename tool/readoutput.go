package tool

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// ReadOutputArgs are read_output's arguments.
type ReadOutputArgs struct {
	Ref     string `json:"ref" desc:"The output's reference, sidecar:sha256:..., as a cut result names it"`
	Offset  int    `json:"offset,omitempty" desc:"The first line to show, from 1 (default 1)"`
	Limit   int    `json:"limit,omitempty" desc:"How many lines to show (default 200, at most 2000)"`
	Pattern string `json:"pattern,omitempty" desc:"A regular expression: show only the lines that match it, numbered"`
}

const (
	defaultOutputLines = 200
	maxOutputLines     = 2000
)

// ReadOutput returns read_output: the whole of a tool result the
// session cut to its cap and kept, read through get (the session's own
// outputs alone), as numbered lines or the lines matching a pattern. It
// runs where the session does, not in the workspace.
func ReadOutput(get func(ctx context.Context, ref string) ([]byte, error)) agenttool.Tool {
	return exactArgs[ReadOutputArgs](agenttool.New("read_output",
		"Read the whole output of a tool call whose result was cut, by the sidecar:sha256:... reference the cut result names: numbered lines from offset, or the lines matching a pattern.",
		func(ctx context.Context, in ReadOutputArgs) (string, error) {
			if strings.TrimSpace(in.Ref) == "" {
				return "", errors.New("ref is required: the sidecar:sha256:... a cut result names")
			}
			data, err := get(ctx, in.Ref)
			if err != nil {
				return "", err
			}
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			limit := in.Limit
			if limit <= 0 {
				limit = defaultOutputLines
			}
			limit = min(limit, maxOutputLines)
			var b strings.Builder
			if in.Pattern != "" {
				re, err := regexp.Compile(in.Pattern)
				if err != nil {
					return "", fmt.Errorf("pattern: %w", err)
				}
				n := 0
				for i, l := range lines {
					if re.MatchString(l) {
						if n == limit {
							fmt.Fprintf(&b, "[more matches; narrow the pattern]\n")
							break
						}
						fmt.Fprintf(&b, "%6d\t%s\n", i+1, truncate(l, maxLineBytes))
						n++
					}
				}
				if n == 0 {
					return fmt.Sprintf("no line of the %d matches", len(lines)), nil
				}
				return strings.TrimRight(b.String(), "\n"), nil
			}
			from := max(in.Offset, 1)
			if from > len(lines) {
				return fmt.Sprintf("the output has %d lines", len(lines)), nil
			}
			to := min(from-1+limit, len(lines))
			for i := from - 1; i < to; i++ {
				fmt.Fprintf(&b, "%6d\t%s\n", i+1, truncate(lines[i], maxLineBytes))
			}
			fmt.Fprintf(&b, "[lines %d-%d of %d]", from, to, len(lines))
			return b.String(), nil
		}, agenttool.WithAnnotations(agenttool.Annotations{ReadOnly: true})))
}
