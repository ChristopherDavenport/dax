//go:build !unix

package hooks

import "os/exec"

// ownGroup leaves cmd as it is where there are no process groups: its
// cancellation kills the hook's own process alone.
func ownGroup(*exec.Cmd) {}
