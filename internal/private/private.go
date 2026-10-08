// Package private keeps dax's own directories private and holds the
// writer dax's warnings go to, which a front about to take the screen
// swaps for a buffer (agent.CaptureWarnings). The session and the
// extensions dax ships share both, so a warning about the memory store
// is held back with one about the session store.
package private

import (
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	mu       sync.Mutex
	warnings io.Writer = os.Stderr
)

// Warnings is where a warning goes now.
func Warnings() io.Writer {
	mu.Lock()
	defer mu.Unlock()
	return warnings
}

// Capture sends warnings to w until the returned function is called.
func Capture(w io.Writer) (restore func()) {
	mu.Lock()
	old := warnings
	warnings = w
	mu.Unlock()
	return func() {
		mu.Lock()
		warnings = old
		mu.Unlock()
	}
}

// SecureDir makes sure path exists and only its owner can enter it.
// What dax keeps there is transcripts, with the contents of every file
// the model read and every command's output, and memory. A missing
// directory is created 0700, with any parents. One that exists and is
// readable by group or others is made private, and the user is warned,
// since another account may have read it already.
func SecureDir(path string) error {
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
		fmt.Fprintf(Warnings(), "dax: %s was readable by other users (mode %04o); it is now private (%04o)\n", path, mode, mode&^0o077)
	}
	return nil
}
