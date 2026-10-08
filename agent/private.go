package agent

import (
	"io"
	"os"

	"github.com/ChristopherDavenport/agentsession/cas"

	"github.com/ChristopherDavenport/dax/internal/private"
)

// stderr is where dax warns; a variable so tests can read it.
var stderr io.Writer = os.Stderr

// CaptureWarnings sends the warnings dax writes while it opens a store
// and a session to w instead of standard error, and returns what
// undoes it. A front that is about to take the screen (the terminal
// client) collects them to show before it does. The extensions dax
// ships warn through the same writer.
func CaptureWarnings(w io.Writer) (restore func()) {
	old := stderr
	stderr = w
	undo := private.Capture(w)
	return func() {
		undo()
		stderr = old
	}
}

// openStore opens the session store for writing, creating its root
// private.
func openStore(root string, opts ...cas.Option) (*cas.Store, error) {
	if err := private.SecureDir(root); err != nil {
		return nil, err
	}
	return cas.Open(root, opts...)
}
