package cmd

import "os"

// dieOfSignal has nothing to do on plan9: it has notes rather than signals,
// so there is neither a signal to re-raise nor a 128 + signum convention to
// approximate, and syscall.Signal does not exist to name one. rdsh is not
// supported here — this only keeps the tree building, as on Windows.
func dieOfSignal(os.Signal) int {
	return 1
}
