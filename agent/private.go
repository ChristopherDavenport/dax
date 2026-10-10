package agent

import (
	"io"
	"os"
	"sync"

	"github.com/ChristopherDavenport/agentsession/cas"

	"github.com/ChristopherDavenport/dax/internal/private"
)

// stderr is where dax warns, swapped by CaptureWarnings under
// stderrMu; dax writes to it through warnings.
var (
	stderrMu sync.Mutex
	stderr   io.Writer = os.Stderr
)

// warnings writes to where dax warns at the time of each write, not
// where it warned when the writer was made: the standard error of a
// process started before a front took the screen (the executor's
// launcher) is then held with the front's other warnings.
type warnings struct{}

func (warnings) Write(p []byte) (int, error) {
	stderrMu.Lock()
	w := stderr
	stderrMu.Unlock()
	return w.Write(p)
}

// CaptureWarnings sends the warnings dax writes to w instead of
// standard error until what it returns is called. A front that takes
// the screen (the terminal client) collects them while it opens the
// session, to show before it does, and while it has the screen, to
// show after. They are what dax notes about the store and the session,
// what the extensions dax ships warn about, and what the executor's
// launcher writes to its standard error, cleaned. w may be written to
// from several goroutines at once.
func CaptureWarnings(w io.Writer) (restore func()) {
	stderrMu.Lock()
	old := stderr
	stderr = w
	stderrMu.Unlock()
	undo := private.Capture(w)
	return func() {
		undo()
		stderrMu.Lock()
		stderr = old
		stderrMu.Unlock()
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
