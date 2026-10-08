// Command dax is a coding agent: a terminal client, a REPL, or one
// prompt with -p, over an Open Responses model, with read, write, edit,
// glob, grep, ls and bash tools under a policy, recording every session.
// It is dax.Main with no options; a program built on dax is this file
// with options of its own.
package main

import (
	"context"
	"os"

	"github.com/ChristopherDavenport/dax"
)

func main() {
	os.Exit(dax.Main(context.Background(), os.Args[1:]))
}
