package agent

import (
	"slices"
	"testing"

	"github.com/ChristopherDavenport/dax/internal/cmdline"
)

// An editor's MCP server's program and arguments reach the server as the
// editor gave them, whatever they hold: the command line dax splits
// reads each back whole. (#167)
func TestAnEditorsServerCommandSplitsBackWhole(t *testing.T) {
	words := []string{"/opt/my tools/server", "--flag=a b", "it's", `"quoted"`, "$HOME", "a;b", ""}
	line := ""
	for i, w := range words {
		if i > 0 {
			line += " "
		}
		line += shellQuote(w)
	}
	got, err := cmdline.Split(line)
	if err != nil || !slices.Equal(got, words) {
		t.Fatalf("Split(%q) = %q, %v; want %q", line, got, err, words)
	}
}
