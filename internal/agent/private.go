package agent

import (
	"fmt"
	"io"
	"os"

	"github.com/ChristopherDavenport/agentsession/cas"
)

// stderr is where dax warns; a variable so tests can read it.
var stderr io.Writer = os.Stderr

// CaptureWarnings sends the warnings dax writes while it opens a store
// and a session to w instead of standard error, and returns what
// undoes it. A front that is about to take the screen (the terminal
// client) collects them to show before it does.
func CaptureWarnings(w io.Writer) (restore func()) {
	old := stderr
	stderr = w
	return func() { stderr = old }
}

// secureDir makes sure path exists and only its owner can enter it.
// What dax keeps there is transcripts, with the contents of every file
// the model read and every command's output, and memory. A missing
// directory is created 0700, with any parents. One that exists and is
// readable by group or others is made private, and the user is told,
// since another account may have read it already.
func secureDir(path string) error {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return os.MkdirAll(path, 0o700)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		if err := os.Chmod(path, mode&^0o077); err != nil {
			return fmt.Errorf("%s is readable by others (mode %04o) and could not be made private: %w", path, mode, err)
		}
		fmt.Fprintf(stderr, "dax: %s was readable by other users (mode %04o); it is now private (%04o)\n", path, mode, mode&^0o077)
	}
	return nil
}

// openStore opens the session store for writing, creating its root
// private.
func openStore(root string, opts ...cas.Option) (*cas.Store, error) {
	if err := secureDir(root); err != nil {
		return nil, err
	}
	return cas.Open(root, opts...)
}
