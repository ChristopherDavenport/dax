//go:build !unix

package agent

// lockBeside takes no lock between processes on a system without
// flock: a save is still atomic (a temporary file renamed), and two
// processes saving at once may lose one grant, signed in for again.
func lockBeside(string) (func(), error) { return func() {}, nil }
