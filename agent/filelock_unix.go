//go:build unix

package agent

import (
	"os"
	"syscall"
)

// lockBeside takes an exclusive lock on path+".lock", a private file
// beside path, waiting for another process that holds it, and returns
// what releases it. The lock is flock's, held by the open file, so two
// processes, or two opens in one, exclude each other.
func lockBeside(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
