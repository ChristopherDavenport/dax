//go:build unix

package dax

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f, held until f is closed, or
// fails at once: with errLockHeld when another open file holds one, and
// with the system's error otherwise.
func lockFile(f *os.File) error {
	return lockResult(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
}

// lockResult is flock's error as lockFile says it: only "would block"
// means another holds the lock.
func lockResult(err error) error {
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockHeld
	}
	return err
}
