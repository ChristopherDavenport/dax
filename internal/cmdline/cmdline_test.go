package cmdline

import (
	"slices"
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   \t\n", nil},
		{"docker exec -i box dax execute -root /work", []string{"docker", "exec", "-i", "box", "dax", "execute", "-root", "/work"}},
		{"  a   b  ", []string{"a", "b"}},
		// Quotes keep a space in a word, and join what they touch.
		{`dax execute -root "/my work"`, []string{"dax", "execute", "-root", "/my work"}},
		{`dax execute -root '/my work'`, []string{"dax", "execute", "-root", "/my work"}},
		{`-root=/my\ work`, []string{"-root=/my work"}},
		{`a"b c"d 'e'f`, []string{"ab cd", "ef"}},
		// An ssh command whose remote side is a shell's: the inner quotes
		// reach it as written.
		{`ssh -T host bash -lc 'exec dax execute -root "/home/me/my app"'`, []string{"ssh", "-T", "host", "bash", "-lc", `exec dax execute -root "/home/me/my app"`}},
		// Empty quotes are an empty word.
		{`a '' "" b`, []string{"a", "", "", "b"}},
		// Inside single quotes nothing is special.
		{`'a\b "c" $X'`, []string{`a\b "c" $X`}},
		// Inside double quotes a backslash escapes " \ $ ` and a
		// newline, and is itself before anything else.
		{`"a\"b\\c\$d\` + "`" + `e\nf"`, []string{"a\"b\\c$d`e\\nf"}},
		{"\"a\\\nb\"", []string{"ab"}},
		// Outside quotes a backslash makes the next character itself,
		// and a backslash and newline are dropped.
		{`a\'b \"c\\`, []string{`a'b`, `"c\`}},
		{"a\\\nb", []string{"ab"}},
		// Nothing is expanded or interpreted.
		{`echo $HOME ${PWD} $(id) ` + "`id`" + ` ~ ~/x *.go a|b ; c && d > e`, []string{"echo", "$HOME", "${PWD}", "$(id)", "`id`", "~", "~/x", "*.go", "a|b", ";", "c", "&&", "d", ">", "e"}},
		{"héllo wörld", []string{"héllo", "wörld"}},
	} {
		got, err := Split(tc.in)
		if err != nil {
			t.Errorf("Split(%q): %v", tc.in, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Split(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitRefusesWhatIsNotClosed(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`a 'b`, "single quote"},
		{`a "b`, "double quote"},
		{`a "b\"`, "double quote"},
		{`a \`, "backslash"},
	} {
		if got, err := Split(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Split(%q) = %q, %v; want an error about a %s", tc.in, got, err, tc.want)
		}
	}
}
