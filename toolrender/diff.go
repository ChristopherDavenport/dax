package toolrender

import "github.com/ChristopherDavenport/agentconsole/toolview"

// op is one line of a diff: kept, removed or added.
type op struct {
	kind byte // ' ', '-' or '+'
	text string
}

// maxDiffCells bounds the table a diff of the changed middle builds;
// past it the middle is shown as removed and then added whole. The
// client lays out every row on each frame of a spinner, so an edit's
// head is diffed that often.
const maxDiffCells = 40_000

// diff is the line diff that takes old to new: the lines they share at
// the start and the end kept, and the middle by its longest common
// subsequence.
func diff(old, new []string) []op {
	pre := 0
	for pre < len(old) && pre < len(new) && old[pre] == new[pre] {
		pre++
	}
	suf := 0
	for suf < len(old)-pre && suf < len(new)-pre && old[len(old)-1-suf] == new[len(new)-1-suf] {
		suf++
	}
	ops := make([]op, 0, len(old)+len(new))
	for _, l := range old[:pre] {
		ops = append(ops, op{' ', l})
	}
	ops = append(ops, middle(old[pre:len(old)-suf], new[pre:len(new)-suf])...)
	for _, l := range old[len(old)-suf:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

// middle diffs a and b, which share no first or last line.
func middle(a, b []string) []op {
	var ops []op
	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			ops = append(ops, op{'-', l})
		}
		for _, l := range b {
			ops = append(ops, op{'+', l})
		}
		return ops
	}
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] > lcs[i+1][j]):
			ops = append(ops, op{'+', b[j]})
			j++
		default:
			ops = append(ops, op{'-', a[i]})
			i++
		}
	}
	return ops
}

// counts are the lines ops adds and removes.
func counts(ops []op) (added, removed int) {
	for _, o := range ops {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// diffLines draws ops. Collapsed, the kept lines at either end are cut
// to one, so the change itself fills the room, and the whole is cut to
// collapsedLines with a note of what is left.
func diffLines(ops []op, expanded bool) []toolview.Line {
	if !expanded {
		first, last := 0, len(ops)
		for first < last && ops[first].kind == ' ' {
			first++
		}
		for last > first && ops[last-1].kind == ' ' {
			last--
		}
		ops = ops[max(first-1, 0):min(last+1, len(ops))]
	}
	out := make([]toolview.Line, 0, len(ops))
	for i, o := range ops {
		if !expanded && i == collapsedLines {
			out = append(out, more(len(ops)-i))
			break
		}
		text := o.text
		if !expanded {
			text = clip(text)
		}
		role := toolview.Dim
		switch o.kind {
		case '+':
			role = toolview.Added
		case '-':
			role = toolview.Removed
		}
		out = append(out, toolview.Line{toolview.S(role, string(o.kind)+" "+text)})
	}
	return out
}
