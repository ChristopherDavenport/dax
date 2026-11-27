package dax

import (
	"testing"

	"github.com/ChristopherDavenport/dax/policy"
)

// TestASpaceAfterACommandListsItsSubcommands: the space after a command
// with subcommands lists them before any is typed, and Tab puts the
// chosen one in the input. The keys are sent once the list is drawn and
// none changes the word, so they act on the list waited for: add, then
// remove. Its help is on no list of the first word.
func TestASpaceAfterACommandListsItsSubcommands(t *testing.T) {
	r := startTUI(t, &steps{}, policy.Rules{}, nil)
	r.type_("/mcp ")
	r.waitOutput("stop an MCP server")
	r.type_("\x1b[B\t")
	r.type_("\r")
	r.waitOutput("usage: /mcp remove <label>")
	r.quit()
}
