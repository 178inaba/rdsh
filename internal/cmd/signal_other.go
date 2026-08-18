//go:build !unix && !plan9

package cmd

import (
	"os"
	"syscall"
)

// dieOfSignal is the stand-in for platforms without syscall.Kill (Windows
// is the one that matters: rdsh is distributed through `go install` alone,
// so the tree has to keep building there). Nothing can be re-raised, so the
// run exits normally with the closest approximation — the status a shell
// reports for a process the signal had killed.
func dieOfSignal(sig os.Signal) int {
	s, ok := sig.(syscall.Signal)
	if !ok {
		// Unreachable: every signal interruptSignals lists is one.
		return 1
	}
	return 128 + int(s)
}
