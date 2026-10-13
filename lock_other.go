//go:build !unix

package dax

import (
	"errors"
	"os"
)

// lockFile refuses: dax serve -a2a keeps its conversations under a
// lock it takes only on a Unix system.
func lockFile(*os.File) error {
	return errors.New("dax serve -a2a needs a Unix system's file locks")
}
