//go:build unix

package dax

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f, held until f is closed, or
// fails at once when another open file holds one.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
